package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
)

func routed(m *Manager) { m.SetClaudeCodeRouted(func() bool { return true }) }

// settlingNative is a native login that knows, right now, whether it has one.
type settlingNative struct {
	*autoNative
	loggedIn bool
	err      error
	calls    int
}

func (p *settlingNative) SettleNative(context.Context) (bool, error) {
	p.calls++
	return p.loggedIn, p.err
}

func settlingManager(t *testing.T, loggedIn bool, err error) (*Manager, *settlingNative) {
	t.Helper()
	_, inner := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	st := store.New(t.TempDir())
	for _, a := range []store.Account{claudeAccount("a"), claudeAccount("b")} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	native := &settlingNative{autoNative: inner, loggedIn: loggedIn, err: err}
	m, newErr := New(st, map[string]provider.Provider{"claude": native}, []string{"claude"})
	if newErr != nil {
		t.Fatal(newErr)
	}
	routed(m)
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	return m, native
}

// deadNative cannot refresh any account: its sign-in no longer works.
type deadNative struct{ *autoNative }

func (deadNative) IsExpired(store.Account) bool { return true }
func (deadNative) Refresh(context.Context, *store.Account) error {
	return provider.ErrReloginRequired
}

func TestARoutedClaudeCodeKeepsItsLoginOnASwitch(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	result, err := m.ActivateForClient(context.Background(), "b")
	if err != nil || result != nil {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if len(native.switched) != 0 || native.status.ActiveID != "a" {
		t.Fatalf("the login moved: %v, now %q", native.switched, native.status.ActiveID)
	}
	if m.ActiveID("claude") != "b" || m.SelectedID("claude") != "b" {
		t.Fatalf("active %q, selected %q, want b", m.ActiveID("claude"), m.SelectedID("claude"))
	}
	if got := NewDesktopCredentialSource(m).Selected(); got != "b" {
		t.Fatalf("relay uses %q, want b", got)
	}
}

func TestAClaudeCodeNotRoutedStillSwitchesItsLogin(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	m.SetClaudeCodeRouted(func() bool { return false })
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if len(native.switched) != 1 || native.switched[0] != "b" || m.SelectedID("claude") != "b" {
		t.Fatalf("switched %v, selected %q", native.switched, m.SelectedID("claude"))
	}
}

// Handing back relies on this: a Claude Code without a login gets one.
func TestARoutedClaudeCodeWithoutALoginGetsOneFromASwitch(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	native.status = claudecode.Status{Condition: "logged_out"}
	result, err := m.ActivateForClient(context.Background(), "b")
	if err != nil || result == nil || !result.Changed {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if len(native.switched) != 1 || native.switched[0] != "b" {
		t.Fatalf("switched %v", native.switched)
	}
}

func TestASwitchGoesByTheLoginRightNowNotTheLastStatus(t *testing.T) {
	// A /login a moment ago: the status still says logged out, and replacing
	// the login would lose its Remote Control.
	m, native := settlingManager(t, true, nil)
	native.status = claudecode.Status{Condition: "logged_out"}
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if len(native.switched) != 0 || m.ActiveID("claude") != "b" {
		t.Fatalf("switched %v, active %q", native.switched, m.ActiveID("claude"))
	}
	// A /logout a moment ago: the status still says ready, and it needs a login.
	m, native = settlingManager(t, false, nil)
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if len(native.switched) != 1 || native.switched[0] != "b" {
		t.Fatalf("switched %v", native.switched)
	}
}

func TestAKeptSwitchFinishesAnInterruptedSwitchFirst(t *testing.T) {
	m, native := settlingManager(t, true, nil)
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if native.calls != 1 {
		t.Fatalf("settled %d times", native.calls)
	}
	failure := errors.New("backup retained for manual recovery")
	m, native = settlingManager(t, true, failure)
	result, err := m.ActivateForClient(context.Background(), "b")
	if !errors.Is(err, failure) || result == nil {
		t.Fatalf("result %+v, err %v: the caller must be able to say why", result, err)
	}
	if m.ActiveID("claude") != "a" || len(native.switched) != 0 {
		t.Fatalf("active %q, switched %v", m.ActiveID("claude"), native.switched)
	}
}

func TestASwitchToAnAccountThatCannotSignInIsRefused(t *testing.T) {
	_, inner := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	st := store.New(t.TempDir())
	for _, a := range []store.Account{claudeAccount("a"), claudeAccount("b")} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(st, map[string]provider.Provider{"claude": deadNative{inner}}, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	routed(m)
	if _, err := m.ActivateForClient(context.Background(), "b"); !errors.Is(err, ErrAccountUnusable) {
		t.Fatalf("err %v", err)
	}
	if m.ActiveID("claude") != "a" {
		t.Fatalf("selection moved to %q", m.ActiveID("claude"))
	}
}

func TestTheSelectionStartsAtTheLoginOnce(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	// Drifted: a /login elsewhere, or a proxy failover, left the route on c.
	if err := m.Activate("c"); err != nil {
		t.Fatal(err)
	}
	if got := m.SelectedID("claude"); got != "a" {
		t.Fatalf("before connecting %q, want the login's a", got)
	}
	routed(m)
	if got := NewDesktopCredentialSource(m).Selected(); got != "a" {
		t.Fatalf("connecting moved the relay to %q", got)
	}
	if m.ActiveID("claude") != "a" {
		t.Fatalf("active %q", m.ActiveID("claude"))
	}
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	// A restart must not take the user's pick back to the login.
	again, err := New(m.store, map[string]provider.Provider{"claude": native}, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	routed(again)
	if got := again.SelectedID("claude"); got != "b" {
		t.Fatalf("after a restart %q, want b", got)
	}
}

func TestDisconnectingGoesBackToTheLogin(t *testing.T) {
	m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	on := true
	m.SetClaudeCodeRouted(func() bool { return on })
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	on = false
	if m.NativeLoginKept("claude") || m.SelectedID("claude") != "a" {
		t.Fatalf("kept %v, selected %q", m.NativeLoginKept("claude"), m.SelectedID("claude"))
	}
	// A moment of settings that cannot be read, or an undone hand-back, moved
	// nothing: the pick is still there.
	on = true
	if m.SelectedID("claude") != "b" {
		t.Fatalf("selected %q, want the pick b", m.SelectedID("claude"))
	}
	// Connecting on purpose starts at the login again.
	if err := m.RestartSelectionAtLogin(); err != nil {
		t.Fatal(err)
	}
	if m.SelectedID("claude") != "a" || m.ActiveID("claude") != "a" {
		t.Fatalf("selected %q, active %q", m.SelectedID("claude"), m.ActiveID("claude"))
	}
	state, err := m.store.LoadState()
	if err != nil || !state.KeptLoginAligned {
		t.Fatalf("state %+v, err %v", state, err)
	}
}

// Without knowing the login, the relay leaves the caller's own token, which is
// that login, rather than a stale account until the selection can start.
func TestAKeptLoginNotKnownYetAnswersWithItself(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	routed(m)
	native.status = claudecode.Status{Condition: "unavailable"}
	source := NewDesktopCredentialSource(m)
	if got := source.Selected(); got != "" {
		t.Fatalf("relay uses %q while the login is unknown", got)
	}
	native.status = claudecode.Status{Condition: "ready", ActiveID: "a"}
	if got := source.Selected(); got != "a" {
		t.Fatalf("relay uses %q, want the login's a", got)
	}
}

func TestAKeptSelectionThatStopsWorkingIsShownMoving(t *testing.T) {
	m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := m.deactivate("b"); err != nil {
		t.Fatal(err)
	}
	event := m.LastAutoSwitch()
	if m.ActiveID("claude") != "a" || event == nil || event.From != "b" || event.To != "a" {
		t.Fatalf("active %q, event %+v", m.ActiveID("claude"), event)
	}
}

func TestARemovedSelectionFallsBackToTheLogin(t *testing.T) {
	m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteAccount("b"); err != nil {
		t.Fatal(err)
	}
	if m.SelectedID("claude") != "a" || m.ActiveID("claude") != "a" {
		t.Fatalf("selected %q, active %q", m.SelectedID("claude"), m.ActiveID("claude"))
	}
}

func TestAKeptLoginOutsideSwitcherLeavesTheSelection(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	native.status = claudecode.Status{Condition: "unmanaged"}
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	routed(m)
	if m.SelectedID("claude") != "b" {
		t.Fatalf("selected %q", m.SelectedID("claude"))
	}
}

func TestARoutedClaudeCodeMovesItsSelectionWhenItRunsOut(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	native.status = claudecode.Status{Condition: "unmanaged"}
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	m.setUsage("a", windows(100, 40))
	m.setUsage("b", windows(10, 20))
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("the login moved: %v", native.switched)
	}
	if m.ActiveID("claude") != "b" {
		t.Fatalf("selection %q, want b", m.ActiveID("claude"))
	}
	if event := m.LastAutoSwitch(); event == nil || event.From != "a" || event.To != "b" || event.Where != "claude_code" {
		t.Fatalf("event %+v", event)
	}
}

func TestAKeptLoginsAccountRunningOutMovesNothingElse(t *testing.T) {
	// The login's account is out, the selection has room: nothing to do.
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	m.setUsage("a", windows(100, 40))
	m.setUsage("b", windows(10, 20))
	m.AutoSwitchNative()
	if len(native.switched) != 0 || m.ActiveID("claude") != "b" {
		t.Fatalf("switched %v, selection %q", native.switched, m.ActiveID("claude"))
	}
}

func TestAKeptSwitchStaysOnTheStandIn(t *testing.T) {
	m, _ := autoManager(t, true, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	routed(m)
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	m.setUsage("a", windows(100, 40))
	m.setUsage("b", windows(60, 60))
	m.setUsage("c", windows(10, 10))
	m.mu.Lock()
	m.claudeReroute["a"] = "b"
	m.mu.Unlock()
	m.AutoSwitchNative()
	if m.ActiveID("claude") != "b" {
		t.Fatalf("moved to %q, off the stand-in that already answers", m.ActiveID("claude"))
	}
}

func TestAPickAtTheLimitAnswersUntilAnthropicRefusesIt(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	native.status.ActiveID = "b"
	routed(m)
	m.setUsage("a", windows(100, 0))
	m.setUsage("b", windows(0, 0))
	if _, err := m.ActivateForClient(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	source := NewDesktopCredentialSource(m)
	if got := source.Selected(); got != "a" {
		t.Fatalf("the relay ignored the pick: %q", got)
	}
	m.AutoSwitchNative()
	if m.ActiveID("claude") != "a" {
		t.Fatalf("selection %q", m.ActiveID("claude"))
	}
	source.TookOver("a", "b")
	if got := source.Selected(); got != "b" {
		t.Fatalf("after a refusal %q, want the stand-in", got)
	}
}

func TestProxyFailoverOfAKeptLoginIsShown(t *testing.T) {
	run := func(t *testing.T, kept bool) (*Manager, *AutoSwitch) {
		m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
		if kept {
			routed(m)
		}
		if err := m.Activate("a"); err != nil {
			t.Fatal(err)
		}
		_, epoch, err := m.pickForRequest("claude")
		if err != nil {
			t.Fatal(err)
		}
		if retry, _, err := m.handleExhaustion("claude", "a", time.Now().Add(time.Hour), epoch, false); err != nil || !retry {
			t.Fatalf("retry %v, err %v", retry, err)
		}
		return m, m.LastAutoSwitch()
	}
	m, event := run(t, true)
	if m.SelectedID("claude") != "b" || event == nil || event.From != "a" || event.To != "b" {
		t.Fatalf("selected %q, event %+v", m.SelectedID("claude"), event)
	}
	m, event = run(t, false)
	if m.SelectedID("claude") != "a" || event != nil {
		t.Fatalf("without the relay: selected %q, event %+v", m.SelectedID("claude"), event)
	}
}
