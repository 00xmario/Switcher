package remote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A paired Switcher can stop relying on its host without unpairing: nothing
// is forwarded, the choice survives a restart, and turning it back on uses
// the host again.
func TestLocalOnlyKeepsThePairingAndRoutesLocally(t *testing.T) {
	_, client := pairedPair(t)
	var changes []bool
	client.OnChange(func(connected bool) { changes = append(changes, connected) })
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local "+r.URL.Path) })
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, local)
	serve := func(method, path string) string {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		return w.Body.String()
	}

	status, err := client.SetLocalOnly(true)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Connected || !status.LocalOnly || client.Connected() || !client.Paired() {
		t.Fatalf("local only status: %+v connected=%v paired=%v", status, client.Connected(), client.Paired())
	}
	for _, path := range []string{"/codex/v1/responses", "/api/state", "/api/accounts/x/activate", "/api/login"} {
		if got := serve("POST", path); got != "local "+path {
			t.Fatalf("%s while local only: %q", path, got)
		}
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	if resp, handled, err := client.Inference(context.Background(), "host-account", r, []byte(`{}`)); handled || resp != nil || err != nil {
		t.Fatalf("inference while local only: handled=%v err=%v", handled, err)
	}

	// A restart keeps working locally, still paired.
	reloaded := NewClient(client.dir, "client-key")
	if reloaded.Connected() || !reloaded.Paired() || !reloaded.Status().LocalOnly {
		t.Fatalf("after restart: %+v", reloaded.Status())
	}
	var restarted []bool
	reloaded.OnChange(func(connected bool) { restarted = append(restarted, connected) })
	if len(restarted) != 1 || restarted[0] {
		t.Fatalf("restart reported %v", restarted)
	}

	if _, err := client.SetLocalOnly(false); err != nil {
		t.Fatal(err)
	}
	if !client.Connected() {
		t.Fatal("using the host again did not reconnect")
	}
	if got := serve("POST", "/codex/v1/responses"); !strings.HasPrefix(got, "POST /codex/v1/responses") {
		t.Fatalf("provider request after resuming: %q", got)
	}
	if len(changes) != 3 || !changes[0] || changes[1] || !changes[2] {
		t.Fatalf("change notifications: %v", changes)
	}
	if NewClient(client.dir, "").Status().LocalOnly {
		t.Fatal("resuming was not saved")
	}
}

func TestLocalOnlyNeedsAPairing(t *testing.T) {
	client := NewClient(t.TempDir(), "")
	if _, err := client.SetLocalOnly(true); err == nil {
		t.Fatal("an unpaired Switcher switched to local only")
	}
}

// A new pairing starts out using its host, even after the old one was local.
func TestNewPairingUsesTheHost(t *testing.T) {
	host, client := pairedPair(t)
	if _, err := client.SetLocalOnly(true); err != nil {
		t.Fatal(err)
	}
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	code, err := host.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, "Laptop"); err != nil {
		t.Fatal(err)
	}
	if !client.Connected() || client.Status().LocalOnly {
		t.Fatalf("fresh pairing: %+v", client.Status())
	}
}
