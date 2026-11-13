package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"switcher/internal/usage"
)

// hostMux stands in for the host's server: it echoes what reached it.
func hostMux(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(DeviceHeader) != "" || r.Header.Get("Cookie") != "" {
			t.Error("device token or cookie reached the host's routes")
		}
		for _, name := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key", "Chatgpt-Account-Id", "X-Management-Key"} {
			if r.Header.Get(name) != "" {
				t.Errorf("this Mac's %s reached the host", name)
			}
		}
		switch {
		case r.URL.Path == "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": "host-account", "provider": "claude"}}, "version": "host-version", "hub_management_key": "host-key"})
		default:
			b, _ := io.ReadAll(r.Body)
			device, _ := PairedDevice(r)
			io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(b)+" auth="+r.Header.Get("Authorization")+" account="+r.Header.Get(AccountHeader)+" device="+device)
		}
	})
}

func pairedPair(t *testing.T) (*Host, *Client) {
	t.Helper()
	return pairedWith(t, hostMux(t))
}

// pairedWith pairs a client with a host whose routes next serves.
func pairedWith(t *testing.T, next http.Handler) (*Host, *Client) {
	t.Helper()
	host, err := NewHost(t.TempDir(), 0, next)
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	code, err := host.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(t.TempDir(), "client-key")
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, strings.ToLower(code.Code), "Laptop"); err != nil {
		t.Fatal(err)
	}
	return host, client
}

func TestPairedClientUsesTheHostForAccountsAndProviders(t *testing.T) {
	host, client := pairedPair(t)
	if s := host.Status(); len(s.Devices) != 1 || s.Devices[0].Name != "Laptop" || s.Pairing != nil {
		t.Fatalf("host status after pairing: %+v", s)
	}
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local "+r.URL.Path) })
	handler := client.Middleware(func(*http.Request) map[string]any {
		return map[string]any{"version": "client-version", "hub_management_key": "client-key"}
	}, local)
	get := func(method, path, body string, header http.Header) string {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range header {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Body.String()
	}
	var state map[string]any
	json.Unmarshal([]byte(get("GET", "/api/state", "", nil)), &state)
	if state["version"] != "client-version" || state["hub_management_key"] != "client-key" || len(state["accounts"].([]any)) != 1 || state["remote"].(map[string]any)["connected"] != true {
		t.Fatalf("merged state: %v", state)
	}
	if got := get("POST", "/codex/v1/responses?x=1", `{"model":"m"}`, http.Header{"Authorization": {"Bearer cli"}, "Cookie": {"s=1"}}); got != `POST /codex/v1/responses?x=1 {"model":"m"} auth= account= device=Laptop` {
		t.Fatalf("provider request: %q", got)
	}
	if got := get("GET", "/v0/management/auth-files", "", http.Header{"Authorization": {"Bearer client-key"}}); !strings.HasPrefix(got, "GET /v0/management/auth-files  auth= ") {
		t.Fatalf("management request: %q", got)
	}
	if got := get("GET", "/v0/management/auth-files", "", http.Header{"Authorization": {"Bearer wrong"}}); !strings.Contains(got, "management key required") {
		t.Fatalf("unauthenticated management request: %q", got)
	}
	if got := get("GET", "/api/settings", "", nil); got != "local /api/settings" {
		t.Fatalf("local route: %q", got)
	}
	r := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(`{}`))
	resp, ok, err := client.Inference(context.Background(), "host-account", r, []byte(`{}`))
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "POST /remote/anthropic/v1/messages?beta=true {} auth= account=host-account device=Laptop" {
		t.Fatalf("inference: %q", b)
	}
}

func TestPairingRejectsWrongCodesAndRevokedDevices(t *testing.T) {
	host, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	host.SetEnabled(true)
	host.NewPairingCode()
	code := host.Status().Pairing.Code
	if len(code) != 14 || strings.Count(code, "-") != 2 {
		t.Fatalf("pairing code %q is not 12 characters in fours", code)
	}
	client := NewClient(t.TempDir(), "")
	// Wrong guesses do not use the code up: anyone could otherwise keep
	// pairing from working by sending a few.
	for i := 0; i < 20; i++ {
		if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, "AAAA-AAAA-AAAA", "x"); err == nil {
			t.Fatal("wrong code paired")
		}
	}
	if host.Status().Pairing == nil {
		t.Fatal("wrong guesses ended the code")
	}
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, strings.ToLower(code), "x"); err != nil {
		t.Fatalf("the right code after wrong guesses: %v", err)
	}
	if host.Status().Pairing != nil {
		t.Fatal("a code was left after it paired")
	}
	_, client = pairedPair(t)
	_ = client
	host2, client2 := pairedPair(t)
	if err := host2.Revoke(host2.Status().Devices[0].ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	if _, _, err := client2.Inference(context.Background(), "", r, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "pair again") {
		t.Fatalf("revoked device: %v", err)
	}
	if s := client2.Status(); s.Reachable || !strings.Contains(s.Error, "pair again") {
		t.Fatalf("client status after revocation: %+v", s)
	}
}

// A device in the middle with its own certificate cannot use a code it relays.
func TestPairingProofIsBoundToTheHostCertificateAndRole(t *testing.T) {
	a, _ := pairingKey("ABCD-EFGH", "aa")
	b, _ := pairingKey("ABCD-EFGH", "bb")
	c, _ := pairingKey("abcd efgh", "aa")
	if pairingProof(a, "client", "n") == pairingProof(b, "client", "n") {
		t.Fatal("proof ignores the certificate")
	}
	if pairingProof(a, "client", "n") != pairingProof(c, "client", "n") {
		t.Fatal("code formatting changes the proof")
	}
	if pairingProof(a, "client", "n") == pairingProof(a, "host", "n") {
		t.Fatal("the host could replay the client's proof")
	}
}

// A device pretending to be the host, without the code, cannot complete a
// pairing: the client checks the host's proof before saving anything.
func TestClientRefusesAHostThatDoesNotKnowTheCode(t *testing.T) {
	var asked bool
	rogue := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
		json.NewEncoder(w).Encode(pairResponse{DeviceID: "d", Token: "stolen", HostName: "Studio Mac", HostProof: "made-up"})
	}))
	defer rogue.Close()
	host, port, _ := net.SplitHostPort(rogue.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	client := NewClient(t.TempDir(), "")
	_, err := client.Connect(context.Background(), host, n, "ABCD-EFGH", "Laptop")
	if !asked || err == nil || client.Connected() {
		t.Fatalf("paired with a host that does not know the code: asked=%v err=%v", asked, err)
	}
	if _, statErr := os.Stat(client.path()); !os.IsNotExist(statErr) {
		t.Fatal("pairing was saved")
	}
}

func TestDisconnectRestoresLocalRoutes(t *testing.T) {
	_, client := pairedPair(t)
	var changes []bool
	client.OnChange(func(connected bool) { changes = append(changes, connected) })
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local") }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/codex/v1/responses", nil))
	if w.Body.String() != "local" || len(changes) != 2 || changes[0] != true || changes[1] != false {
		t.Fatalf("after disconnect: %q %v", w.Body.String(), changes)
	}
}

func TestHostServesOnlyThisMacTheLocalNetworkAndTailscale(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "192.168.178.20": true, "10.0.0.4": true, "172.16.5.1": true,
		"169.254.1.2": true, "fe80::1": true, "fd12:3456::1": true, "100.101.2.3": true, "fd7a:115c:a1e0::1": true,
		"::ffff:192.168.1.5": true,
		"8.8.8.8":            false, "84.105.1.2": false, "100.128.0.1": false, "2a02:a210::1": false, "::ffff:84.105.1.2": false,
	} {
		if got := privateSource(net.ParseIP(ip)); got != want {
			t.Errorf("%s: accepted=%v, want %v", ip, got, want)
		}
	}
}

func TestClientLearnsTheHostsNewAddresses(t *testing.T) {
	host, client := pairedPair(t)
	host.addresses = func() ([]string, []string) { return []string{"studio.local"}, []string{"100.101.2.3"} }
	client.refreshAddresses()
	saved := NewClient(client.dir, "")
	want := []string{"studio.local", "100.101.2.3", "127.0.0.1"}
	if got := saved.cfg.Addresses; strings.Join(got[:3], ",") != strings.Join(want, ",") {
		t.Fatalf("saved addresses %v, want first %v", got, want)
	}
	client.refreshAddresses() // rate limited: no second request within two minutes
}

func TestPairedMacsCannotManagePhoneAccess(t *testing.T) {
	for _, path := range []string{"/api/phone", "/api/phone/approve", "/api/phone/access", "/api/phone/devices/x"} {
		if allowedRemotePath(path) {
			t.Errorf("%s is open to paired devices", path)
		}
	}
}

// Requests forwarded to the host never carry this Mac's own credentials:
// on /api/ routes the local device token, which also unlocks this Mac's LAN
// listener, and on provider routes the CLIs' own logins. The host answers
// with its own accounts and never reads them.
func TestClientKeepsItsCredentialsFromTheHost(t *testing.T) {
	_, client := pairedPair(t)
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	for _, path := range []string{"/api/accounts/host-account/activate", "/api/usage/refresh", "/api/providers/codex/hide",
		"/codex/v1/responses", "/claude/v1/messages", "/gemini/v1internal:generateContent"} {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("Authorization", "Bearer local-device-token")
		r.Header.Set("X-Management-Key", "client-key")
		r.Header.Set("X-Api-Key", "sk-local")
		r.Header.Set("Api-Key", "sk-local")
		r.Header.Set("X-Goog-Api-Key", "local")
		r.Header.Set("ChatGPT-Account-Id", "local-account")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if got := w.Body.String(); !strings.HasPrefix(got, "POST "+path) || !strings.Contains(got, " auth= ") {
			t.Fatalf("%s reached the host as %q", path, got)
		}
	}
}

// The host's state is drawn by this Mac's dashboard and menu, which put ids
// into markup and request paths. Only ids shaped like Switcher's own reach
// them, so a hostile host cannot inject markup or a path such as ../update.
func TestClientDropsHostIDsThatSwitcherNeverGenerates(t *testing.T) {
	_, client := pairedWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"accounts": []any{
				map[string]any{"id": "claude-1a2b", "provider": "claude", "email": "a@example.com"},
				map[string]any{"id": "../claude/sync?", "provider": "claude"},
				map[string]any{"id": "codex-9f", "provider": "x\"><meta http-equiv=refresh content=0>"},
				map[string]any{"id": 7, "provider": "codex"},
				"not an account",
			},
			"order":  []any{"claude", "x\"><style>", "codex", 3},
			"hidden": []any{"../update?"},
			"active": map[string]any{"claude": "claude-1a2b", "codex": "../update?", "x\"": "claude-1a2b"},
		})
	}))
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	var state struct {
		Accounts []map[string]any `json:"accounts"`
		Order    []any            `json:"order"`
		Hidden   []any            `json:"hidden"`
		Active   map[string]any   `json:"active"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Accounts) != 1 || state.Accounts[0]["id"] != "claude-1a2b" || state.Accounts[0]["email"] != "a@example.com" {
		t.Fatalf("accounts: %v", state.Accounts)
	}
	if len(state.Order) != 2 || state.Order[0] != "claude" || state.Order[1] != "codex" || len(state.Hidden) != 0 {
		t.Fatalf("order %v hidden %v", state.Order, state.Hidden)
	}
	if len(state.Active) != 1 || state.Active["claude"] != "claude-1a2b" {
		t.Fatalf("active: %v", state.Active)
	}
}

// A host is another computer. Whatever it answers reaches the browser under
// this Switcher's own address, so it must stay data: no page, no script, no
// cookie, no way out.
func TestHostAnswersCannotRunOnThisSwitchersDashboard(t *testing.T) {
	hostile := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Set-Cookie", "switcher_session=stolen; Path=/")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Refresh", "0; url=https://attacker.example/")
		h.Set("Content-Security-Policy", "script-src *")
		switch r.URL.Path {
		case "/api/providers/page":
			h.Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<script src="/api/providers/code.js"></script>`)
		case "/api/providers/code.js":
			h.Set("Content-Type", "text/javascript")
			io.WriteString(w, `fetch('/api/state')`)
		case "/api/providers/svg":
			h.Set("Content-Type", "image/svg+xml")
			io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
		case "/api/providers/odd":
			h.Set("Content-Type", "text/html;;;")
			io.WriteString(w, "<b>x</b>")
		case "/api/providers/twice":
			// A browser takes the last of two content types.
			h.Add("Content-Type", "application/json")
			h.Add("Content-Type", "text/html")
			io.WriteString(w, "<b>x</b>")
		case "/api/providers/untyped":
			// Without a type the server passing it on would guess one.
			h["Content-Type"] = nil
			io.WriteString(w, "<!doctype html><html><body><script>1</script></body></html>")
		case "/api/providers/download":
			h.Set("Content-Type", "application/json")
			h.Set("Content-Disposition", `attachment; filename="Switcher Update.command"`)
			io.WriteString(w, "{}")
		case "/api/providers/away":
			h.Set("Location", "https://attacker.example/")
			w.WriteHeader(http.StatusFound)
		case "/api/providers/away2":
			h.Set("Location", "//attacker.example/")
			w.WriteHeader(http.StatusFound)
		case "/api/providers/stay":
			h.Set("Location", "/api/quit")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/codex/redirect", "/remote/anthropic/v1/redirect":
			h.Set("Location", "https://attacker.example/v1/messages")
			w.WriteHeader(http.StatusPermanentRedirect)
		case "/codex/v1/responses":
			h.Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {}\n\n")
		case "/api/accounts/a/refresh":
			h.Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"ok"}`)
		case "/v0/management/x":
			h.Set("Content-Type", "application/problem+json")
			io.WriteString(w, `{}`)
		}
	})
	_, client := pairedWith(t, hostile)
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	serve := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer client-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		h := w.Header()
		for _, name := range []string{"Set-Cookie", "Access-Control-Allow-Origin", "Refresh"} {
			if h.Get(name) != "" {
				t.Errorf("%s: the host's %s reached the browser", path, name)
			}
		}
		if w.Code == http.StatusBadGateway {
			// This Switcher's own refusal, as JSON.
			if !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
				t.Errorf("%s: refusal is %q", path, h.Get("Content-Type"))
			}
			return w
		}
		if csp := h.Values("Content-Security-Policy"); len(csp) != 1 || !strings.HasPrefix(csp[0], "sandbox;") {
			t.Errorf("%s: answer is not a sandboxed document: %v", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: answer may be sniffed", path)
		}
		return w
	}
	if w := serve("/api/providers/download"); w.Header().Get("Content-Disposition") != "" {
		t.Errorf("the host named a file to save: %q", w.Header().Get("Content-Disposition"))
	}
	for _, path := range []string{"/api/providers/page", "/api/providers/code.js", "/api/providers/svg", "/api/providers/odd", "/api/providers/twice", "/api/providers/untyped"} {
		w := serve(path)
		if kinds := w.Header().Values("Content-Type"); len(kinds) != 1 {
			t.Errorf("%s has %d content types", path, len(kinds))
		}
		if got := w.Header().Get("Content-Type"); got != "application/octet-stream" || w.Header().Get("Content-Disposition") != "attachment" {
			t.Errorf("%s would render or run as %q", path, got)
		}
	}
	for path, want := range map[string]string{"/codex/v1/responses": "text/event-stream", "/api/accounts/a/refresh": "application/json", "/v0/management/x": "application/problem+json"} {
		w := serve(path)
		if got := w.Header().Get("Content-Type"); got != want || w.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: data answer changed to %q", path, got)
		}
	}
	if w := serve("/codex/v1/responses"); w.Body.String() != "data: {}\n\n" {
		t.Errorf("stream body changed: %q", w.Body.String())
	}
	// No redirect is passed on: followed, it would repeat the request with
	// this Mac's credentials at this Switcher's own controls or elsewhere.
	for _, path := range []string{"/api/providers/away", "/api/providers/away2", "/api/providers/stay", "/codex/redirect"} {
		w := serve(path)
		if w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "redirect") {
			t.Errorf("%s: a host's redirect was passed on: %d %q", path, w.Code, w.Header().Get("Location"))
		}
	}
	r := httptest.NewRequest("POST", "/v1/redirect", strings.NewReader(`{}`))
	if resp, ok, err := client.Inference(context.Background(), "", r, []byte(`{}`)); !ok || err == nil || resp != nil {
		t.Errorf("Claude was handed the host's redirect: %v %v", ok, err)
	}
	r = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	resp, ok, err := client.Inference(context.Background(), "", r, []byte(`{}`))
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	resp.Body.Close()
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("the host's cookie reached Claude")
	}
}

// Names and messages other computers choose reach this user's screens and
// terminal without control characters.
func TestNetworkSuppliedTextIsPlain(t *testing.T) {
	esc := "\x1b]0;owned\a\x1b[2K"
	// A Switcher advertised on the network, as avahi-browse reports it.
	out := []byte("=;eth0;IPv4;Studio\\027]0\\059owned\\007;_switcher._tcp;local;studio.local;192.168.1.5;8788;\"v=1\" \"fp=0123456789abcdef\"\n" +
		"=;eth0;IPv4;Bad\\032Address;_switcher._tcp;local;evil\\027[2J.local;192.168.1.6;8788;\"v=1\"\n")
	found := Presentable(parseAvahi(out, ""))
	if len(found) != 1 || strings.ContainsAny(found[0].Name, "\x1b\a") || !strings.HasPrefix(found[0].Name, "Studio") {
		t.Fatalf("discovered hosts: %+v", found)
	}
	for _, f := range Presentable([]Found{{Name: esc, Address: "a.local"}, {Name: "ok", Address: "a b"}, {Name: "ok", Address: "a.local", Tailnet: "x\x1b"}, {Name: " Mac‮ ", Address: "fe80::1%en0"}}) {
		if f.Name != "]0;owned[2K" && f.Name != "Mac" {
			t.Fatalf("kept %+v", f)
		}
	}
	// The name a paired device chooses.
	if d, _ := newDevice("Laptop" + esc + "\u009b31m"); d.Name != "Laptop]0;owned[2K31m" {
		t.Fatalf("device name: %q", d.Name)
	}
	if d, _ := newDevice("\x1b\x07\x00"); d.Name != "Paired device" {
		t.Fatalf("empty device name: %q", d.Name)
	}
	// Everything a host says about its accounts.
	state := map[string]any{
		"accounts": []any{map[string]any{"id": "a", "provider": "claude", "email": "you@example.com" + esc, "plan": "max\r\x1b[31m",
			"health": map[string]any{"message": "re" + esc + "login", "bad\x1bkey": "x"},
			"usage":  map[string]any{"windows": []any{map[string]any{"label": "Weekly‮\x1b[1m", "used_percent": 12.5}}}}},
		"active": map[string]any{"claude": "a"},
		"note":   strings.Repeat("long ", 200) + "\x1b",
	}
	sanitizeHostState(state)
	encoded, _ := json.Marshal(state)
	for _, bad := range []string{"\\u001b", "\\u0007", "\\r", "‮", "bad"} {
		if strings.Contains(string(encoded), bad) {
			t.Fatalf("%q survived in the host's state: %s", bad, encoded)
		}
	}
	account := state["accounts"].([]any)[0].(map[string]any)
	if account["email"] != "you@example.com]0;owned[2K" || account["usage"].(map[string]any)["windows"].([]any)[0].(map[string]any)["used_percent"] != 12.5 {
		t.Fatalf("account after cleaning: %v", account)
	}
	if len(state["note"].(string)) != 1000 {
		t.Fatalf("long text was cut: %d", len(state["note"].(string)))
	}
}

// Whatever answers at an address writes the pairing error, before any proof.
func TestPairingErrorFromAnUnknownHostIsPlain(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "Connected\x1b[2K\x1b]52;c;aGk=\a to Studio"})
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	_, err := NewClient(t.TempDir(), "").Connect(context.Background(), host, n, "ABCD-EFGH", "Laptop")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a") || !strings.Contains(err.Error(), "to Studio") {
		t.Fatalf("pairing error: %q", err)
	}
}

// Removing a device ends what it already started, such as an answer still
// streaming, not only its next request.
func TestRemovingADeviceEndsItsRequestsInFlight(t *testing.T) {
	started, ended := make(chan struct{}, 1), make(chan struct{}, 1)
	host, client := pairedWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/stream" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
		ended <- struct{}{}
	}))
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/codex/stream", strings.NewReader("{}")))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never started")
	}
	if err := host.Revoke(host.Status().Devices[0].ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a removed device's stream kept running")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the removed device's request never returned")
	}
	host.mu.Lock()
	left := len(host.running)
	host.mu.Unlock()
	if left != 0 {
		t.Fatalf("requests of removed devices are still tracked: %d", left)
	}
}

// One paired device holds only so many requests at once on the host; the
// next is told to try again, and room comes back as requests finish.
func TestADeviceHasABoundedNumberOfRequestsRunning(t *testing.T) {
	started := make(chan struct{}, perDeviceRequests+8)
	release := make(chan struct{})
	host, client := pairedWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/codex/hold" {
			started <- struct{}{}
			<-release
		}
		io.WriteString(w, "done")
	}))
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	call := func(path string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		return w.Code
	}
	finished := make(chan int, perDeviceRequests)
	for i := 0; i < perDeviceRequests; i++ {
		go func() { finished <- call("/codex/hold") }()
	}
	for i := 0; i < perDeviceRequests; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d requests started", i, perDeviceRequests)
		}
	}
	if code := call("/codex/one-more"); code != http.StatusTooManyRequests {
		t.Fatalf("request beyond the bound: %d", code)
	}
	if !client.Connected() || len(host.Status().Devices) != 1 {
		t.Fatal("being told to wait unpaired the device")
	}
	close(release)
	for i := 0; i < perDeviceRequests; i++ {
		if code := <-finished; code != http.StatusOK {
			t.Fatalf("held request: %d", code)
		}
	}
	if code := call("/codex/after"); code != http.StatusOK {
		t.Fatalf("after the others finished: %d", code)
	}
	host.mu.Lock()
	left := len(host.running)
	host.mu.Unlock()
	if left != 0 {
		t.Fatalf("finished requests are still counted: %d", left)
	}
}

// The bodies of one device's running requests hold only so much of the
// host's memory together; what a request held is given back when it ends.
func TestADevicesRunningRequestsHoldBoundedMemory(t *testing.T) {
	was := perDeviceBytes
	perDeviceBytes = 1 << 20
	defer func() { perDeviceBytes = was }()
	read := make(chan struct{}, 4)
	release := make(chan struct{})
	host, client := pairedWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body refused: "+err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		if r.URL.Path == "/codex/hold" {
			read <- struct{}{}
			<-release
		}
		io.WriteString(w, strconv.Itoa(len(body)))
	}))
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	send := func(path string, size int) (int, string) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(strings.Repeat("x", size))))
		return w.Code, w.Body.String()
	}
	held := make(chan int, 1)
	go func() { code, _ := send("/codex/hold", 700<<10); held <- code }()
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		t.Fatal("the first request was not read")
	}
	// A second large body does not fit beside the first: the device is
	// asked to try again, before any of the body is read.
	if code, body := send("/codex/more", 700<<10); code != http.StatusTooManyRequests || !strings.Contains(body, "too much") {
		t.Fatalf("a body beyond the device's bound: %d %s", code, body)
	}
	// A small one still does.
	if code, body := send("/codex/small", 1000); code != http.StatusOK || body != "1000" {
		t.Fatalf("a small request beside a large one: %d %s", code, body)
	}
	close(release)
	if code := <-held; code != http.StatusOK {
		t.Fatalf("the held request: %d", code)
	}
	// What ended gave its share back.
	if code, body := send("/codex/again", 700<<10); code != http.StatusOK || body != strconv.Itoa(700<<10) {
		t.Fatalf("after the first ended: %d %s", code, body)
	}
	host.heldMu.Lock()
	left := len(host.held)
	host.heldMu.Unlock()
	if left != 0 {
		t.Fatalf("finished requests still hold memory: %v", left)
	}
	// A read after a request has ended is refused and counts for nothing.
	_, charge, done, ok := host.track(context.Background(), host.Status().Devices[0].ID)
	if !ok || !charge(1000) {
		t.Fatal("a running request could not send")
	}
	done()
	if charge(1000) {
		t.Fatal("a read after the request ended was counted")
	}
	host.heldMu.Lock()
	left = len(host.held)
	host.heldMu.Unlock()
	if left != 0 {
		t.Fatalf("bytes read after the end are held for good: %v", left)
	}
}

// Failed pairings are counted per address, per IPv6 /64 (link-local ones
// whatever interface they name), and not from loopback, where the Tailscale
// add-on's connections arrive.
func TestFailedPairingsAreCountedPerNetwork(t *testing.T) {
	for addr, want := range map[string]string{
		"192.168.1.20:5000":           "192.168.1.20",
		"[::ffff:192.168.1.20]:5000":  "192.168.1.20",
		"[2001:db8:1:2::a]:5000":      "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::b]:5000": "2001:db8:1:2::/64",
		"[fe80::a%eth0]:5000":         "fe80::/64",
		"[fe80::b%en1]:5000":          "fe80::/64",
		"127.0.0.1:5000":              "",
		"[::1]:5000":                  "",
	} {
		if got := failureSource(addr); got != want {
			t.Errorf("%s counts as %q, want %q", addr, got, want)
		}
	}
}

// With the record of failures full of sources still counting, new sources
// share one count: none is dropped to make room.
func TestAFullFailureRecordDropsNoCount(t *testing.T) {
	h, err := NewHost(t.TempDir(), 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	h.Quiet = true
	t.Cleanup(h.Close)
	if err := h.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.NewPairingCode(); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.failures = map[string]failure{}
	for i := 0; i < maxFailureSources; i++ {
		h.failures[fmt.Sprintf("10.%d.%d.1", i/256, i%256)] = failure{count: sourceFailures, since: time.Now()}
	}
	h.mu.Unlock()
	try := func(remote string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/remote/pair", strings.NewReader(`{"nonce":"`+strings.Repeat("a", 32)+`","proof":"wrong","name":"x"}`))
		r.RemoteAddr = remote
		h.pair(w, r)
		return w.Code
	}
	for i := 0; i < sourceFailures; i++ {
		try(fmt.Sprintf("192.168.%d.%d:5000", i/250, i%250+1))
	}
	h.mu.Lock()
	known, others := h.failures["10.0.0.1"], h.failures["others"]
	h.mu.Unlock()
	if known.count != sourceFailures || others.count != sourceFailures {
		t.Fatalf("a known source's count %d, the new sources' %d", known.count, others.count)
	}
	// A device with the right code still pairs from a new address.
	nonce := strings.Repeat("b", 32)
	h.mu.Lock()
	proof := pairingProof(h.codeKey, "client", nonce)
	h.mu.Unlock()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/remote/pair", strings.NewReader(`{"nonce":"`+nonce+`","proof":"`+proof+`","name":"Laptop"}`))
	r.RemoteAddr = "192.168.200.1:5000"
	h.pair(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("the right code from a new address: %d %s", w.Code, w.Body.String())
	}
}

// A ledger too large to send is cut until it fits, and gives up once
// nothing is left: it never loops.
func TestUsageBodyAlwaysEnds(t *testing.T) {
	one := &usage.Ledger{Entries: []usage.LedgerEntry{{Quarter: 1, Provider: "claude", Model: strings.Repeat("m", usageSendLimit+1)}}}
	done := make(chan error, 1)
	var body []byte
	go func() {
		var err error
		body, err = usageBody(strings.Repeat("a", 32), one)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil || len(body) > usageSendLimit || strings.Contains(string(body), "mmmm") {
			t.Fatalf("a ledger too large: %v, %d bytes", err, len(body))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cutting a ledger to size never ended")
	}
}

// The usage id is this install's, differs from host to host, and is not
// the install id the host may learn.
func TestUsageIDIsPerHostAndSecret(t *testing.T) {
	c := NewClient(t.TempDir(), "")
	a, err := c.usageIDFor("host-a")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := c.usageIDFor("host-a")
	b, _ := c.usageIDFor("host-b")
	install, _ := c.InstallID()
	if a != again || a == b || a == install || !ValidInstallID(a) {
		t.Fatalf("ids: %s %s %s (install %s)", a, again, b, install)
	}
	if other, _ := NewClient(t.TempDir(), "").usageIDFor("host-a"); other == a {
		t.Fatal("two installs share an id")
	}
	info, err := os.Stat(filepath.Join(c.Dir(), "usage-secret.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the secret's file: %v %v", info, err)
	}
}

type answerTransport func(*http.Request) *http.Response

func (f answerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

// A host that kept this computer's usage a moment ago says so apart from one
// too busy to take it: only the first is current enough.
func TestTooSoonIsToldApartFromBusy(t *testing.T) {
	c := NewClient(t.TempDir(), "")
	cfg := &clientFile{Token: "t"}
	answer := func(recent bool) error {
		req, _ := http.NewRequest(http.MethodPut, "https://switcher-host/api/tokens/device", strings.NewReader("{}"))
		return c.usageCall(req, nil, cfg, answerTransport(func(*http.Request) *http.Response {
			h := http.Header{}
			if recent {
				h.Set(UsageRecentHeader, "recent")
			}
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{}`))}
		}))
	}
	if err := answer(true); !errors.Is(err, errUsageTooSoon) {
		t.Fatalf("sent a moment ago: %v", err)
	}
	if err := answer(false); err == nil || errors.Is(err, errUsageTooSoon) {
		t.Fatalf("a busy host: %v", err)
	}
}

// A new certificate is one no paired device knows: their pairings go.
func TestANewCertificateEndsThePairings(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHost(dir, 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	h.Quiet = true
	if err := h.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	code, err := h.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(t.TempDir(), "").Connect(context.Background(), "127.0.0.1", h.Status().Port, code.Code, "Laptop"); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if again, err := NewHost(dir, 0, http.NotFoundHandler()); err != nil || len(again.Status().Devices) != 1 {
		t.Fatalf("a restart with the same certificate lost the pairing: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "host-cert.pem")); err != nil {
		t.Fatal(err)
	}
	again, err := NewHost(dir, 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Status().Devices); n != 0 {
		t.Fatalf("%d pairings kept past a new certificate", n)
	}
}

// A certificate that cannot be read right now is left as it is, pairings and
// all; one that was read and is broken, or another put in its place, ends
// the pairings no device can reach any more.
func TestTheCertificateIsReplacedOnlyWhenItIsGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	paired := func(t *testing.T) string {
		dir := t.TempDir()
		h, err := NewHost(dir, 0, http.NotFoundHandler())
		if err != nil {
			t.Fatal(err)
		}
		h.Quiet = true
		if err := h.SetEnabled(true); err != nil {
			t.Fatal(err)
		}
		code, err := h.NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewClient(t.TempDir(), "").Connect(context.Background(), "127.0.0.1", h.Status().Port, code.Code, "Laptop"); err != nil {
			t.Fatal(err)
		}
		h.Close()
		return dir
	}
	devices := func(dir string) (int, error) {
		h, err := NewHost(dir, 0, http.NotFoundHandler())
		if err != nil {
			return 0, err
		}
		return len(h.Status().Devices), nil
	}

	unreadable := paired(t)
	cert := filepath.Join(unreadable, "host-cert.pem")
	before, _ := os.ReadFile(cert)
	if err := os.Chmod(cert, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := devices(unreadable); err == nil {
		t.Fatal("a certificate that could not be read was replaced")
	}
	_ = os.Chmod(cert, 0o600)
	if after, _ := os.ReadFile(cert); string(after) != string(before) {
		t.Fatal("the certificate was overwritten")
	}
	if n, err := devices(unreadable); err != nil || n != 1 {
		t.Fatalf("readable again: %d pairings, %v", n, err)
	}

	broken := paired(t)
	if err := os.WriteFile(filepath.Join(broken, "host-cert.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := devices(broken); err != nil || n != 0 {
		t.Fatalf("a broken certificate: %d pairings kept, %v", n, err)
	}

	swapped, other := paired(t), t.TempDir()
	if _, _, save, err := hostCertificate(other); err != nil || save() != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"host-cert.pem", "host-key.pem"} {
		raw, _ := os.ReadFile(filepath.Join(other, name))
		if err := os.WriteFile(filepath.Join(swapped, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := devices(swapped); err != nil || n != 0 {
		t.Fatalf("another certificate put in place: %d pairings kept, %v", n, err)
	}
}

// A host that shared before keeps pairing without a code off, and its offer,
// when a new certificate ends its pairings: it is decided from the devices
// as they were.
func TestANewCertificateKeepsTheTailnetChoice(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "host.json"), map[string]any{"enabled": false, "devices": []map[string]any{{"id": "d1", "name": "Mini", "token_sha256": "x"}}}); err != nil {
		t.Fatal(err)
	}
	h, err := NewHost(dir, 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if s := h.Status(); len(s.Devices) != 0 || s.TailnetPairing || !s.TailnetNotice {
		t.Fatalf("after a new certificate: %d devices, tailnet pairing %v, notice %v", len(s.Devices), s.TailnetPairing, s.TailnetNotice)
	}
	if again, err := NewHost(dir, 0, http.NotFoundHandler()); err != nil || again.Status().TailnetPairing || !again.Status().TailnetNotice {
		t.Fatalf("after a restart: %v %+v", err, again.Status())
	}
}
