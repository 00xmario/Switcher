package proxy

import (
	"context"
	"testing"

	"switcher/internal/claudecode"
)

func routed(m *Manager) { m.SetClaudeCodeRouted(func() bool { return true }) }

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

func TestAKeptLoginStandsInUntilTheProxyHasAnAccount(t *testing.T) {
	m, _ := autoManager(t, false, claudeAccount("a"), claudeAccount("b"))
	routed(m)
	m.mu.Lock()
	delete(m.active, "claude")
	m.mu.Unlock()
	if got := m.SelectedID("claude"); got != "a" {
		t.Fatalf("selected %q, want the login's a", got)
	}
}

func TestARoutedClaudeCodeMovesItsSelectionWhenItRunsOut(t *testing.T) {
	// b has no identity for a login to carry; the relay needs none.
	b := claudeAccount("b")
	b.Token.AccountID = ""
	m, native := autoManager(t, true, claudeAccount("a"), b)
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
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	m.setUsage("a", windows(100, 40))
	m.setUsage("b", windows(10, 20))
	m.AutoSwitchNative()
	if len(native.switched) != 0 || m.ActiveID("claude") != "b" {
		t.Fatalf("switched %v, selection %q", native.switched, m.ActiveID("claude"))
	}
}
