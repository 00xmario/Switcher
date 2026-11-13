package remote

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DeviceHeader carries a paired device's token. The device proves itself
// apart from Authorization, which carries its CLIs' own logins and stays on
// the device.
const DeviceHeader = "X-Switcher-Device"

// AccountHeader names the host account for Claude inference; empty means the
// host's own Claude selection.
const AccountHeader = "X-Switcher-Account"

// rejectedHeader marks the host's own refusal of a device, as opposed to a
// provider's 401 passed through.
const rejectedHeader = "X-Switcher-Device-Rejected"

// A pairing code is 12 characters of 32, 60 bits: too many to guess in the
// ten minutes it lives even at network speed, so wrong guesses never end
// it. A limit on guesses that did end it would let anyone on the network
// keep pairing from working by sending a few.
const (
	codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	codeLength   = 12
	codeTTL      = 10 * time.Minute
	// sourceFailures bounds wrong guesses per address, which is how much
	// work one address can make this computer do.
	sourceFailures = 10
)

type device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_sha256"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	// A device paired without a code, by its Tailscale identity, keeps that
	// node's stable id and name, and the random id of the Switcher there
	// that paired. A new pairing from the same Switcher on the same node
	// replaces this one.
	TailscaleNode    string `json:"tailscale_node,omitempty"`
	TailscaleName    string `json:"tailscale_name,omitempty"`
	TailscaleInstall string `json:"tailscale_install,omitempty"`
}

type hostFile struct {
	Enabled bool     `json:"enabled"`
	Devices []device `json:"devices"`
	// TailnetPairing lets the user's own Tailscale devices pair without a
	// code. A host that first shares with this version has it on; one that
	// shared before keeps it off, since other accounts and machines on the
	// user's devices would otherwise start pairing unannounced, and
	// TailnetNotice offers it once.
	TailnetPairing *bool `json:"tailnet_pairing,omitempty"`
	TailnetNotice  bool  `json:"tailnet_notice,omitempty"`
}

// settleTailnetPairing decides pairing without a code for a host file from
// before it existed: off, with the notice, for a host that has shared, and
// on for one that never has.
func (f *hostFile) settleTailnetPairing() {
	if f.TailnetPairing != nil {
		return
	}
	shared := f.Enabled || len(f.Devices) > 0
	on := !shared
	f.TailnetPairing, f.TailnetNotice = &on, shared
}

func (f *hostFile) tailnetPairing() bool { return f.TailnetPairing != nil && *f.TailnetPairing }

// DeviceView is a paired device as the UI shows it. Tailscale is the
// Tailscale name of a device that paired without a code.
type DeviceView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	Tailscale string    `json:"tailscale,omitempty"`
}

// PairingView is the code another device enters to pair.
type PairingView struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HostStatus describes this Switcher as a host. TailnetPairing lets the
// user's own Tailscale devices pair without a code; TailnetNotice offers
// it, once, to a host that shared before it existed.
type HostStatus struct {
	Enabled        bool         `json:"enabled"`
	TailnetPairing bool         `json:"tailnet_pairing"`
	TailnetNotice  bool         `json:"tailnet_notice,omitempty"`
	Listening      bool         `json:"listening"`
	Name           string       `json:"name"`
	Port           int          `json:"port"`
	LAN            []string     `json:"lan"`
	Tailscale      []string     `json:"tailscale"`
	Devices        []DeviceView `json:"devices"`
	Pairing        *PairingView `json:"pairing,omitempty"`
	Error          string       `json:"error,omitempty"`
}

// Host serves this Switcher's accounts to paired devices.
type Host struct {
	dir, name, fp string
	port          int
	cert          tls.Certificate
	next          http.Handler
	mu            sync.Mutex
	state         hostFile
	code          string
	codeKey       []byte
	codeExpires   time.Time
	failures      map[string]failure
	tailnetTries  map[string]failure
	lookups       chan struct{} // turns to ask the Tailscale app; see tailnetLookups
	srv           *http.Server
	listening     atomic.Bool
	listenErr     string
	advertising   func()
	// Quiet skips the Bonjour advertisement (tests).
	Quiet bool
	// addresses reports how devices can reach this Mac; tests replace it.
	addresses func() (lan, tailscale []string)
	// Tailscale's word on who is calling, for pairing without a code: the
	// add-on, for the connections it forwards, and the Tailscale app, for
	// those that reach this computer's own Tailscale address. ownTailnet
	// lists this computer's own Tailscale addresses. Tests replace them.
	addonWhois func(ctx context.Context, conn, from string) (tailnetIdentity, error)
	system     *systemTailscale
	ownTailnet func() ([]string, error)
	// beforeStop runs before the listener closes, under the lock.
	beforeStop func()
	// running holds, per paired device, how to end its requests in flight:
	// removing a device also ends what it already started, such as a long
	// answer still streaming.
	running    map[string]map[uint64]context.CancelFunc
	runningSeq uint64
	// held is how many bytes of request bodies each device's running
	// requests have sent: the handlers keep a body in memory until they
	// are done with it. It has a lock of its own: it is counted on every
	// read of a body.
	heldMu sync.Mutex
	held   map[string]int64
}

// NewHost loads the host state from dir. next serves the routes paired devices
// may use; it must apply no further authentication.
func NewHost(dir string, port int, next http.Handler) (*Host, error) {
	cert, fp, err := hostCertificate(dir)
	if err != nil {
		return nil, err
	}
	h := &Host{dir: dir, port: port, cert: cert, fp: fp, next: next, name: MachineName(), addresses: Addresses,
		system: newSystemTailscale(), ownTailnet: localTailnetIPs}
	if err := readJSON(h.path(), &h.state); err != nil && !errors.Is(err, io.EOF) && !isNotExist(err) {
		return nil, err
	}
	// Kept with the next change to the file; until then every start
	// decides the same.
	h.state.settleTailnetPairing()
	return h, nil
}

func (h *Host) path() string { return filepath.Join(h.dir, "host.json") }

// AlsoReachableAt adds addresses, such as the Tailscale add-on's name, to the
// ones paired devices learn.
func (h *Host) AlsoReachableAt(extra func() []string) {
	base := h.addresses
	h.addresses = func() ([]string, []string) {
		lan, tailscale := base()
		return lan, append(tailscale, extra()...)
	}
}

// Listening reports whether the host listener is serving. It takes no lock,
// so the add-on's forwarding can follow it from inside the stop hook.
func (h *Host) Listening() bool { return h.listening.Load() }

// Fingerprint identifies the host certificate.
func (h *Host) Fingerprint() string { return h.fp }

// Start opens the listener when hosting is enabled.
func (h *Host) Start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.Enabled {
		h.listenLocked()
	}
}

func (h *Host) listenLocked() {
	if h.srv != nil {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", h.port))
	if err != nil {
		h.listenErr = fmt.Sprintf("port %d is in use", h.port)
		return
	}
	h.listenErr = ""
	h.port = ln.Addr().(*net.TCPAddr).Port
	ln = privateOnly{ln}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS12},
		ConnContext: withLoopbackConn}
	h.srv = srv
	h.listening.Store(true)
	go func() {
		srv.ServeTLS(ln, "", "")
		h.mu.Lock()
		if h.srv == srv {
			h.srv = nil
			h.listening.Store(false)
		}
		h.mu.Unlock()
	}()
	if !h.Quiet {
		h.advertising = advertise(h.name, h.port, h.fp)
	}
}

func (h *Host) stopLocked() {
	if h.advertising != nil {
		h.advertising()
		h.advertising = nil
	}
	if h.srv != nil {
		// No longer listening before the hook runs, so a forwarding update
		// that comes after it cannot turn forwarding on again.
		h.listening.Store(false)
		if h.beforeStop != nil {
			h.beforeStop()
		}
		h.srv.Close()
		h.srv = nil
	}
}

// BeforeStop runs fn before the listener closes, such as having the
// Tailscale add-on stop forwarding to the port that is about to be freed,
// which another program could take. fn must not call back into the host.
func (h *Host) BeforeStop(fn func()) {
	h.mu.Lock()
	h.beforeStop = fn
	h.mu.Unlock()
}

// Close stops serving without changing the saved preference.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopLocked()
}

// SetEnabled turns hosting on or off and remembers the choice.
func (h *Host) SetEnabled(enabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.state.Enabled
	h.state.Enabled = enabled
	if err := writeJSON(h.path(), h.state); err != nil {
		h.state.Enabled = previous
		return err
	}
	if enabled {
		h.listenLocked()
	} else {
		h.stopLocked()
		h.code = ""
	}
	return nil
}

// SetTailnetPairing lets the user's own Tailscale devices pair without a
// code, or not, and remembers the choice. Either choice answers the notice.
func (h *Host) SetTailnetPairing(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous, notice := h.state.TailnetPairing, h.state.TailnetNotice
	h.state.TailnetPairing, h.state.TailnetNotice = &on, false
	if err := writeJSON(h.path(), h.state); err != nil {
		h.state.TailnetPairing, h.state.TailnetNotice = previous, notice
		return err
	}
	return nil
}

// IdentifyThrough has the host ask the Tailscale add-on which tailnet
// device is behind a connection it forwarded, for pairing without a code.
func (h *Host) IdentifyThrough(t *Tailnet) {
	h.mu.Lock()
	h.addonWhois = t.whoIsBehind
	h.mu.Unlock()
}

// NewPairingCode shows a fresh code for one device to pair with.
func (h *Host) NewPairingCode() (PairingView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.Enabled {
		return PairingView{}, errors.New("turn on sharing first")
	}
	b := make([]byte, codeLength)
	if _, err := rand.Read(b); err != nil {
		return PairingView{}, err
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	key, err := pairingKey(string(b), h.fp)
	if err != nil {
		return PairingView{}, err
	}
	h.code, h.codeKey, h.codeExpires = string(b), key, time.Now().Add(codeTTL)
	return PairingView{Code: formatCode(h.code), ExpiresAt: h.codeExpires}, nil
}

// Enabled reports whether this computer shares its accounts.
func (h *Host) Enabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.Enabled
}

// Paired reports whether a device id is paired with this host now.
func (h *Host) Paired(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.ContainsFunc(h.state.Devices, func(d device) bool { return d.ID == id })
}

// Revoke removes a paired device; its token stops working immediately.
func (h *Host) Revoke(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, d := range h.state.Devices {
		if d.ID == id {
			previous := h.state.Devices
			h.state.Devices = append(append([]device(nil), h.state.Devices[:i]...), h.state.Devices[i+1:]...)
			if err := writeJSON(h.path(), h.state); err != nil {
				h.state.Devices = previous
				return err
			}
			h.endRemovedLocked()
			return nil
		}
	}
	return errors.New("device not found")
}

// Status reports hosting, addresses, paired devices and an active code.
func (h *Host) Status() HostStatus {
	lan, tailscale := h.addresses()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := HostStatus{Enabled: h.state.Enabled, TailnetPairing: h.state.tailnetPairing(), TailnetNotice: h.state.TailnetNotice, Listening: h.listening.Load(), Name: h.name, Port: h.port,
		LAN: lan, Tailscale: tailscale, Devices: []DeviceView{}, Error: h.listenErr}
	for _, d := range h.state.Devices {
		// Cleaned again here for pairings an older release saved.
		s.Devices = append(s.Devices, DeviceView{ID: d.ID, Name: plainText(d.Name), CreatedAt: d.CreatedAt, LastSeen: d.LastSeen, Tailscale: plainText(d.TailscaleName)})
	}
	if h.code != "" && time.Now().Before(h.codeExpires) {
		s.Pairing = &PairingView{Code: formatCode(h.code), ExpiresAt: h.codeExpires}
	}
	return s
}

// remotePaths are the route prefixes a paired device may use: account
// actions, provider proxies, the T3 Code hub and Claude inference.
var remotePaths = []string{"/remote/", "/api/accounts/", "/api/usage/refresh", "/api/providers/", "/v0/management/",
	"/codex/", "/claude/", "/grok/", "/opencode/", "/antigravity/", "/gemini/", "/copilot/"}

// remoteExactPaths are single routes a paired device may use: the state, and
// waiting for it to change, which answers only an opaque tag and whether the
// state changed, so a paired Mac learns at once of a switch made elsewhere;
// checking for and installing an update of this Switcher, an official
// release only; and how busy this computer is, for the paired Mac's load
// card: processor, memory and graphics shares, and for its load page the
// leading applications by name (no arguments, files or users); and
// Switcher's skill for AI agents, which a paired Mac may read, install or
// remove: the skill's text is the host's own release's, so all a paired Mac
// chooses is whether it is there.
//
// And usage, added up across the computers: a paired computer sends its own
// ledger (only its own, see usage.DeviceStore) and reads everyone's, so
// every computer shows the same combined usage.
var remoteExactPaths = []string{"/api/state", "/api/state/changes", "/api/update", "/api/update/check", "/api/system",
	"/api/system/apps", "/api/agent-skill", "/api/tokens/device", "/api/tokens/ledgers"}

func allowedRemotePath(path string) bool {
	if slices.Contains(remoteExactPaths, path) {
		return true
	}
	for _, prefix := range remotePaths {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

type deviceKey struct{}
type deviceIDKey struct{}

// PairedDevice reports which paired device made a request, if any.
func PairedDevice(r *http.Request) (string, bool) {
	name, ok := r.Context().Value(deviceKey{}).(string)
	return name, ok
}

// PairedDeviceID reports the id of the paired device that made a request.
// Unlike the name, which the device chose, it is unique to one pairing.
func PairedDeviceID(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(deviceIDKey{}).(string)
	return id, ok
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/remote/pair" && r.Method == http.MethodPost {
		h.pair(w, r)
		return
	}
	if r.URL.Path == "/remote/pair/tailnet" && r.Method == http.MethodPost {
		h.pairTailnet(w, r)
		return
	}
	d, ok := h.authenticate(r.Header.Get(DeviceHeader))
	if !ok {
		w.Header().Set(rejectedHeader, "1")
		writeError(w, http.StatusUnauthorized, "this device is not paired with this Switcher")
		return
	}
	// A device that stops using this host, by disconnecting or switching to
	// another one, removes its own pairing, so its token goes with it.
	if r.URL.Path == "/remote/unpair" && r.Method == http.MethodPost {
		if err := h.Revoke(d.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "could not remove the pairing")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !allowedRemotePath(r.URL.Path) {
		writeError(w, http.StatusNotFound, "not available to paired devices")
		return
	}
	r.Header.Del(DeviceHeader)
	// A paired device is never this Mac, even when it arrives through the
	// Tailscale add-on's loopback connection: local-only actions and the
	// management key stay out of reach.
	r.RemoteAddr, r.Host = "192.0.2.1:1", "switcher-host"
	if r.URL.Path == "/remote/hello" {
		lan, tailscale := h.addresses()
		writeJSONResponse(w, http.StatusOK, hello{Name: h.name, Addresses: append(lan, tailscale...)})
		return
	}
	ctx, charge, done, ok := h.track(context.WithValue(context.WithValue(r.Context(), deviceKey{}, d.Name), deviceIDKey{}, d.ID), d.ID)
	if errors.Is(context.Cause(ctx), errTooBusy) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "this device has too many requests running on this Switcher; try again in a moment")
		return
	}
	if !ok {
		w.Header().Set(rejectedHeader, "1")
		writeError(w, http.StatusUnauthorized, "this device is not paired with this Switcher")
		return
	}
	defer done()
	// A body that will not fit beside the device's other running requests
	// is turned away before any of it is read, in the way a client tries
	// again after: the room comes back as those requests finish.
	if r.ContentLength > 0 && !h.fits(d.ID, r.ContentLength) {
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, errTooMuch.Error())
		return
	}
	r.Body = &chargedBody{ReadCloser: r.Body, charge: charge}
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

// fits reports whether n more bytes fit the device's share right now.
func (h *Host) fits(id string, n int64) bool {
	h.heldMu.Lock()
	defer h.heldMu.Unlock()
	return h.held[id]+n <= perDeviceBytes
}

// chargedBody counts what is read of a request's body against its device.
type chargedBody struct {
	io.ReadCloser
	charge func(n int) bool
}

func (b *chargedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.charge(n) {
		return 0, errTooMuch
	}
	return n, err
}

// track ties a request to its device's pairing: its context ends when the
// device is removed. A device removed since it was authenticated gets no
// request at all.
//
// charge counts bytes of the request's body against the device and says
// whether it may send them: all of a device's running requests together
// hold at most perDeviceBytes.
func (h *Host) track(ctx context.Context, id string) (tracked context.Context, charge func(int) bool, done func(), ok bool) {
	ctx, cancel := context.WithCancelCause(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.Enabled || !slices.ContainsFunc(h.state.Devices, func(d device) bool { return d.ID == id }) {
		cancel(nil)
		return ctx, nil, nil, false
	}
	// One device holds only so many requests at once.
	if len(h.running[id]) >= perDeviceRequests {
		cancel(errTooBusy)
		return ctx, nil, nil, false
	}
	// What the request sent, and whether it is over: a read after its
	// handler has returned is not counted, since nothing would give it back.
	var sent int64
	var over bool
	charge = func(n int) bool {
		h.heldMu.Lock()
		defer h.heldMu.Unlock()
		if over || h.held[id]+int64(n) > perDeviceBytes {
			return false
		}
		if h.held == nil {
			h.held = map[string]int64{}
		}
		h.held[id] += int64(n)
		sent += int64(n)
		return true
	}
	if h.running == nil {
		h.running = map[string]map[uint64]context.CancelFunc{}
	}
	if h.running[id] == nil {
		h.running[id] = map[uint64]context.CancelFunc{}
	}
	h.runningSeq++
	seq := h.runningSeq
	h.running[id][seq] = func() { cancel(nil) }
	return ctx, charge, func() {
		cancel(nil)
		h.mu.Lock()
		if set := h.running[id]; set != nil {
			delete(set, seq)
			if len(set) == 0 {
				delete(h.running, id)
			}
		}
		h.mu.Unlock()
		h.heldMu.Lock()
		if over = true; sent > 0 {
			if h.held[id] -= sent; h.held[id] <= 0 {
				delete(h.held, id)
			}
			sent = 0
		}
		h.heldMu.Unlock()
	}, true
}

// perDeviceRequests is how many requests one paired device may have running
// at once: plenty for many conversations and agents, with their state
// polls. perDeviceBytes is how much those requests' bodies may hold of this
// computer's memory together: a few of the largest requests Claude takes at
// once, or hundreds of ordinary ones. Together they bound what a device
// gone wrong can make this computer hold.
const perDeviceRequests = 128

var perDeviceBytes int64 = 256 << 20 // tests lower it

var (
	errTooBusy = errors.New("too many requests running for this device")
	errTooMuch = errors.New("this device's running requests hold too much; wait for some to finish")
)

// endRemovedLocked ends the requests in flight of every device that is no
// longer paired.
func (h *Host) endRemovedLocked() {
	for id, set := range h.running {
		if slices.ContainsFunc(h.state.Devices, func(d device) bool { return d.ID == id }) {
			continue
		}
		for _, cancel := range set {
			cancel()
		}
		delete(h.running, id)
	}
}

func (h *Host) authenticate(token string) (device, bool) {
	if token == "" {
		return device{}, false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.Enabled {
		return device{}, false
	}
	for i, d := range h.state.Devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hash)) == 1 {
			if time.Since(d.LastSeen) > time.Minute {
				h.state.Devices[i].LastSeen = time.Now().UTC()
				_ = writeJSON(h.path(), h.state)
			}
			return d, true
		}
	}
	return device{}, false
}

// hello tells a paired device the host's current name and addresses.
type hello struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
}

// privateOnly drops connections from the internet before any TLS or HTTP:
// the host serves only this Mac, the local network and Tailscale, even if a
// router forwards the port or IPv6 makes this Mac directly reachable.
type privateOnly struct{ net.Listener }

func (l privateOnly) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok && privateSource(addr.IP) {
			if addr.IP.IsLoopback() {
				return &loopbackConn{Conn: conn}, nil
			}
			return conn, nil
		}
		conn.Close()
	}
}

var tailnet6 = &net.IPNet{IP: net.ParseIP("fd7a:115c:a1e0::"), Mask: net.CIDRMask(48, 128)}

// privateSource accepts loopback, private and link-local addresses, and
// Tailscale's address ranges.
func privateSource(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || tailnet.Contains(ip) || tailnet6.Contains(ip)
}

type pairRequest struct {
	Name  string `json:"name"`
	Nonce string `json:"nonce"`
	Proof string `json:"proof"`
}

type pairResponse struct {
	DeviceID  string   `json:"device_id"`
	Token     string   `json:"token"`
	HostName  string   `json:"host_name"`
	Addresses []string `json:"addresses"`
	// HostProof shows the host knows the code too, so a device pretending to
	// be the host cannot complete a pairing. Pairing without a code has
	// none: there the client checks the host's Tailscale identity instead.
	HostProof string `json:"host_proof,omitempty"`
}

type failure struct {
	count int
	since time.Time
}

const pairFailed = "pairing failed: check the code shown on the other Mac"

func (h *Host) pair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil || len(req.Nonce) < 16 || len(req.Nonce) > 128 {
		writeError(w, http.StatusBadRequest, "invalid pairing request")
		return
	}
	// Tailnet connections all arrive from the add-on on loopback; for those
	// the code's length is the whole defence.
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(source); ip != nil && ip.IsLoopback() {
		source = ""
	} else if ip != nil && ip.To4() == nil {
		// One IPv6 network hands out as many addresses as it likes.
		source = ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	h.mu.Lock()
	if h.failures == nil || len(h.failures) > 4096 {
		kept := map[string]failure{}
		for k, f := range h.failures {
			if time.Since(f.since) <= codeTTL && len(kept) < 4096 {
				kept[k] = f
			}
		}
		h.failures = kept
	}
	f := h.failures[source]
	if time.Since(f.since) > codeTTL {
		f = failure{since: time.Now()}
	}
	// Both "no code" and "wrong code" give the same answer, so nobody can
	// probe for a live code; a source that keeps failing is refused.
	if f.count >= sourceFailures || !h.state.Enabled || h.code == "" || time.Now().After(h.codeExpires) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, pairFailed)
		return
	}
	if !hmac.Equal([]byte(req.Proof), []byte(pairingProof(h.codeKey, "client", req.Nonce))) {
		if source != "" {
			f.count++
			h.failures[source] = f
		}
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, pairFailed)
		return
	}
	hostProof := pairingProof(h.codeKey, "host", req.Nonce)
	d, token := newDevice(req.Name)
	if err := h.addDeviceLocked(d); err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "could not save the pairing")
		return
	}
	h.code, h.codeKey = "", nil
	h.mu.Unlock()
	lan, tailscale := h.addresses()
	writeJSONResponse(w, http.StatusOK, pairResponse{DeviceID: d.ID, Token: token, HostName: h.name, Addresses: append(lan, tailscale...), HostProof: hostProof})
}

// newDevice makes a paired device with a fresh token, which the host keeps
// only as its hash.
func newDevice(name string) (device, string) {
	token := randomHex(32)
	sum := sha256.Sum256([]byte(token))
	// The device chose its name, and the owner reads it in the dashboard,
	// the menus and the terminal.
	name = strings.TrimSpace(plainText(name))
	if name == "" || len(name) > 120 {
		name = "Paired device"
	}
	return device{ID: randomHex(8), Name: name, TokenHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC()}, token
}

const (
	// perNode bounds the pairings kept for one Tailscale device. A new one
	// beyond it takes the place only of a pairing that has gone unused for
	// staleAfter, so a stream of new Switchers on one device cannot push
	// out one in use.
	perNode    = 8
	staleAfter = 14 * 24 * time.Hour
)

// errTooManyOnDevice refuses a pairing beyond perNode for one device.
var errTooManyOnDevice = errors.New("too many Switchers on this device are paired")

// lastUsed is when a paired device last used its token, or paired.
func (d device) lastUsed() time.Time {
	if d.LastSeen.After(d.CreatedAt) {
		return d.LastSeen
	}
	return d.CreatedAt
}

// addDeviceLocked saves a new pairing. One from the same Switcher on the
// same Tailscale device replaces that Switcher's earlier pairing, whose
// token stops working; another Switcher behind the same device, such as
// another account's sharing its Tailscale, keeps its own. One device keeps
// at most perNode pairings (see staleAfter).
func (h *Host) addDeviceLocked(d device) error {
	previous := h.state.Devices
	kept := make([]device, 0, len(previous)+1)
	sameNode := 0
	for _, old := range previous {
		if d.TailscaleNode != "" && old.TailscaleNode == d.TailscaleNode {
			if old.TailscaleInstall == d.TailscaleInstall {
				continue
			}
			sameNode++
		}
		kept = append(kept, old)
	}
	for ; d.TailscaleNode != "" && sameNode >= perNode; sameNode-- {
		unused := -1
		for i, old := range kept {
			if old.TailscaleNode == d.TailscaleNode && (unused < 0 || old.lastUsed().Before(kept[unused].lastUsed())) {
				unused = i
			}
		}
		if time.Since(kept[unused].lastUsed()) < staleAfter {
			return errTooManyOnDevice
		}
		kept = append(kept[:unused], kept[unused+1:]...)
	}
	h.state.Devices = append(kept, d)
	if err := writeJSON(h.path(), h.state); err != nil {
		h.state.Devices = previous
		return err
	}
	// A pairing this one replaced is over, with what it had started.
	h.endRemovedLocked()
	return nil
}

// pairingKey stretches the code with the host certificate the client saw.
// The slow derivation makes guessing a code from a captured proof infeasible
// within its ten minutes, and binding the certificate stops a device in the
// middle from reusing a code with its own certificate.
func pairingKey(code, fp string) ([]byte, error) {
	return pbkdf2.Key(sha256.New, normalizeCode(code), []byte("switcher-pair-v1|"+fp), 600_000, 32)
}

// pairingProof shows knowledge of the code for one side of one pairing.
func pairingProof(key []byte, role, nonce string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(role + "|" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}

// formatCode groups a code in fours, as people read it out and type it.
func formatCode(code string) string {
	var groups []string
	for len(code) > 4 {
		groups, code = append(groups, code[:4]), code[4:]
	}
	return strings.Join(append(groups, code), "-")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSONResponse(w, status, map[string]string{"error": message})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
