package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// pairedFake is a Switcher paired with "Studio" that records use-host calls.
type pairedFake struct {
	mu        sync.Mutex
	localOnly bool
	posts     []map[string]bool
}

func (f *pairedFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /api/remote":
		json.NewEncoder(w).Encode(map[string]any{"client": map[string]any{"connected": true, "local_only": f.localOnly, "host_name": "Studio", "reachable": !f.localOnly}})
	case "POST /api/remote/use-host":
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body)
		f.localOnly = !body["enabled"]
		io.WriteString(w, `{"client":{"connected":true}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"no such route"}`)
	}
}

func TestUseHostTurnsTheHostOffAndOnAgain(t *testing.T) {
	f := &pairedFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	code, out, errOut := run(t, srv.URL, "", "use-host", "off")
	if code != 0 || !strings.Contains(out, "Not using Studio") {
		t.Fatalf("use-host off: %d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, srv.URL, "", "use-host", "--json")
	var answer map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &answer) != nil || answer["use_host"] != false || answer["paired"] != true {
		t.Fatalf("use-host --json: %d %q", code, out)
	}
	code, out, _ = run(t, srv.URL, "", "use-host", "on")
	if code != 0 || !strings.Contains(out, "Using the accounts of Studio") {
		t.Fatalf("use-host on: %d %q", code, out)
	}
	if len(f.posts) != 2 || f.posts[0]["enabled"] || !f.posts[1]["enabled"] {
		t.Fatalf("requests: %v", f.posts)
	}
	if code, _, _ := run(t, srv.URL, "", "use-host", "maybe"); code != exitUsage {
		t.Fatalf("bad argument: %d", code)
	}
}

// handbackFake answers the hand-back routes like the server does.
type handbackFake struct {
	mu         sync.Mutex
	handedBack bool
	posts      []bool
}

func (f *handbackFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /api/handback":
		json.NewEncoder(w).Encode(map[string]any{"handed_back": f.handedBack, "steps": []any{}})
	case "POST /api/handback":
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body["enabled"])
		f.handedBack = body["enabled"]
		steps := []map[string]string{{"tool": "codex", "outcome": "done", "message": "Codex uses its previous provider again."}}
		json.NewEncoder(w).Encode(map[string]any{"handback": map[string]any{"handed_back": f.handedBack, "steps": steps}})
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"no such route"}`)
	}
}

func TestHandBackNeedsYesAndReportsEachTool(t *testing.T) {
	f := &handbackFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	if code, _, errOut := run(t, srv.URL, "", "hand-back", "on"); code != exitUsage || !strings.Contains(errOut, "--yes") {
		t.Fatalf("without --yes: %d %q", code, errOut)
	}
	if len(f.posts) != 0 {
		t.Fatal("handed back without --yes")
	}
	code, out, errOut := run(t, srv.URL, "", "hand-back", "on", "--yes")
	if code != 0 || !strings.Contains(out, "✓ codex: Codex uses its previous provider again.") || !strings.Contains(out, "hand-back off") {
		t.Fatalf("hand-back on: %d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, srv.URL, "", "hand-back", "--json")
	var status map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &status) != nil || status["handed_back"] != true {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := run(t, srv.URL, "", "hand-back", "off"); code != 0 || !strings.Contains(out, "set up again") {
		t.Fatalf("hand-back off: %d %q", code, out)
	}
	if len(f.posts) != 2 || !f.posts[0] || f.posts[1] {
		t.Fatalf("requests: %v", f.posts)
	}
}

func TestUseHostNeedsAPairing(t *testing.T) {
	_, url := fake(t) // its client is not connected
	if code, _, errOut := run(t, url, "", "use-host", "off"); code != exitFail || !strings.Contains(errOut, "not paired") {
		t.Fatalf("unpaired: %d %q", code, errOut)
	}
}
