package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A paired Mac sees its host's Switcher version and has the host update
// itself, also while it works without the host.
func TestAClientUpdatesItsHost(t *testing.T) {
	var installs atomic.Int32
	var installedBy atomic.Value
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/update":
			json.NewEncoder(w).Encode(map[string]any{"current": "1.19.0", "latest": "v1.21.0", "update_available": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/update/check":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/update":
			installs.Add(1)
			device, _ := PairedDevice(r)
			installedBy.Store(device)
			json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.URL.Path == "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"version": "1.19.0", "update": map[string]any{"latest": "<b>v9</b>", "update_available": true},
				"accounts": []any{}, "active": map[string]any{}})
		default:
			http.NotFound(w, r)
		}
	})
	_, client := pairedWith(t, next)
	ctx := context.Background()

	got, err := client.HostUpdate(ctx, "")
	if err != nil || got.Current != "1.19.0" || got.Latest != "v1.21.0" || !got.Available || got.Host == "" {
		t.Fatalf("host update state: %+v %v", got, err)
	}
	if _, err := client.SetLocalOnly(true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.HostUpdate(ctx, "check"); err != nil {
		t.Fatalf("check while working without the host: %v", err)
	}
	got, err = client.HostUpdate(ctx, "install")
	if err != nil || got.Install == nil || got.Install.Step != "restarting" || installs.Load() != 1 || installedBy.Load() != "Laptop" {
		t.Fatalf("install: %+v %v installs=%d by=%v", got, err, installs.Load(), installedBy.Load())
	}
	if _, err := client.HostUpdate(ctx, "rm -rf"); err == nil {
		t.Fatal("an unknown action reached the host")
	}
}

// What the host says about its version reaches this Mac's menus only as a
// version: anything else is dropped.
func TestTheHostsVersionIsOnlyEverAVersion(t *testing.T) {
	u := HostUpdate{Current: "1.19.0<script>", Latest: "<b>v9</b>", Available: true, Install: &HostUpdateSteps{Step: "pwned", Fraction: 3}}
	u.sanitize()
	if u.Current != "" || u.Latest != "" || u.Available || u.Install != nil {
		t.Fatalf("sanitized: %+v", u)
	}
	u = HostUpdate{Install: &HostUpdateSteps{Step: "downloading", Fraction: 3}}
	u.sanitize()
	if u.Install == nil || u.Install.Fraction != 1 {
		t.Fatalf("progress: %+v", u.Install)
	}
}

func TestOnlyTheUpdateRoutesThemselvesAreOpen(t *testing.T) {
	for _, path := range []string{"/api/update", "/api/update/check"} {
		if !allowedRemotePath(path) {
			t.Errorf("%s closed to paired devices", path)
		}
	}
	for _, path := range []string{"/api/updates", "/api/update-channel", "/api/update/check/x", "/api/update/"} {
		if allowedRemotePath(path) {
			t.Errorf("%s open to paired devices", path)
		}
	}
}

// A host before 1.21.0 keeps its update routes from paired devices: it shows
// its version, without an update to offer from here.
func TestAnOlderHostIsNotOfferedARemoteUpdate(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"version": "1.19.0", "update": map[string]any{"latest": "v1.21.0", "update_available": true},
				"accounts": []any{}, "active": map[string]any{}})
		default: // what an older host answers a paired device
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not available to paired devices"})
		}
	})
	_, client := pairedWith(t, next)
	state := hostState(t, client)
	if state.Current != "1.19.0" || state.Remote {
		t.Fatalf("state's host update: %+v", state)
	}
	got, err := client.HostUpdate(context.Background(), "")
	if err != nil || got.Remote || got.Current != "1.19.0" || got.Latest != "v1.21.0" || !got.Available {
		t.Fatalf("older host: %+v %v", got, err)
	}
}

// What the host's /api/state says about its update reaches this Mac's state
// only as versions and known steps; an install this Mac waits on shows.
func TestTheStateCarriesTheHostsUpdateCleaned(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"version": "1.21.0", "update": map[string]any{"latest": "<b>v9</b>", "update_available": true},
				"accounts": []any{}, "active": map[string]any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/update":
			<-block
		}
	})
	_, client := pairedWith(t, next)
	state := hostState(t, client)
	if state.Current != "1.21.0" || !state.Remote || state.Latest != "" || state.Available {
		t.Fatalf("host update: %+v", state)
	}
	go client.HostUpdate(context.Background(), "install")
	deadline := time.Now().Add(5 * time.Second)
	for !client.hostInstalling.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if state := hostState(t, client); state.Install == nil || state.Install.Step != "updating" {
		t.Fatalf("while updating: %+v", state)
	}
	if _, err := client.HostUpdate(context.Background(), "install"); err == nil {
		t.Fatal("a second install started while the first runs")
	}
}

// hostState is the host_update this Mac's merged /api/state carries.
func hostState(t *testing.T, client *Client) HostUpdate {
	t.Helper()
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	var merged struct {
		Remote struct {
			HostUpdate HostUpdate `json:"host_update"`
		} `json:"remote"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &merged); err != nil {
		t.Fatalf("state: %v %s", err, w.Body)
	}
	return merged.Remote.HostUpdate
}
