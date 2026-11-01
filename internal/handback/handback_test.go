package handback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/codexcfg"
	"switcher/internal/desktoprelay"
	"switcher/internal/store"
)

type fakeDesktop struct {
	configured, passthrough bool
	restoreErr              error
}

func (d *fakeDesktop) SetupStatus() desktoprelay.SetupStatus {
	if d.configured {
		return desktoprelay.SetupStatus{Condition: "configured", Configured: true}
	}
	return desktoprelay.SetupStatus{Condition: "not_configured"}
}
func (d *fakeDesktop) Configure(context.Context) error { d.configured = true; return nil }
func (d *fakeDesktop) Restore(context.Context) error {
	if d.restoreErr != nil {
		return d.restoreErr
	}
	d.configured = false
	return nil
}
func (d *fakeDesktop) SetPassthrough(on bool) { d.passthrough = on }

type flag struct {
	on  bool
	err error
}

func (f *flag) On() bool { return f.on }
func (f *flag) Set(on bool) error {
	if f.err != nil {
		return f.err
	}
	f.on = on
	return nil
}

type fakeClaude struct {
	loggedIn, available bool
	used                bool
}

func (c *fakeClaude) LoggedIn() (bool, bool) { return c.loggedIn, c.available }
func (c *fakeClaude) UseSelected(context.Context) (string, error) {
	c.used, c.loggedIn = true, true
	return "work@example.com", nil
}

type fakeAccounts struct {
	selected   *store.Account
	accounts   map[string]store.Account
	adopted    map[string]store.Token
	handedBack bool
}

func (a *fakeAccounts) SelectedCodex(context.Context) (store.Account, bool, error) {
	if a.handedBack {
		return store.Account{}, false, errors.New("accounts used after hand-back")
	}
	if a.selected == nil {
		return store.Account{}, false, nil
	}
	return *a.selected, true, nil
}
func (a *fakeAccounts) Get(id string) (store.Account, error) {
	acc, ok := a.accounts[id]
	if !ok {
		return store.Account{}, os.ErrNotExist
	}
	return acc, nil
}
func (a *fakeAccounts) AdoptToken(id string, token store.Token) error {
	if a.adopted == nil {
		a.adopted = map[string]store.Token{}
	}
	a.adopted[id] = token
	return nil
}
func (a *fakeAccounts) SetHandedBack(on bool) { a.handedBack = on }

type fixture struct {
	dir, codexPath string
	codex          Codex
	desktop        *fakeDesktop
	sharing, host  *flag
	claude         *fakeClaude
	accounts       *fakeAccounts
}

func (f *fixture) deps() Deps {
	return Deps{Codex: f.codex, Desktop: f.desktop, Sharing: f.sharing, UseHost: f.host, ClaudeCode: f.claude, Accounts: f.accounts,
		Now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
}

// newFixture is a Codex set up to use Switcher (previously "openai"), with
// a login of its own, and every other tool set up too.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	codexDir := t.TempDir()
	path := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(path, []byte(`model_provider = "openai"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexcfg.InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexcfg.AuthPath(path), []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"i","access_token":"own-a","refresh_token":"own-r","account_id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: t.TempDir(), codexPath: path, codex: CodexFiles(path, 8787), desktop: &fakeDesktop{configured: true},
		sharing: &flag{on: true}, host: &flag{on: true}, claude: &fakeClaude{loggedIn: true, available: true}, accounts: &fakeAccounts{}}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestHandBackUndoesTheSetupAndSetUpRedoesIt(t *testing.T) {
	f := newFixture(t)
	m := New(f.dir, f.deps())
	if m.HandedBack() || f.accounts.handedBack {
		t.Fatal("a fresh Switcher starts handed back")
	}
	status, err := m.HandBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.HandedBack || status.Incomplete || !f.accounts.handedBack || !f.desktop.passthrough {
		t.Fatalf("after hand-back: %+v accounts=%v passthrough=%v", status, f.accounts.handedBack, f.desktop.passthrough)
	}
	config := readText(t, f.codexPath)
	if !strings.Contains(config, `model_provider = "openai"`) || strings.Contains(config, "switcher") {
		t.Fatalf("Codex config after hand-back:\n%s", config)
	}
	if f.desktop.configured || f.sharing.on || f.host.on || f.claude.used {
		t.Fatalf("desktop=%v sharing=%v host=%v claude used=%v", f.desktop.configured, f.sharing.on, f.host.on, f.claude.used)
	}
	if got := readText(t, codexcfg.AuthPath(f.codexPath)); !strings.Contains(got, "own-r") {
		t.Fatalf("Codex's own login changed: %s", got)
	}

	// The choice survives a restart, before any request.
	f.accounts.handedBack, f.desktop.passthrough = false, false
	m = New(f.dir, f.deps())
	if !m.HandedBack() || !f.accounts.handedBack || !f.desktop.passthrough {
		t.Fatal("hand-back did not survive a restart")
	}

	status, err = m.SetUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.HandedBack || f.accounts.handedBack || f.desktop.passthrough {
		t.Fatalf("after setup: %+v", status)
	}
	if got := codexcfg.Check(f.codexPath, 8787); got.Condition != "ready" {
		t.Fatalf("Codex after setup: %+v", got)
	}
	if !f.desktop.configured || !f.sharing.on || !f.host.on {
		t.Fatalf("not set up again: desktop=%v sharing=%v host=%v", f.desktop.configured, f.sharing.on, f.host.on)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "handback.json")); !os.IsNotExist(err) {
		t.Fatal("the hand-back record outlived the setup")
	}
}

func jwtWithExpiry(exp int64) string {
	claims, _ := json.Marshal(map[string]int64{"exp": exp})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

func TestCodexWithoutALoginGetsTheSelectedAccount(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Email: "work@example.com",
		Token: store.Token{IDToken: "id", AccessToken: "switcher-a", RefreshToken: "switcher-r", AccountID: "acct"}}
	f.accounts.selected = &account
	f.accounts.accounts = map[string]store.Account{account.ID: account}
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(codexcfg.AuthPath(f.codexPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json: %v %v", info, err)
	}
	login, err := codexcfg.ReadLogin(f.codexPath)
	tokens, _, ok := login.Tokens()
	if err != nil || !ok || tokens.RefreshToken != "switcher-r" || tokens.AccountID != "acct" {
		t.Fatalf("Codex login: %+v %v", tokens, err)
	}

	// Codex refreshes its login meanwhile; setting up takes the newer one.
	exp := time.Now().Add(time.Hour).Unix()
	os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(`{"auth_mode":"chatgpt","tokens":{"id_token":"id2","access_token":"`+jwtWithExpiry(exp)+`","refresh_token":"codex-r2","account_id":"acct"}}`), 0o600)
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := f.accounts.adopted["codex-1"]
	if !ok || got.RefreshToken != "codex-r2" || got.IDToken != "id2" || got.ExpiresAt != exp || got.AccountID != "acct" {
		t.Fatalf("adopted: %+v %v", got, ok)
	}
}

func TestAnotherCodexLoginIsNotTakenOver(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Email: "w@example.com",
		Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	f.accounts.selected, f.accounts.accounts = &account, map[string]store.Account{account.ID: account}
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The user logged Codex in to another account meanwhile.
	os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(`{"tokens":{"access_token":"x","refresh_token":"other-r","account_id":"other"}}`), 0o600)
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.accounts.adopted) != 0 {
		t.Fatalf("took over another login: %v", f.accounts.adopted)
	}
}

func TestAFailedStepPutsEverythingBack(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	f.accounts.selected = &account
	f.host.err = errors.New("disk full")
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nothing was handed back") || status.HandedBack {
		t.Fatalf("failed hand-back: %+v %v", status, err)
	}
	if got := codexcfg.Check(f.codexPath, 8787); got.Condition != "ready" {
		t.Fatalf("Codex not put back: %+v", got)
	}
	if _, err := os.Stat(codexcfg.AuthPath(f.codexPath)); !os.IsNotExist(err) {
		t.Fatal("the login written for Codex stayed")
	}
	if !f.desktop.configured || !f.sharing.on || f.accounts.handedBack || f.desktop.passthrough {
		t.Fatalf("not put back: desktop=%v sharing=%v accounts=%v", f.desktop.configured, f.sharing.on, f.accounts.handedBack)
	}
	last := status.Steps[len(status.Steps)-1]
	if last.Tool != "host" || last.Outcome != "failed" {
		t.Fatalf("steps: %+v", status.Steps)
	}
	if m.HandedBack() {
		t.Fatal("recorded as handed back")
	}
}

func TestDesktopSettingsChangedElsewhereStopTheHandBack(t *testing.T) {
	f := newFixture(t)
	f.desktop.restoreErr = errors.New("Desktop settings changed; owned values were not overwritten")
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err == nil || !strings.Contains(err.Error(), "claude-desktop") {
		t.Fatalf("hand-back: %v", err)
	}
	if got := codexcfg.Check(f.codexPath, 8787); got.Condition != "ready" {
		t.Fatalf("Codex not put back: %+v", got)
	}
}

func TestAKeyringLoginIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	raw := readText(t, f.codexPath)
	os.WriteFile(f.codexPath, []byte("cli_auth_credentials_store = \"keyring\"\n"+raw), 0o600)
	f.accounts.selected = &store.Account{ID: "codex-1", Token: store.Token{AccessToken: "a", RefreshToken: "r"}}
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codexcfg.AuthPath(f.codexPath)); !os.IsNotExist(err) {
		t.Fatal("wrote auth.json although Codex uses the keyring")
	}
	found := false
	for _, s := range status.Steps {
		found = found || s.Tool == "codex-login" && strings.Contains(s.Message, "keyring")
	}
	if !found {
		t.Fatalf("steps: %+v", status.Steps)
	}
}

func TestClaudeCodeWithoutALoginGetsTheSelectedAccount(t *testing.T) {
	f := newFixture(t)
	f.claude.loggedIn = false
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.claude.used {
		t.Fatal("Claude Code was left without a login")
	}
}

func TestACodexThatHadSwitchedAwayIsNotPointedBack(t *testing.T) {
	f := newFixture(t)
	raw := strings.Replace(readText(t, f.codexPath), `model_provider = "switcher"`, `model_provider = "openai"`, 1)
	os.WriteFile(f.codexPath, []byte(raw), 0o600)
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if config := readText(t, f.codexPath); strings.Contains(config, "switcher") {
		t.Fatalf("Codex pointed back at Switcher:\n%s", config)
	}
}

func TestAnInterruptedHandBackStaysOffTheAccounts(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z","sharing":true}`), 0o600)
	m := New(f.dir, f.deps())
	status := m.Status()
	if !status.HandedBack || !status.Incomplete || !f.accounts.handedBack {
		t.Fatalf("interrupted: %+v", status)
	}
	// The first attempt had turned sharing off; finishing keeps that on record.
	f.sharing.on = false
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.sharing.on {
		t.Fatal("sharing from the first attempt was not turned back on")
	}
}

func TestSetUpWhenNotHandedBackChangesNothing(t *testing.T) {
	f := newFixture(t)
	before := readText(t, f.codexPath)
	m := New(f.dir, f.deps())
	status, err := m.SetUp(context.Background())
	if err != nil || status.HandedBack || readText(t, f.codexPath) != before {
		t.Fatalf("setup without hand-back: %+v %v", status, err)
	}
}
