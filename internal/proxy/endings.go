package proxy

import (
	"errors"
	"time"
)

// Ending subscriptions: an account can carry the day its subscription ends,
// such as a Claude subscription canceled for November 29, which no provider
// tells Switcher. Until that day is over Switcher uses the account first, so
// the usage paid for is not lost:
//
//   - an automatic pick (takeover, after an account runs out or reaches its
//     usage limit, and the proxy's failover) takes the paid account that
//     ends soonest over one with more usage left;
//   - after a usage poll the provider moves onto that account whenever it
//     has room (pullHeadroom), unless the account in use ends no later or is
//     the one the user picked last.
//
// An end date never blocks a request: an account past it, out of usage or
// signed out is passed over like any other.

// pullHeadroom is the share an ending account needs left in each of its
// windows for the provider to move onto it, so a move is worth its while.
const pullHeadroom = 10

// maxEndAhead bounds how far ahead an end date can be.
const maxEndAhead = 10 * 366 * 24 * time.Hour

// ErrInvalidEndDate rejects an end date that is not a day from today on.
var ErrInvalidEndDate = errors.New("an end date is a day (YYYY-MM-DD) from today to ten years ahead, or empty for none")

// parseEndDate reads a day, YYYY-MM-DD, in local time.
func parseEndDate(day string) (time.Time, bool) {
	t, err := time.ParseInLocation(time.DateOnly, day, time.Local)
	if err != nil || t.Year() < 2000 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t, true
}

// endOf is the moment an end date is over: the start of the next day.
func endOf(day string) (time.Time, bool) {
	t, ok := parseEndDate(day)
	if !ok {
		return time.Time{}, false
	}
	return t.AddDate(0, 0, 1), true
}

// CheckEndDate reports whether a day can be set as an end date now: empty
// (none), or a day from today to ten years ahead.
func CheckEndDate(day string) error {
	if day == "" {
		return nil
	}
	start, ok := parseEndDate(day)
	end, _ := endOf(day)
	if !ok || !time.Now().Before(end) || start.After(time.Now().Add(maxEndAhead)) {
		return ErrInvalidEndDate
	}
	return nil
}

// validEndDates keeps the stored end dates that are days.
func validEndDates(stored map[string]string) map[string]string {
	dates := make(map[string]string, len(stored))
	for id, day := range stored {
		if _, ok := parseEndDate(day); id != "" && ok {
			dates[id] = day
		}
	}
	return dates
}

// EndDate is the day an account's subscription ends, or "" for none.
func (m *Manager) EndDate(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endDates[id]
}

// Ending reports whether an account has an end date that is not over yet,
// so Switcher uses it first.
func (m *Manager) Ending(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.endingLocked(id)
	return ok
}

// SetEndDate stores the day an account's subscription ends; "" removes it.
// It is saved before it applies; a new date then moves the provider onto the
// account right away if it should be used first (in the background), even
// away from an account the user picked: setting the date is that choice.
func (m *Manager) SetEndDate(id, day string) error {
	if err := CheckEndDate(day); err != nil {
		return err
	}
	m.mu.Lock()
	account, err := m.store.Get(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	changed := m.endDates[id] != day
	if changed {
		err = m.mutateStateLocked(func() {
			if m.endDates == nil {
				m.endDates = map[string]string{}
			}
			if day == "" {
				delete(m.endDates, id)
			} else {
				m.endDates[id] = day
			}
		})
		if err == nil && day != "" {
			delete(m.handPicked, account.Provider)
			delete(m.autoSwitchAt, id)
		}
	}
	m.mu.Unlock()
	if err == nil && changed && day != "" {
		go m.AutoSwitchNative()
	}
	return err
}

// endingLocked is when an account's subscription ends, if it has an end date
// that is not over yet. Requires m.mu.
func (m *Manager) endingLocked(id string) (time.Time, bool) {
	end, ok := endOf(m.endDates[id])
	if !ok || !time.Now().Before(end) {
		return time.Time{}, false
	}
	return end, true
}

// endsSooner reports whether an account ending at a (aEnds false: no end) is
// used before one ending at b.
func endsSooner(a time.Time, aEnds bool, b time.Time, bEnds bool) bool {
	return aEnds && (!bEnds || a.Before(b))
}

// pullLocked is the account to move providerID onto, away from from, to use
// up an ending subscription, or "": the paid account that ends soonest, has
// pullHeadroom left and was not moved away from within the cooldown, when it
// ends before from does and the user did not pick from last. native asks for
// an account whose identity Claude Code's login can carry. Requires m.mu.
func (m *Manager) pullLocked(providerID, from string, native bool) (string, error) {
	if from == "" || m.handPicked[providerID] == from {
		return "", nil
	}
	accounts, err := m.store.List()
	if err != nil {
		return "", err
	}
	fromEnd, fromEnds := m.endingLocked(from)
	now := time.Now()
	best, bestEnd := "", time.Time{}
	for _, a := range accounts {
		if a.Provider != providerID || a.ID == from || native && a.Token.AccountID == "" || freePlan(a.Plan) {
			continue
		}
		end, ok := m.endingLocked(a.ID)
		if !ok || !endsSooner(end, true, fromEnd, fromEnds) || best != "" && !end.Before(bestEnd) {
			continue
		}
		if now.Before(m.autoSwitchAt[a.ID]) || m.headroomLocked(a) < pullHeadroom {
			continue
		}
		best, bestEnd = a.ID, end
	}
	return best, nil
}

// pickedByHand records the account the user picked for its provider: the
// provider stays on it until it should be left, instead of moving onto an
// ending account. Requires m.mu.
func (m *Manager) pickedByHandLocked(providerID, id string) {
	if m.handPicked == nil {
		m.handPicked = map[string]string{}
	}
	m.handPicked[providerID] = id
}

// leftAutomaticallyLocked forgets the user's pick once Switcher moved the
// provider off it (it ran out or reached its limit). Requires m.mu.
func (m *Manager) leftAutomaticallyLocked(providerID, from string) {
	if m.handPicked[providerID] == from {
		delete(m.handPicked, providerID)
	}
}
