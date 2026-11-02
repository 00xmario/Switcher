package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"switcher/internal/autostart"
)

// pairedFake is a Switcher paired with "Studio" that records use-host calls.
type pairedFake struct {
	mu        sync.Mutex
	localOnly bool
	posts     []map[string]bool
	actions   []string // host-update actions
	olderHost bool     // a host before 1.21.0
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
	case "POST /api/remote/host-update":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.actions = append(f.actions, body["action"])
		if f.olderHost {
			io.WriteString(w, `{"host_update":{"host":"Studio","current":"1.20.0","latest":"v1.21.0","update_available":true,"remote_update":false}}`)
			return
		}
		io.WriteString(w, `{"host_update":{"host":"Studio","current":"1.21.0","latest":"v1.22.0","update_available":true,"remote_update":true}}`)
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

// Starting at login is this computer's own setting: it works whether or not
// Switcher runs, so the Linux install can turn it on first.
func TestAutostartWorksWithoutARunningSwitcher(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	m := autostart.NewAt("linux", dir, "/home/u/.local/bin/switcher", func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	})
	var out, errOut bytes.Buffer
	code := Run([]string{"autostart", "on"}, Options{Port: 1, Stdout: &out, Stderr: &errOut, Autostart: m,
		Getenv: func(string) string { return "http://127.0.0.1:1" }})
	if code != 0 || !strings.Contains(out.String(), "switcher.service") {
		t.Fatalf("autostart on: %d %q %q", code, out.String(), errOut.String())
	}
	if !m.Status().Service || len(calls) != 2 {
		t.Fatalf("status %+v calls %v", m.Status(), calls)
	}
	out.Reset()
	if code := Run([]string{"autostart", "off", "--json"}, Options{Port: 1, Stdout: &out, Stderr: &errOut, Autostart: m}); code != 0 || !strings.Contains(out.String(), `"enabled":false`) {
		t.Fatalf("autostart off: %d %q", code, out.String())
	}
	mac := autostart.NewAt("darwin", dir, "/x", nil)
	out.Reset()
	if code := Run([]string{"autostart"}, Options{Port: 1, Stdout: &out, Stderr: &errOut, Autostart: mac}); code != 0 || !strings.Contains(out.String(), "menu") {
		t.Fatalf("autostart on a Mac: %d %q", code, out.String())
	}
}

func TestUseHostNeedsAPairing(t *testing.T) {
	_, url := fake(t) // its client is not connected
	if code, _, errOut := run(t, url, "", "use-host", "off"); code != exitFail || !strings.Contains(errOut, "not paired") {
		t.Fatalf("unpaired: %d %q", code, errOut)
	}
}

func TestUpdateHostAsksForYesBeforeInstalling(t *testing.T) {
	f := &pairedFake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	code, out, errOut := run(t, srv.URL, "", "update-host")
	if code != 0 || !strings.Contains(out, "Studio runs Switcher 1.21.0; 1.22.0 is out") || !strings.Contains(out, "--yes") {
		t.Fatalf("update-host: %d %q %q", code, out, errOut)
	}
	if strings.Join(f.actions, ",") != "check" {
		t.Fatalf("installed without --yes: %v", f.actions)
	}
	code, out, errOut = run(t, srv.URL, "", "update-host", "--yes")
	if code != 0 || !strings.Contains(out, "Studio is installing Switcher 1.22.0") || strings.Join(f.actions, ",") != "check,check,install" {
		t.Fatalf("update-host --yes: %d %q %q %v", code, out, errOut, f.actions)
	}
	f.olderHost, f.actions = true, nil
	code, out, _ = run(t, srv.URL, "", "update-host", "--yes")
	if code != 0 || !strings.Contains(out, "Update it on Studio once") || strings.Join(f.actions, ",") != "check" {
		t.Fatalf("older host: %d %q %v", code, out, f.actions)
	}
}
