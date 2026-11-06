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

// A paired Mac updates its host: the host installs unattended, since nobody
// may be at it to answer a password prompt, and only this Mac's own
// dashboard can ask for it.
func TestAPairedMacUpdatesItsHostUnattended(t *testing.T) {
	updater := &fakeUpdateChecker{}
	hostAPI := &API{Updater: updater, Version: "1.19.0"}
	hostRoutes := http.NewServeMux()
	hostRoutes.HandleFunc("GET /api/update", hostAPI.handleUpdateState)
	hostRoutes.HandleFunc("POST /api/update", hostAPI.handleUpdate)
	hostRoutes.HandleFunc("POST /api/update/check", hostAPI.handleUpdateCheck)
	host, err := remote.NewHost(t.TempDir(), 0, hostRoutes)
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
	api := &API{RemoteClient: client}
	mux := http.NewServeMux()
	api.registerRemoteRoutes(mux)

	if w := phoneRequest(mux, http.MethodPost, "/api/remote/host-update", "192.168.1.20:5000", map[string]string{"action": "install"}); w.Code != http.StatusForbidden {
		t.Fatalf("host update from another device: %d", w.Code)
	}
	w := phoneRequest(mux, http.MethodGet, "/api/remote/host-update", "127.0.0.1:5000", nil)
	var answer struct {
		HostUpdate remote.HostUpdate `json:"host_update"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || answer.HostUpdate.Current != "1.19.0" || answer.HostUpdate.Latest != "v9.0.0" {
		t.Fatalf("host version: %d %s", w.Code, w.Body)
	}
	w = phoneRequest(mux, http.MethodPost, "/api/remote/host-update", "127.0.0.1:5000", map[string]string{"action": "install"})
	if updater.unattended.Load() != 1 || updater.installs.Load() != 0 {
		t.Fatalf("installs: unattended=%d attended=%d (%d %s)", updater.unattended.Load(), updater.installs.Load(), w.Code, w.Body)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("the host's refusal must reach the dashboard: %d %s", w.Code, w.Body)
	}
}

// Without a code, connecting works only over Tailscale; anything else is
// told to use a code, which the dashboard and the command line then ask for.
func TestConnectWithoutACodeAsksForOneOffTailscale(t *testing.T) {
	api := &API{RemoteClient: remote.NewClient(t.TempDir(), "")}
	mux := http.NewServeMux()
	api.registerRemoteRoutes(mux)
	w := phoneRequest(mux, http.MethodPost, "/api/remote/connect", "127.0.0.1:5000", map[string]any{"address": "studio.local"})
	var answer struct {
		Error     string `json:"error"`
		NeedsCode bool   `json:"needs_code"`
	}
	if w.Code != http.StatusBadRequest || json.Unmarshal(w.Body.Bytes(), &answer) != nil || !answer.NeedsCode || answer.Error == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/connect", "192.168.1.20:5000", map[string]any{"address": "x.ts.net"}); w.Code != http.StatusForbidden {
		t.Fatalf("from the network: %d", w.Code)
	}
}

// A paired Mac switches hosts only when asked to, and a sharing Mac uses
// no other host at all.
func TestSwitchingHostsNeedsTheChoiceAndNoSharing(t *testing.T) {
	api, mux := pairedRemoteAPI(t)
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/connect", "127.0.0.1:5000", map[string]any{"address": "studio.local"}); w.Code != http.StatusConflict {
		t.Fatalf("a second host without switching: %d %s", w.Code, w.Body)
	}
	// With switch, a host that needs a code leaves the current one in use.
	w := phoneRequest(mux, http.MethodPost, "/api/remote/connect", "127.0.0.1:5000", map[string]any{"address": "studio.local", "switch": true})
	if w.Code != http.StatusBadRequest || !api.RemoteClient.Connected() {
		t.Fatalf("a failed switch: %d %s, connected=%v", w.Code, w.Body, api.RemoteClient.Connected())
	}
	sharing, err := remote.NewHost(t.TempDir(), 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	sharing.Quiet = true
	t.Cleanup(sharing.Close)
	sharing.SetEnabled(true)
	alone := &API{RemoteClient: remote.NewClient(t.TempDir(), ""), RemoteHost: sharing}
	own := http.NewServeMux()
	alone.registerRemoteRoutes(own)
	if w := phoneRequest(own, http.MethodPost, "/api/remote/connect", "127.0.0.1:5000", map[string]any{"address": "x.ts.net"}); w.Code != http.StatusConflict {
		t.Fatalf("a sharing Mac: %d %s", w.Code, w.Body)
	}
}

func TestTheHostChoosesWhetherItsOwnDevicesNeedACode(t *testing.T) {
	api, mux := pairedRemoteAPI(t)
	for _, remoteAddr := range []string{"192.168.1.20:5000", "100.101.102.103:443"} {
		if w := phoneRequest(mux, http.MethodPost, "/api/remote/host/tailnet", remoteAddr, map[string]bool{"enabled": false}); w.Code != http.StatusForbidden {
			t.Fatalf("from %s: %d", remoteAddr, w.Code)
		}
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/remote/host/tailnet", "127.0.0.1:5000", map[string]string{}); w.Code != http.StatusBadRequest {
		t.Fatalf("no choice: %d", w.Code)
	}
	w := phoneRequest(mux, http.MethodPost, "/api/remote/host/tailnet", "127.0.0.1:5000", map[string]bool{"enabled": false})
	if w.Code != http.StatusOK || api.RemoteHost.Status().TailnetPairing {
		t.Fatalf("turning it off: %d %s", w.Code, w.Body)
	}
	var status struct {
		Host struct {
			TailnetPairing *bool `json:"tailnet_pairing"`
		} `json:"host"`
	}
	w = phoneRequest(mux, http.MethodGet, "/api/remote", "127.0.0.1:5000", nil)
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Host.TailnetPairing == nil || *status.Host.TailnetPairing {
		t.Fatalf("status: %s", w.Body)
	}
}
