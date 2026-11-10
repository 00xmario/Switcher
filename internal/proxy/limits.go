package proxy

import (
	"errors"
	"maps"
)

// Usage limits: an account can keep a share of its weekly or monthly usage
// for what Switcher cannot route, such as the Claude and ChatGPT chat apps.
// Once its usage shows that much or less left, Switcher moves the provider
// to another account (AutoSwitchNative), never moves onto it automatically
// (takeover and failover), and moves back only when you pick it. With
// nowhere else to go the account keeps serving: a limit never blocks a
// request.

// MaxUsageLimit is the largest share an account can keep. A limit is the
// share left at which Switcher moves away, so 100 would mean never using it.
const MaxUsageLimit = 95

// ErrInvalidLimit rejects a limit outside 0 (none) to MaxUsageLimit.
var ErrInvalidLimit = errors.New("a usage limit is a share between 1 and 95 percent, or 0 for none")

// limitWindow reports whether a usage window counts toward a limit: the
// account's long windows. A session window refills within hours and a
// model's own window does not stand for the whole account.
func limitWindow(label string) bool { return label == "Weekly" || label == "Monthly" }

// validLimits keeps the stored limits that are in range.
func validLimits(stored map[string]int) map[string]int {
	limits := make(map[string]int, len(stored))
	for id, limit := range stored {
		if id != "" && limit > 0 && limit <= MaxUsageLimit {
			limits[id] = limit
		}
	}
	return limits
}

// UsageLimit is the share an account keeps, or 0 for none.
func (m *Manager) UsageLimit(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits[id]
}

// UsageLimits returns every account's limit.
func (m *Manager) UsageLimits() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.limits)
}

// AtUsageLimit reports whether an account's latest usage shows it at its
// limit.
func (m *Manager) AtUsageLimit(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.atLimitLocked(id)
}

// SetUsageLimit stores the share of an account's usage to keep; 0 removes
// the limit. It is saved before it applies, and an account already at its
// new limit is moved away from right after (in the background).
func (m *Manager) SetUsageLimit(id string, limit int) error {
	if limit < 0 || limit > MaxUsageLimit {
		return ErrInvalidLimit
	}
	if _, err := m.store.Get(id); err != nil {
		return err
	}
	m.mu.Lock()
	err := m.mutateStateLocked(func() {
		if m.limits == nil {
			m.limits = map[string]int{}
		}
		if limit == 0 {
			delete(m.limits, id)
		} else {
			m.limits[id] = limit
		}
		// A new limit is a new decision: the cooldown of an earlier switch
		// or a hold from picking the account does not delay it.
		delete(m.autoSwitchAt, id)
		delete(m.manualHold, id)
	})
	m.mu.Unlock()
	if err == nil && limit > 0 {
		go m.AutoSwitchNative()
	}
	return err
}

// atLimitLocked reports whether an account has a limit and its latest usage
// shows that share or less left in a weekly or monthly window. Requires m.mu.
func (m *Manager) atLimitLocked(id string) bool {
	limit := m.limits[id]
	if limit <= 0 {
		return false
	}
	u, ok := m.cachedUsageLocked(id)
	if !ok {
		return false
	}
	for _, w := range u.Windows {
		if limitWindow(w.Label) && 100-w.UsedPercent <= limit {
			return true
		}
	}
	return false
}
