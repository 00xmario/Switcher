package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The "Away from home" add-on: Switcher downloads switcher-tailnet only when
// the user turns it on, so the app itself carries no Tailscale code.
const tailnetRelease = "https://github.com/00xmario/switcher/releases/download"

// addonSHA256 is the released add-on's digest for this build's platform, set
// by the release workflow with -ldflags. Release builds trust only this; other
// builds fall back to the release's .sha256 file.
var addonSHA256 string

// TailnetStatus describes the add-on for the UI.
type TailnetStatus struct {
	Installed   bool          `json:"installed"`
	Enabled     bool          `json:"enabled"`
	Running     bool          `json:"running"`
	Downloading bool          `json:"downloading,omitempty"`
	State       string        `json:"state,omitempty"`
	AuthURL     string        `json:"auth_url,omitempty"`
	Tailnet     string        `json:"tailnet,omitempty"`
	Name        string        `json:"name,omitempty"`
	DNSName     string        `json:"dns_name,omitempty"`
	IP          string        `json:"ip,omitempty"`
	Peers       []TailnetPeer `json:"peers"`
	Error       string        `json:"error,omitempty"`
	// HTTPS reports that the tailnet issues certificates (MagicDNS and HTTPS
	// on), which the phone dashboard needs. Phone reports that the add-on
	// serves the dashboard; PhoneError why it cannot.
	HTTPS      bool   `json:"https"`
	Phone      bool   `json:"phone"`
	PhoneError string `json:"phone_error,omitempty"`
}

// TailnetPeer is another Switcher in the user's tailnet.
type TailnetPeer struct {
	Name    string `json:"name"`
	DNSName string `json:"dns_name"`
	IP      string `json:"ip"`
	Online  bool   `json:"online"`
}

type tailnetFile struct {
	Enabled bool `json:"enabled"`
}

// Tailnet manages the add-on process.
type Tailnet struct {
	dir, version    string
	hostPort        int
	mu              sync.Mutex
	enabled         bool
	cmd             *exec.Cmd
	stdin           io.Closer
	control         string
	token           string
	downloading     bool
	lastErr         string
	cached          TailnetStatus
	cachedAt        time.Time
	client          *http.Client
	forwarding      func() bool
	sentForward     string
	forwardMu       sync.Mutex  // one forwarding update at a time, in order
	phoneAddr       string      // Switcher's phone listener
	phoneOn         func() bool // phone access is on
	sentPhone       string
	phoneKey        string     // the running add-on's phone key
	pendingPhoneKey string     // the key of the add-on being started
	phoneSync       sync.Mutex // keeps on/off messages in order
	phoneDirty      bool       // a phone sync was asked for
	starting        bool
	retrying        bool
	restarts        int
	retryDelay      time.Duration
	// download fetches the add-on; tests replace it.
	download func(ctx context.Context, dest string) error
}

// NewTailnet loads the saved choice. dir is ~/.switcher/remote; the add-on and
// its Tailscale state live in dir/tailnet.
func NewTailnet(dir, version string, hostPort int) *Tailnet {
	t := &Tailnet{dir: filepath.Join(dir, "tailnet"), version: version, hostPort: hostPort, client: &http.Client{Timeout: 10 * time.Second}, retryDelay: 5 * time.Second}
	t.download = t.fetch
	var f tailnetFile
	if readJSON(t.settingsPath(), &f) == nil {
		t.enabled = f.Enabled
	}
	return t
}

func (t *Tailnet) settingsPath() string { return filepath.Join(t.dir, "settings.json") }
func (t *Tailnet) binary() string {
	return filepath.Join(t.dir, "switcher-tailnet-"+t.version)
}

// ForwardWhen tells the add-on to accept tailnet connections only while
// hosting reports true.
func (t *Tailnet) ForwardWhen(hosting func() bool) {
	t.mu.Lock()
	t.forwarding = hosting
	t.mu.Unlock()
	t.SyncForwarding()
}

// SyncForwarding passes the current hosting state to a running add-on.
// Updates go one at a time, each reading the hosting state only once it
// has its turn, so one that was on its way cannot land after StopForwarding
// and turn forwarding on again. hosting must take no lock the host holds
// while stopping.
func (t *Tailnet) SyncForwarding() {
	t.forwardMu.Lock()
	defer t.forwardMu.Unlock()
	t.mu.Lock()
	hosting := t.forwarding
	t.mu.Unlock()
	t.sendForwardingLocked(hosting != nil && hosting(), false)
}

// StopForwarding has a running add-on stop passing tailnet connections on,
// and waits for its answer, before the host listener closes: once the port
// is free another program could take it, and the add-on must not hand it
// the user's devices. The host calls it under its lock, so it asks nothing
// of the host.
func (t *Tailnet) StopForwarding() {
	t.forwardMu.Lock()
	defer t.forwardMu.Unlock()
	t.sendForwardingLocked(false, true)
}

// sendForwardingLocked tells the add-on to forward or not, unless it was
// told so already; always resends it.
func (t *Tailnet) sendForwardingLocked(forward, always bool) {
	t.mu.Lock()
	control, token, sent := t.control, t.token, t.sentForward
	t.mu.Unlock()
	on := "0"
	if forward {
		on = "1"
	}
	if control == "" || !always && sent == control+on {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+control+"/forwarding?on="+on, nil)
	req.Header.Set("X-Switcher-Tailnet", token)
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			t.mu.Lock()
			t.sentForward = control + on
			t.mu.Unlock()
		}
	}
}

// ServePhone hands the add-on Switcher's phone listener and asks it to serve
// the phone dashboard while on reports true. Call it before Start.
func (t *Tailnet) ServePhone(addr string, on func() bool) {
	t.mu.Lock()
	t.phoneAddr, t.phoneOn = addr, on
	t.mu.Unlock()
	t.SyncPhone()
}

// SyncPhone passes the current phone access choice to a running add-on. One
// call sends at a time, in order; calls that arrive meanwhile are folded into
// one more round, so a slow add-on never piles up waiting calls.
func (t *Tailnet) SyncPhone() {
	t.mu.Lock()
	t.phoneDirty = true
	t.mu.Unlock()
	for t.phoneSync.TryLock() {
		for {
			t.mu.Lock()
			dirty := t.phoneDirty
			t.phoneDirty = false
			t.mu.Unlock()
			if !dirty {
				break
			}
			t.sendPhone()
		}
		t.phoneSync.Unlock()
		// A call that arrived after the last round but before the unlock
		// found the lock taken; take one more round for it.
		t.mu.Lock()
		dirty := t.phoneDirty
		t.mu.Unlock()
		if !dirty {
			return
		}
	}
}

func (t *Tailnet) sendPhone() {
	t.mu.Lock()
	control, token, on := t.control, t.token, t.phoneOn
	t.mu.Unlock()
	if control == "" || on == nil {
		return
	}
	value := "0"
	if on() {
		value = "1"
	}
	t.mu.Lock()
	if t.sentPhone == control+value {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, "http://"+control+"/phone?on="+value, nil)
	req.Header.Set("X-Switcher-Tailnet", token)
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			t.mu.Lock()
			t.sentPhone = control + value
			t.cachedAt = time.Time{}
			t.mu.Unlock()
		}
	}
}

// PhoneKey is the secret the running add-on adds to every phone request it
// passes on; the phone listener accepts nothing else. Empty while the add-on
// is not running.
func (t *Tailnet) PhoneKey() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.control == "" {
		return ""
	}
	return t.phoneKey
}

// verify checks the add-on against the digest it was installed with and, in
// release builds, the digest built into Switcher.
func (t *Tailnet) verify() error {
	saved, err := os.ReadFile(t.binary() + ".sha256")
	if err != nil {
		return err
	}
	body, err := os.ReadFile(t.binary())
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(strings.TrimSpace(string(saved)), got) || addonSHA256 != "" && !strings.EqualFold(addonSHA256, got) {
		return errors.New("the Tailscale add-on failed its check")
	}
	return nil
}

// Start runs the add-on when it was turned on before.
func (t *Tailnet) Start() {
	t.mu.Lock()
	enabled := t.enabled
	t.mu.Unlock()
	if enabled {
		go t.Enable(context.Background())
	}
}

// errStarting reports that another start is already under way.
var errStarting = errors.New("the Tailscale add-on is starting")

// Enable turns the add-on on, downloading it if needed. While it stays on,
// Switcher keeps trying to start it.
func (t *Tailnet) Enable(ctx context.Context) error {
	t.mu.Lock()
	t.enabled, t.lastErr, t.restarts = true, "", 0
	writeJSON(t.settingsPath(), tailnetFile{Enabled: true})
	t.mu.Unlock()
	err := t.start(ctx)
	if errors.Is(err, errStarting) {
		return nil
	}
	if err != nil {
		go t.retry()
	}
	return err
}

func (t *Tailnet) start(ctx context.Context) error {
	t.mu.Lock()
	if t.cmd != nil || !t.enabled {
		t.mu.Unlock()
		return nil
	}
	if t.starting {
		t.mu.Unlock()
		return errStarting
	}
	t.starting = true
	t.mu.Unlock()
	// Starting can take a while; nothing else waits on the lock meanwhile.
	cmd, stdin, control, token, err := t.launch(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.starting = false
	if err != nil {
		t.lastErr = err.Error()
		return err
	}
	if !t.enabled {
		stdin.Close()
		go cmd.Wait()
		return nil
	}
	t.cmd, t.stdin, t.control, t.token, t.lastErr, t.sentForward, t.sentPhone = cmd, stdin, control, token, "", "", ""
	t.phoneKey = t.pendingPhoneKey
	go t.SyncForwarding()
	go t.SyncPhone()
	go t.watch(cmd)
	return nil
}

func (t *Tailnet) launch(ctx context.Context) (*exec.Cmd, io.WriteCloser, string, string, error) {
	// A missing, outdated or altered add-on is downloaded again.
	if t.verify() != nil {
		t.mu.Lock()
		t.downloading = true
		t.mu.Unlock()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := t.download(ctx, t.binary())
		cancel()
		t.mu.Lock()
		t.downloading = false
		t.mu.Unlock()
		if err != nil {
			return nil, nil, "", "", err
		}
		t.removeOtherVersions()
		if err := t.verify(); err != nil {
			return nil, nil, "", "", err
		}
	}
	token := randomHex(24)
	args := []string{"-dir", filepath.Join(t.dir, "state"), "-hostname", TailnetHostname(MachineName()),
		"-forward", fmt.Sprintf("127.0.0.1:%d", t.hostPort), "-port", fmt.Sprint(t.hostPort)}
	cmd := exec.Command(t.binary(), args...)
	// The add-on's settings come only from here, never from Switcher's own
	// environment.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "SWITCHER_TAILNET_TOKEN" && name != "SWITCHER_TAILNET_PHONE" && name != "SWITCHER_TAILNET_PHONE_KEY" {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "SWITCHER_TAILNET_TOKEN="+token)
	// The phone dashboard gets its own key, as older add-ons ignore unknown
	// variables but stop on unknown flags.
	phoneKey := randomHex(24)
	t.mu.Lock()
	if t.phoneAddr != "" {
		cmd.Env = append(cmd.Env, "SWITCHER_TAILNET_PHONE="+t.phoneAddr, "SWITCHER_TAILNET_PHONE_KEY="+phoneKey)
	} else {
		phoneKey = ""
	}
	t.pendingPhoneKey = phoneKey
	t.mu.Unlock()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, "", "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, "", "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, "", "", errors.New("the Tailscale add-on could not start")
	}
	var hello struct {
		Control string `json:"control"`
	}
	line := make(chan error, 1)
	go func() { line <- json.NewDecoder(bufio.NewReader(stdout)).Decode(&hello) }()
	select {
	case err = <-line:
	case <-time.After(30 * time.Second):
		err = errors.New("timed out")
	}
	if err != nil || hello.Control == "" {
		stdin.Close()
		cmd.Process.Kill()
		go cmd.Wait()
		return nil, nil, "", "", errors.New("the Tailscale add-on did not start")
	}
	return cmd, stdin, hello.Control, token, nil
}

// watch starts the add-on again if it stops while enabled.
func (t *Tailnet) watch(cmd *exec.Cmd) {
	started := time.Now()
	cmd.Wait()
	t.mu.Lock()
	if t.cmd != cmd {
		t.mu.Unlock()
		return
	}
	t.cmd, t.stdin, t.control = nil, nil, ""
	if time.Since(started) > time.Minute {
		t.restarts = 0
	}
	t.lastErr = "the Tailscale add-on stopped; restarting"
	t.mu.Unlock()
	t.retry()
}

// retry starts the add-on after a growing pause, up to ten minutes, until it
// runs or is turned off.
func (t *Tailnet) retry() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.retrying {
		return
	}
	t.retrying = true
	for t.enabled && t.cmd == nil {
		delay := min(t.retryDelay<<min(t.restarts, 10), 10*time.Minute)
		t.restarts++
		t.mu.Unlock()
		time.Sleep(delay)
		t.start(context.Background())
		t.mu.Lock()
	}
	t.retrying = false
}

// Disable stops the add-on and remembers the choice.
func (t *Tailnet) Disable() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled, t.lastErr = false, ""
	writeJSON(t.settingsPath(), tailnetFile{})
	t.stopLocked()
}

func (t *Tailnet) stopLocked() {
	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.cmd != nil {
		cmd := t.cmd
		time.AfterFunc(3*time.Second, func() { cmd.Process.Kill() })
	}
	t.cmd, t.stdin, t.control = nil, nil, ""
}

// Remove signs this Mac out of Tailscale and deletes the add-on.
func (t *Tailnet) Remove(ctx context.Context) error {
	t.mu.Lock()
	control, token := t.control, t.token
	t.mu.Unlock()
	if control != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+control+"/logout", nil)
		req.Header.Set("X-Switcher-Tailnet", token)
		if resp, err := t.client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	t.Disable()
	time.Sleep(200 * time.Millisecond)
	return os.RemoveAll(t.dir)
}

// Close stops the add-on without changing the saved choice.
func (t *Tailnet) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLocked()
}

// Status reports the add-on and its tailnet, refreshed at most every two seconds.
func (t *Tailnet) Status() TailnetStatus {
	go t.SyncForwarding()
	go t.SyncPhone()
	t.mu.Lock()
	_, err := os.Stat(t.binary())
	s := TailnetStatus{Installed: err == nil, Enabled: t.enabled, Running: t.cmd != nil, Downloading: t.downloading, Error: t.lastErr, Peers: []TailnetPeer{}}
	control, token := t.control, t.token
	if control != "" && time.Since(t.cachedAt) < 2*time.Second {
		cached := t.cached
		t.mu.Unlock()
		cached.Installed, cached.Enabled, cached.Running, cached.Error = s.Installed, s.Enabled, s.Running, s.Error
		return cached
	}
	t.mu.Unlock()
	if control == "" {
		return s
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+control+"/status", nil)
	req.Header.Set("X-Switcher-Tailnet", token)
	resp, err := t.client.Do(req)
	if err != nil {
		return s
	}
	defer resp.Body.Close()
	var live struct {
		State      string        `json:"state"`
		AuthURL    string        `json:"auth_url"`
		Tailnet    string        `json:"tailnet"`
		Name       string        `json:"name"`
		DNSName    string        `json:"dns_name"`
		IP         string        `json:"ip"`
		Peers      []TailnetPeer `json:"peers"`
		HTTPS      bool          `json:"https"`
		Phone      bool          `json:"phone"`
		PhoneError string        `json:"phone_error"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&live) != nil {
		return s
	}
	// Names in a tailnet are chosen by its devices, shared-in ones included;
	// they are shown in the dashboard, the menus and the terminal.
	s.State, s.AuthURL, s.Tailnet = plainText(live.State), plainText(live.AuthURL), plainText(live.Tailnet)
	s.Name, s.DNSName, s.IP = plainText(live.Name), plainText(live.DNSName), plainText(live.IP)
	s.HTTPS, s.Phone, s.PhoneError = live.HTTPS, live.Phone, plainText(live.PhoneError)
	if live.Peers != nil {
		for i, p := range live.Peers {
			live.Peers[i].Name, live.Peers[i].DNSName, live.Peers[i].IP = plainText(p.Name), plainText(p.DNSName), plainText(p.IP)
		}
		s.Peers = live.Peers
	}
	t.mu.Lock()
	t.cached, t.cachedAt = s, time.Now()
	t.mu.Unlock()
	return s
}

// Addresses are this Mac's tailnet name and address while signed in.
func (t *Tailnet) Addresses() []string {
	s := t.Status()
	if s.State != "Running" {
		return nil
	}
	var out []string
	for _, a := range []string{s.DNSName, s.IP} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// whoIsBehind asks the add-on which tailnet device is behind one of the
// connections it forwarded to the host listener: conn is the id its
// preamble carried, from the loopback address the host saw it come from.
// The add-on answers only for a connection that is still open, from that
// same address, with what its own node's Tailscale says.
func (t *Tailnet) whoIsBehind(ctx context.Context, conn, from string) (tailnetIdentity, error) {
	t.mu.Lock()
	control, token := t.control, t.token
	t.mu.Unlock()
	if control == "" {
		return tailnetIdentity{}, errors.New("the Tailscale add-on is not running")
	}
	query := url.Values{"conn": {conn}, "from": {from}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+control+"/whois?"+query.Encode(), nil)
	if err != nil {
		return tailnetIdentity{}, err
	}
	req.Header.Set("X-Switcher-Tailnet", token)
	resp, err := t.client.Do(req)
	if err != nil {
		return tailnetIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tailnetIdentity{}, fmt.Errorf("the Tailscale add-on does not know that connection (%d)", resp.StatusCode)
	}
	var id tailnetIdentity
	if err := decodeOne(resp.Body, 64<<10, &id); err != nil {
		return tailnetIdentity{}, err
	}
	return id, nil
}

// identityHeader carries, in the add-on's answer to a CONNECT that asked
// for it, what Tailscale says about the node the tunnel reached.
const identityHeader = "X-Switcher-Identity"

// Dial reaches address through the tailnet, or reports false when the add-on
// is not running, the address is not on the tailnet or the add-on cannot reach
// it; the caller then dials directly, through the Tailscale app if installed.
func (t *Tailnet) Dial(ctx context.Context, address string) (net.Conn, bool, error) {
	conn, _, handled, err := t.dial(ctx, address, false)
	return conn, handled, err
}

// dialIdentified is Dial that also has the add-on say which node the tunnel
// reached, as its own node's Tailscale sees it. The identity is nil when
// the add-on could not tell.
func (t *Tailnet) dialIdentified(ctx context.Context, address string) (net.Conn, *tailnetIdentity, bool, error) {
	return t.dial(ctx, address, true)
}

func (t *Tailnet) dial(ctx context.Context, address string, identify bool) (net.Conn, *tailnetIdentity, bool, error) {
	host, _, _ := net.SplitHostPort(address)
	// The address goes into the CONNECT line as it is.
	if !onTailnet(host) || strings.ContainsAny(address, " \t\r\n") {
		return nil, nil, false, nil
	}
	// Only a signed-in add-on handles tailnet addresses; otherwise they go
	// to the Tailscale app, if one is installed.
	if t.Status().State != "Running" {
		return nil, nil, false, nil
	}
	t.mu.Lock()
	control, token := t.control, t.token
	t.mu.Unlock()
	if control == "" {
		return nil, nil, false, nil
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", control)
	if err != nil {
		return nil, nil, false, nil
	}
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	ask := ""
	if identify {
		ask = "X-Switcher-Whois: 1\r\n"
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-Switcher-Tailnet: %s\r\n%s\r\n", address, address, token, ask)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, true, ctx.Err()
		}
		return nil, nil, false, nil
	}
	if !stop() {
		conn.Close()
		return nil, nil, true, ctx.Err()
	}
	conn.SetDeadline(time.Time{})
	var id *tailnetIdentity
	if identify {
		id = decodeIdentityHeader(resp.Header.Get(identityHeader))
	}
	if br.Buffered() > 0 {
		return &prefixedConn{Conn: conn, r: br}, id, true, nil
	}
	return conn, id, true, nil
}

// decodeIdentityHeader reads the add-on's identity header, or returns nil.
func decodeIdentityHeader(value string) *tailnetIdentity {
	if value == "" || len(value) > 16<<10 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	var id tailnetIdentity
	if decodeOne(bytes.NewReader(raw), 16<<10, &id) != nil || cleanIdentity(&id) != nil {
		return nil
	}
	return &id
}

type prefixedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// onTailnet recognizes Tailscale addresses and MagicDNS names.
func onTailnet(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return tailnet.Contains(ip) || tailnet6.Contains(ip)
	}
	return strings.HasSuffix(strings.TrimSuffix(host, "."), ".ts.net")
}

var hostnameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// TailnetHostname is the tailnet node name for a Mac, such as
// switcher-marios-macbook-pro.
func TailnetHostname(machine string) string {
	machine = strings.NewReplacer("'", "", "\u2019", "").Replace(machine)
	name := strings.Trim(hostnameUnsafe.ReplaceAllString(strings.ToLower(machine), "-"), "-")
	if name == "" {
		name = "mac"
	}
	if len(name) > 50 {
		name = name[:50]
	}
	return "switcher-" + name
}

// fetch downloads the add-on for this Switcher version and checks its digest.
// removeOtherVersions deletes add-ons for other Switcher versions.
func (t *Tailnet) removeOtherVersions() {
	matches, _ := filepath.Glob(filepath.Join(t.dir, "switcher-tailnet*"))
	for _, m := range matches {
		if m != t.binary() && m != t.binary()+".sha256" {
			os.Remove(m)
		}
	}
}

func (t *Tailnet) fetch(ctx context.Context, dest string) error {
	if t.version == "" || t.version == "dev" {
		return errors.New("the Tailscale add-on is only available for released versions")
	}
	asset := fmt.Sprintf("switcher-tailnet_%s_%s_%s", t.version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/v%s/%s", tailnetRelease, t.version, asset)
	get := func(url string, limit int64) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download failed (HTTP %d)", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, limit))
	}
	want := addonSHA256
	if want == "" {
		digest, err := get(base+".sha256", 4<<10)
		if err != nil {
			return fmt.Errorf("could not download the Tailscale add-on: %w", err)
		}
		if fields := strings.Fields(string(digest)); len(fields) > 0 {
			want = fields[0]
		}
	}
	body, err := get(base, 200<<20)
	if err != nil {
		return fmt.Errorf("could not download the Tailscale add-on: %w", err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if want == "" || !strings.EqualFold(want, got) {
		return errors.New("the downloaded Tailscale add-on failed its checksum")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	tmp := dest + ".download"
	if err := os.WriteFile(tmp, body, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dest+".sha256", []byte(got+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
