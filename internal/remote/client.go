package remote

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type clientFile struct {
	HostName    string    `json:"host_name"`
	Addresses   []string  `json:"addresses"`
	Port        int       `json:"port"`
	Fingerprint string    `json:"fingerprint"`
	DeviceID    string    `json:"device_id"`
	Token       string    `json:"token"`
	PairedAt    time.Time `json:"paired_at"`
	// LocalOnly keeps the pairing but stops relying on the host: this
	// Switcher uses its own accounts and routing until it is turned off.
	LocalOnly bool `json:"local_only,omitempty"`
}

// ClientStatus describes the host this Switcher is paired with, if any.
// Connected means paired; LocalOnly means paired but not relying on the host.
type ClientStatus struct {
	Connected bool      `json:"connected"`
	LocalOnly bool      `json:"local_only,omitempty"`
	HostName  string    `json:"host_name,omitempty"`
	Address   string    `json:"address,omitempty"`
	Reachable bool      `json:"reachable"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// Client forwards this Switcher's accounts and provider traffic to a host.
type Client struct {
	dir           string
	managementKey string
	mu            sync.Mutex
	cfg           *clientFile
	transport     *http.Transport
	lastGood      string
	reachable     bool
	lastErr       string
	checkedAt     time.Time
	hostState     map[string]any
	hostRead      bool // the last read of hostState succeeded
	onChange      func(bool)
	refreshedAt   time.Time
	tailnet       func(ctx context.Context, address string) (net.Conn, bool, error)
	// hostInstalling is set while this Mac waits on the host's install.
	hostInstalling atomic.Bool
}

// UseTailnet reaches tailnet addresses through the Tailscale add-on.
func (c *Client) UseTailnet(dial func(ctx context.Context, address string) (net.Conn, bool, error)) {
	c.mu.Lock()
	c.tailnet = dial
	c.mu.Unlock()
}

// NewClient loads a saved pairing from dir. managementKey authenticates this
// Switcher's own T3 Code hub requests before they are forwarded.
func NewClient(dir, managementKey string) *Client {
	c := &Client{dir: dir, managementKey: managementKey}
	var cfg clientFile
	if err := readJSON(c.path(), &cfg); err == nil && cfg.Token != "" && cfg.Fingerprint != "" && len(cfg.Addresses) > 0 {
		c.setLocked(&cfg)
	}
	return c
}

func (c *Client) path() string { return filepath.Join(c.dir, "client.json") }

// OnChange is called with true when this Switcher starts using a host and
// false when it stops (disconnected, or local only), including once now for
// the saved state.
func (c *Client) OnChange(fn func(connected bool)) {
	c.mu.Lock()
	c.onChange = fn
	connected := c.usingHostLocked()
	c.mu.Unlock()
	fn(connected)
}

func (c *Client) usingHostLocked() bool { return c.cfg != nil && !c.cfg.LocalOnly }

func (c *Client) setLocked(cfg *clientFile) {
	c.cfg = cfg
	c.transport = nil
	c.hostState, c.hostRead = nil, false
	c.lastGood = ""
	if cfg != nil {
		c.transport = c.newTransport(cfg)
	}
}

func (c *Client) newTransport(cfg *clientFile) *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         c.dial,
		TLSClientConfig:     pinned(cfg.Fingerprint),
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		// Notice a connection that died with sleep or a network change.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 5 * time.Second},
	}
}

// pinned accepts exactly the host certificate that was paired.
func pinned(fp string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || fingerprint(cs.PeerCertificates[0].Raw) != fp {
				return errors.New("this is not the paired Switcher host")
			}
			return nil
		}}
}

// dial races the host's addresses, the one that worked last first and the
// others shortly after, so moving between home Wi-Fi and Tailscale needs no
// reconfiguration and a stale address costs no waiting.
func (c *Client) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	c.mu.Lock()
	if c.cfg == nil {
		c.mu.Unlock()
		return nil, errors.New("no Switcher host connected")
	}
	port := c.cfg.Port
	viaTailnet := c.tailnet
	addresses := append([]string(nil), c.cfg.Addresses...)
	if c.lastGood != "" {
		addresses = append([]string{c.lastGood}, addresses...)
	}
	c.mu.Unlock()
	var unique []string
	seen := map[string]bool{}
	for _, a := range addresses {
		if !seen[a] {
			seen[a] = true
			unique = append(unique, a)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	type result struct {
		conn    net.Conn
		address string
		err     error
	}
	results := make(chan result, len(unique))
	for i, address := range unique {
		go func(i int, address string) {
			select {
			case <-time.After(time.Duration(i) * 250 * time.Millisecond):
			case <-ctx.Done():
				results <- result{err: ctx.Err()}
				return
			}
			target := net.JoinHostPort(address, strconv.Itoa(port))
			var conn net.Conn
			var err error
			handled := false
			if viaTailnet != nil {
				conn, handled, err = viaTailnet(ctx, target)
			}
			if !handled {
				d := net.Dialer{KeepAlive: 30 * time.Second}
				conn, err = d.DialContext(ctx, network, target)
			}
			results <- result{conn, address, err}
		}(i, address)
	}
	var lastErr error
	for range unique {
		r := <-results
		if r.err == nil {
			cancel()
			// Close the slower winners that still arrive.
			go func(left int) {
				for ; left > 0; left-- {
					if late := <-results; late.conn != nil {
						late.conn.Close()
					}
				}
			}(len(unique) - 1)
			c.mu.Lock()
			c.lastGood = r.address
			c.mu.Unlock()
			return r.conn, nil
		}
		lastErr = r.err
	}
	return nil, fmt.Errorf("the Switcher host is unreachable: %w", lastErr)
}

// Connected reports whether this Switcher uses a host right now: paired, and
// not set to work locally.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usingHostLocked()
}

// Paired reports whether a host is paired, whether or not it is in use.
func (c *Client) Paired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg != nil
}

// SetLocalOnly stops relying on the paired host (true) or uses it again
// (false), without unpairing. The choice survives restarts. While local
// only, nothing is forwarded and this Switcher refreshes its own accounts.
func (c *Client) SetLocalOnly(on bool) (ClientStatus, error) {
	c.mu.Lock()
	if c.cfg == nil {
		c.mu.Unlock()
		return ClientStatus{}, errors.New("this Switcher is not paired with a host")
	}
	if c.cfg.LocalOnly == on {
		c.mu.Unlock()
		return c.Status(), nil
	}
	updated := *c.cfg
	updated.LocalOnly = on
	if err := writeJSON(c.path(), &updated); err != nil {
		c.mu.Unlock()
		return ClientStatus{}, err
	}
	c.cfg = &updated
	if on && c.transport != nil {
		// Requests already on their way finish; nothing new goes out.
		c.transport.CloseIdleConnections()
	}
	if !on {
		c.reachable, c.lastErr = false, ""
	}
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(!on)
	}
	return c.Status(), nil
}

// Status reports the connected host and whether it answered recently.
func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil {
		return ClientStatus{}
	}
	address := c.lastGood
	if address == "" {
		address = c.cfg.Addresses[0]
	}
	return ClientStatus{Connected: true, LocalOnly: c.cfg.LocalOnly, HostName: c.cfg.HostName, Address: address, Reachable: c.reachable, Error: c.lastErr, CheckedAt: c.checkedAt}
}

// Connect pairs with the host at address using the code it shows.
func (c *Client) Connect(ctx context.Context, address string, port int, code, deviceName string) (ClientStatus, error) {
	address = strings.TrimSpace(address)
	if host, p, err := net.SplitHostPort(address); err == nil {
		address = host
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	if address == "" {
		return ClientStatus{}, errors.New("enter the host's address")
	}
	if port == 0 {
		port = DefaultPort
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Tailscale addresses go through the add-on when it runs.
	c.mu.Lock()
	viaTailnet := c.tailnet
	c.mu.Unlock()
	dialRaw := func(ctx context.Context, _, addr string) (net.Conn, error) {
		if viaTailnet != nil {
			if conn, handled, err := viaTailnet(ctx, addr); handled {
				return conn, err
			}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	}
	// Read the host's certificate, then prove the code against exactly it.
	var fp string
	raw, err := dialRaw(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return ClientStatus{}, fmt.Errorf("could not reach a Switcher at %s: is sharing turned on there?", address)
	}
	conn := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return ClientStatus{}, fmt.Errorf("could not reach a Switcher at %s: is sharing turned on there?", address)
	}
	if certs := conn.ConnectionState().PeerCertificates; len(certs) > 0 {
		fp = fingerprint(certs[0].Raw)
	}
	conn.Close()
	if fp == "" {
		return ClientStatus{}, errors.New("the host did not present a certificate")
	}
	cfg := &clientFile{Addresses: []string{address}, Port: port, Fingerprint: fp}
	key, err := pairingKey(code, fp)
	if err != nil {
		return ClientStatus{}, err
	}
	nonce := randomHex(16)
	body, _ := json.Marshal(pairRequest{Name: deviceName, Nonce: nonce, Proof: pairingProof(key, "client", nonce)})
	transport := &http.Transport{Proxy: nil, DialContext: dialRaw, TLSClientConfig: pinned(fp)}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+net.JoinHostPort(address, strconv.Itoa(port))+"/remote/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return ClientStatus{}, fmt.Errorf("pairing failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = "the host refused the pairing"
		}
		return ClientStatus{}, errors.New(e.Error)
	}
	var paired pairResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&paired); err != nil || paired.Token == "" {
		return ClientStatus{}, errors.New("the host sent an invalid pairing reply")
	}
	// Only a host that knows the code can answer this; anything else is a
	// device pretending to be the host.
	if !hmac.Equal([]byte(paired.HostProof), []byte(pairingProof(key, "host", nonce))) {
		return ClientStatus{}, errors.New("this is not the Switcher you meant to pair with; check the code and address")
	}
	cfg.HostName, cfg.DeviceID, cfg.Token, cfg.PairedAt = paired.HostName, paired.DeviceID, paired.Token, time.Now().UTC()
	for _, a := range paired.Addresses {
		if a != address {
			cfg.Addresses = append(cfg.Addresses, a)
		}
	}
	// Saved under the lock, so an address refresh for an older pairing
	// cannot overwrite it.
	c.mu.Lock()
	if err := writeJSON(c.path(), cfg); err != nil {
		c.mu.Unlock()
		return ClientStatus{}, err
	}
	c.setLocked(cfg)
	c.lastGood, c.reachable, c.lastErr, c.checkedAt = address, true, "", time.Now()
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(true)
	}
	return c.Status(), nil
}

// Disconnect forgets the host; this Switcher uses its own accounts again.
func (c *Client) Disconnect() error {
	c.mu.Lock()
	if err := os.Remove(c.path()); err != nil && !os.IsNotExist(err) {
		c.mu.Unlock()
		return err
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	c.setLocked(nil)
	c.reachable, c.lastErr = false, ""
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(false)
	}
	return nil
}

func (c *Client) note(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = time.Now()
	c.reachable = err == nil
	c.lastErr = ""
	if err != nil {
		c.lastErr = err.Error()
	}
}

// do sends one request to the host as this paired device.
func (c *Client) do(req *http.Request) (*http.Response, error) { return c.send(req, false) }

// send is do; explicit requests, such as updating the host, reach it also
// while this Mac works without it, since they are not routing.
func (c *Client) send(req *http.Request, explicit bool) (*http.Response, error) {
	c.mu.Lock()
	cfg, transport := c.cfg, c.transport
	c.mu.Unlock()
	if cfg == nil {
		return nil, errors.New("no Switcher host connected")
	}
	if cfg.LocalOnly && !explicit {
		return nil, errors.New("this Switcher is set to work without its host")
	}
	req.URL.Scheme, req.URL.Host, req.Host = "https", "switcher-host", "switcher-host"
	req.Header.Set(DeviceHeader, cfg.Token)
	resp, err := transport.RoundTrip(req)
	if explicit {
		return resp, err // says nothing about routing to the host
	}
	c.note(err)
	if err == nil && resp.Header.Get(rejectedHeader) != "" {
		c.note(errors.New("the host no longer accepts this Mac; pair again"))
	}
	return resp, err
}

// HostUpdate is the paired host's Switcher version and update state.
// Remote says the host takes updates from here: Switcher 1.21.0 and later.
type HostUpdate struct {
	Host      string           `json:"host"`
	Current   string           `json:"current,omitempty"`
	Latest    string           `json:"latest,omitempty"`
	Available bool             `json:"update_available"`
	Remote    bool             `json:"remote_update"`
	Install   *HostUpdateSteps `json:"install,omitempty"`
}

// remoteUpdateSince is the first release a paired Mac can update.
const remoteUpdateSince = "1.21.0"

// atLeast compares release versions such as "1.21.0" or "v1.21.0".
func atLeast(version, min string) bool {
	parse := func(v string) (out [3]int, ok bool) {
		parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
		if len(parts) != 3 {
			return out, false
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	a, okA := parse(version)
	b, okB := parse(min)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// plainText keeps what a host says printable, for this Mac's terminal and
// dialogs.
func plainText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return -1
		}
		return r
	}, s)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// HostUpdateSteps is the host's install in flight.
type HostUpdateSteps struct {
	Step     string  `json:"step"`
	Fraction float64 `json:"fraction"`
}

// hostVersion accepts only version strings, so a host's answer never puts
// markup into this Mac's menu or dashboard.
var hostVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// installSteps are the steps a host's install reports, and "updating" while
// this Mac waits on an install it asked for.
var installSteps = map[string]bool{"downloading": true, "checking": true, "installing": true, "restarting": true, "updating": true}

func (u *HostUpdate) sanitize() {
	if !hostVersion.MatchString(u.Current) {
		u.Current = ""
	}
	u.Remote = u.Remote && u.Current != ""
	if !hostVersion.MatchString(u.Latest) {
		u.Latest, u.Available = "", false
	}
	if u.Install != nil && !installSteps[u.Install.Step] {
		u.Install = nil
	}
	if u.Install != nil {
		u.Install.Fraction = min(max(u.Install.Fraction, 0), 1)
	}
}

// HostUpdate asks the paired host which Switcher it runs and whether an
// update is out. Action "check" has it check for one first; "install" has
// it install the newest release and restart, which ends with the host
// unreachable for a moment. It works while this Mac works without the host
// too: an update may be what the host needs.
func (c *Client) HostUpdate(ctx context.Context, action string) (HostUpdate, error) {
	host := c.Status().HostName
	call := func(method, path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, "https://switcher-host"+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.send(req, true)
		if err == nil && resp.StatusCode != http.StatusOK {
			var answer struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
			resp.Body.Close()
			if answer.Error = plainText(answer.Error); answer.Error == "" {
				answer.Error = fmt.Sprintf("the host answered %d", resp.StatusCode)
			}
			return nil, &hostError{resp.StatusCode, answer.Error}
		}
		return resp, err
	}
	// A host older than remoteUpdateSince keeps its update routes from
	// paired devices: it shows its version, to be updated on it once.
	older := func(err error) (HostUpdate, error) {
		var he *hostError
		if errors.As(err, &he) && he.status == http.StatusNotFound {
			u := HostUpdate{Host: host}
			c.mu.Lock()
			u.Current, _ = c.hostState["version"].(string)
			if update, ok := c.hostState["update"].(map[string]any); ok {
				u.Latest, _ = update["latest"].(string)
				u.Available, _ = update["update_available"].(bool)
			}
			c.mu.Unlock()
			u.sanitize()
			return u, nil
		}
		return HostUpdate{Host: host}, err
	}
	switch action {
	case "check":
		resp, err := call(http.MethodPost, "/api/update/check")
		if err != nil {
			return older(err)
		}
		resp.Body.Close()
	case "install":
		if !c.hostInstalling.CompareAndSwap(false, true) {
			return HostUpdate{Host: host}, errors.New(host + " is already updating")
		}
		defer c.hostInstalling.Store(false)
		resp, err := call(http.MethodPost, "/api/update")
		if err != nil {
			// The host restarts into the new release, which may cut this
			// request off; anything else is a real failure.
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !strings.Contains(err.Error(), "connection reset") {
				return HostUpdate{Host: host}, err
			}
		} else {
			resp.Body.Close()
		}
		return HostUpdate{Host: host, Install: &HostUpdateSteps{Step: "restarting", Fraction: 1}}, nil
	case "":
	default:
		return HostUpdate{Host: host}, fmt.Errorf("unknown host update action %q", action)
	}
	resp, err := call(http.MethodGet, "/api/update")
	if err != nil {
		return older(err)
	}
	defer resp.Body.Close()
	var u HostUpdate
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&u); err != nil {
		return HostUpdate{Host: host}, fmt.Errorf("read the host's update state: %w", err)
	}
	u.Host, u.Remote = host, true
	c.markInstalling(&u)
	u.sanitize()
	return u, nil
}

// markInstalling shows an install this Mac is waiting on, which the host's
// own state reports only on its update route.
func (c *Client) markInstalling(u *HostUpdate) {
	if u.Install == nil && c.hostInstalling.Load() {
		u.Install = &HostUpdateSteps{Step: "updating"}
	}
}

// hostError is the host's answer to a request it refused.
type hostError struct {
	status  int
	message string
}

func (e *hostError) Error() string { return e.message }

// Inference sends a Claude request to the host. It implements the Desktop
// relay's remote hook.
func (c *Client) Inference(ctx context.Context, account string, r *http.Request, body []byte) (*http.Response, bool, error) {
	if !c.Connected() {
		return nil, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, "https://switcher-host/remote/anthropic"+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return nil, true, err
	}
	req.URL.RawQuery = r.URL.RawQuery
	req.Header = r.Header.Clone()
	// The host sends its own account's credential; this Mac's login stays here.
	for _, key := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		req.Header.Del(key)
	}
	req.Header.Set(AccountHeader, account)
	resp, err := c.do(req)
	if err != nil {
		return nil, true, errors.New("Switcher could not reach the Switcher host this Mac is connected to")
	}
	if resp.Header.Get(rejectedHeader) != "" {
		resp.Body.Close()
		return nil, true, errors.New("the Switcher host no longer accepts this Mac; pair again in Switcher")
	}
	return resp, true, nil
}

// forwardedPaths go to the host while connected; everything else, including
// this Mac's settings, CLI setup and usage logs, stays local. So does
// waiting for /api/state to change: this Mac's state merges its own settings
// into the host's, so this Switcher watches it itself.
var forwardedPaths = []string{"/api/accounts/", "/api/usage/refresh", "/api/providers/", "/v0/management/",
	"/codex/", "/claude/", "/grok/", "/opencode/", "/antigravity/", "/gemini/", "/copilot/"}

func forwarded(path string) bool {
	for _, prefix := range forwardedPaths {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// LocalState supplies this Mac's own fields of /api/state: version, update,
// Desktop relay, hub address and display preferences.
type LocalState func(r *http.Request) map[string]any

// localStateKeys stay this Mac's own in the merged /api/state.
var localStateKeys = []string{"version", "update", "desktop_relay", "hub_url", "hub_management_key",
	"menu_usage_bars", "reset_notifications", "compact_accounts", "merge_accounts", "auto_switch", "awake",
	"platform", "handed_back", "autostart"}

// Middleware sends account and provider traffic to the host while connected.
func (c *Client) Middleware(local LocalState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Connected() {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
			c.serveState(w, r, local)
		case strings.HasPrefix(r.URL.Path, "/api/login") || r.URL.Path == "/api/accounts" && r.Method == http.MethodPost:
			writeError(w, http.StatusConflict, "accounts are added and signed in on "+c.Status().HostName)
		case forwarded(r.URL.Path):
			if strings.HasPrefix(r.URL.Path, "/v0/management/") && !c.localManagementKey(r) {
				writeError(w, http.StatusUnauthorized, "management key required")
				return
			}
			c.proxy(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (c *Client) localManagementKey(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Management-Key")
	}
	return c.managementKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(c.managementKey)) == 1
}

// ownCredentials are this Mac's, never the host's to see: the device token
// (which also unlocks this Mac's LAN listener), the management key, the
// browser session and the CLIs' own logins. The host answers with its own
// accounts and never reads them.
var ownCredentials = []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key", "ChatGPT-Account-Id",
	"X-Management-Key", "Cookie", "X-Switcher-CSRF"}

func (c *Client) proxy(w http.ResponseWriter, r *http.Request) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "https", "switcher-host"
			for _, name := range ownCredentials {
				pr.Out.Header.Del(name)
			}
		},
		Transport:     roundTripFunc(c.do),
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() == nil {
				writeError(w, http.StatusBadGateway, "the Switcher host "+c.Status().HostName+" is unreachable")
			}
		},
	}
	rp.ServeHTTP(w, r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serveState shows the host's accounts with this Mac's own settings. While the
// host is unreachable the last host state is shown with a warning.
func (c *Client) serveState(w http.ResponseWriter, r *http.Request, local LocalState) {
	writeJSONResponse(w, http.StatusOK, c.State(r, local, true))
}

// readHostState asks the host for its state and keeps it for State. On a
// failure it answers the last state read, and false.
func (c *Client) readHostState(ctx context.Context) (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://switcher-host/api/state", nil)
	var state map[string]any
	resp, err := c.do(req)
	if err == nil {
		switch {
		case resp.StatusCode == http.StatusOK:
			err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&state)
		case resp.Header.Get(rejectedHeader) != "":
			err = errors.New("the host no longer accepts this Mac; pair again")
		default:
			err = fmt.Errorf("the host answered %d", resp.StatusCode)
		}
		resp.Body.Close()
		if err != nil {
			c.note(err)
		}
	}
	if err == nil {
		sanitizeHostState(state)
	}
	c.mu.Lock()
	if err == nil {
		c.hostState = state
	} else {
		state = c.hostState
	}
	c.hostRead = err == nil
	c.mu.Unlock()
	if err == nil {
		go c.refreshAddresses()
	}
	return state, err == nil
}

// State is /api/state as this Switcher shows it while it uses its host: the
// host's accounts with this Mac's own settings. With read false it reuses the
// host state read last, for a watcher the host itself tells about changes;
// it still asks the host when it has read nothing yet.
func (c *Client) State(r *http.Request, local LocalState, read bool) map[string]any {
	c.mu.Lock()
	state, ok := c.hostState, c.hostRead
	c.mu.Unlock()
	if read || state == nil {
		state, ok = c.readHostState(r.Context())
	}
	if state == nil {
		state = map[string]any{"accounts": []any{}, "active": map[string]any{}}
	}
	merged := make(map[string]any, len(state)+4)
	for k, v := range state {
		merged[k] = v
	}
	// The host's own version and update, before this Mac's replace them,
	// so the menus can offer to update the host.
	hostUpdate := HostUpdate{Host: c.Status().HostName}
	hostUpdate.Current, _ = state["version"].(string)
	if u, ok := state["update"].(map[string]any); ok {
		hostUpdate.Latest, _ = u["latest"].(string)
		hostUpdate.Available, _ = u["update_available"].(bool)
		if p, ok := u["install"].(map[string]any); ok {
			step, _ := p["step"].(string)
			fraction, _ := p["fraction"].(float64)
			hostUpdate.Install = &HostUpdateSteps{Step: step, Fraction: fraction}
		}
	}
	hostUpdate.Remote = atLeast(hostUpdate.Current, remoteUpdateSince)
	c.markInstalling(&hostUpdate)
	hostUpdate.sanitize()

	mine := local(r)
	for _, k := range localStateKeys {
		if v, ok := mine[k]; ok {
			merged[k] = v
		} else {
			delete(merged, k)
		}
	}
	status := c.Status()
	merged["remote"] = map[string]any{"role": "client", "host": status.HostName, "address": status.Address, "connected": ok, "error": status.Error,
		"host_update": hostUpdate}
	return merged
}

// ErrHostCannotWait means the host is a Switcher from before it could say
// when its accounts change; this Switcher then reads its state now and then.
var ErrHostCannotWait = errors.New("the host cannot say when its accounts change")

// WaitHostChange waits until the host's state differs from since, the etag
// it answered last ("" for none), or until the host's own limit of about 25
// seconds, and answers the host's etag. A wait says nothing about routing to
// the host, so it marks the host neither reachable nor unreachable; reading
// its state does that.
func (c *Client) WaitHostChange(ctx context.Context, since string) (string, bool, error) {
	if !c.Connected() {
		return "", false, errors.New("this Switcher does not use a host")
	}
	// A host that never answers cannot hold the wait forever.
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://switcher-host/api/state/changes?since="+url.QueryEscape(since), nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.send(req, true)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", false, ErrHostCannotWait
	case resp.StatusCode != http.StatusOK:
		return "", false, fmt.Errorf("the host answered %d", resp.StatusCode)
	}
	var answer struct {
		ETag    string `json:"etag"`
		Changed bool   `json:"changed"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&answer) != nil || answer.ETag == "" || len(answer.ETag) > 128 {
		return "", false, errors.New("the host sent an invalid answer")
	}
	return answer.ETag, answer.Changed, nil
}

// sanitizeHostState runs once, before the host's state is shared with other
// requests. The host's own Claude Code login does not apply here: this Mac
// uses the host's selected account, like any other provider. This Mac's
// dashboard and menu put account and provider ids into markup and request
// paths, so only ids shaped like the ones Switcher generates are kept.
func sanitizeHostState(state map[string]any) {
	accounts, _ := state["accounts"].([]any)
	kept := make([]any, 0, len(accounts))
	for _, a := range accounts {
		if account, ok := a.(map[string]any); ok && validID(account["id"]) && validID(account["provider"]) {
			delete(account, "native_switch_available")
			delete(account, "native_active")
			kept = append(kept, account)
		}
	}
	state["accounts"] = kept
	for _, key := range []string{"order", "hidden"} {
		if list, ok := state[key].([]any); ok {
			ids := make([]any, 0, len(list))
			for _, id := range list {
				if validID(id) {
					ids = append(ids, id)
				}
			}
			state[key] = ids
		} else {
			delete(state, key)
		}
	}
	active, _ := state["active"].(map[string]any)
	selected := map[string]any{}
	for provider, id := range active {
		if validID(provider) && validID(id) {
			selected[provider] = id
		}
	}
	state["active"] = selected
	delete(state, "claude_code")
}

// validID accepts only the identifiers Switcher itself generates.
func validID(v any) bool {
	id, ok := v.(string)
	if !ok || id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// HostAccount reports the provider of one of the host's accounts, from the
// last host state this Mac saw.
func (c *Client) HostAccount(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	accounts, _ := c.hostState["accounts"].([]any)
	for _, a := range accounts {
		if account, ok := a.(map[string]any); ok && account["id"] == id {
			provider, _ := account["provider"].(string)
			return provider, true
		}
	}
	return "", false
}

// refreshAddresses learns the host's current addresses, at most every two
// minutes, so a host that later joins Tailscale or changes its LAN address
// stays reachable without pairing again.
func (c *Client) refreshAddresses() {
	c.mu.Lock()
	if !c.usingHostLocked() || time.Since(c.refreshedAt) < 2*time.Minute {
		c.mu.Unlock()
		return
	}
	c.refreshedAt = time.Now()
	pairing := c.cfg
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://switcher-host/remote/hello", nil)
	resp, err := c.do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var h hello
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&h) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg != pairing {
		return // disconnected or paired elsewhere meanwhile
	}
	var merged []string
	seen := map[string]bool{}
	for _, a := range append(h.Addresses, c.cfg.Addresses...) {
		if a != "" && !seen[a] && len(merged) < 12 {
			seen[a] = true
			merged = append(merged, a)
		}
	}
	updated := *c.cfg
	updated.Addresses = merged
	if h.Name != "" {
		updated.HostName = h.Name
	}
	if strings.Join(updated.Addresses, ",") == strings.Join(c.cfg.Addresses, ",") && updated.HostName == c.cfg.HostName {
		return
	}
	if writeJSON(c.path(), &updated) == nil {
		c.cfg = &updated
	}
}

func isNotExist(err error) bool { return os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) }
