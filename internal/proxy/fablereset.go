package proxy

import (
	"context"
	"errors"
	"log"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Automatic Fable resets: a Claude account can spend its banked Fable resets
// by itself (store.Account.AutoFableReset, off unless the user turns it on
// for that account) once a usage poll shows Fable's own window, the weekly
// window, or either, used up. A Fable reset gives back part of the weekly
// usage as well. It runs after every usage poll, before the automatic switch,
// so it works whichever way the account is used (Claude Code's own login,
// Claude Desktop, a paired Mac or the proxy), and an account it refilled is
// not switched away from.

// Settings for AutoFableReset.
const (
	FableResetOff    = ""
	FableResetFable  = "fable"
	FableResetWeekly = "weekly"
	FableResetBoth   = "both"
)

// fableResetKind is the kind Claude's provider gives a Fable reset.
const fableResetKind = "Fable reset"

// fableResetIdle is how long an account waits before Switcher asks again
// after it found no Fable reset it could spend.
const fableResetIdle = 15 * time.Minute

// ValidFableReset reports whether s is a setting for AutoFableReset.
func ValidFableReset(s string) bool {
	switch s {
	case FableResetOff, FableResetFable, FableResetWeekly, FableResetBoth:
		return true
	}
	return false
}

// fableResetDue reports whether usage shows a limit the setting names used up.
func fableResetDue(setting string, u provider.Usage) bool {
	fable, weekly := false, false
	for _, w := range u.Windows {
		if w.UsedPercent < 100 {
			continue
		}
		switch {
		case provider.FableWindow(w.Label):
			fable = true
		case w.Label == "Weekly":
			weekly = true
		}
	}
	switch setting {
	case FableResetFable:
		return fable
	case FableResetWeekly:
		return weekly
	case FableResetBoth:
		return fable || weekly
	}
	return false
}

// chooseFableReset names the first Fable reset Claude says can be used now.
func chooseFableReset(credits []provider.ResetCredit) string {
	for _, c := range credits {
		if c.Kind == fableResetKind && c.Unusable == "" {
			return c.ID
		}
	}
	return ""
}

// CheckFableResets spends the Fable resets the latest usage calls for, then
// moves off any account still out of usage: right after a setting changes.
func (m *Manager) CheckFableResets() {
	m.autoFableResets()
	m.AutoSwitchNative()
}

// autoFableResets spends a Fable reset on each account that asks for one and
// whose latest usage shows the limit it names used up, at most once per
// cooldown per account.
func (m *Manager) autoFableResets() {
	if m.HandedBack() {
		return
	}
	accounts, err := m.store.List()
	if err != nil {
		return
	}
	for _, a := range accounts {
		if a.AutoFableReset == FableResetOff {
			continue
		}
		if _, ok := m.providers[a.Provider].(provider.ResetCreditProvider); !ok {
			continue
		}
		m.autoFableReset(a)
	}
}

func (m *Manager) autoFableReset(a store.Account) {
	m.mu.Lock()
	usage, ok := m.cachedUsageLocked(a.ID)
	wait := m.autoResetAt[a.ID]
	due := ok && usage.Available && fableResetDue(a.AutoFableReset, usage)
	// After a spend, the next waits until usage shows the account below the
	// limit again: a reset that did not clear it is not followed by the rest.
	if ok && usage.Available && !due {
		delete(m.fableSpent, a.ID)
	}
	spent := m.fableSpent[a.ID]
	m.mu.Unlock()
	if !due || spent || time.Now().Before(wait) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	outcome, err := m.useBankedReset(ctx, a.ID, "", chooseFableReset)
	switch {
	case err == nil && (outcome == "reset" || outcome == "already_redeemed"):
		log.Printf("proxy: auto-used a Fable reset for %s", a.Email)
		m.mu.Lock()
		if m.fableSpent == nil {
			m.fableSpent = map[string]bool{}
		}
		m.fableSpent[a.ID] = true
		m.mu.Unlock()
		return
	case errors.Is(err, ErrResetPending):
		return
	case errors.Is(err, ErrNoResetCredits):
		m.holdAutoReset(a.ID, fableResetIdle)
	default:
		if err != nil {
			log.Printf("proxy: auto-use Fable reset for %s: %v", a.Email, err)
		} else {
			log.Printf("proxy: Fable reset for %s did not clear the limit (%s)", a.Email, outcome)
		}
		m.holdAutoReset(a.ID, autoUseResetCooldown)
	}
}

func (m *Manager) holdAutoReset(id string, d time.Duration) {
	m.mu.Lock()
	m.autoResetAt[id] = time.Now().Add(d)
	m.mu.Unlock()
}
