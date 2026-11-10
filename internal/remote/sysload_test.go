package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A paired Mac shows its host's load: numbers kept in range, named after the
// host as paired, never what the host itself writes.
func TestAClientReadsItsHostsLoad(t *testing.T) {
	answer := map[string]any{"machine": "<img src=x>", "platform": "linux", "available": true, "reason": "<b>",
		"cpu": 250, "cores": 16, "memory": -3, "memory_used": 30, "memory_total": 20, "gpu": 140}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/system" {
			if _, ok := PairedDevice(r); !ok {
				t.Error("the load was asked without the paired device")
			}
			json.NewEncoder(w).Encode(answer)
			return
		}
		http.NotFound(w, r)
	})
	_, client := pairedWith(t, next)
	got := client.SystemLoad(context.Background())
	host := client.Status().HostName
	if !got.Remote || !got.Available || got.Machine != host || got.Reason != "" || got.Platform != "linux" {
		t.Fatalf("view: %+v (host %q)", got, host)
	}
	if got.CPU != 100 || got.Memory != 0 || got.Cores != 16 || got.MemUsed != 0 || got.MemTotal != 0 || got.GPU == nil || *got.GPU != 100 {
		t.Fatalf("readings out of range: %+v gpu=%v", got, got.GPU)
	}

	answer = map[string]any{"available": false, "reason": "unsupported", "cpu": 40}
	if got = client.SystemLoad(context.Background()); got.Available || got.Reason != "unsupported" || got.CPU != 0 {
		t.Fatalf("a host without readings: %+v", got)
	}
}

// An older host has no load route: the card says so instead of failing.
func TestAnOlderHostHasNoLoad(t *testing.T) {
	_, client := pairedWith(t, http.NotFoundHandler())
	if got := client.SystemLoad(context.Background()); got.Available || got.Reason != "update" || !got.Remote {
		t.Fatalf("older host: %+v", got)
	}
}

func TestOnlyTheLoadRouteItselfIsOpen(t *testing.T) {
	if !allowedRemotePath("/api/system") {
		t.Fatal("/api/system closed to paired devices")
	}
	for _, path := range []string{"/api/system/", "/api/systems", "/api/system/x", "/api/sys"} {
		if allowedRemotePath(path) {
			t.Errorf("%s open to paired devices", path)
		}
	}
}

// A host's usage limits reach this Mac's screens only as whole shares from
// 1 to 95: anything else, markup included, is dropped.
func TestAHostsUsageLimitsAreOnlyNumbers(t *testing.T) {
	state := map[string]any{"accounts": []any{
		map[string]any{"id": "a", "provider": "claude", "usage_limit": `"><img src=x onerror=alert(1)>`, "at_limit": true},
		map[string]any{"id": "b", "provider": "claude", "usage_limit": 10.5, "at_limit": "yes"},
		map[string]any{"id": "c", "provider": "claude", "usage_limit": 96.0},
		map[string]any{"id": "d", "provider": "claude", "usage_limit": 20.0, "at_limit": "true"},
		map[string]any{"id": "e", "provider": "claude", "usage_limit": 30.0, "at_limit": true},
	}}
	sanitizeHostState(state)
	accounts := state["accounts"].([]any)
	for i, want := range []map[string]any{{}, {}, {}, {"usage_limit": 20.0}, {"usage_limit": 30.0, "at_limit": true}} {
		account := accounts[i].(map[string]any)
		for _, key := range []string{"usage_limit", "at_limit"} {
			if account[key] != want[key] {
				t.Errorf("account %v: %s = %v, want %v", account["id"], key, account[key], want[key])
			}
		}
	}
}
