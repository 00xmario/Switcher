package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"switcher/internal/remote"
)

// pairedRemoteAPI pairs a fresh client with a fresh host over loopback.
func pairedRemoteAPI(t *testing.T) (*API, *http.ServeMux) {
	t.Helper()
	host, err := remote.NewHost(t.TempDir(), 0, http.NotFoundHandler())
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
	client := remote.NewClient(t.TempDir(), "")
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, "Laptop"); err != nil {
		t.Fatal(err)
	}
	own, err := remote.NewHost(t.TempDir(), 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	own.Quiet = true
	t.Cleanup(own.Close)
	api := &API{RemoteClient: client, RemoteHost: own}
	mux := http.NewServeMux()
	api.registerRemoteRoutes(mux)
	return api, mux
}

func TestUseHostSwitchesBetweenTheHostAndThisSwitcher(t *testing.T) {
	api, mux := pairedRemoteAPI(t)
	for _, remoteAddr := range []string{"192.168.1.20:5000", "100.101.102.103:443"} {
		if w := phoneRequest(mux, http.MethodPost, "/api/remote/use-host", remoteAddr, map[string]bool{"enabled": false}); w.Code != http.StatusForbidden {
			t.Fatalf("use-host from %s: %d", remoteAddr, w.Code)
		}
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/use-host", "127.0.0.1:5000", map[string]string{}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing choice: %d", w.Code)
	}
	w := phoneRequest(mux, http.MethodPost, "/api/remote/use-host", "127.0.0.1:5000", map[string]bool{"enabled": false})
	var answer struct {
		Client remote.ClientStatus `json:"client"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || !answer.Client.Connected || !answer.Client.LocalOnly {
		t.Fatalf("stop using the host: %d %s", w.Code, w.Body)
	}
	if api.RemoteClient.Connected() {
		t.Fatal("still forwarding to the host")
	}
	fields := api.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil))
	if got, _ := fields["remote"].(map[string]any); got["role"] != "local" || got["host"] == "" {
		t.Fatalf("state while working locally: %v", fields["remote"])
	}
	// Still paired, so sharing onward stays refused and so does a second pairing.
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/host", "127.0.0.1:5000", map[string]bool{"enabled": true}); w.Code != http.StatusConflict {
		t.Fatalf("share while paired but local: %d %s", w.Code, w.Body)
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/connect", "127.0.0.1:5000", map[string]string{"address": "127.0.0.1", "code": "AAAA-AAAA"}); w.Code != http.StatusConflict {
		t.Fatalf("second pairing: %d %s", w.Code, w.Body)
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/use-host", "127.0.0.1:5000", map[string]bool{"enabled": true}); w.Code != http.StatusOK || !api.RemoteClient.Connected() {
		t.Fatalf("use the host again: %d %s", w.Code, w.Body)
	}
	if _, ok := api.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil))["remote"]; ok {
		t.Fatal("local state still claims to work locally")
	}
}

func TestUseHostNeedsAPairing(t *testing.T) {
	api := &API{RemoteClient: remote.NewClient(t.TempDir(), "")}
	mux := http.NewServeMux()
	api.registerRemoteRoutes(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/use-host", "127.0.0.1:5000", map[string]bool{"enabled": false}); w.Code != http.StatusConflict {
		t.Fatalf("unpaired: %d %s", w.Code, w.Body)
	}
}
