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
	// HasRun reports whether Codex has run here: its directory exists.
	HasRun() bool
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

// errLoginKept: asked for a login, Claude Code turned out to have one.
var errLoginKept = errors.New("Claude Code keeps its login")

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

// Settle keeps a handed-back Switcher off sharing and its paired host.
// Older releases let either be turned back on while handed back, which left
// the menu saying both at once; each is turned off again and recorded, so
// setting up again turns it back on.
func (m *Manager) Settle() error {
	if !m.busy.TryLock() {
		return ErrBusy
	}
	defer m.busy.Unlock()
	m.mu.Lock()
	cur := m.rec
	m.mu.Unlock()
	if cur == nil || cur.Phase != "handed_back" {
		return nil
	}
	rec := *cur
	var toggles []Toggle
	if s := m.deps.Sharing; s != nil && s.On() {
		rec.Sharing = true
		toggles = append(toggles, s)
	}
	if h := m.deps.UseHost; h != nil && h.On() {
		rec.UseHost = true
		toggles = append(toggles, h)
	}
	if len(toggles) == 0 {
		return nil
	}
	// Recorded first: a Switcher that stops halfway still turns them back
	// on when set up again.
	if err := m.save(&rec); err != nil {
		return err
	}
	m.mu.Lock()
	m.rec = &rec
	m.mu.Unlock()
	var errs []error
	for _, t := range toggles {
		errs = append(errs, t.Set(false))
	}
	return errors.Join(errs...)
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
//
// Each change is recorded in handback.json before it is made, so a Switcher
// that stops halfway restarts off its accounts and knows what to redo. One
// that stopped halfway is finished by setting up again what it undid and
// handing back afresh: the tools' logins need Switcher's accounts, which it
// stays off after such a restart.
func (m *Manager) HandBack(ctx context.Context) (Status, error) {
	if !m.busy.TryLock() {
		return m.Status(), ErrBusy
	}
	defer m.busy.Unlock()
	m.mu.Lock()
	prev := m.rec
	m.mu.Unlock()
	if prev != nil && prev.Phase == "handed_back" {
		return m.Status(), nil
	}
	rec := record{Phase: "handing_back", Since: m.deps.Now().UTC()}
	var steps []Step
	carried := false // tools still handed back from the earlier attempt
	if prev != nil {
		failed, err := m.setUp(ctx)
		if err != nil {
			return m.Status(), fmt.Errorf("could not finish the earlier hand-back: %w", err)
		}
		// A tool that could not be set up again is still handed back: it
		// stays on record, so setting up again later redoes it.
		for _, s := range m.Status().Steps {
			if s.Outcome == "failed" {
				steps = append(steps, s)
			}
		}
		carried = len(failed) > 0
		for _, tool := range failed {
			switch tool {
			case "host":
				rec.UseHost = true
			case "sharing":
				rec.Sharing = true
			case "claude-desktop":
				rec.Desktop = true
			case "codex":
				rec.CodexConfig = prev.CodexConfig
			}
		}
	}

	note := func(tool, outcome, message string) { steps = append(steps, Step{tool, outcome, message}) }
	var undo []func() error
	fail := func(tool string, err error) (Status, error) {
		note(tool, "failed", err.Error())
		var undoErr error
		for i := len(undo) - 1; i >= 0; i-- {
			if e := undo[i](); e != nil && undoErr == nil {
				undoErr = e
			}
		}
		if undoErr != nil || carried {
			// What could not be put back stays on record, with Switcher off
			// its accounts, so setting up again redoes it.
			m.apply(true)
			rec.Phase = "handing_back"
			_ = m.save(&rec)
			kept := rec
			m.mu.Lock()
			m.rec, m.last = &kept, steps
			m.mu.Unlock()
			why := "an earlier attempt left tools to set up again"
			if undoErr != nil {
				why = fmt.Sprintf("putting the earlier steps back also failed (%v)", undoErr)
			}
			return m.Status(), fmt.Errorf("%s: %w; %s, so Switcher stays handed back: set it up again to finish", tool, err, why)
		}
		_ = m.save(nil)
		m.mu.Lock()
		m.last = steps
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("%s: %w; nothing was handed back", tool, err)
	}
	checkpoint := func() error {
		if err := m.save(&rec); err != nil {
			return fmt.Errorf("could not save the hand-back: %w", err)
		}
		return nil
	}
	if err := checkpoint(); err != nil {
		return fail("switcher", err)
	}

	// Logins first: they need Switcher's accounts, which it stops using as
	// soon as the tools have logins of their own.
	if c := m.deps.ClaudeCode; c != nil {
		loggedIn, available := c.LoggedIn()
		switch {
		case !available:
			note("claude-code", "skipped", "Switcher has not managed Claude Code's login here; Claude Code keeps whatever login it has.")
		case loggedIn:
			note("claude-code", "unchanged", "Claude Code keeps its current login and refreshes it itself from now on.")
		default:
			email, err := c.UseSelected(ctx)
			switch {
			case errors.Is(err, errLoginKept):
				note("claude-code", "unchanged", "Claude Code keeps its current login and refreshes it itself from now on.")
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
		if err := m.handBackCodex(ctx, &rec, checkpoint, note, &undo); err != nil {
			return fail("codex", err)
		}
	}
	m.apply(true)
	undo = append(undo, func() error { m.apply(false); return nil })

	if d := m.deps.Desktop; d != nil {
		setup := d.SetupStatus()
		switch {
		case setup.Configured || setup.Condition == "pending":
			// A restore that stopped halfway leaves Desktop's own setup
			// pending; restoring again finishes it.
			rec.Desktop = true
			if err := checkpoint(); err != nil {
				return fail("switcher", err)
			}
			if err := d.Restore(ctx); err != nil {
				return fail("claude-desktop", err)
			}
			undo = append(undo, func() error { return d.Configure(context.Background()) })
			note("claude-desktop", "done", "Claude Desktop's settings are restored. Until you restart Desktop it keeps running through Switcher, on its own login.")
		case setup.Condition == "unavailable":
			note("claude-desktop", "skipped", "Claude Desktop routing is unavailable here.")
		default:
			note("claude-desktop", "unchanged", "Claude Desktop was not routed through Switcher.")
		}
	}
	if s := m.deps.Sharing; s != nil && s.On() {
		rec.Sharing = true
		if err := checkpoint(); err != nil {
			return fail("switcher", err)
		}
		if err := s.Set(false); err != nil {
			return fail("sharing", err)
		}
		undo = append(undo, func() error { return s.Set(true) })
		note("sharing", "done", "Sharing is off. Switchers that used this one can work without it.")
	}
	if h := m.deps.UseHost; h != nil && h.On() {
		rec.UseHost = true
		if err := checkpoint(); err != nil {
			return fail("switcher", err)
		}
		if err := h.Set(false); err != nil {
			return fail("host", err)
		}
		undo = append(undo, func() error { return h.Set(true) })
		note("host", "done", "This Switcher no longer uses its paired host; the pairing stays.")
	}

	rec.Phase = "handed_back"
	if err := checkpoint(); err != nil {
		return fail("switcher", err)
	}
	note("switcher", "done", "Switcher no longer uses its accounts: nothing is routed, refreshed or switched until you set it up again.")
	m.mu.Lock()
	m.rec, m.last = &rec, steps
	m.mu.Unlock()
	return m.Status(), nil
}

func (m *Manager) handBackCodex(ctx context.Context, rec *record, checkpoint func() error, note func(tool, outcome, message string), undo *[]func() error) error {
	c := m.deps.Codex
	status := c.Check()
	// Only a Codex that used Switcher is pointed at it again afterwards; an
	// entry the user had switched away from is just removed.
	inUse := status.Condition == "ready" || status.Condition == "misconfigured"
	uninstall := func(kind string, run func() error) error {
		if inUse {
			rec.CodexConfig = kind
			if err := checkpoint(); err != nil {
				return err
			}
		}
		if err := run(); err != nil {
			return err
		}
		if inUse {
			*undo = append(*undo, c.Install)
		}
		return nil
	}
	switch {
	case status.Ownership == "tracked":
		if err := uninstall("tracked", c.Uninstall); err != nil {
			return err
		}
		note("codex", "done", "Codex uses its previous provider again.")
	case status.Ownership == "legacy_candidate":
		if err := uninstall("legacy", c.UninstallLegacy); err != nil {
			return err
		}
		note("codex", "done", "Switcher's entry is gone from Codex's config; Codex uses OpenAI directly.")
	case status.Condition == "pending" || status.Condition == "conflict" || status.Condition == "invalid":
		return errors.New("Codex's config.toml changed outside Switcher or is invalid; fix it, or remove the [model_providers.switcher] entry yourself")
	case rec.CodexConfig != "":
		note("codex", "unchanged", "Codex still uses its previous provider; setting Switcher up again points it at Switcher.")
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
	case m.deps.Accounts == nil:
		note("codex-login", "skipped", "Codex has no login; run codex login.")
		return nil
	case !c.HasRun():
		// A login nobody uses would only rotate alongside Switcher's copy
		// once Codex is installed.
		note("codex-login", "skipped", "Codex has not run on this computer, so it gets no login; run codex login if you install it.")
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
	rec.CodexLogin = account.ID
	if err := checkpoint(); err != nil {
		return err
	}
	written, err := c.WriteLogin(codexcfg.AuthTokens{IDToken: account.Token.IDToken, AccessToken: account.Token.AccessToken,
		RefreshToken: account.Token.RefreshToken, AccountID: account.Token.AccountID})
	if errors.Is(err, codexcfg.ErrNoCodex) {
		rec.CodexLogin = ""
		note("codex-login", "skipped", "Codex has not run on this computer, so it gets no login; run codex login if you install it.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not give Codex a login: %w", err)
	}
	rec.CodexLoginDigest = written
	*undo = append(*undo, func() error { return c.RemoveLoginIfUnchanged(written) })
	if err := checkpoint(); err != nil {
		return err
	}
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
	failed, err := m.setUp(ctx)
	if err != nil {
		return m.Status(), err
	}
	if len(failed) > 0 {
		return m.Status(), fmt.Errorf("Switcher is set up again, except: %v", failed)
	}
	return m.Status(), nil
}

// setUp is SetUp with the busy lock held. It returns the tools that could
// not be set up again; an error means Switcher stayed handed back.
func (m *Manager) setUp(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	if m.rec == nil {
		m.mu.Unlock()
		return nil, nil
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

	// Codex had no login before the hand-back. The one it was given goes back
	// to Switcher, with any tokens Codex refreshed meanwhile, so the account
	// has one owner again. Until that works Switcher stays off its accounts.
	if rec.CodexLogin != "" && m.deps.Codex != nil && m.deps.Accounts != nil {
		took, adopted, err := m.takeBackCodexLogin(rec.CodexLogin, rec.CodexLoginDigest)
		if err != nil {
			note("codex-login", "failed", "Could not take back the login Codex was given: "+err.Error())
			m.mu.Lock()
			m.last = steps
			m.mu.Unlock()
			return nil, fmt.Errorf("could not take back the login Codex was given: %w; Switcher stays handed back, so try again", err)
		}
		if took {
			message := "Switcher took back the login it gave Codex"
			if adopted {
				message += ", with the tokens Codex refreshed meanwhile"
			}
			message += "."
			if rec.CodexConfig == "" {
				message += " Codex has no login again, as before the hand-back; run codex login to give it one."
			}
			note("codex-login", "done", message+" Restart Codex if it is running, so it lets go of that login.")
		}
	}
	// The redo steps are safe to repeat, so the record goes only after them:
	// a Switcher that stops meanwhile stays handed back and redoes them all.
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
	if d := m.deps.Desktop; rec.Desktop && d != nil {
		// A restore that stopped halfway has to finish before Desktop can be
		// connected again.
		var err error
		if d.SetupStatus().Condition == "pending" {
			err = d.Restore(ctx)
		}
		if err == nil {
			err = d.Configure(ctx)
		}
		if err != nil {
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
	if err := m.save(nil); err != nil {
		m.mu.Lock()
		m.last = steps
		m.mu.Unlock()
		return nil, fmt.Errorf("could not save the setup: %w; Switcher stays handed back, so try again", err)
	}
	m.mu.Lock()
	m.rec = nil
	m.mu.Unlock()
	m.apply(false)
	note("switcher", "done", "Switcher uses its accounts again.")
	if c := m.deps.ClaudeCode; c != nil {
		note("claude-code", "done", "Switcher manages Claude Code's account again; switch accounts as before.")
	}
	m.mu.Lock()
	m.last = steps
	m.mu.Unlock()
	return failed, nil
}

// takeBackCodexLogin takes back the login hand-back gave Codex. Only a file
// that holds nothing but that account's ChatGPT login goes: Switcher adopts
// its tokens if Codex refreshed them, then removes it. Any other login is
// Codex's own and stays, and Switcher keeps its own tokens, so neither has
// two owners.
func (m *Manager) takeBackCodexLogin(id, written string) (took, adopted bool, err error) {
	c := m.deps.Codex
	login, err := c.ReadLogin()
	if err != nil {
		return false, false, err
	}
	tokens, _, ok := login.Tokens()
	if !ok || login.APIKey {
		return false, false, nil
	}
	account, err := m.deps.Accounts.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return false, false, nil // removed from Switcher meanwhile: the login is Codex's alone
	}
	if err != nil {
		return false, false, err
	}
	if account.Token.AccountID == "" {
		// Without an account id only the file as written, or one still
		// holding the account's own refresh token, is known to be Switcher's.
		if login.Digest() != written && tokens.RefreshToken != account.Token.RefreshToken {
			return false, false, nil
		}
		return true, false, m.removeCodexLogin(login.Digest())
	}
	// Team seats share an account id; the email tells them apart.
	if tokens.AccountID != account.Token.AccountID || !sameEmail(tokens.IDToken, account.Email) {
		return false, false, nil
	}
	if tokens.RefreshToken != account.Token.RefreshToken {
		token := account.Token
		token.IDToken, token.AccessToken, token.RefreshToken = tokens.IDToken, tokens.AccessToken, tokens.RefreshToken
		token.ExpiresAt = jwtClaimsOf(tokens.AccessToken).Exp
		if err := m.deps.Accounts.AdoptToken(id, token); err != nil {
			return false, false, err
		}
		adopted = true
	}
	return true, adopted, m.removeCodexLogin(login.Digest())
}

// removeCodexLogin removes auth.json while it is still the file as read. One
// Codex rewrote since is newer still: that is an error, so Switcher stays
// handed back and the next set-up takes the newer tokens.
func (m *Manager) removeCodexLogin(read string) error {
	if err := m.deps.Codex.RemoveLoginIfUnchanged(read); err != nil {
		return err
	}
	if login, err := m.deps.Codex.ReadLogin(); err != nil || login.Digest() != "" {
		return errors.New("Codex changed its login meanwhile; set Switcher up again to take the newer one")
	}
	return nil
}

// sameEmail reports whether an id token belongs to email. A token or account
// without an email cannot tell, and does not refuse.
func sameEmail(idToken, email string) bool {
	claim := jwtClaimsOf(idToken).Email
	return claim == "" || email == "" || strings.EqualFold(claim, email)
}

type jwtClaims struct {
	Exp   int64  `json:"exp"`
	Email string `json:"email"`
}

// jwtClaimsOf reads a JWT's claims without verifying it: the token came from
// Codex's own store, and the claims only decide when Switcher refreshes it
// and whose it is. A zero exp means unknown, and the first rejection
// refreshes it.
func jwtClaimsOf(token string) jwtClaims {
	var claims jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims
	}
	_ = json.Unmarshal(raw, &claims)
	return claims
}
