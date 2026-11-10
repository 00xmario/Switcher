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

// limit sets a usage limit without the background round SetUsageLimit
// starts, so a test decides when the switch runs.
func (m *Manager) limit(id string, share int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limits[id] = share
}

func codexAccount(id, plan string) store.Account {
	return store.Account{ID: id, Provider: "codex", Email: id + "@example.com", Plan: plan,
		Token: store.Token{AccessToken: id + "-token", AccountID: "acct-" + id}}
}

func monthlyUsage(used int) provider.Usage {
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "Monthly", UsedPercent: used, ResetsAt: time.Now().Add(240 * time.Hour).Unix()}}}
}

func codexManager(t *testing.T, accounts ...store.Account) *Manager {
	t.Helper()
	st := store.New(t.TempDir())
	for _, a := range accounts {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(st, map[string]provider.Provider{"codex": &fakeProvider{id: "codex"}}, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	return m
}

// An account that keeps 10% of its weekly usage gives way at 10% left, also
// with switching on running out turned off, and says why.
func TestALimitMovesClaudeCodeAway(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	m.limit("a", 10)
	m.setUsage("a", windows(20, 89))
	m.setUsage("b", windows(0, 30))
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("switched at 11%% left: %v", native.switched)
	}
	m.setUsage("a", windows(20, 90))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "b" || m.ActiveID("claude") != "b" {
		t.Fatalf("switched %v, route %q", native.switched, m.ActiveID("claude"))
	}
	if event := m.LastAutoSwitch(); event == nil || event.From != "a" || event.To != "b" || event.Reason != "limit" || event.Where != "claude_code" {
		t.Fatalf("event %+v", event)
	}
	if !m.AtUsageLimit("a") || m.AtUsageLimit("b") {
		t.Fatal("at-limit marks are wrong")
	}
}

// Only the long windows count: a session window refills within hours, and a
// model's own window is not the account.
func TestALimitWatchesTheWeeklyWindow(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	m.limit("a", 10)
	u := windows(95, 50)
	u.Windows = append(u.Windows, provider.UsageWindow{Label: "Fable", UsedPercent: 99})
	m.setUsage("a", u)
	m.setUsage("b", windows(0, 0))
	m.AutoSwitchNative()
	if len(native.switched) != 0 || m.AtUsageLimit("a") {
		t.Fatalf("a session or model window triggered the limit: %v", native.switched)
	}
}

// Switcher never moves onto an account at its limit, counts a limit against
// an account's room, and stays where it is when nothing else has room.
func TestTakeoverRespectsEveryLimit(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	m.limit("a", 10)
	m.limit("b", 30)
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 75)) // 25% left, keeps 30%: at its limit
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("moved onto an account at its limit: %v", native.switched)
	}
	m.limit("b", 20)
	m.setUsage("b", windows(0, 50)) // 30% room above its limit
	m.setUsage("c", windows(0, 65)) // 35% room
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "c" {
		t.Fatalf("switched %v, want c, the most room above its limit", native.switched)
	}
}

// Codex has no native login: the proxy's selection moves, to a paid account
// before a Free one.
func TestALimitMovesCodexsSelection(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("free", "free"), codexAccount("y", "plus"))
	m.limit("x", 10)
	m.setUsage("x", monthlyUsage(91))
	m.setUsage("free", monthlyUsage(0))
	m.setUsage("y", monthlyUsage(70))
	m.AutoSwitchNative()
	if got := m.ActiveID("codex"); got != "y" {
		t.Fatalf("codex uses %q, want y (paid before Free)", got)
	}
	if event := m.LastAutoSwitch(); event == nil || event.Where != "proxy" || event.Reason != "limit" || event.From != "x" || event.To != "y" {
		t.Fatalf("event %+v", event)
	}
}

// Running out is the proxy's own business for Codex: without a limit, a poll
// that shows an account at 100% does not move the selection.
func TestCodexWithoutALimitStays(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("y", "plus"))
	m.setUsage("x", monthlyUsage(100))
	m.setUsage("y", monthlyUsage(0))
	m.AutoSwitchNative()
	if got := m.ActiveID("codex"); got != "x" {
		t.Fatalf("codex uses %q", got)
	}
}

// When a refused request moves the proxy, an account below its limit comes
// first, even a Free one; an account at its limit only when nothing else is
// left, since a limit never blocks a request.
func TestFailoverLeavesLimitedAccountsForLast(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("kept", "plus"), codexAccount("free", "free"))
	m.limit("kept", 20)
	m.setUsage("kept", monthlyUsage(85))
	m.mu.Lock()
	next, downgrade, err := m.failoverLocked("x", "codex")
	m.mu.Unlock()
	if err != nil || next != "free" || !downgrade {
		t.Fatalf("failover: %q %v %v", next, downgrade, err)
	}
	m.mu.Lock()
	m.exhausted["free"] = time.Now().Add(time.Hour)
	next, downgrade, err = m.failoverLocked("x", "codex")
	m.mu.Unlock()
	// A banked reset on x comes first: it spares the share kept.
	if err != nil || next != "kept" || !downgrade {
		t.Fatalf("last resort: %q %v %v", next, downgrade, err)
	}
}

// Picking an account at its limit yourself holds it, as picking one that ran
// out does.
func TestAnAccountPickedAtItsUsageLimitStaysPicked(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	native.status.ActiveID = "b"
	m.limit("a", 10)
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 0))
	if _, err := m.ActivateForClient(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.status.ActiveID != "a" {
		t.Fatalf("switched %v; the user's pick did not hold", native.switched)
	}
}

// A paired Mac's Desktop request for an account at its limit goes to the one
// with the most room, with switching on running out turned off too.
func TestAPairedMacsRequestAvoidsAnAccountAtItsLimit(t *testing.T) {
	m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 10))
	if got := m.ClaudeRoute("a"); got != "a" {
		t.Fatalf("without a limit: %q", got)
	}
	m.limit("a", 10)
	if got := m.ClaudeRoute("a"); got != "b" {
		t.Fatalf("at its limit: %q", got)
	}
}

// A limit is saved with the routing state, survives a restart, goes with
// its account, and stays within range.
func TestLimitsPersistAndGoWithTheirAccount(t *testing.T) {
	dir := t.TempDir()
	st := store.New(dir)
	for _, a := range []store.Account{claudeAccount("a"), claudeAccount("b")} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	providers := map[string]provider.Provider{"claude": &fakeProvider{id: "claude"}}
	m, err := New(st, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetUsageLimit("a", 15); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int{-1, 96, 100} {
		if err := m.SetUsageLimit("a", bad); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("limit %d: %v", bad, err)
		}
	}
	if err := m.SetUsageLimit("missing", 10); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("limit on a missing account: %v", err)
	}
	again, err := New(st, providers, nil)
	if err != nil || again.UsageLimit("a") != 15 {
		t.Fatalf("after a restart: %d %v", again.UsageLimit("a"), err)
	}
	if err := again.SetUsageLimit("a", 0); err != nil || again.UsageLimit("a") != 0 {
		t.Fatalf("off: %v", err)
	}
	if err := again.SetUsageLimit("b", 25); err != nil {
		t.Fatal(err)
	}
	if err := again.DeleteAccount("b"); err != nil {
		t.Fatal(err)
	}
	state, _ := st.LoadState()
	if len(state.UsageLimits) != 0 || len(again.UsageLimits()) != 0 {
		t.Fatalf("limits left behind: %v %v", state.UsageLimits, again.UsageLimits())
	}
	// A hand-edited state keeps only what a limit can be.
	if got := validLimits(map[string]int{"a": 10, "b": 0, "c": 96, "": 5, "d": -2}); len(got) != 1 || got["a"] != 10 {
		t.Fatalf("validLimits: %v", got)
	}
}

// Setting a limit the account already passed moves away from it right away,
// without waiting for the next usage poll; even a round already running
// picks it up.
func TestSettingALimitSwitchesAtOnce(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	m.setUsage("a", windows(0, 80))
	m.setUsage("b", windows(0, 0))
	m.autoSwitching.Store(true) // a round is running
	if err := m.SetUsageLimit("a", 25); err != nil {
		t.Fatal(err)
	}
	for wait := time.Now().Add(5 * time.Second); !m.autoSwitchAgain.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(wait) {
			t.Fatal("the running round was not asked to go again")
		}
	}
	// The running round ends: it finds the work left to it.
	m.autoSwitching.Store(false)
	m.AutoSwitchNative()
	deadline := time.Now().Add(5 * time.Second)
	for {
		native.mu.Lock()
		active := native.status.ActiveID
		native.mu.Unlock()
		if active == "b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still on %q", active)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Saving the limit an account already has changes nothing: an account the
// user picked while at its limit stays picked.
func TestSavingTheSameLimitKeepsTheUsersPick(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	native.status.ActiveID = "b"
	if err := m.SetUsageLimit("a", 10); err != nil {
		t.Fatal(err)
	}
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 0))
	if _, err := m.ActivateForClient(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUsageLimit("a", 10); err != nil {
		t.Fatal(err)
	}
	m.AutoSwitchNative()
	if native.status.ActiveID != "a" {
		t.Fatalf("saving the same limit undid the pick: %v", native.switched)
	}
}

// With no Claude Code login Switcher can move (signed out, or one it does
// not manage), a limit still moves the selection, which other tools use,
// and leaves the login alone.
func TestALimitMovesTheSelectionWithoutAUsableLogin(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	native.status = claudecode.Status{Condition: "logged_out"}
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	m.limit("a", 10)
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 0))
	m.AutoSwitchNative()
	if m.ActiveID("claude") != "b" || len(native.switched) != 0 {
		t.Fatalf("route %q, login switched %v", m.ActiveID("claude"), native.switched)
	}
	if event := m.LastAutoSwitch(); event == nil || event.Where != "proxy" || event.Reason != "limit" {
		t.Fatalf("event %+v", event)
	}
}

// Moving the selection while Claude Code's login was not ready leaves no
// cooldown: once the login is ready on the limited account, it moves at once.
func TestTheLoginFollowsAtOnceAfterASelectionMove(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	native.status = claudecode.Status{Condition: "checking", ActiveID: "a"}
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	m.limit("a", 10)
	m.setUsage("a", windows(0, 95))
	m.setUsage("b", windows(0, 0))
	m.AutoSwitchNative()
	if m.ActiveID("claude") != "b" || len(native.switched) != 0 {
		t.Fatalf("route %q, login switched %v", m.ActiveID("claude"), native.switched)
	}
	native.mu.Lock()
	native.status.Condition = "ready"
	native.mu.Unlock()
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "b" {
		t.Fatalf("the login stayed on the limited account: %v", native.switched)
	}
}

// An automatic switch never lands on an account that ran out meanwhile, and
// never clears its out-of-usage mark the way a pick by the user does.
func TestAnAutomaticSwitchKeepsAnExhaustionMark(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("y", "plus"))
	m.limit("x", 10)
	m.setUsage("x", monthlyUsage(95))
	m.setUsage("y", monthlyUsage(0))
	err := m.selectUnder("codex", "y", func() bool {
		m.exhausted["y"] = time.Now().Add(time.Hour) // a refusal arrived meanwhile
		return true
	})
	if err != nil || !m.exhaustedNow("y") {
		t.Fatalf("the switch cleared y's mark: %v", err)
	}
	if err := m.Activate("x"); err != nil {
		t.Fatal(err)
	}
	m.AutoSwitchNative()
	if m.ActiveID("codex") != "x" {
		t.Fatalf("moved onto an exhausted account: %q", m.ActiveID("codex"))
	}
}
