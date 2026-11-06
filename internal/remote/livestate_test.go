package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A paired Mac may read the host's state and wait for it to change; the
// wait answers only an opaque tag and whether the state changed.
func TestPairedDevicesMayWaitForTheHostsState(t *testing.T) {
	for _, path := range []string{"/api/state", "/api/state/changes"} {
		if !allowedRemotePath(path) {
			t.Errorf("%s closed to paired devices", path)
		}
	}
	for _, path := range []string{"/api/state/", "/api/state/x", "/api/statex", "/api/state/changes/x"} {
		if allowedRemotePath(path) {
			t.Errorf("%s open to paired devices", path)
		}
	}
}

// quietPairing pairs with a host whose routes next serves, with the
// background address refresh done, so nothing writes into the test's
// directory after it ends.
func quietPairing(t *testing.T, next http.Handler) (*Host, *Client) {
	t.Helper()
	host, client := pairedWith(t, next)
	client.mu.Lock()
	client.refreshedAt = time.Now()
	client.mu.Unlock()
	return host, client
}

func TestClientWaitsOnItsHostsChanges(t *testing.T) {
	var since atomic.Value
	_, client := quietPairing(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state/changes" || r.Header.Get(DeviceHeader) != "" {
			http.NotFound(w, r)
			return
		}
		since.Store(r.URL.Query().Get("since"))
		switch r.URL.Query().Get("since") {
		case "slow":
			<-r.Context().Done()
		case "empty":
			io.WriteString(w, `{"etag":"","changed":true}`)
		case "long":
			io.WriteString(w, `{"etag":"`+strings.Repeat("e", 200)+`","changed":true}`)
		default:
			io.WriteString(w, `{"etag":"host-2","changed":true}`)
		}
	}))
	etag, changed, err := client.WaitHostChange(context.Background(), "host 1&x=y")
	if err != nil || etag != "host-2" || !changed || since.Load() != "host 1&x=y" {
		t.Fatalf("wait: %q %v %v, host saw since %q", etag, changed, err, since.Load())
	}
	for _, odd := range []string{"empty", "long"} {
		if _, _, err := client.WaitHostChange(context.Background(), odd); err == nil {
			t.Errorf("%s etag accepted", odd)
		}
	}
	// A wait that ends early says nothing about the host's reachability.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := client.WaitHostChange(ctx, "slow"); err == nil {
		t.Fatal("a cut-off wait reported success")
	}
	if status := client.Status(); !status.Reachable || status.Error != "" {
		t.Fatalf("a cut-off wait marked the host: %+v", status)
	}
	// Working without the host, this Mac does not wait on it.
	if _, err := client.SetLocalOnly(true); err != nil {
		t.Fatal(err)
	}
	since.Store("")
	if _, _, err := client.WaitHostChange(context.Background(), "x"); err == nil || since.Load() != "" {
		t.Fatalf("waited on the host while working without it: %v", err)
	}
}

// A host from before the changes route answers 404; this Mac then reads its
// state now and then, as before.
func TestAnOlderHostCannotWait(t *testing.T) {
	_, client := quietPairing(t, http.NotFoundHandler())
	if _, _, err := client.WaitHostChange(context.Background(), ""); !errors.Is(err, ErrHostCannotWait) {
		t.Fatalf("older host: %v", err)
	}
}

// This Mac's state merges its own settings into the host's, so a wait for it
// to change stays on this Mac, where its own watcher answers it.
func TestClientWatchesItsOwnStateChanges(t *testing.T) {
	_, client := quietPairing(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s reached the host", r.URL.Path)
	}))
	handler := client.Middleware(func(*http.Request) map[string]any { return nil },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local "+r.URL.Path) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/state/changes?since=x", nil))
	if w.Body.String() != "local /api/state/changes" {
		t.Fatalf("the wait went to %q", w.Body.String())
	}
}

func TestStateReusesTheHostsStateWhenTold(t *testing.T) {
	var reads atomic.Int32
	_, client := quietPairing(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": "claude-a", "provider": "claude"}},
			"active": map[string]any{"claude": "claude-a"}, "version": "1.21.0"})
	}))
	local := func(*http.Request) map[string]any { return map[string]any{"version": "client-version"} }
	r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	// Nothing read yet: the host is asked even when the caller would reuse.
	first := client.State(r, local, false)
	second := client.State(r, local, false)
	if reads.Load() != 1 {
		t.Fatalf("host read %d times, want once", reads.Load())
	}
	if mustMarshal(t, first) != mustMarshal(t, second) {
		t.Fatalf("a reused state differs:\n%s\n%s", mustMarshal(t, first), mustMarshal(t, second))
	}
	if remote, _ := second["remote"].(map[string]any); remote["connected"] != true || second["version"] != "client-version" {
		t.Fatalf("reused state: %v", second)
	}
	client.State(r, local, true)
	if reads.Load() != 2 {
		t.Fatalf("host read %d times, want twice", reads.Load())
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
