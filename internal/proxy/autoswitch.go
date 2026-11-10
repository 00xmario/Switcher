package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Claude moves off an account that runs out of usage. Claude Code's own login
// (or, with the login kept, the selected account its requests use) switches
// after a usage poll shows its account at the limit; a Claude Desktop
// conversation moves when Anthropic refuses one of its requests for that
// reason (desktop_credentials.go). Both take the account with the most usage
// left, and both follow the "Switch Claude automatically" setting.

// autoSwitchCooldown keeps an account from being switched away from twice in
// quick succession, so a provider whose usage lags cannot make Switcher flip
// back and forth. A switch that did not go through, for instance because
// Claude Code held its lock, is tried again after autoSwitchRetry.
const (
	autoSwitchCooldown = 10 * time.Minute
	autoSwitchRetry    = time.Minute
	// takeoverFreshness is how recent an account's last successful usage
	// poll must be for it to take over: an account Switcher has not heard
	// from lately may need a new sign-in or have run out meanwhile.
	takeoverFreshness = 15 * time.Minute
	// manualHold keeps an account the user picked while it was at its limit,
	// for example one with extra usage, from being switched away from.
	manualHold = time.Hour
)

var errSwitchOvertaken = errors.New("the switch was overtaken")

// AutoSwitch is the latest automatic switch, for the dashboard.
type AutoSwitch struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Where    string `json:"where"` // "claude_code", "desktop" or "proxy"
	From     string `json:"from"`
	To       string `json:"to"`
	At       int64  `json:"at"`
	// Reason is "limit" when the account reached its usage limit, and empty
	// when it ran out of usage.
	Reason string `json:"reason,omitempty"`
}

// SetClaudeAutoSwitch wires the user's choice; nil leaves the feature off.
func (m *Manager) SetClaudeAutoSwitch(on func() bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claudeAuto = on
}

func (m *Manager) claudeAutoOn() bool {
	m.mu.Lock()
	on := m.claudeAuto
	m.mu.Unlock()
	return on != nil && on()
}

// LastAutoSwitch returns the latest automatic switch, if any.
func (m *Manager) LastAutoSwitch() *AutoSwitch {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastAutoSwitch.ID == "" {
		return nil
	}
	event := m.lastAutoSwitch
	return &event
}

func (m *Manager) recordAutoSwitchLocked(providerID, where, from, to string) {
	m.autoSwitchSerial++
	m.lastAutoSwitch = AutoSwitch{ID: fmt.Sprintf("%s-%d", m.probeBootID, m.autoSwitchSerial), Provider: providerID,
		Where: where, From: from, To: to, At: time.Now().Unix()}
}

// outOfUsageLocked reports whether an account has run out: parked after a
// refused request, or its latest usage shows the session or weekly window at
// the limit. A model's own window does not make the whole account run out.
// Requires m.mu.
func (m *Manager) outOfUsageLocked(id string) bool {
	if m.exhaustedNow(id) {
		return true
	}
	u, ok := m.cachedUsageLocked(id)
	if !ok {
		return false
	}
	for _, w := range u.Windows {
		if (w.Label == "Session" || w.Label == "Weekly") && w.UsedPercent >= 100 {
			return true
		}
	}
	return false
}

// headroomLocked is the share left in an account's tightest account-wide
// window (session, weekly or monthly), less the share its usage limit keeps,
// or -1 when it cannot take over: no credentials, parked, needing a new
// sign-in, at its limit, or with usage Switcher has not seen. Requires m.mu.
func (m *Manager) headroomLocked(a store.Account) int {
	if !hasCredentials(a) || m.exhaustedNow(a.ID) {
		return -1
	}
	if h := m.health[a.ID]; h == nil || h.relogin || h.lastSuccess.IsZero() || time.Since(h.lastSuccess) > takeoverFreshness {
		return -1
	}
	u, ok := m.cachedUsageLocked(a.ID)
	if !ok || !u.Available {
		return -1
	}
	limit := m.limits[a.ID]
	left, seen := 100, false
	for _, w := range u.Windows {
		if w.Label != "Session" && !limitWindow(w.Label) {
			continue
		}
		seen = true
		room := 100 - w.UsedPercent
		if limitWindow(w.Label) {
			room -= limit
		}
		left = min(left, room)
	}
	if !seen || left <= 0 {
		return -1
	}
	return left
}

// takeoverLocked picks the account of providerID with the most usage left,
// other than exclude, or "" when none has room. A paid account comes before
// a Free one, which lacks the paid models. native asks for an account whose
// identity Claude Code's login can carry. Requires m.mu.
func (m *Manager) takeoverLocked(providerID, exclude string, native bool) (string, error) {
	accounts, err := m.store.List()
	if err != nil {
		return "", err
	}
	best, bestLeft, bestPaid := "", 0, false
	for _, a := range accounts {
		if a.Provider != providerID || a.ID == exclude || (native && a.Token.AccountID == "") {
			continue
		}
		left := m.headroomLocked(a)
		if left <= 0 {
			continue
		}
		paid := !freePlan(a.Plan)
		if best == "" || paid && !bestPaid || paid == bestPaid && left > bestLeft {
			best, bestLeft, bestPaid = a.ID, left, paid
		}
	}
	return best, nil
}

// leaveLocked reports whether an account should be moved away from: it is
// at its usage limit, or, with Claude's automatic switching on (auto), out
// of usage. Requires m.mu.
func (m *Manager) leaveLocked(id string, auto bool) bool {
	return m.atLimitLocked(id) || auto && m.outOfUsageLocked(id)
}

// ClaudeRoute is the Claude account a paired Mac's Desktop request should use
// on this host: the one it asked for, unless that account is at its usage
// limit, or has run out of usage and Claude switches automatically; then the
// one with the most usage left.
func (m *Manager) ClaudeRoute(account string) string {
	if account == "" {
		return account
	}
	auto := m.claudeAutoOn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.leaveLocked(account, auto) {
		delete(m.claudeReroute, account)
		return account
	}
	// Picked at its limit, perhaps for extra usage: it answers until Anthropic
	// refuses it (TookOver).
	if time.Now().Before(m.manualHold[account]) {
		return account
	}
	// Keep the same stand-in while it has room, so a conversation does not
	// hop between accounts and lose its cache.
	if to := m.claudeReroute[account]; to != "" {
		if a, err := m.store.Get(to); err == nil && m.headroomLocked(a) > 0 {
			return to
		}
	}
	if next, err := m.takeoverLocked("claude", account, false); err == nil && next != "" {
		m.claudeReroute[account] = next
		return next
	}
	return account
}

// AutoSwitchNative moves a provider off an account that should not serve any
// more, to the account of the same provider with the most usage left. For
// Claude Code's native login that is an account out of usage (with Claude's
// automatic switching on) or at its usage limit; for a provider only the
// proxy routes, such as Codex, an account at its usage limit, since the proxy
// itself moves off an account that runs out. It runs after a usage poll and
// when a limit is set, outside every lock the poll holds, on its own context
// so a switch is never cut off halfway, and moves away from an account at
// most once per cooldown. For Claude Code it only acts while Claude Code is
// signed in with an account Switcher manages, or, with the login kept
// (NativeLoginKept), on the selection instead. A call that arrives while a
// round runs makes that round go once more, so a new limit is never missed.
func (m *Manager) AutoSwitchNative() {
	if m.HandedBack() {
		return
	}
	m.autoSwitchAgain.Store(true)
	for m.autoSwitching.CompareAndSwap(false, true) {
		for m.autoSwitchAgain.Swap(false) {
			m.autoSwitchRound()
		}
		m.autoSwitching.Store(false)
		// A call that came after the last look and found this round still
		// running left the work here: take it, unless another round has.
		if !m.autoSwitchAgain.Load() {
			return
		}
	}
}

// autoSwitchRound looks at every provider once.
func (m *Manager) autoSwitchRound() {
	auto := m.claudeAutoOn()
	for providerID, prov := range m.providers {
		if m.HandedBack() {
			return
		}
		native, ok := prov.(provider.NativeLoginProvider)
		if ok && native.NativeEnabled() {
			m.autoSwitchLogin(providerID, native, auto)
		} else {
			m.autoSwitchRoute(providerID, true)
		}
	}
}

// autoSwitchLogin moves Claude Code's login (or, kept, the selection).
func (m *Manager) autoSwitchLogin(providerID string, native provider.NativeLoginProvider, auto bool) {
	// With the login kept the selection is what moves: it is the account
	// Claude Code's requests use.
	kept := m.NativeLoginKept(providerID)
	// Not the selection yet: the login answers, and it is not known.
	if kept && m.selectionWaitsForLogin(providerID) {
		return
	}
	status := native.NativeStatus()
	if !kept && status.Condition != "ready" {
		// No login Switcher manages to move: the selection, which the proxy
		// and other tools use, still leaves an account at its limit. Without
		// a cooldown: once the login is ready again, it moves at once.
		m.autoSwitchRoute(providerID, false)
		return
	}
	from := status.ActiveID
	if kept {
		from = m.ActiveID(providerID)
	}
	if from == "" {
		return
	}
	m.mu.Lock()
	if !m.leaveDueLocked(from, auto) {
		m.mu.Unlock()
		return
	}
	limited := m.atLimitLocked(from)
	to, err := m.takeoverLocked(providerID, from, true)
	if stand := m.claudeReroute[from]; kept && stand != "" {
		// The relay already answers from this one (ClaudeRoute): stay on
		// it, so a conversation keeps its cache.
		if a, gone := m.store.Get(stand); gone == nil && a.Token.AccountID != "" && m.headroomLocked(a) > 0 {
			to, err = stand, nil
		}
	}
	if err == nil && to != "" {
		m.autoSwitchAt[from] = time.Now().Add(autoSwitchRetry)
	}
	m.mu.Unlock()
	if err != nil || to == "" {
		return
	}
	// Right before switching, under the switch lock, make sure nobody
	// switched meanwhile and the account should still be left.
	still := func() bool {
		now := native.NativeStatus().ActiveID
		if kept {
			now = m.ActiveID(providerID)
		}
		if now != from {
			return false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.leaveLocked(from, auto) && time.Now().After(m.manualHold[from])
	}
	why := "ran out of usage"
	if limited {
		why = "reached its usage limit"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	_, err = m.activateNative(ctx, to, still)
	cancel()
	if errors.Is(err, errSwitchOvertaken) {
		return
	}
	if err != nil {
		log.Printf("proxy: Claude Code's account %s, but switching it did not go through; trying again in a minute", why)
		return
	}
	m.mu.Lock()
	m.autoSwitchAt[from] = time.Now().Add(autoSwitchCooldown)
	m.recordAutoSwitchLocked(providerID, "claude_code", from, to)
	if limited {
		m.lastAutoSwitch.Reason = "limit"
	}
	m.mu.Unlock()
	log.Printf("proxy: Claude Code's account %s; switched to the account with the most usage left", why)
}

// autoSwitchRoute moves the proxy's selection off an account at its usage
// limit: for a provider without a native login, and for Claude while Claude
// Code's login is not one Switcher can move. Only the selection moves, never
// a native login.
func (m *Manager) autoSwitchRoute(providerID string, cooldown bool) {
	from := m.ActiveID(providerID)
	if from == "" {
		return
	}
	m.mu.Lock()
	if !m.atLimitLocked(from) || !m.leaveDueLocked(from, false) {
		m.mu.Unlock()
		return
	}
	to, err := m.takeoverLocked(providerID, from, false)
	if cooldown && err == nil && to != "" {
		m.autoSwitchAt[from] = time.Now().Add(autoSwitchRetry)
	}
	m.mu.Unlock()
	if err != nil || to == "" {
		return
	}
	// Right before switching: nobody switched meanwhile, from is still at
	// its limit and not held, and to has not run out or reached its own.
	still := func() bool {
		return m.active[providerID] == from && m.atLimitLocked(from) && time.Now().After(m.manualHold[from]) &&
			!m.exhaustedNow(to) && !m.atLimitLocked(to)
	}
	err = m.selectUnder(providerID, to, still)
	if errors.Is(err, errSwitchOvertaken) {
		return
	}
	if err != nil {
		log.Printf("proxy: a %s account reached its usage limit, but switching did not go through; trying again in a minute", providerID)
		return
	}
	m.mu.Lock()
	if cooldown {
		m.autoSwitchAt[from] = time.Now().Add(autoSwitchCooldown)
	}
	m.recordAutoSwitchLocked(providerID, "proxy", from, to)
	m.lastAutoSwitch.Reason = "limit"
	m.mu.Unlock()
	log.Printf("proxy: a %s account reached its usage limit; switched to the account with the most usage left", providerID)
}

// selectUnder selects an account for the proxy alone, under the native
// activation lock so it never interleaves with a switch of the login.
// stillLocked runs under m.mu first, and false abandons the switch. Unlike
// a pick by the user, it leaves exhaustion marks as they are.
func (m *Manager) selectUnder(providerID, id string, stillLocked func() bool) error {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !stillLocked() {
		return errSwitchOvertaken
	}
	return m.mutateStateLocked(func() {
		m.active[providerID] = id
		m.selectionRevision[providerID]++
	})
}

// leaveDueLocked reports whether an account should be left now: it should be
// left (leaveLocked), no switch away from it happened within the cooldown,
// and the user did not pick it while it was at its limit. Requires m.mu.
func (m *Manager) leaveDueLocked(id string, auto bool) bool {
	now := time.Now()
	return m.leaveLocked(id, auto) && now.After(m.autoSwitchAt[id]) && now.After(m.manualHold[id])
}
