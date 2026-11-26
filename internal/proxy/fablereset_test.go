package proxy

import (
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func fableManager(t *testing.T, setting string, credits ...provider.ResetCredit) (*Manager, *fakeCreditProvider) {
	t.Helper()
	st := store.New(t.TempDir())
	a := claudeAccount("a")
	a.Token.ExpiresAt = time.Now().Add(time.Hour).Unix()
	a.AutoFableReset = setting
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	prov := &fakeCreditProvider{fakeProvider: &fakeProvider{id: "claude"}, outcome: "reset", credits: credits}
	m, err := New(st, map[string]provider.Provider{"claude": prov}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, prov
}

func fableCredits() []provider.ResetCredit {
	later := time.Now().Add(72 * time.Hour).Unix()
	return []provider.ResetCredit{
		{ID: "weekly-1", Kind: "Weekly reset", ExpiresAt: later},
		{ID: "fable-blocked", Kind: "Fable reset", ExpiresAt: later, Unusable: "Cooling down, try again later"},
		{ID: "fable-1", Kind: "Fable reset", ExpiresAt: later},
	}
}

// Each setting spends a Fable reset, never another kind and never one Claude
// says cannot be used now, only once the limit it names is used up.
func TestAutoFableResetSpendsOnlyAtTheLimitItNames(t *testing.T) {
	for _, tc := range []struct {
		setting       string
		fable, weekly int
		spend         bool
	}{
		{"", 100, 100, false},
		{"fable", 100, 40, true},
		{"fable", 50, 100, false},
		{"weekly", 100, 40, false},
		{"weekly", 50, 100, true},
		{"both", 100, 40, true},
		{"both", 50, 100, true},
		{"both", 99, 99, false},
	} {
		m, prov := fableManager(t, tc.setting, fableCredits()...)
		m.setUsage("a", withModel(windows(20, tc.weekly), "Fable", tc.fable))
		m.autoFableResets()
		prov.mu.Lock()
		consumed := append([]string(nil), prov.consumed...)
		prov.mu.Unlock()
		if !tc.spend {
			if len(consumed) != 0 {
				t.Errorf("%q fable=%d weekly=%d: spent %v", tc.setting, tc.fable, tc.weekly, consumed)
			}
			continue
		}
		if len(consumed) != 1 || consumed[0] != "fable-1" {
			t.Errorf("%q fable=%d weekly=%d: spent %v, want fable-1", tc.setting, tc.fable, tc.weekly, consumed)
			continue
		}
		if event := m.LastReset("a"); event == nil || !event.Automatic {
			t.Errorf("%q: reset event %+v", tc.setting, event)
		}
	}
}

// One reset at a time: the next poll does not spend another while the first
// is fresh, and an account with none to spend is not asked every poll.
func TestAutoFableResetWaitsBetweenResets(t *testing.T) {
	m, prov := fableManager(t, "fable", fableCredits()...)
	m.setUsage("a", withModel(windows(20, 40), "Fable", 100))
	m.autoFableResets()
	m.setUsage("a", withModel(windows(20, 40), "Fable", 100))
	m.autoFableResets()
	if n := prov.spends(); n != 1 {
		t.Fatalf("spent %d resets, want 1", n)
	}

	m, prov = fableManager(t, "both")
	m.setUsage("a", withModel(windows(20, 100), "Fable", 100))
	m.autoFableResets()
	m.autoFableResets()
	prov.mu.Lock()
	lists := prov.listCalls
	prov.mu.Unlock()
	if lists != 1 || prov.spends() != 0 {
		t.Fatalf("with no Fable reset: %d lists, %d spends", lists, prov.spends())
	}
}

// An account whose Fable reset was just spent is not switched away from
// before usage shows the refill.
func TestAFreshFableResetKeepsTheAccount(t *testing.T) {
	m, _ := fableManager(t, "fable", fableCredits()...)
	m.setUsage("a", withModel(windows(20, 40), "Fable", 100))
	m.mu.Lock()
	out := m.outOfUsageLocked("a")
	m.mu.Unlock()
	if !out {
		t.Fatal("Fable used up is not out of usage")
	}
	m.mu.Lock()
	m.resetEvents["a"] = ResetEvent{ID: "x", Pending: true}
	out = m.outOfUsageLocked("a")
	m.mu.Unlock()
	if out {
		t.Fatal("left an account whose reset is still confirming")
	}
}

// A reset that did not clear the limit is not followed by another, even
// after the cooldown; once usage shows the account below the limit, the next
// time it is used up spends one again.
func TestAutoFableResetNeverDrainsOnALimitItDidNotClear(t *testing.T) {
	m, prov := fableManager(t, "fable", append(fableCredits(), provider.ResetCredit{ID: "fable-2", Kind: "Fable reset", ExpiresAt: time.Now().Add(96 * time.Hour).Unix()})...)
	spentOut := withModel(windows(20, 40), "Fable", 100)
	m.setUsage("a", spentOut)
	m.autoFableResets()
	clearCooldown := func() {
		// Let the refresh after a reset finish first: it replaces usage.
		for i := 0; i < 200; i++ {
			if e := m.LastReset("a"); e == nil || !e.Pending {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		m.mu.Lock()
		m.autoResetAt["a"] = time.Time{}
		m.resetEvents["a"] = ResetEvent{ID: "done"}
		m.mu.Unlock()
	}
	clearCooldown()
	m.setUsage("a", spentOut)
	m.autoFableResets()
	if n := prov.spends(); n != 1 {
		t.Fatalf("spent %d resets on a limit the first did not clear", n)
	}
	clearCooldown()
	m.setUsage("a", withModel(windows(20, 40), "Fable", 30))
	m.autoFableResets()
	m.setUsage("a", spentOut)
	m.autoFableResets()
	if n := prov.spends(); n != 2 {
		t.Fatalf("spent %d resets, want 2 after the limit came back", n)
	}
}
