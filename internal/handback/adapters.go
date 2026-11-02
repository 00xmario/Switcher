package handback

import (
	"context"
	"errors"
	"time"

	"switcher/internal/codexcfg"
	"switcher/internal/desktoprelay"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

// CodexFiles is Switcher's Codex setup in the user's config.toml, pointed
// at the Switcher listening on port.
func CodexFiles(configPath string, port int) Codex { return codexFiles{configPath, port} }

type codexFiles struct {
	path string
	port int
}

func (c codexFiles) Check() codexcfg.Status             { return codexcfg.Check(c.path, c.port) }
func (c codexFiles) Install() error                     { return codexcfg.InstallAt(c.path, c.port) }
func (c codexFiles) Uninstall() error                   { return codexcfg.Uninstall(c.path) }
func (c codexFiles) UninstallLegacy() error             { return codexcfg.UninstallLegacy(c.path) }
func (c codexFiles) ReadLogin() (codexcfg.Login, error) { return codexcfg.ReadLogin(c.path) }
func (c codexFiles) WriteLogin(t codexcfg.AuthTokens) (string, error) {
	return codexcfg.WriteLogin(c.path, t, time.Now())
}
func (c codexFiles) RemoveLoginIfUnchanged(written string) error {
	return codexcfg.RemoveLoginIfUnchanged(c.path, written)
}
func (c codexFiles) HasRun() bool { return codexcfg.HasRun(c.path) }

// DesktopRelay is Claude Desktop's relay setup in the given settings file.
// A nil manager has nothing to restore.
func DesktopRelay(m *desktoprelay.Manager, settingsPath string) Desktop {
	return desktopRelay{m, settingsPath}
}

type desktopRelay struct {
	m    *desktoprelay.Manager
	path string
}

func (d desktopRelay) SetupStatus() desktoprelay.SetupStatus {
	if d.m == nil || d.path == "" {
		return desktoprelay.SetupStatus{Condition: "unavailable"}
	}
	return d.m.SetupStatus(d.path)
}

func (d desktopRelay) Configure(ctx context.Context) error {
	if d.m == nil {
		return desktoprelay.ErrUnavailable
	}
	_, err := d.m.Configure(ctx, d.path)
	return err
}

func (d desktopRelay) Restore(ctx context.Context) error {
	if d.m == nil {
		return desktoprelay.ErrUnavailable
	}
	_, err := d.m.RestoreSetup(ctx, d.path)
	return err
}

func (d desktopRelay) SetPassthrough(on bool) {
	if d.m != nil {
		d.m.SetPassthrough(on)
	}
}

// Funcs is a Toggle from two functions.
type Funcs struct {
	OnFunc  func() bool
	SetFunc func(bool) error
}

func (f Funcs) On() bool          { return f.OnFunc() }
func (f Funcs) Set(on bool) error { return f.SetFunc(on) }

// ProxyAccounts are the accounts the proxy manager owns.
func ProxyAccounts(m *proxy.Manager, st *store.Store) Accounts { return proxyAccounts{m, st} }

type proxyAccounts struct {
	m  *proxy.Manager
	st *store.Store
}

func (p proxyAccounts) SelectedCodex(ctx context.Context) (store.Account, bool, error) {
	id := p.m.SelectedID("codex")
	if id == "" {
		return store.Account{}, false, nil
	}
	a, err := p.m.PrepareAccount(ctx, id)
	if err != nil {
		return store.Account{}, false, err
	}
	return a, true, nil
}

func (p proxyAccounts) Get(id string) (store.Account, error) { return p.st.Get(id) }
func (p proxyAccounts) AdoptToken(id string, token store.Token) error {
	return p.m.AdoptToken(id, token)
}
func (p proxyAccounts) SetHandedBack(on bool) { p.m.SetHandedBack(on) }

// ClaudeNative is Claude Code's login through Switcher's native switching.
// native may be nil when native switching is unavailable here.
func ClaudeNative(m *proxy.Manager, st *store.Store, native provider.NativeLoginProvider) ClaudeCode {
	return claudeNative{m, st, native}
}

type claudeNative struct {
	m      *proxy.Manager
	st     *store.Store
	native provider.NativeLoginProvider
}

func (c claudeNative) LoggedIn() (bool, bool) {
	if c.native == nil || !c.native.NativeEnabled() {
		return false, false
	}
	switch c.native.NativeStatus().Condition {
	case "ready", "unmanaged":
		return true, true
	case "logged_out":
		return false, true
	}
	return false, false
}

func (c claudeNative) UseSelected(ctx context.Context) (string, error) {
	id := c.m.SelectedID("claude")
	if id == "" {
		return "", nil
	}
	account, err := c.st.Get(id)
	if err != nil {
		return "", err
	}
	result, err := c.m.ActivateForClient(ctx, id)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errors.New("Claude Code's login did not change")
	}
	return account.Email, nil
}
