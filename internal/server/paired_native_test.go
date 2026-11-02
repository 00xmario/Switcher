package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"switcher/internal/remote"
)

// pairedClientOf pairs a client with a host that serves mux, and returns the
// client's handler, which forwards account actions to the host.
func pairedClientOf(t *testing.T, mux http.Handler) http.Handler {
	t.Helper()
	host, err := remote.NewHost(t.TempDir(), 0, mux)
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
	return client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
}

// A paired Mac uses its host's accounts, so switching there switches the
// host completely: the account every Mac uses and the host's own Claude Code.
func TestAPairedMacsSwitchSwitchesTheHostsClaudeCodeToo(t *testing.T) {
	api, manager, paths, _, _, _ := nativeClaudeFixtureAPI(t)
	mux := http.NewServeMux()
	api.Register(mux)
	client := pairedClientOf(t, mux)

	w := httptest.NewRecorder()
	client.ServeHTTP(w, httptest.NewRequest("POST", "/api/accounts/claude-b/activate", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"changed":true`) || !strings.Contains(w.Body.String(), "Claude Code on ") {
		t.Fatalf("switch from the paired Mac: %d %s", w.Code, w.Body)
	}
	if manager.ActiveID("claude") != "claude-b" {
		t.Fatalf("the host's selection is %s", manager.ActiveID("claude"))
	}
	raw, err := os.ReadFile(paths.CredentialsFile)
	if err != nil || !strings.Contains(string(raw), "fixture-token-b") {
		t.Fatalf("the host's Claude Code still has its old login: %s %v", raw, err)
	}
}

// When the host's Claude Code cannot switch, the account every Mac uses
// still does, and the paired Mac is told why Claude Code did not follow.
func TestAPairedMacsSwitchStillSwitchesWhenClaudeCodeCannot(t *testing.T) {
	api, manager, paths, _, _, _ := nativeClaudeFixtureAPI(t)
	mux := http.NewServeMux()
	api.Register(mux)
	client := pairedClientOf(t, mux)
	if err := os.WriteFile(paths.CredentialsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	client.ServeHTTP(w, httptest.NewRequest("POST", "/api/accounts/claude-b/activate", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "still uses its previous login") {
		t.Fatalf("switch from the paired Mac: %d %s", w.Code, w.Body)
	}
	if manager.ActiveID("claude") != "claude-b" {
		t.Fatalf("the host's selection is %s", manager.ActiveID("claude"))
	}
}

// A Claude Code login on the host to an account Switcher does not have is
// the host's own: a paired Mac's switch leaves it, as automatic switching
// does, and still switches the account every Mac uses.
func TestAPairedMacsSwitchLeavesALoginSwitcherDoesNotManage(t *testing.T) {
	api, manager, paths, _, _, _ := nativeClaudeFixtureAPI(t)
	mux := http.NewServeMux()
	api.Register(mux)
	client := pairedClientOf(t, mux)
	own := `{"claudeAiOauth":{"accessToken":"fixture-token-x","refreshToken":"fixture-refresh-x","expiresAt":4102444800000}}`
	if err := os.WriteFile(paths.CredentialsFile, []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	client.ServeHTTP(w, httptest.NewRequest("POST", "/api/accounts/claude-b/activate", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "keeps its own login") || strings.Contains(w.Body.String(), "backup") {
		t.Fatalf("switch from the paired Mac: %d %s", w.Code, w.Body)
	}
	if manager.ActiveID("claude") != "claude-b" {
		t.Fatalf("the host's selection is %s", manager.ActiveID("claude"))
	}
	if raw, _ := os.ReadFile(paths.CredentialsFile); string(raw) != own {
		t.Fatalf("the host's own Claude Code login was replaced: %s", raw)
	}
}
