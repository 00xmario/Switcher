package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"switcher/internal/store"
)

// day is the local day n days from today, as an end date.
func day(n int) string { return time.Now().AddDate(0, 0, n).Format(time.DateOnly) }

// end sets an end date without the background round SetEndDate starts, so a
// test decides when the switch runs.
func (m *Manager) end(id, date string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endDates[id] = date
}

// The provider moves onto an account whose subscription ends, once it has
// room, says why, and then stays there.
func TestTheProviderMovesOntoAnEndingAccount(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	m.setUsage("a", windows(0, 10))
	m.setUsage("b", windows(0, 60))
	m.end("b", day(7))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "b" || m.ActiveID("claude") != "b" {
		t.Fatalf("switched %v, route %q", native.switched, m.ActiveID("claude"))
	}
	if event := m.LastAutoSwitch(); event == nil || event.From != "a" || event.To != "b" || event.Reason != "ending" || event.Where != "claude_code" {
		t.Fatalf("event %+v", event)
	}
	m.AutoSwitchNative()
	if len(native.switched) != 1 {
		t.Fatalf("moved again: %v", native.switched)
	}
}

// Without room worth a move, at its usage limit, on a Free plan or past its
// day, an ending account is not moved onto.
func TestAnEndingAccountNeedsRoom(t *testing.T) {
	for name, setup := range map[string]func(m *Manager){
		"little room":   func(m *Manager) { m.setUsage("b", windows(95, 20)) },
		"at its limit":  func(m *Manager) { m.limit("b", 30); m.setUsage("b", windows(0, 75)) },
		"usage unknown": func(m *Manager) {},
		"over":          func(m *Manager) { m.end("b", day(-1)); m.setUsage("b", windows(0, 0)) },
	} {
		t.Run(name, func(t *testing.T) {
			m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
			m.setUsage("a", windows(0, 10))
			m.end("b", day(7))
			setup(m)
			m.AutoSwitchNative()
			if len(native.switched) != 0 {
				t.Fatalf("moved onto b: %v", native.switched)
			}
		})
	}
	free := claudeAccount("b")
	free.Plan = "free"
	m, native := autoManager(t, false, claudeAccount("a"), free)
	m.setUsage("a", windows(0, 10))
	m.setUsage("b", windows(0, 0))
	m.end("b", day(7))
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("moved onto a Free account: %v", native.switched)
	}
}

// The account that ends soonest goes first; one that ends no sooner than the
// account in use is not moved onto.
func TestTheSoonestEndGoesFirst(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	for _, id := range []string{"a", "b", "c"} {
		m.setUsage(id, windows(0, 10))
	}
	m.end("a", day(5))
	m.end("b", day(5))
	m.end("c", day(9))
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("moved off an account that ends as soon: %v", native.switched)
	}
	m.end("b", day(2))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "b" {
		t.Fatalf("switched %v, want b", native.switched)
	}
}

// An account the user picks stays in use; once Switcher moves off it because
// it ran out, the ending account goes first again.
func TestAHandPickIsNotMovedOff(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	m.end("b", day(7))
	if _, err := m.ActivateForClient(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	native.switched = nil
	// A switch forgets what usage was seen; the next poll brings it back.
	m.setUsage("a", windows(0, 10))
	m.setUsage("b", windows(0, 60))
	m.setUsage("c", windows(0, 0))
	m.AutoSwitchNative()
	if len(native.switched) != 0 {
		t.Fatalf("moved off a hand pick: %v", native.switched)
	}
	// a runs out: the ending account comes before c, which has more left.
	m.setUsage("a", windows(100, 10))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "b" {
		t.Fatalf("switched %v, want b", native.switched)
	}
	m.mu.Lock()
	picked := m.handPicked["claude"]
	m.mu.Unlock()
	if picked != "" {
		t.Fatalf("the hand pick outlived the move off it: %q", picked)
	}
}

// Setting a date is a choice too: it moves the provider even off a hand pick.
func TestSettingADateMovesOffAHandPick(t *testing.T) {
	m, native := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	if _, err := m.ActivateForClient(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	native.switched = nil
	m.setUsage("a", windows(0, 10))
	m.setUsage("b", windows(0, 60))
	if err := m.SetEndDate("b", day(3)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.ActiveID("claude") != "b" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.ActiveID("claude") != "b" {
		t.Fatalf("route %q after setting b's end date", m.ActiveID("claude"))
	}
}

// Codex has no native login: the proxy's selection moves onto the ending
// account, and a refused request fails over to it first.
func TestCodexUsesAnEndingAccountFirst(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("y", "plus"), codexAccount("z", "plus"))
	m.setUsage("x", monthlyUsage(10))
	m.setUsage("y", monthlyUsage(10))
	m.setUsage("z", monthlyUsage(50))
	m.end("z", day(4))
	m.mu.Lock()
	next, downgrade, err := m.failoverLocked("x", "codex")
	m.mu.Unlock()
	if err != nil || next != "z" || downgrade {
		t.Fatalf("failover: %q %v %v", next, downgrade, err)
	}
	m.AutoSwitchNative()
	if got := m.ActiveID("codex"); got != "z" {
		t.Fatalf("codex uses %q, want z", got)
	}
	if event := m.LastAutoSwitch(); event == nil || event.Where != "proxy" || event.Reason != "ending" {
		t.Fatalf("event %+v", event)
	}
}

// An end date is a day from today on; it survives a restart, can be removed,
// and goes with the account.
func TestEndDateRoundTrip(t *testing.T) {
	m := codexManager(t, codexAccount("x", "plus"), codexAccount("y", "plus"))
	for _, bad := range []string{day(-1), "2026-13-01", "29-11-2026", "tomorrow", time.Now().AddDate(11, 0, 0).Format(time.DateOnly)} {
		if err := m.SetEndDate("x", bad); !errors.Is(err, ErrInvalidEndDate) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if err := m.SetEndDate("missing", day(3)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if err := m.SetEndDate("y", day(0)); err != nil || !m.Ending("y") {
		t.Fatalf("today: %v %v", err, m.Ending("y"))
	}
	again, err := New(m.store, m.providers, []string{"codex"})
	if err != nil || again.EndDate("y") != day(0) {
		t.Fatalf("after a restart: %q %v", again.EndDate("y"), err)
	}
	if err := again.SetEndDate("y", ""); err != nil || again.EndDate("y") != "" || again.Ending("y") {
		t.Fatalf("removed: %v %q", err, again.EndDate("y"))
	}
	if err := again.SetEndDate("x", day(30)); err != nil {
		t.Fatal(err)
	}
	if err := again.DeleteAccount("x"); err != nil {
		t.Fatal(err)
	}
	state, _ := again.store.LoadState()
	if len(state.EndDates) != 0 {
		t.Fatalf("end dates left behind: %v", state.EndDates)
	}
}
