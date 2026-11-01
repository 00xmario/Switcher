// Package handback hands every tool Switcher set up back to its provider, for
// example before Switcher is removed, and sets Switcher up again afterwards.
//
// Handing back undoes what Switcher's setup changed: Codex's provider entry,
// Claude Desktop's relay settings, sharing with other Switchers and the use
// of a paired host. Each tool is left with a login of its own: Claude Code
// and Codex keep theirs, and one that has none gets the account selected in
// Switcher. From then on Switcher stops using its accounts entirely (no
// proxying, no token refresh, no usage polls), because a login rotated by
// two owners signs one of them out.
//
// The record of what was undone lives in handback.json, so turning hand-back
// off redoes exactly that, also after a restart.
package handback

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"switcher/internal/codexcfg"
	"switcher/internal/desktoprelay"
	"switcher/internal/store"
)

// Codex is Switcher's Codex setup, codexcfg in production.
type Codex interface {
	Check() codexcfg.Status
	Install() error
	Uninstall() error
	UninstallLegacy() error
	ReadLogin() (codexcfg.Login, error)
	WriteLogin(codexcfg.AuthTokens) (string, error)
	RemoveLoginIfUnchanged(written string) error
}

// Desktop is the Claude Desktop relay's settings setup.
type Desktop interface {
	SetupStatus() desktoprelay.SetupStatus
	Configure(context.Context) error
	Restore(context.Context) error
	SetPassthrough(bool)
}

// Toggle is an on/off choice owned elsewhere: sharing this Switcher, or
// using a paired host.
type Toggle interface {
	On() bool
	Set(bool) error
}

// ClaudeCode is Claude Code's native login as Switcher manages it.
type ClaudeCode interface {
	// LoggedIn reports whether Claude Code has a login, and whether Switcher
	// can see its store at all.
	LoggedIn() (loggedIn, available bool)
	// UseSelected writes the account selected in Switcher as Claude Code's
	// login and returns its email; "" when no account is selected.
	UseSelected(context.Context) (string, error)
}

// Accounts are Switcher's own accounts.
type Accounts interface {
	// SelectedCodex returns the selected Codex account with a current token.
	SelectedCodex(context.Context) (store.Account, bool, error)
	Get(id string) (store.Account, error)
	AdoptToken(id string, token store.Token) error
	// SetHandedBack stops or resumes every use of the accounts.
	SetHandedBack(bool)
}

// Deps are the tools and settings hand-back acts on. Nil ones are skipped.
type Deps struct {
	Codex      Codex
	Desktop    Desktop
	Sharing    Toggle // hosting other Switchers
	UseHost    Toggle // using a paired host; nil or off when not paired
	ClaudeCode ClaudeCode
	Accounts   Accounts
	Now        func() time.Time
}

// Step is the outcome for one tool, for the UI.
type Step struct {
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"` // done, unchanged, skipped, failed
	Message string `json:"message"`
}

// Status is what the UIs show.
type Status struct {
	HandedBack bool      `json:"handed_back"`
	Since      time.Time `json:"since,omitempty"`
	// Incomplete marks a hand-back that stopped halfway, for example when
	// Switcher quit during it. Handing back again finishes it.
	Incomplete bool   `json:"incomplete,omitempty"`
	Steps      []Step `json:"steps"`
}

// record is handback.json: what was undone, so it can be redone.
type record struct {
	Phase string    `json:"phase"` // handing_back, handed_back
	Since time.Time `json:"since"`
	// CodexConfig is "tracked" or "legacy" when Switcher's Codex provider
	// entry was removed.
	CodexConfig string `json:"codex_config,omitempty"`
	// CodexLogin is the Switcher account written as Codex's login, and the
	// digest of the auth.json written for it.
	CodexLogin       string `json:"codex_login,omitempty"`
	CodexLoginDigest string `json:"codex_login_digest,omitempty"`
	Desktop          bool   `json:"desktop,omitempty"`
	Sharing          bool   `json:"sharing,omitempty"`
	UseHost          bool   `json:"use_host,omitempty"`
}

// ErrBusy is returned while another hand-back or setup runs.
var ErrBusy = errors.New("Switcher is already handing back or setting up; try again in a moment")

// Manager owns the hand-back state.
type Manager struct {
	path string
	deps Deps
	busy sync.Mutex
	mu   sync.Mutex
	rec  *record
	last []Step
}

// New loads handback.json in dir. A Switcher that was handed back, or
// stopped halfway through, stays off its accounts from the first request.
func New(dir string, deps Deps) *Manager {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	m := &Manager{path: filepath.Join(dir, "handback.json"), deps: deps}
	var rec record
	if raw, err := os.ReadFile(m.path); err == nil && json.Unmarshal(raw, &rec) == nil && rec.Phase != "" {
		m.rec = &rec
	}
	m.apply(m.rec != nil)
	return m
}

func (m *Manager) apply(on bool) {
	if m.deps.Accounts != nil {
		m.deps.Accounts.SetHandedBack(on)
	}
	if m.deps.Desktop != nil {
		m.deps.Desktop.SetPassthrough(on)
	}
}

// HandedBack reports whether the tools are handed back, fully or halfway.
func (m *Manager) HandedBack() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rec != nil
}

// Status reports the state and the last action's steps.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Steps: append([]Step{}, m.last...)}
	if m.rec != nil {
		s.HandedBack, s.Since, s.Incomplete = true, m.rec.Since, m.rec.Phase != "handed_back"
	}
	return s
}

func (m *Manager) save(rec *record) error {
	if rec == nil {
		if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// HandBack undoes Switcher's setup of every tool and stops using its
// accounts. When a step fails, the steps before it are put back and nothing
// is handed back; the error says which tool needs attention.
func (m *Manager) HandBack(ctx context.Context) (Status, error) {
	if !m.busy.TryLock() {
		return m.Status(), ErrBusy
	}
	defer m.busy.Unlock()
	m.mu.Lock()
	rec := record{Phase: "handing_back", Since: m.deps.Now().UTC()}
	if m.rec != nil {
		rec = *m.rec // finish an earlier hand-back, keeping what it undid
		rec.Phase = "handing_back"
	}
	m.mu.Unlock()

	var steps []Step
	note := func(tool, outcome, message string) { steps = append(steps, Step{tool, outcome, message}) }
	var undo []func()
	fail := func(tool string, err error) (Status, error) {
		note(tool, "failed", err.Error())
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		m.mu.Lock()
		m.last = steps
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("%s: %w; nothing was handed back", tool, err)
	}

	// Logins first: they need Switcher's accounts, which stop working once
	// the hand-back is saved.
	if c := m.deps.ClaudeCode; c != nil {
		loggedIn, available := c.LoggedIn()
		switch {
		case !available:
			note("claude-code", "skipped", "Switcher cannot see Claude Code's login here; Claude Code keeps whatever login it has.")
		case loggedIn:
			note("claude-code", "unchanged", "Claude Code keeps its current login and refreshes it itself from now on.")
		default:
			email, err := c.UseSelected(ctx)
			switch {
			case err != nil:
				return fail("claude-code", fmt.Errorf("could not give Claude Code a login: %w", err))
			case email == "":
				note("claude-code", "skipped", "Claude Code has no login and no Claude account is selected in Switcher; run /login in Claude Code.")
			default:
				note("claude-code", "done", "Claude Code is now logged in as "+email+" and refreshes that login itself.")
			}
		}
	}
	if c := m.deps.Codex; c != nil {
		if err := m.handBackCodex(ctx, &rec, note, &undo); err != nil {
			return fail("codex", err)
		}
	}
	if d := m.deps.Desktop; d != nil {
		setup := d.SetupStatus()
		switch {
		case setup.Configured:
			if err := d.Restore(ctx); err != nil {
				return fail("claude-desktop", err)
			}
			rec.Desktop = true
			undo = append(undo, func() { _ = d.Configure(context.Background()); rec.Desktop = false })
			note("claude-desktop", "done", "Claude Desktop's settings are restored. Until you restart Desktop it keeps running through Switcher, on its own login.")
		case setup.Condition == "unavailable":
			note("claude-desktop", "skipped", "Claude Desktop routing is unavailable here.")
		default:
			note("claude-desktop", "unchanged", "Claude Desktop was not routed through Switcher.")
		}
	}
	if s := m.deps.Sharing; s != nil {
		if s.On() {
			if err := s.Set(false); err != nil {
				return fail("sharing", err)
			}
			rec.Sharing = true
			undo = append(undo, func() { _ = s.Set(true); rec.Sharing = false })
			note("sharing", "done", "Sharing is off. Switchers that used this one can work without it.")
		}
	}
	if h := m.deps.UseHost; h != nil {
		if h.On() {
			if err := h.Set(false); err != nil {
				return fail("host", err)
			}
			rec.UseHost = true
			undo = append(undo, func() { _ = h.Set(true); rec.UseHost = false })
			note("host", "done", "This Switcher no longer uses its paired host; the pairing stays.")
		}
	}

	rec.Phase = "handed_back"
	if err := m.save(&rec); err != nil {
		return fail("switcher", fmt.Errorf("could not save the hand-back: %w", err))
	}
	m.apply(true)
	note("switcher", "done", "Switcher no longer uses its accounts: nothing is routed, refreshed or switched until you set it up again.")
	m.mu.Lock()
	m.rec, m.last = &rec, steps
	m.mu.Unlock()
	return m.Status(), nil
}

func (m *Manager) handBackCodex(ctx context.Context, rec *record, note func(tool, outcome, message string), undo *[]func()) error {
	c := m.deps.Codex
	status := c.Check()
	// Only a Codex that used Switcher is pointed at it again afterwards; an
	// entry the user had switched away from is just removed.
	inUse := status.Condition == "ready" || status.Condition == "misconfigured"
	remember := func(kind string) {
		if inUse {
			rec.CodexConfig = kind
			*undo = append(*undo, func() { _ = c.Install(); rec.CodexConfig = "" })
		}
	}
	switch {
	case status.Ownership == "tracked":
		if err := c.Uninstall(); err != nil {
			return err
		}
		remember("tracked")
		note("codex", "done", "Codex uses its previous provider again.")
	case status.Ownership == "legacy_candidate":
		if err := c.UninstallLegacy(); err != nil {
			return err
		}
		remember("legacy")
		note("codex", "done", "Switcher's entry is gone from Codex's config; Codex uses OpenAI directly.")
	case status.Condition == "pending" || status.Condition == "conflict" || status.Condition == "invalid":
		return errors.New("Codex's config.toml changed outside Switcher or is invalid; fix it, or remove the [model_providers.switcher] entry yourself")
	default:
		note("codex", "unchanged", "Codex was not set up to use Switcher.")
	}

	login, err := c.ReadLogin()
	if err != nil {
		return fmt.Errorf("could not read Codex's login: %w", err)
	}
	switch {
	case login.Present && login.Store != "file":
		note("codex-login", "unchanged", "Codex keeps its login in the system keyring; Switcher leaves it alone.")
		return nil
	case login.Present:
		note("codex-login", "unchanged", "Codex keeps its own login.")
		return nil
	}
	if m.deps.Accounts == nil {
		note("codex-login", "skipped", "Codex has no login; run codex login.")
		return nil
	}
	account, ok, err := m.deps.Accounts.SelectedCodex(ctx)
	if err != nil {
		return fmt.Errorf("could not prepare the selected Codex account: %w", err)
	}
	if !ok {
		note("codex-login", "skipped", "Codex has no login and no Codex account is selected in Switcher; run codex login.")
		return nil
	}
	written, err := c.WriteLogin(codexcfg.AuthTokens{IDToken: account.Token.IDToken, AccessToken: account.Token.AccessToken,
		RefreshToken: account.Token.RefreshToken, AccountID: account.Token.AccountID})
	if err != nil {
		return fmt.Errorf("could not give Codex a login: %w", err)
	}
	rec.CodexLogin, rec.CodexLoginDigest = account.ID, written
	*undo = append(*undo, func() {
		_ = c.RemoveLoginIfUnchanged(written)
		rec.CodexLogin, rec.CodexLoginDigest = "", ""
	})
	note("codex-login", "done", "Codex is now logged in as "+account.Email+" and refreshes that login itself.")
	return nil
}

// SetUp redoes what the hand-back undid and lets Switcher use its accounts
// again. A step that fails is reported; the rest still run, and that tool
// can be set up again from its own settings.
func (m *Manager) SetUp(ctx context.Context) (Status, error) {
	if !m.busy.TryLock() {
		return m.Status(), ErrBusy
	}
	defer m.busy.Unlock()
	m.mu.Lock()
	if m.rec == nil {
		m.mu.Unlock()
		return m.Status(), nil
	}
	rec := *m.rec
	m.mu.Unlock()

	var steps []Step
	var failed []string
	note := func(tool, outcome, message string) { steps = append(steps, Step{tool, outcome, message}) }
	failure := func(tool, message string, err error) {
		note(tool, "failed", message+": "+err.Error())
		failed = append(failed, tool)
	}

	// A login Codex kept refreshing is newer than Switcher's copy of it.
	if rec.CodexLogin != "" && m.deps.Codex != nil && m.deps.Accounts != nil {
		if adopted, err := m.adoptCodexLogin(rec.CodexLogin); err != nil {
			failure("codex-login", "Could not take back Codex's newer login", err)
		} else if adopted {
			note("codex-login", "done", "Switcher took over the newer login Codex refreshed meanwhile.")
		}
	}
	if err := m.save(nil); err != nil {
		return m.Status(), fmt.Errorf("could not save the setup: %w", err)
	}
	m.mu.Lock()
	m.rec = nil
	m.mu.Unlock()
	m.apply(false)
	note("switcher", "done", "Switcher uses its accounts again.")

	if rec.UseHost && m.deps.UseHost != nil {
		if err := m.deps.UseHost.Set(true); err != nil {
			failure("host", "Could not use the paired host again", err)
		} else {
			note("host", "done", "This Switcher uses its paired host again.")
		}
	}
	if rec.Sharing && m.deps.Sharing != nil {
		if err := m.deps.Sharing.Set(true); err != nil {
			failure("sharing", "Could not turn sharing back on", err)
		} else {
			note("sharing", "done", "Sharing is on again.")
		}
	}
	if rec.Desktop && m.deps.Desktop != nil {
		if err := m.deps.Desktop.Configure(ctx); err != nil {
			failure("claude-desktop", "Could not connect Claude Desktop again", err)
		} else {
			note("claude-desktop", "done", "Claude Desktop is connected again; restart it when your work is done.")
		}
	}
	if rec.CodexConfig != "" && m.deps.Codex != nil {
		if err := m.deps.Codex.Install(); err != nil {
			failure("codex", "Could not point Codex at Switcher again", err)
		} else {
			note("codex", "done", "Codex uses Switcher again.")
		}
	}
	if c := m.deps.ClaudeCode; c != nil {
		note("claude-code", "done", "Switcher follows Claude Code's login again; switch accounts as before.")
	}
	m.mu.Lock()
	m.last = steps
	m.mu.Unlock()
	if len(failed) > 0 {
		return m.Status(), fmt.Errorf("Switcher is set up again, except: %v", failed)
	}
	return m.Status(), nil
}

// adoptCodexLogin takes back the tokens of the login Switcher gave Codex,
// when Codex still holds that account and refreshed it.
func (m *Manager) adoptCodexLogin(id string) (bool, error) {
	login, err := m.deps.Codex.ReadLogin()
	if err != nil {
		return false, err
	}
	tokens, _, ok := login.Tokens()
	if !ok {
		return false, nil
	}
	account, err := m.deps.Accounts.Get(id)
	if err != nil {
		return false, nil // removed meanwhile: nothing to take back
	}
	if account.Token.AccountID == "" || tokens.AccountID != account.Token.AccountID || tokens.RefreshToken == account.Token.RefreshToken {
		return false, nil
	}
	token := account.Token
	token.IDToken, token.AccessToken, token.RefreshToken = tokens.IDToken, tokens.AccessToken, tokens.RefreshToken
	token.ExpiresAt = jwtExpiry(tokens.AccessToken)
	return true, m.deps.Accounts.AdoptToken(id, token)
}

// jwtExpiry reads a JWT's exp claim without verifying it: the token came from
// Codex's own store and only decides when Switcher refreshes it. 0 means
// unknown, and the first rejection refreshes it.
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	return claims.Exp
}
