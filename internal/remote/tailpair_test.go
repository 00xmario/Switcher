package remote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	me      = 7017007006444457 // a real-sized Tailscale user id
	someone = 7017007006444999
)

// ownIdentity is the user's own Air as this computer's Tailscale sees it.
func ownIdentity() tailnetIdentity {
	return tailnetIdentity{
		Self: tailnetSelf{ID: "nStudio11CNTRL", User: me, Suffix: "tail1234.ts.net", IPs: []string{"100.101.2.3", "fd7a:115c:a1e0::3"}},
		Peer: tailnetPeer{ID: "nAir11CNTRL", Name: "switcher-air.tail1234.ts.net", User: me, IPs: []string{"100.101.2.4", "fd7a:115c:a1e0::4"}, Addr: "100.101.2.4:51234"},
	}
}

func TestOnlyTheUsersOwnDevicesPairWithoutACode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*tailnetIdentity)
		client, host bool // accepted as a host by a client, as a client by a host
	}{
		{"the same user's device", func(*tailnetIdentity) {}, true, true},
		{"another user", func(id *tailnetIdentity) { id.Peer.User = someone }, false, false},
		{"a tagged device", func(id *tailnetIdentity) { id.Peer.Tagged = true }, false, false},
		{"a device shared in from another tailnet", func(id *tailnetIdentity) { id.Peer.Shared = true }, false, false},
		{"an expired device", func(id *tailnetIdentity) { id.Peer.Limited = true }, false, false},
		{"a device in another tailnet", func(id *tailnetIdentity) { id.Peer.Name = "switcher-air.tail9999.ts.net" }, false, false},
		{"a name that only ends like the tailnet", func(id *tailnetIdentity) { id.Peer.Name = "eviltail1234.ts.net" }, false, false},
		{"this computer's own node", func(id *tailnetIdentity) { id.Peer.ID = id.Self.ID }, false, false},
		{"an address that is not the device's", func(id *tailnetIdentity) { id.Peer.Addr = "100.101.2.99:51234" }, false, false},
		{"a tagged computer here", func(id *tailnetIdentity) { id.Self.Tagged = true }, false, false},
		{"this computer not signed in", func(id *tailnetIdentity) { id.Self = tailnetSelf{} }, false, false},
		{"a device without a user", func(id *tailnetIdentity) { id.Peer.User = 0 }, false, false},
		// A subnet router or exit node may carry anyone's connection.
		{"a device that routes for others", func(id *tailnetIdentity) { id.Peer.Routes = true }, true, false},
		// Another account on this computer, reaching the add-on through
		// this computer's own Tailscale app.
		{"this computer under its other address", func(id *tailnetIdentity) { id.Peer.Addr = "100.101.2.3:4000"; id.Peer.IPs = []string{"100.101.2.3"} }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := ownIdentity()
			tc.change(&id)
			if got := notOwnDevice(id) == ""; got != tc.client {
				t.Errorf("as a host: accepted=%v (%s), want %v", got, notOwnDevice(id), tc.client)
			}
			if got := hostAdmits(id, nil); got != tc.host {
				t.Errorf("as a client: accepted=%v, want %v", got, tc.host)
			}
		})
	}
	// The Tailscale app's address on this computer, while the add-on asks.
	id := ownIdentity()
	id.Peer.ID, id.Peer.IPs, id.Peer.Addr = "nStudioApp11CNTRL", []string{"100.101.9.9"}, "100.101.9.9:4000"
	if hostAdmits(id, []string{"100.101.9.9"}) {
		t.Error("a connection from this computer's own Tailscale app paired")
	}
}

// fakeAddons stands in for the Tailscale add-ons on both computers. The
// client's dials a host and names the node it reached, preamble first, as
// a host's add-on forwards; the host's, asked about such a connection,
// names the client.
type fakeAddons struct {
	mu       sync.Mutex
	conns    map[string]string // preamble id: the loopback address it came from
	host     tailnetIdentity   // what the client's add-on says about the host
	client   tailnetIdentity   // what the host's add-on says about the client
	lookups  atomic.Int32
	dials    atomic.Int32
	redirect int // when set, plain dials go to this port instead
}

func newFakeAddons() *fakeAddons {
	host := ownIdentity()
	host.Self, host.Peer = tailnetSelf{ID: "nAir11CNTRL", User: me, Suffix: "tail1234.ts.net", IPs: []string{"100.101.2.4"}},
		tailnetPeer{ID: "nStudio11CNTRL", Name: "switcher-studio.tail1234.ts.net", User: me, IPs: []string{"100.101.2.3"}, Addr: "100.101.2.3:8788"}
	return &fakeAddons{conns: map[string]string{}, host: host, client: ownIdentity()}
}

func portOf(address string) string {
	_, port, _ := net.SplitHostPort(address)
	return port
}

func (f *fakeAddons) dialIdentified(ctx context.Context, address string) (net.Conn, *tailnetIdentity, bool, error) {
	f.dials.Add(1)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+portOf(address))
	if err != nil {
		return nil, nil, true, err
	}
	id := randomHex(16)
	f.mu.Lock()
	f.conns[id] = conn.LocalAddr().String()
	host := f.host
	f.mu.Unlock()
	fmt.Fprintf(conn, "%s%s\n", tailnetPreamble, id)
	return conn, &host, true, nil
}

// dial is the add-on's plain tunnel, for a paired client's traffic.
func (f *fakeAddons) dial(ctx context.Context, address string) (net.Conn, bool, error) {
	port := portOf(address)
	if f.redirect != 0 {
		port = strconv.Itoa(f.redirect)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+port)
	return conn, true, err
}

func (f *fakeAddons) whois(_ context.Context, conn, from string) (tailnetIdentity, error) {
	f.lookups.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns[conn] != from || from == "" {
		return tailnetIdentity{}, errors.New("no such connection")
	}
	return f.client, nil
}

// tailnetHost is a sharing host whose add-on is fake.
func tailnetHost(t *testing.T, addons *fakeAddons) *Host {
	t.Helper()
	host, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	host.addresses = func() ([]string, []string) { return nil, nil }
	host.ownTailnet = func() []string { return nil }
	host.addonWhois = addons.whois
	host.system = &systemTailscale{run: func(context.Context, int, ...string) ([]byte, error) {
		t.Error("the Tailscale app was asked about a connection through the add-on")
		return nil, errNoTailscale
	}}
	t.Cleanup(host.Close)
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	return host
}

// pairThrough asks host to pair without a code over conn, as a client does.
func pairThrough(t *testing.T, conn net.Conn, name string) (int, pairResponse) {
	t.Helper()
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return 0, pairResponse{}
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequest(http.MethodPost, "https://switcher-host/remote/pair/tailnet", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var paired pairResponse
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &paired)
	if resp.StatusCode != http.StatusOK && !strings.Contains(string(raw), pairFailed) {
		t.Errorf("a refusal said more than a wrong code would: %s", raw)
	}
	return resp.StatusCode, paired
}

func pairViaAddon(t *testing.T, host *Host, addons *fakeAddons) (int, pairResponse) {
	t.Helper()
	conn, _, _, err := addons.dialIdentified(context.Background(), net.JoinHostPort("switcher-studio.tail1234.ts.net", strconv.Itoa(host.Status().Port)))
	if err != nil {
		t.Fatal(err)
	}
	return pairThrough(t, conn, "Air")
}

func TestHostPairsTheUsersOwnTailscaleDeviceWithoutACode(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	status, paired := pairViaAddon(t, host, addons)
	if status != http.StatusOK || !tokenShape.MatchString(paired.Token) || paired.HostProof != "" {
		t.Fatalf("pairing: %d %+v", status, paired)
	}
	devices := host.Status().Devices
	if len(devices) != 1 || devices[0].Name != "Air" || devices[0].Tailscale != "switcher-air.tail1234.ts.net" {
		t.Fatalf("devices: %+v", devices)
	}
	if !strings.Contains(string(mustRead(t, host.path())), `"tailscale_node": "nAir11CNTRL"`) {
		t.Fatal("the node's stable id was not saved")
	}
	if _, ok := host.authenticate(paired.Token); !ok {
		t.Fatal("the new token does not work")
	}
	// Pairing again from the same device replaces its earlier pairing.
	if status, again := pairViaAddon(t, host, addons); status != http.StatusOK || again.Token == paired.Token {
		t.Fatalf("pairing again: %d", status)
	}
	if _, ok := host.authenticate(paired.Token); ok || len(host.Status().Devices) != 1 {
		t.Fatalf("the earlier pairing survived: %+v", host.Status().Devices)
	}
	// Revocable like any pairing.
	if err := host.Revoke(host.Status().Devices[0].ID); err != nil || len(host.Status().Devices) != 0 {
		t.Fatal("could not revoke", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	var f hostFile
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	return b
}

func TestHostRefusesEveryoneElseLikeAWrongCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*tailnetIdentity)
	}{
		{"another user", func(id *tailnetIdentity) { id.Peer.User = someone }},
		{"a tagged device", func(id *tailnetIdentity) { id.Peer.Tagged = true }},
		{"a device shared in", func(id *tailnetIdentity) { id.Peer.Shared = true }},
		{"a subnet router", func(id *tailnetIdentity) { id.Peer.Routes = true }},
		{"a malformed node id", func(id *tailnetIdentity) { id.Peer.ID = "n/../x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addons := newFakeAddons()
			tc.change(&addons.client)
			host := tailnetHost(t, addons)
			if status, _ := pairViaAddon(t, host, addons); status != http.StatusForbidden {
				t.Fatalf("status %d", status)
			}
			if len(host.Status().Devices) != 0 {
				t.Fatal("paired")
			}
		})
	}
}

// The add-on answers only for connections it forwarded that are still
// open, by the id their preamble carried and from the same port; a loopback
// connection without one, or with an id the add-on does not know, has no
// identity at all.
func TestHostTrustsOnlyConnectionsTheAddonVouchesFor(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	port := strconv.Itoa(host.Status().Port)
	plain, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := pairThrough(t, plain, "local"); status != http.StatusForbidden || addons.lookups.Load() != 0 {
		t.Fatalf("a plain loopback connection: %d, %d lookups", status, addons.lookups.Load())
	}
	guessed, _ := net.Dial("tcp", "127.0.0.1:"+port)
	fmt.Fprintf(guessed, "%s%s\n", tailnetPreamble, randomHex(16))
	if status, _ := pairThrough(t, guessed, "local"); status != http.StatusForbidden || addons.lookups.Load() != 1 {
		t.Fatalf("an unknown or closed connection: %d", status)
	}
	// A live id sent on another connection comes from another port.
	conn, _, _, _ := addons.dialIdentified(context.Background(), "x.ts.net:"+port)
	defer conn.Close()
	addons.mu.Lock()
	var live string
	for id, from := range addons.conns {
		if from == conn.LocalAddr().String() {
			live = id
		}
	}
	addons.mu.Unlock()
	thief, _ := net.Dial("tcp", "127.0.0.1:"+port)
	fmt.Fprintf(thief, "%s%s\n", tailnetPreamble, live)
	if status, _ := pairThrough(t, thief, "local"); status != http.StatusForbidden {
		t.Fatalf("another connection's id: %d", status)
	}
	malformed, _ := net.Dial("tcp", "127.0.0.1:"+port)
	fmt.Fprintf(malformed, "%snot-an-id\n", tailnetPreamble)
	if status, _ := pairThrough(t, malformed, "local"); status != 0 {
		t.Fatalf("a malformed preamble was served: %d", status)
	}
	if len(host.Status().Devices) != 0 {
		t.Fatal("paired")
	}
}

func TestPairingWithoutACodeFollowsTheSettingAndSharing(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	if !host.Status().TailnetPairing {
		t.Fatal("off by default")
	}
	if err := host.SetTailnetPairing(false); err != nil {
		t.Fatal(err)
	}
	if status, _ := pairViaAddon(t, host, addons); status != http.StatusForbidden || addons.lookups.Load() != 0 {
		t.Fatalf("setting off: %d", status)
	}
	again, _ := NewHost(host.dir, 0, nil)
	if again.Status().TailnetPairing {
		t.Fatal("the setting was not saved")
	}
	host.SetTailnetPairing(true)
	host.SetEnabled(false)
	w := httptest.NewRecorder()
	host.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/remote/pair/tailnet", strings.NewReader(`{"name":"Air"}`)))
	if w.Code != http.StatusForbidden || addons.lookups.Load() != 0 {
		t.Fatalf("sharing off: %d", w.Code)
	}
}

func TestPairingWithoutACodeIsRateLimitedPerDevice(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	for i := 0; i < tailnetTries; i++ {
		if status, _ := pairViaAddon(t, host, addons); status != http.StatusOK {
			t.Fatalf("attempt %d: %d", i+1, status)
		}
	}
	if status, _ := pairViaAddon(t, host, addons); status != http.StatusForbidden {
		t.Fatalf("over the limit: %d", status)
	}
	// Another device is not held up by it.
	addons.client.Peer.ID, addons.client.Peer.Addr, addons.client.Peer.IPs = "nMini11CNTRL", "100.101.2.5:4000", []string{"100.101.2.5"}
	if status, _ := pairViaAddon(t, host, addons); status != http.StatusOK {
		t.Fatalf("another device: %d", status)
	}
}

// fakeSystem answers like the Tailscale command line, with the shapes a
// real Tailscale 1.104 prints.
func fakeSystem(t *testing.T, whois string, calls *[]string) *systemTailscale {
	return &systemTailscale{run: func(_ context.Context, limit int, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		switch {
		case args[0] == "status":
			return []byte(`{"Version":"1.104.5","TUN":true,"BackendState":"Running","HaveNodeKey":true,
				"TailscaleIPs":["100.101.2.3","fd7a:115c:a1e0::3"],
				"Self":{"ID":"nStudio11CNTRL","HostName":"studio","DNSName":"studio.tail1234.ts.net.","UserID":7017007006444457,
					"TailscaleIPs":["100.101.2.3","fd7a:115c:a1e0::3"],"Online":true},
				"MagicDNSSuffix":"tail1234.ts.net",
				"CurrentTailnet":{"Name":"me.github","StableID":"T1","MagicDNSSuffix":"tail1234.ts.net","MagicDNSEnabled":true}}`), nil
		case args[0] == "whois" && whois != "":
			return []byte(whois), nil
		}
		return nil, errors.New("peer not found")
	}}
}

const airWhois = `{"Node":{"ID":1714604128457008,"StableID":"nAir11CNTRL","Name":"air.tail1234.ts.net.","User":7017007006444457,
	"Key":"nodekey:1","Addresses":["100.101.2.4/32","fd7a:115c:a1e0::4/128"],"AllowedIPs":["100.101.2.4/32","fd7a:115c:a1e0::4/128"],
	"Hostinfo":{"OS":"macOS","Hostname":"Air"},"ComputedName":"air","Online":true},
	"UserProfile":{"ID":7017007006444457,"LoginName":"me@github","DisplayName":"Me"},"CapMap":{}}`

// systemRequest is a pairing request as one from the Tailscale app arrives.
func systemRequest(remote, local string, header http.Header) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/remote/pair/tailnet", strings.NewReader(`{"name":"Air"}`))
	r.RemoteAddr = remote
	for k, v := range header {
		r.Header[k] = v
	}
	host, port, _ := net.SplitHostPort(local)
	n, _ := strconv.Atoi(port)
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(host), Port: n}))
}

func TestHostAsksTheTailscaleAppAboutTailscaleConnections(t *testing.T) {
	routed := strings.Replace(airWhois, `"Hostinfo":{`, `"PrimaryRoutes":["192.168.1.0/24"],"Hostinfo":{"RoutableIPs":["192.168.1.0/24"],`, 1)
	for _, tc := range []struct {
		name, whois, remote, local string
		want                       int
	}{
		{"the user's own device", airWhois, "100.101.2.4:51234", "100.101.2.3:8788", http.StatusOK},
		{"over IPv6", airWhois, "[fd7a:115c:a1e0::4]:51234", "[fd7a:115c:a1e0::3]:8788", http.StatusOK},
		{"another user's device", strings.Replace(airWhois, `"User":7017007006444457`, `"User":7017007006444999`, 1), "100.101.2.4:51234", "100.101.2.3:8788", http.StatusForbidden},
		{"a tagged device", strings.Replace(airWhois, `"Hostinfo"`, `"Tags":["tag:server"],"Hostinfo"`, 1), "100.101.2.4:51234", "100.101.2.3:8788", http.StatusForbidden},
		{"a shared-in device", strings.Replace(airWhois, `"Hostinfo"`, `"Sharer":7017007006444999,"Hostinfo"`, 1), "100.101.2.4:51234", "100.101.2.3:8788", http.StatusForbidden},
		{"a subnet router", routed, "100.101.2.4:51234", "100.101.2.3:8788", http.StatusForbidden},
		{"a connection to this computer's LAN address", airWhois, "100.101.2.4:51234", "192.168.1.10:8788", http.StatusForbidden},
		{"an address Tailscale does not know", "", "100.101.2.77:51234", "100.101.2.3:8788", http.StatusForbidden},
		{"an answer with more after it", airWhois + `{"Node":{}}`, "100.101.2.4:51234", "100.101.2.3:8788", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tailnetHost(t, newFakeAddons())
			var calls []string
			host.system = fakeSystem(t, tc.whois, &calls)
			w := httptest.NewRecorder()
			host.ServeHTTP(w, systemRequest(tc.remote, tc.local, nil))
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s (asked %v)", w.Code, tc.want, w.Body, calls)
			}
			if tc.want == http.StatusOK && host.Status().Devices[0].Tailscale != "air.tail1234.ts.net" {
				t.Fatalf("devices: %+v", host.Status().Devices)
			}
		})
	}
}

// Who is calling never comes from the request: a device on the local
// network that claims a Tailscale identity in headers gets nowhere, and
// Tailscale is not even asked.
func TestHostIgnoresTailscaleClaimsFromTheLocalNetwork(t *testing.T) {
	host := tailnetHost(t, newFakeAddons())
	var calls []string
	host.system = fakeSystem(t, airWhois, &calls)
	claims := http.Header{"X-Forwarded-For": {"100.101.2.4"}, "X-Real-Ip": {"100.101.2.4"}, "Forwarded": {"for=100.101.2.4"},
		"Tailscale-User-Login": {"me@github"}, "Tailscale-User-Name": {"Me"}, "X-Switcher-Tailnet": {"token"},
		"X-Switcher-Identity": {"eyJzZWxmIjp7fX0"}, "X-Switcher-Phone-Node": {"nAir11CNTRL"}}
	for _, remote := range []string{"192.168.1.20:5555", "10.0.0.8:5555", "[fe80::1]:5555", "127.0.0.1:5555"} {
		w := httptest.NewRecorder()
		host.ServeHTTP(w, systemRequest(remote, "100.101.2.3:8788", claims))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), pairFailed) {
			t.Fatalf("%s: %d %s", remote, w.Code, w.Body)
		}
	}
	if len(calls) != 0 || len(host.Status().Devices) != 0 {
		t.Fatalf("asked Tailscale %v, devices %+v", calls, host.Status().Devices)
	}
}

func TestHostRefusesThisComputerItselfOverTailscale(t *testing.T) {
	host := tailnetHost(t, newFakeAddons())
	var calls []string
	self := strings.NewReplacer("nAir11CNTRL", "nStudio11CNTRL", "100.101.2.4", "100.101.2.3", "fd7a:115c:a1e0::4", "fd7a:115c:a1e0::3").Replace(airWhois)
	host.system = fakeSystem(t, self, &calls)
	w := httptest.NewRecorder()
	host.ServeHTTP(w, systemRequest("100.101.2.3:5555", "100.101.2.3:8788", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("another account on this computer paired: %d", w.Code)
	}
}

func TestPairingFromTheTailscaleAppIsRateLimitedPerSource(t *testing.T) {
	host := tailnetHost(t, newFakeAddons())
	var calls []string
	host.system = fakeSystem(t, "", &calls)
	for i := 0; i < tailnetTries+3; i++ {
		w := httptest.NewRecorder()
		host.ServeHTTP(w, systemRequest("100.101.2.77:51234", "100.101.2.3:8788", nil))
	}
	whois := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "whois") {
			whois++
		}
	}
	if whois != tailnetTries {
		t.Fatalf("asked Tailscale %d times for one source, want %d", whois, tailnetTries)
	}
}

func TestPreambleIsReadOnlyFromLoopbackAndOnlyWhenWellFormed(t *testing.T) {
	read := func(input string) (*loopbackConn, string, error) {
		a, b := net.Pipe()
		go func() { io.WriteString(b, input); b.Close() }()
		c := &loopbackConn{Conn: a}
		rest, err := io.ReadAll(c)
		return c, string(rest), err
	}
	id := strings.Repeat("ab", 16)
	if c, rest, _ := read(tailnetPreamble + id + "\n\x16\x03\x01hello"); c.tailnetID() != id || rest != "\x16\x03\x01hello" {
		t.Fatalf("preamble: id %q rest %q", c.tailnetID(), rest)
	}
	if c, rest, _ := read("\x16\x03\x01" + strings.Repeat("x", 200)); c.tailnetID() != "" || len(rest) != 203 {
		t.Fatalf("TLS without preamble: id %q, %d bytes", c.tailnetID(), len(rest))
	}
	for _, bad := range []string{tailnetPreamble + "short\n", tailnetPreamble + strings.ToUpper(id) + "\n", tailnetPreamble + id + strings.Repeat("0", 40) + "\n", "SWITCHER-OTHER " + id + "\n"} {
		if c, _, err := read(bad); err == nil || c.tailnetID() != "" {
			t.Fatalf("%q accepted", bad)
		}
	}
}
