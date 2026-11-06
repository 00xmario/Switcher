package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const studio = "switcher-studio.tail1234.ts.net"

// tailnetClient is a client whose Tailscale add-on is fake.
func tailnetClient(t *testing.T, addons *fakeAddons) *Client {
	t.Helper()
	client := NewClient(t.TempDir(), "")
	client.identified = addons.dialIdentified
	client.tailnet = addons.dial
	client.ownTailnet = func() ([]string, error) { return nil, nil }
	client.system = noTailscaleApp(t)
	return client
}

// A host with all of this device's pairings in use says so, and the client
// says what to do.
func TestClientSaysWhenItsDeviceHasTooManyPairings(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	for i := 0; i < perNode; i++ {
		pairViaAddonAs(t, host, addons, fmt.Sprintf("%032x", i+1))
	}
	client := tailnetClient(t, addons)
	_, err := client.ConnectTailnet(context.Background(), studio, host.Status().Port, "Air")
	var needsCode *NeedsCodeError
	if !errors.As(err, &needsCode) || !strings.Contains(err.Error(), "Too many Switchers on this device are paired") || client.Paired() {
		t.Fatalf("error %v", err)
	}
}

// The client too trusts a host only when it is certain the host is not the
// Tailscale app's node on this very computer.
func TestClientFailsClosedWhenItCannotRuleOutItsOwnTailscaleApp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		system  func(t *testing.T) *systemTailscale
		paired  bool
		message string
	}{
		{"a Tailscale daemon that cannot be asked", func(*testing.T) *systemTailscale {
			return &systemTailscale{run: failingRun(errNoTailscale), present: func() bool { return true }}
		}, false, "could not confirm"},
		{"the host is this computer's own Tailscale app", func(*testing.T) *systemTailscale {
			return fakeSystem(t, "", new([]string)) // its node is the Studio's, the host
		}, false, "this computer itself"},
		{"no Tailscale app here", noTailscaleApp, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addons := newFakeAddons()
			host := tailnetHost(t, addons)
			client := tailnetClient(t, addons)
			client.system = tc.system(t)
			_, err := client.ConnectTailnet(context.Background(), studio, host.Status().Port, "Air")
			if tc.paired {
				if err != nil || !client.Paired() {
					t.Fatalf("not paired: %v", err)
				}
				return
			}
			var needsCode *NeedsCodeError
			if !errors.As(err, &needsCode) || !strings.Contains(err.Error(), tc.message) || addons.lookups.Load() != 0 || client.Paired() {
				t.Fatalf("error %v, %d lookups", err, addons.lookups.Load())
			}
		})
	}
}

// Whatever answers on this computer's own Tailscale address, or under
// another name than the one asked for, is no host to trust without a code.
func TestClientRefusesItselfAndHostsThatAnswerUnderAnotherName(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		own           []string
		ownErr        error
	}{
		{"this computer's own address", studio, []string{"100.101.2.3"}, nil},
		{"unknown own addresses", studio, nil, errors.New("no interfaces")},
		{"another name", "switcher-mini.tail1234.ts.net", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addons := newFakeAddons()
			host := tailnetHost(t, addons)
			client := tailnetClient(t, addons)
			client.ownTailnet = func() ([]string, error) { return tc.own, tc.ownErr }
			_, err := client.ConnectTailnet(context.Background(), tc.address, host.Status().Port, "Air")
			var needsCode *NeedsCodeError
			if !errors.As(err, &needsCode) || addons.lookups.Load() != 0 || client.Paired() {
				t.Fatalf("error %v, %d lookups", err, addons.lookups.Load())
			}
		})
	}
}

func inference(t *testing.T, client *Client) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	resp, _, err := client.Inference(ctx, "a", r, []byte(`{}`))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestClientPairsWithTheUsersOwnHostWithoutACode(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	client := tailnetClient(t, addons)
	status, err := client.ConnectTailnet(context.Background(), studio, host.Status().Port, "Air")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Connected || status.Address != studio || len(host.Status().Devices) != 1 {
		t.Fatalf("status %+v, devices %+v", status, host.Status().Devices)
	}
	if got, err := inference(t, client); err != nil || !strings.Contains(got, "device=Air") {
		t.Fatalf("through the host: %q %v", got, err)
	}
	// The host knows this Switcher by its own id, which outlives the pairing.
	install, err := client.installID()
	if err != nil || host.state.Devices[0].TailscaleInstall != install {
		t.Fatalf("install id %q, host has %q (%v)", install, host.state.Devices[0].TailscaleInstall, err)
	}
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if again, _ := NewClient(client.dir, "").installID(); again != install {
		t.Fatal("the install id changed with the pairing")
	}
}

// The client checks the host before it sends anything: a host Tailscale
// does not vouch for as the user's own gets no pairing request, and the
// client asks for a code instead.
func TestClientRefusesAHostOfAnotherUser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*tailnetIdentity)
	}{
		{"another user's host", func(id *tailnetIdentity) { id.Peer.User = someone }},
		{"a tagged host", func(id *tailnetIdentity) { id.Peer.Tagged = true }},
		{"a host shared in from another tailnet", func(id *tailnetIdentity) { id.Peer.Shared = true }},
		{"a host in another tailnet", func(id *tailnetIdentity) { id.Peer.Name = "switcher-studio.tail9999.ts.net" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addons := newFakeAddons()
			tc.change(&addons.host)
			host := tailnetHost(t, addons)
			client := tailnetClient(t, addons)
			_, err := client.ConnectTailnet(context.Background(), studio, host.Status().Port, "Air")
			var needsCode *NeedsCodeError
			if !errors.As(err, &needsCode) || !strings.Contains(err.Error(), "needs a pairing code") {
				t.Fatalf("error %v", err)
			}
			if addons.lookups.Load() != 0 || len(host.Status().Devices) != 0 || client.Paired() {
				t.Fatalf("a pairing request went out: %d lookups", addons.lookups.Load())
			}
			if _, statErr := os.Stat(client.path()); !os.IsNotExist(statErr) {
				t.Fatal("a pairing was saved")
			}
		})
	}
}

// The certificate pinned is the one presented on the very connection
// Tailscale vouched for: pairing opens no second connection, which could
// reach another machine.
func TestClientPinsTheCertificateOfTheVerifiedConnection(t *testing.T) {
	addons := newFakeAddons()
	host := tailnetHost(t, addons)
	other, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	other.Quiet = true
	t.Cleanup(other.Close)
	other.SetEnabled(true)
	// Any other connection to the host's name ends up at the other host.
	addons.redirect = other.Status().Port
	client := tailnetClient(t, addons)
	if _, err := client.ConnectTailnet(context.Background(), studio, host.Status().Port, "Air"); err != nil {
		t.Fatal(err)
	}
	if addons.dials.Load() != 1 {
		t.Fatalf("pairing used %d verified connections", addons.dials.Load())
	}
	if saved := NewClient(client.dir, "").cfg; saved.Fingerprint != host.Fingerprint() || saved.Fingerprint == other.Fingerprint() {
		t.Fatal("pinned another certificate than the verified host's")
	}
	if _, err := inference(t, client); err == nil {
		t.Fatal("the pinned client talked to another host")
	}
	addons.redirect = 0
	if got, err := inference(t, client); err != nil || !strings.Contains(got, "device=Air") {
		t.Fatalf("the paired host: %q %v", got, err)
	}
}

// Switching hosts pairs with the new host first and keeps the old pairing
// whenever that fails; once it works, the old host drops this computer.
func TestSwitchingHostsKeepsTheOldPairingWhenTheNewOneFails(t *testing.T) {
	old, client := pairedPair(t)
	addons := newFakeAddons()
	client.UseTailnet(addons.dial)
	client.identified = addons.dialIdentified
	client.ownTailnet = func() ([]string, error) { return nil, nil }
	client.system = noTailscaleApp(t)
	next := tailnetHost(t, addons)
	keepsOld := func(when string) {
		t.Helper()
		if !client.Connected() || client.cfg.Fingerprint != old.Fingerprint() || NewClient(client.dir, "").cfg.Fingerprint != old.Fingerprint() {
			t.Fatalf("%s: the old pairing is gone", when)
		}
		if got, err := inference(t, client); err != nil || !strings.Contains(got, "device=Laptop") {
			t.Fatalf("%s: the old host no longer serves: %q %v", when, got, err)
		}
	}
	next.SetTailnetPairing(false)
	_, err := client.ConnectTailnet(context.Background(), studio, next.Status().Port, "Laptop")
	var needsCode *NeedsCodeError
	if !errors.As(err, &needsCode) {
		t.Fatalf("a refusing host: %v", err)
	}
	keepsOld("after a refusal")
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, err := client.ConnectTailnet(context.Background(), studio, port, "Laptop"); err == nil {
		t.Fatal("paired with nothing")
	}
	keepsOld("after an unreachable host")
	if _, err := client.Connect(context.Background(), "127.0.0.1", next.Status().Port, "AAAA-AAAA", "Laptop"); err == nil {
		t.Fatal("paired with a wrong code")
	}
	keepsOld("after a wrong code")

	next.SetTailnetPairing(true)
	if _, err := client.ConnectTailnet(context.Background(), studio, next.Status().Port, "Laptop"); err != nil {
		t.Fatal(err)
	}
	if client.cfg.Fingerprint != next.Fingerprint() || len(next.Status().Devices) != 1 {
		t.Fatal("not switched")
	}
	for deadline := time.Now().Add(5 * time.Second); len(old.Status().Devices) != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the old host kept this computer's pairing")
		}
	}
}

// addressedConn is a connection with the addresses a real one through the
// Tailscale app would have.
type addressedConn struct {
	net.Conn
	local, remote net.Addr
}

func (c addressedConn) LocalAddr() net.Addr  { return c.local }
func (c addressedConn) RemoteAddr() net.Addr { return c.remote }

func tcpAddr(s string) net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", s)
	return a
}

// Without the add-on, the client asks the Tailscale app about the exact
// address its connection reached, and only for a connection that left
// from this computer's own Tailscale address.
func TestClientAsksTheTailscaleAppAboutTheAddressItReached(t *testing.T) {
	client := NewClient(t.TempDir(), "")
	client.ownTailnet = func() ([]string, error) { return nil, nil }
	var calls []string
	// The Tailscale app here is the Studio's; the host is the Air.
	client.system = fakeSystem(t, airWhois, &calls)
	local := "100.101.2.3:50000"
	client.dialDirect = func(ctx context.Context, network, address string) (net.Conn, error) {
		a, _ := net.Pipe()
		return addressedConn{Conn: a, local: tcpAddr(local), remote: tcpAddr("100.101.2.4:8788")}, nil
	}
	conn, id, err := client.dialVerified(context.Background(), "air.tail1234.ts.net:8788")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if notOwnDevice(*id) != "" || id.Peer.ID != "nAir11CNTRL" || strings.Join(calls, ";") != "status --json --peers=false;whois --json 100.101.2.4" {
		t.Fatalf("identity %+v, asked %v", id, calls)
	}
	// A connection that left from another address did not go over
	// Tailscale, whatever address it reached.
	calls, local = nil, "192.168.1.20:50000"
	_, _, err = client.dialVerified(context.Background(), "air.tail1234.ts.net:8788")
	var needsCode *NeedsCodeError
	if !errors.As(err, &needsCode) || strings.Contains(strings.Join(calls, ";"), "whois") {
		t.Fatalf("%v, asked %v", err, calls)
	}
	// A loopback connection did not go over Tailscale: no identity at all.
	calls, client.dialDirect = nil, (&net.Dialer{}).DialContext
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	if _, _, err := client.dialVerified(context.Background(), ln.Addr().String()); !errors.As(err, &needsCode) || len(calls) != 0 {
		t.Fatalf("%v, asked %v", err, calls)
	}
	if _, err := client.ConnectTailnet(context.Background(), "studio.local", 0, "Air"); !errors.As(err, &needsCode) {
		t.Fatalf("a LAN name: %v", err)
	}
}

func TestPairedDeviceCanDropItsOwnPairing(t *testing.T) {
	host, client := pairedPair(t)
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(host.Status().Devices) != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the host kept the pairing")
		}
	}
	// Nobody else can: the route needs the device's own token.
	w := httptest.NewRecorder()
	host.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/remote/unpair", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unpair without a token: %d", w.Code)
	}
}
