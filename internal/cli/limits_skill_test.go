package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// limitServer is the state plus the account PATCH, recording each body.
func limitServer(t *testing.T) (string, func() []string) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/state":
			io.WriteString(w, strings.Replace(fakeState, `"native_active":true,`, `"native_active":true,"usage_limit":10,"at_limit":true,`, 1))
		case "PATCH /api/accounts/c2":
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			var body map[string]int
			json.Unmarshal(raw, &body)
			io.WriteString(w, `{"status":"ok","account":{"id":"c2","provider":"claude","email":"home@example.com","usage_limit":`+
				strings.TrimSpace(string(mustJSON(body["usage_limit"])))+`}}`)
		case "GET /api/system":
			io.WriteString(w, `{"machine":"k12","remote":true,"available":true,"cpu":23,"cores":16,"memory":72,"memory_used":24588543590,"memory_total":34144990003,"gpu":48}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), bodies...) }
}

func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

func TestLimitSetsShowsAndClears(t *testing.T) {
	url, bodies := limitServer(t)
	code, out, _ := run(t, url, "", "limit", "home", "15%")
	if code != 0 || !strings.Contains(out, "home@example.com keeps 15%: Switcher moves Claude to another account at 15% left.") {
		t.Fatalf("set: %d %q", code, out)
	}
	code, out, _ = run(t, url, "", "limit", "home", "off", "--json")
	var answer struct {
		Account Account `json:"account"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &answer) != nil || answer.Account.UsageLimit != 0 {
		t.Fatalf("off: %d %q", code, out)
	}
	if got := bodies(); len(got) != 2 || got[0] != `{"usage_limit":15}` || got[1] != `{"usage_limit":0}` {
		t.Fatalf("requests: %v", got)
	}
	code, out, _ = run(t, url, "", "limit", "claude:work@example.com")
	if code != 0 || !strings.Contains(out, "keeps 10% of its usage and has reached that") {
		t.Fatalf("show: %d %q", code, out)
	}
	for _, bad := range []string{"96", "0.5", "ten", "-3", "10x"} {
		if code, _, _ := run(t, url, "", "limit", "home", bad); code != 2 {
			t.Errorf("limit %q: exit %d, want 2", bad, code)
		}
	}
	code, out, _ = run(t, url, "", "accounts")
	if code != 0 || !strings.Contains(out, "at its 10% limit") {
		t.Fatalf("accounts: %q", out)
	}
}

func TestLoadNamesTheHost(t *testing.T) {
	url, _ := limitServer(t)
	code, out, _ := run(t, url, "", "load")
	if code != 0 || !strings.Contains(out, "k12 (the host whose accounts this computer uses): CPU 23% of 16 cores · Memory 72% (22.9 of 31.8 GB) · GPU 48%") {
		t.Fatalf("load: %d %q", code, out)
	}
}

// The skill installs into the home directory without a running Switcher.
func TestSkillInstallsWithoutAServer(t *testing.T) {
	home := t.TempDir()
	skill := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"skill"}, args...), Options{Port: 1, Stdout: &out, Stderr: &errOut,
			Getenv: func(k string) string {
				if k == "HOME" {
					return home
				}
				return ""
			}})
		return code, out.String() + errOut.String()
	}
	if code, out := skill(); code != 0 || !strings.Contains(out, "not installed") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out := skill("install"); code != 0 || !strings.Contains(out, "Installed the Switcher skill") {
		t.Fatalf("install: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "switcher", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if code, out := skill("--json"); code != 0 || !strings.Contains(out, `"current":true`) {
		t.Fatalf("status json: %d %q", code, out)
	}
	if code, _ := skill("remove"); code != 0 {
		t.Fatal("remove failed")
	}
	if code, _ := skill("frobnicate"); code != 2 {
		t.Fatal("an unknown action was accepted")
	}
}

// On a computer that uses a host, install and remove act on both unless
// --on picks one; the host's part goes through the running Switcher.
func TestSkillOnTheHost(t *testing.T) {
	home := t.TempDir()
	var posts []string
	hostInstalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent-skill" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			posts = append(posts, string(raw))
			hostInstalled = strings.Contains(string(raw), `"installed":true`)
		}
		json.NewEncoder(w).Encode(map[string]any{"host": map[string]any{"host": "k12",
			"skill": map[string]any{"installed": hostInstalled, "current": hostInstalled, "managed": hostInstalled, "path": "/home/h/SKILL.md"}}})
	}))
	t.Cleanup(srv.Close)
	skill := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"skill"}, args...), Options{Port: 1, Stdout: &out, Stderr: &errOut,
			Getenv: func(k string) string {
				switch k {
				case "HOME":
					return home
				case "SWITCHER_URL":
					return srv.URL
				}
				return ""
			}})
		return code, out.String() + errOut.String()
	}
	local := filepath.Join(home, ".agents", "skills", "switcher", "SKILL.md")
	if code, out := skill(); code != 0 || !strings.Contains(out, "on this computer") || !strings.Contains(out, "on k12") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out := skill("install"); code != 0 || !strings.Contains(out, "on k12") || len(posts) != 1 || !strings.Contains(posts[0], `"where":"host"`) {
		t.Fatalf("install on both: %d %q %v", code, out, posts)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("install on both skipped this computer")
	}
	if code, out := skill("remove", "--on", "here", "--json"); code != 0 || len(posts) != 1 || !strings.Contains(out, `"host"`) {
		t.Fatalf("remove here: %d %q %v", code, out, posts)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("remove --on here left this computer's skill")
	}
	if code, _ := skill("remove", "--on", "host"); code != 0 || len(posts) != 2 || !strings.Contains(posts[1], `"installed":false`) {
		t.Fatalf("remove on the host: %d %v", code, posts)
	}
	if code, _ := skill("install", "--on", "elsewhere"); code != 2 {
		t.Fatal("a bad --on was accepted")
	}
}

// Without a host, --on host has nothing to act on.
func TestSkillOnHostNeedsAHost(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"skill", "install", "--on", "host"}, Options{Port: 1, Stdout: &out, Stderr: &errOut,
		Getenv: func(k string) string {
			if k == "HOME" {
				return t.TempDir()
			}
			return ""
		}})
	if code == 0 || !strings.Contains(errOut.String()+out.String(), "no host") {
		t.Fatalf("%d %q", code, errOut.String()+out.String())
	}
}
