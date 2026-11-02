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
	pending                 bool // a restore stopped halfway
	restoreErr              error
	onRestore, onConfigure  func()
}

func (d *fakeDesktop) SetupStatus() desktoprelay.SetupStatus {
	if d.pending {
		return desktoprelay.SetupStatus{Condition: "pending"}
	}
	if d.configured {
		return desktoprelay.SetupStatus{Condition: "configured", Configured: true}
	}
	return desktoprelay.SetupStatus{Condition: "not_configured"}
}

// Configure refuses a pending setup, as Desktop's own does.
func (d *fakeDesktop) Configure(context.Context) error {
	if d.onConfigure != nil {
		d.onConfigure()
	}
	if d.pending {
		return errors.New("Desktop settings changed; owned values were not overwritten")
	}
	d.configured = true
	return nil
}
func (d *fakeDesktop) Restore(context.Context) error {
	if d.onRestore != nil {
		d.onRestore()
	}
	if d.restoreErr != nil {
		return d.restoreErr
	}
	d.configured, d.pending = false, false
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
	onUse               func()
}

func (c *fakeClaude) LoggedIn() (bool, bool) { return c.loggedIn, c.available }
func (c *fakeClaude) UseSelected(context.Context) (string, error) {
	if c.onUse != nil {
		c.onUse()
	}
	c.used, c.loggedIn = true, true
	return "work@example.com", nil
}

type fakeAccounts struct {
	selected   *store.Account
	accounts   map[string]store.Account
	adopted    map[string]store.Token
	adoptErr   error
	onAdopt    func()
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
		return store.Account{}, store.ErrNotFound
	}
	return acc, nil
}
func (a *fakeAccounts) AdoptToken(id string, token store.Token) error {
	if a.onAdopt != nil {
		a.onAdopt()
	}
	if a.adoptErr != nil {
		return a.adoptErr
	}
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

func idToken(email string) string {
	claims, _ := json.Marshal(map[string]string{"email": email})
	return "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
}

// watchedCodex is Codex's real setup, with a look at handback.json just
// before each change, as a Switcher stopping right then would leave it.
type watchedCodex struct {
	Codex
	dir        string
	before     map[string]record
	installErr error
}

func (c *watchedCodex) Install() error {
	if c.installErr != nil {
		return c.installErr
	}
	return c.Codex.Install()
}

func (c *watchedCodex) look(name string) {
	var rec record
	raw, _ := os.ReadFile(filepath.Join(c.dir, "handback.json"))
	_ = json.Unmarshal(raw, &rec)
	c.before[name] = rec
}
func (c *watchedCodex) Uninstall() error { c.look("uninstall"); return c.Codex.Uninstall() }
func (c *watchedCodex) WriteLogin(t codexcfg.AuthTokens) (string, error) {
	c.look("write-login")
	return c.Codex.WriteLogin(t)
}

// handedBackWithCodexLogin is a hand-back that gave Codex, which had no login,
// the selected account.
func handedBackWithCodexLogin(t *testing.T) (*fixture, *Manager, store.Account) {
	t.Helper()
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Email: "work@example.com",
		Token: store.Token{IDToken: idToken("work@example.com"), AccessToken: "switcher-a", RefreshToken: "switcher-r", AccountID: "acct"}}
	f.accounts.selected, f.accounts.accounts = &account, map[string]store.Account{account.ID: account}
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f, m, account
}

func TestEachChangeIsRecordedBeforeItIsMade(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	f.accounts.selected = &store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	watched := &watchedCodex{Codex: f.codex, dir: f.dir, before: map[string]record{}}
	f.codex = watched
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := watched.before["uninstall"]; got.Phase != "handing_back" || got.CodexConfig != "tracked" {
		t.Fatalf("before Codex's entry went: %+v", got)
	}
	if got := watched.before["write-login"]; got.CodexLogin != "codex-1" {
		t.Fatalf("before Codex got a login: %+v", got)
	}
}

// Setting up again redoes a recorded change even when the stop came before
// it was made.
func TestSetUpRedoesAChangeThatWasNeverMade(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z","codex_config":"tracked","desktop":true}`), 0o600)
	m := New(f.dir, f.deps())
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := codexcfg.Check(f.codexPath, 8787); got.Condition != "ready" {
		t.Fatalf("Codex: %+v", got)
	}
}

func TestSetUpFinishesAHalfDoneDesktopRestoreFirst(t *testing.T) {
	f := newFixture(t)
	f.desktop.configured, f.desktop.pending = false, true
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z","desktop":true}`), 0o600)
	m := New(f.dir, f.deps())
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.desktop.configured || f.desktop.pending {
		t.Fatalf("Desktop: %+v", f.desktop)
	}
}

// Finishing a stopped hand-back keeps a tool that could not be set up again
// on record, so the next set-up still redoes it.
func TestFinishingKeepsWhatCouldNotBeSetUpOnRecord(t *testing.T) {
	f := newFixture(t)
	f.sharing.on, f.sharing.err = false, errors.New("port in use")
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z","sharing":true}`), 0o600)
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, s := range status.Steps {
		reported = reported || s.Tool == "sharing" && s.Outcome == "failed"
	}
	if !reported {
		t.Fatalf("steps: %+v", status.Steps)
	}
	f.sharing.err = nil
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.sharing.on {
		t.Fatal("sharing was never turned back on")
	}
}

// A later step failing while finishing keeps what the earlier attempt left.
func TestAFailedFinishKeepsWhatWasCarried(t *testing.T) {
	f := newFixture(t)
	f.sharing.on, f.sharing.err = false, errors.New("port in use")
	f.desktop.restoreErr = errors.New("Desktop settings changed")
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z","sharing":true}`), 0o600)
	m := New(f.dir, f.deps())
	if status, err := m.HandBack(context.Background()); err == nil || !status.HandedBack || !f.accounts.handedBack {
		t.Fatalf("failed finish: %+v %v", status, err)
	}
	f.sharing.err, f.desktop.restoreErr = nil, nil
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.sharing.on {
		t.Fatal("sharing was never turned back on")
	}
}

// When a step fails and an earlier one cannot be put back, Switcher stays
// handed back with it on record rather than claiming nothing changed.
func TestAnUndoThatFailsStaysOnRecord(t *testing.T) {
	f := newFixture(t)
	watched := &watchedCodex{Codex: f.codex, dir: f.dir, before: map[string]record{}, installErr: errors.New("config.toml is read-only")}
	f.codex = watched
	f.host.err = errors.New("disk full")
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err == nil || !status.HandedBack || !status.Incomplete || !f.accounts.handedBack || !New(f.dir, Deps{}).HandedBack() {
		t.Fatalf("after a failed undo: %+v %v", status, err)
	}
	watched.installErr, f.host.err = nil, nil
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := codexcfg.Check(f.codexPath, 8787); got.Condition != "ready" {
		t.Fatalf("Codex: %+v", got)
	}
}

// Set-up keeps its record until every tool is set up again, so a stop
// meanwhile still knows what to redo.
func TestSetUpKeepsTheRecordUntilItIsDone(t *testing.T) {
	f := newFixture(t)
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recorded bool
	f.desktop.onConfigure = func() { recorded = New(f.dir, Deps{}).HandedBack() }
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("the record was gone while Desktop was set up again")
	}
	if m.HandedBack() || New(f.dir, Deps{}).HandedBack() {
		t.Fatal("still handed back after set-up")
	}
}

// Codex rewriting its login while Switcher takes it back leaves the newer
// login with Codex and Switcher handed back, until set-up takes that one.
func TestALoginCodexRewritesDuringTheTakeBackIsNotLost(t *testing.T) {
	f, m, _ := handedBackWithCodexLogin(t)
	write := func(r string) {
		os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(`{"tokens":{"id_token":"`+idToken("work@example.com")+`","access_token":"a","refresh_token":"`+r+`","account_id":"acct"}}`), 0o600)
	}
	write("codex-r2")
	f.accounts.onAdopt = func() { f.accounts.onAdopt = nil; write("codex-r3") }
	if status, err := m.SetUp(context.Background()); err == nil || !status.HandedBack {
		t.Fatalf("set-up while Codex rewrote its login: %+v %v", status, err)
	}
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.accounts.adopted["codex-1"]; got.RefreshToken != "codex-r3" {
		t.Fatalf("adopted: %+v", got)
	}
}

func TestALoginWithoutAnAccountIDIsTakenBackByItsToken(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "switcher-r"}}
	f.accounts.selected, f.accounts.accounts = &account, map[string]store.Account{account.ID: account}
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same login, rewritten; as after a stop before its digest was saved.
	os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(`{"tokens":{"access_token":"a","refresh_token":"switcher-r"}}`), 0o600)
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codexcfg.AuthPath(f.codexPath)); !os.IsNotExist(err) {
		t.Fatal("the login given to Codex stayed")
	}
}

func TestAHalfDoneDesktopRestoreIsFinished(t *testing.T) {
	f := newFixture(t)
	f.desktop.configured, f.desktop.pending = false, true
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.desktop.pending {
		t.Fatal("Desktop's restore was left halfway")
	}
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.desktop.configured {
		t.Fatal("Desktop was not connected again")
	}
}

// Finishing a hand-back that stopped halfway still gives a tool without a
// login the selected account, although Switcher stayed off its accounts.
func TestFinishingAStoppedHandBackGivesCodexItsLogin(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	f.accounts.selected, f.accounts.accounts = &account, map[string]store.Account{account.ID: account}
	os.WriteFile(filepath.Join(f.dir, "handback.json"), []byte(`{"phase":"handing_back","since":"2026-11-01T12:00:00Z"}`), 0o600)
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.HandedBack || status.Incomplete || !f.accounts.handedBack {
		t.Fatalf("finished: %+v", status)
	}
	if login, _ := codexcfg.ReadLogin(f.codexPath); !login.Present {
		t.Fatal("Codex was left without a login")
	}
}

// A login Codex now keeps beside an API key, or one for another seat of the
// same workspace, is Codex's own: Switcher neither adopts nor removes it.
func TestALoginThatIsNotAllSwitchersStaysWithCodex(t *testing.T) {
	for name, content := range map[string]string{
		"api key":    `{"OPENAI_API_KEY":"sk-x","tokens":{"id_token":"` + idToken("work@example.com") + `","access_token":"a2","refresh_token":"codex-r2","account_id":"acct"}}`,
		"other seat": `{"tokens":{"id_token":"` + idToken("colleague@example.com") + `","access_token":"a2","refresh_token":"codex-r2","account_id":"acct"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, m, _ := handedBackWithCodexLogin(t)
			os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(content), 0o600)
			if _, err := m.SetUp(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.accounts.adopted) != 0 {
				t.Fatalf("adopted Codex's login: %v", f.accounts.adopted)
			}
			if !strings.Contains(readText(t, codexcfg.AuthPath(f.codexPath)), "codex-r2") {
				t.Fatal("removed Codex's own login")
			}
		})
	}
}

// When the login cannot be taken back, Switcher stays handed back so the
// account keeps one owner and set-up can be tried again.
func TestAFailedTakeBackStaysHandedBack(t *testing.T) {
	f, m, _ := handedBackWithCodexLogin(t)
	os.WriteFile(codexcfg.AuthPath(f.codexPath), []byte(`{"tokens":{"id_token":"`+idToken("work@example.com")+`","access_token":"a2","refresh_token":"codex-r2","account_id":"acct"}}`), 0o600)
	f.accounts.adoptErr = errors.New("disk full")
	status, err := m.SetUp(context.Background())
	if err == nil || !status.HandedBack || !f.accounts.handedBack || !New(f.dir, Deps{}).HandedBack() {
		t.Fatalf("after a failed take-back: %+v %v", status, err)
	}
	if !strings.Contains(readText(t, codexcfg.AuthPath(f.codexPath)), "codex-r2") {
		t.Fatal("removed Codex's login without taking its tokens")
	}
	f.accounts.adoptErr = nil
	if _, err := m.SetUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.accounts.adopted["codex-1"]; got.RefreshToken != "codex-r2" {
		t.Fatalf("retry adopted: %+v", got)
	}
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
	if _, err := os.Stat(codexcfg.AuthPath(f.codexPath)); !os.IsNotExist(err) {
		t.Fatal("the login given to Codex stayed, so Codex and Switcher would both refresh it")
	}
}

func TestACodexThatNeverRanGetsNoLogin(t *testing.T) {
	f := newFixture(t)
	f.codexPath = filepath.Join(t.TempDir(), "never-ran", "config.toml")
	f.codex = CodexFiles(f.codexPath, 8787)
	f.accounts.selected = &store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	m := New(f.dir, f.deps())
	status, err := m.HandBack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(f.codexPath)); !os.IsNotExist(err) {
		t.Fatal("created a Codex login nobody uses; it would rotate alongside Switcher's copy")
	}
	skipped := false
	for _, s := range status.Steps {
		skipped = skipped || s.Tool == "codex-login" && s.Outcome == "skipped"
	}
	if !skipped {
		t.Fatalf("steps: %+v", status.Steps)
	}
}

// A Switcher that stops during a hand-back, here while Desktop's settings are
// restored, restarts off its accounts and knows what the first part undid.
func TestAHandBackStoppedHalfwayRestartsOffTheAccounts(t *testing.T) {
	f := newFixture(t)
	os.Remove(codexcfg.AuthPath(f.codexPath))
	account := store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	f.accounts.selected, f.accounts.accounts = &account, map[string]store.Account{account.ID: account}
	var halfway record
	var restartedFirst, restarted Status
	var stillUsingAccounts bool
	f.claude.loggedIn = false
	f.claude.onUse = func() { restartedFirst = New(f.dir, Deps{Accounts: &fakeAccounts{}}).Status() }
	f.desktop.onRestore = func() {
		stillUsingAccounts = !f.accounts.handedBack
		raw, _ := os.ReadFile(filepath.Join(f.dir, "handback.json"))
		_ = json.Unmarshal(raw, &halfway)
		restarted = New(f.dir, Deps{Accounts: &fakeAccounts{}}).Status()
	}
	m := New(f.dir, f.deps())
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stillUsingAccounts {
		t.Fatal("Switcher kept using its accounts after the tools got logins of their own")
	}
	if !restartedFirst.HandedBack || !restartedFirst.Incomplete {
		t.Fatalf("a restart while Claude Code gets its login: %+v", restartedFirst)
	}
	if !restarted.HandedBack || !restarted.Incomplete {
		t.Fatalf("a restart halfway: %+v", restarted)
	}
	if halfway.Phase != "handing_back" || halfway.CodexConfig != "tracked" || halfway.CodexLogin != "codex-1" {
		t.Fatalf("recorded halfway: %+v", halfway)
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
	if !strings.Contains(readText(t, codexcfg.AuthPath(f.codexPath)), "other-r") {
		t.Fatal("removed Codex's own login")
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
	if m.HandedBack() || New(f.dir, Deps{}).HandedBack() {
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
