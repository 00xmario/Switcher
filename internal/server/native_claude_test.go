package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
)

type nativeClaudeFixture struct {
	*recheckProvider
	bridge         *claudecode.Manager
	statusOverride *claudecode.Status
}

func (*nativeClaudeFixture) ID() string          { return "claude" }
func (*nativeClaudeFixture) NativeEnabled() bool { return true }
func (p *nativeClaudeFixture) NativeStatus() claudecode.Status {
	if p.statusOverride != nil {
		return *p.statusOverride
	}
	return p.bridge.Status()
}
func (p *nativeClaudeFixture) SwitchNative(ctx context.Context, id string, commit func(string) error) (claudecode.SwitchResult, error) {
	return p.bridge.SwitchWithReceipt(ctx, id, commit)
}
func (p *nativeClaudeFixture) SyncNative(ctx context.Context, a *store.Account) (bool, error) {
	return p.bridge.Synchronize(ctx, a)
}

// nativeClaudeFixtureAPI builds an API whose Claude provider switches a
// native login in a temporary home, with accounts claude-a (active) and
// claude-b, and a password-protected settings store.
func nativeClaudeFixtureAPI(t *testing.T) (*API, *proxy.Manager, claudecode.Paths, *settings.Store, string, *nativeClaudeFixture) {
	t.Helper()
	home, data := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model_provider = \"other\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(home, ".claude")
	if err := os.MkdirAll(configHome, 0700); err != nil {
		t.Fatal(err)
	}
	paths := claudecode.Paths{Home: home, ConfigHome: configHome, ConfigFile: filepath.Join(home, ".claude.json"), CredentialsFile: filepath.Join(configHome, ".credentials.json")}
	st := store.New(data)
	identities := map[string]claudecode.Identity{}
	for _, name := range []string{"a", "b"} {
		identity := claudecode.Identity{UUID: "uuid-" + name, Email: name + "@example.test"}
		identities["fixture-token-"+name] = identity
		if err := st.Save(store.Account{ID: "claude-" + name, Provider: "claude", Email: identity.Email, Token: store.Token{AccessToken: "fixture-token-" + name, RefreshToken: "fixture-refresh-" + name, AccountID: identity.UUID, ExpiresAt: time.Now().Add(time.Hour).Unix()}}); err != nil {
			t.Fatal(err)
		}
	}
	// A login of the user's own that Switcher does not have.
	identities["fixture-token-x"] = claudecode.Identity{UUID: "uuid-x", Email: "x@work.example.test"}
	config, _ := json.Marshal(map[string]any{"oauthAccount": identities["fixture-token-a"], "projects": map[string]any{"fixture": "preserve"}})
	credential, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "fixture-token-a", "refreshToken": "fixture-refresh-a", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "mcpOAuth": map[string]any{"fixture-secret": "preserve"}})
	if err := os.WriteFile(paths.ConfigFile, config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CredentialsFile, credential, 0600); err != nil {
		t.Fatal(err)
	}
	bridge := claudecode.New(paths, data, st, nil, func(ctx context.Context, token string) (claudecode.Identity, error) { return identities[token], nil }, func(context.Context, *store.Account) error { t.Fatal("fixture tokens must not refresh"); return nil })
	p := &nativeClaudeFixture{recheckProvider: &recheckProvider{}, bridge: bridge}
	manager, err := proxy.New(st, map[string]provider.Provider{"claude": p}, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate("claude-a"); err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: st, Proxy: manager, Settings: prefs, Providers: map[string]provider.Provider{"claude": p},
		CodexConfigPath: filepath.Join(t.TempDir(), "config.toml")}
	return api, manager, paths, prefs, session, p
}

// With Claude Code routed through the Desktop relay, a switch must not touch
// its login: a Remote Control environment belongs to the account that
// registered it. The screens then mark the selection, not the login.
func TestARoutedClaudeCodeKeepsItsLoginFiles(t *testing.T) {
	api, manager, paths, prefs, session, _ := nativeClaudeFixtureAPI(t)
	manager.SetClaudeCodeRouted(func() bool { return true })
	mux := http.NewServeMux()
	api.Register(mux)
	gate := (&AuthGate{Store: prefs}).Wrap(mux)
	// A state read lets the bridge learn the login first.
	api.handleState(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/state", nil))
	credentials, err := os.ReadFile(paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/accounts/claude-b/activate", nil)
	r.RemoteAddr = "127.0.0.1:2222"
	r.AddCookie(&http.Cookie{Name: "switcher_session", Value: session})
	r.Header.Set("X-Switcher-CSRF", prefs.CSRFToken())
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if manager.ActiveID("claude") != "claude-b" || manager.SelectedID("claude") != "claude-b" {
		t.Fatalf("active %q, selected %q", manager.ActiveID("claude"), manager.SelectedID("claude"))
	}
	if after, _ := os.ReadFile(paths.CredentialsFile); string(after) != string(credentials) {
		t.Fatal("the switch rewrote Claude Code's credentials")
	}
	if after, _ := os.ReadFile(paths.ConfigFile); string(after) != string(config) {
		t.Fatal("the switch rewrote Claude Code's config")
	}
	state := httptest.NewRecorder()
	api.handleState(state, httptest.NewRequest("GET", "/api/state", nil))
	var body struct {
		Accounts []accountView `json:"accounts"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, a := range body.Accounts {
		if a.NativeSwitchAvailable {
			t.Fatalf("%s still offers a native switch", a.ID)
		}
		if a.Active != (a.ID == "claude-b") {
			t.Fatalf("%s active=%v", a.ID, a.Active)
		}
	}
	cli := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/cli-setup", nil)
	req.RemoteAddr = "127.0.0.1:2222"
	api.handleCLISetup(cli, req)
	if !strings.Contains(cli.Body.String(), "Remote Control keeps working") {
		t.Fatalf("setup row does not say the login stays: %s", cli.Body.String())
	}
}

// A kept login does not widen who may switch: a switch from the network still
// needs what a native switch needs.
func TestARoutedClaudeCodeStillRefusesASwitchFromTheNetwork(t *testing.T) {
	api, manager, _, prefs, session, _ := nativeClaudeFixtureAPI(t)
	manager.SetClaudeCodeRouted(func() bool { return true })
	mux := http.NewServeMux()
	api.Register(mux)
	gate := (&AuthGate{Store: prefs}).Wrap(mux)
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/accounts/claude-b/activate", nil)
	r.RemoteAddr = "192.168.1.4:2222"
	r.AddCookie(&http.Cookie{Name: "switcher_session", Value: session})
	r.Header.Set("X-Switcher-CSRF", prefs.CSRFToken())
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, r)
	if w.Code != 403 || manager.ActiveID("claude") != "claude-a" {
		t.Fatalf("status=%d active=%q", w.Code, manager.ActiveID("claude"))
	}
}

func TestNativeClaudeActivationAuthLocalityAndActualCredentialWrite(t *testing.T) {
	api, manager, paths, prefs, session, p := nativeClaudeFixtureAPI(t)
	mux := http.NewServeMux()
	api.Register(mux)
	gate := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, tc := range []struct {
		name, remote, cookie, csrf string
		status                     int
	}{
		{"no session", "127.0.0.1:2222", "", "", 401},
		{"no CSRF", "127.0.0.1:2222", session, "", 403},
		{"LAN spoofed host", "192.168.1.4:2222", session, prefs.CSRFToken(), 403},
		{"local native switch", "127.0.0.1:2222", session, prefs.CSRFToken(), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/accounts/claude-b/activate", nil)
			r.RemoteAddr = tc.remote
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "switcher_session", Value: tc.cookie})
			}
			r.Header.Set("X-Switcher-CSRF", tc.csrf)
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status != 200 {
				if manager.ActiveID("claude") != "claude-a" {
					t.Fatal("rejected action changed selected route")
				}
			} else {
				if !strings.Contains(w.Body.String(), `"changed":true`) {
					t.Fatal("native result missing")
				}
			}
		})
	}
	if manager.ActiveID("claude") != "claude-b" {
		t.Fatal("native success did not commit proxy selection")
	}
	actual, err := os.ReadFile(paths.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(actual), "fixture-token-b") {
		t.Fatal("activation changed proxy but not native credential")
	}
	w := httptest.NewRecorder()
	api.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	if strings.Contains(w.Body.String(), "fixture-token") || strings.Contains(w.Body.String(), "fixture-refresh") || strings.Contains(w.Body.String(), "fixture-secret") {
		t.Fatal("native credentials leaked in account view")
	}
	var body struct {
		Accounts []accountView     `json:"accounts"`
		Native   claudecode.Status `json:"claude_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Native.ActiveID != "claude-b" {
		t.Fatal("native selected account missing from state")
	}
	for _, a := range body.Accounts {
		if a.ID == "claude-b" && (!a.NativeActive || !a.NativeSwitchAvailable) {
			t.Fatal("native badge capability not exposed")
		}
	}
	cli := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/cli-setup", nil)
	req.RemoteAddr = "127.0.0.1:2222"
	api.handleCLISetup(cli, req)
	if !strings.Contains(cli.Body.String(), "switch_native_login") {
		t.Fatal("setup row still reports native switching unavailable")
	}
	p.statusOverride = &claudecode.Status{Condition: "unavailable", Message: "fixture credential override blocks native switching"}
	cli = httptest.NewRecorder()
	api.handleCLISetup(cli, req)
	var setup struct {
		Clients []cliSetupClient `json:"clients"`
	}
	if err := json.Unmarshal(cli.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	for _, client := range setup.Clients {
		if client.ID == "codex" && client.Configuration.Condition != "missing" {
			t.Fatal("Codex setup did not inspect the isolated empty fixture")
		}
		if client.ID == "claude-code" && (client.Stage != "unavailable" || client.NextStep != p.statusOverride.Message) {
			t.Fatal("failed native initialization was advertised as available")
		}
	}
}
