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

// A Claude account banks kinds of reset; --kind picks one by the start of
// its name, and without it the one Claude suggests (next_id) is spent.
func TestResetKindChoosesTheClaudeReset(t *testing.T) {
	var mu sync.Mutex
	var spent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/state":
			io.WriteString(w, `{"version":"1","accounts":[{"id":"c1","provider":"claude","email":"me@example.com","active":true,
			 "usage":{"windows":[]},"reset_credits":{"count":4,"next_id":"1-0011aabb-weekly","kinds":[
			  {"kind":"Weekly reset","count":1,"next_id":"1-0011aabb-weekly"},
			  {"kind":"Fable reset","count":2,"next_id":"1-0011aabb-fable","unusable":"Usable once you reach a limit it refills"},
			  {"kind":"Full reset","count":1,"next_id":"1-0011aabb-full"}]}}]}`)
		case "POST /api/accounts/c1/use-reset":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			spent = append(spent, body["credit_id"])
			mu.Unlock()
			io.WriteString(w, `{"status":"ok","outcome":"reset","account":{"reset_credits":{"count":3}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	if code, _, errOut := run(t, srv.URL, "", "reset", "claude", "--kind", "fable"); code != exitUsage || !strings.Contains(errOut, "2 fable resets of me@example.com; add --yes") {
		t.Fatalf("without --yes: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, srv.URL, "", "reset", "claude", "--kind", "f", "--yes"); code != exitUsage || !strings.Contains(errOut, "fits both Fable reset and Full reset") {
		t.Fatalf("ambiguous kind: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, srv.URL, "", "reset", "claude", "--kind", "gold", "--yes"); code != exitFail || !strings.Contains(errOut, "it has: Weekly reset, Fable reset, Full reset") {
		t.Fatalf("unknown kind: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, srv.URL, "", "reset", "claude", "--kind", "Fab", "--yes"); code != 0 {
		t.Fatalf("fable: exit %d %s", code, errOut)
	}
	if code, _, errOut := run(t, srv.URL, "", "reset", "claude", "--yes"); code != 0 {
		t.Fatalf("suggested: exit %d %s", code, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(spent, ",") != "1-0011aabb-fable,1-0011aabb-weekly" {
		t.Fatalf("spent %v", spent)
	}
}
