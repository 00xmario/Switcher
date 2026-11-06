package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// keptLoginWait is how long the selection waits to start at a login not known
// yet before it applies as it is.
const keptLoginWait = 2 * time.Minute

// ErrAccountUnusable refuses a switch to an account whose credential does not
// work: with the login kept nothing else would say so, and another account
// would answer instead.
var ErrAccountUnusable = errors.New("this account cannot be used right now; sign in to it again or try later")

// nativeSettler is a native login that can finish what a switch finishes first
// and say whether Claude Code has a login at this moment.
type nativeSettler interface {
	SettleNative(context.Context) (loggedIn bool, err error)
}

// SetClaudeCodeRouted wires whether Claude Code's requests go through the
// Desktop relay on this computer. The answer must not depend on m.mu.
func (m *Manager) SetClaudeCodeRouted(routed func() bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claudeCodeRouted = routed
}

func (m *Manager) routed() bool {
	m.mu.Lock()
	routed := m.claudeCodeRouted
	m.mu.Unlock()
	return routed != nil && routed()
}

// NativeLoginKept reports that a switch leaves the provider's native login
// alone. Claude Code's requests go through the Desktop relay, which sends
// them with the selected account, so its own login no longer picks the
// account, and what belongs to that login keeps working: a Remote Control
// environment is only found with the account that registered it. A Claude
// Code without a login still gets one from a switch. Never call with m.mu held.
func (m *Manager) NativeLoginKept(providerID string) bool {
	native, ok := m.providers[providerID].(provider.NativeLoginProvider)
	if !ok || !native.NativeEnabled() || !m.routed() {
		return false
	}
	status := native.NativeStatus()
	if status.Condition == "logged_out" {
		return false
	}
	m.alignKeptLogin(providerID, status)
	return true
}

// alignKeptLogin keeps the selection from jumping when the login starts being
// kept: until then the login was the selection, so the selection starts there,
// once. That is on the first start of a version that keeps it, and after an
// explicit connect (RestartSelectionAtLogin); a moment of unreadable settings
// never gets here. A selection that is gone, such as a removed account, falls
// back to the login the same way.
func (m *Manager) alignKeptLogin(providerID string, status claudecode.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keptLoginAligned && m.active[providerID] != "" {
		return
	}
	if status.Condition != "ready" && status.Condition != "unmanaged" {
		// Not known yet. Some logins stay that way, such as an API key beside
		// OAuth, and the selection on the screen must not wait for them forever.
		// "checking" only means nobody has looked yet.
		if m.keptLoginAligned || status.Condition != "unavailable" {
			return
		}
		if m.keptLoginWaiting.IsZero() {
			m.keptLoginWaiting = time.Now()
		}
		if time.Since(m.keptLoginWaiting) < keptLoginWait || time.Now().Before(m.keptLoginRetry) {
			return
		}
		if err := m.mutateStateLocked(func() { m.keptLoginAligned = true }); err != nil {
			m.keptLoginRetry = time.Now().Add(time.Minute)
			log.Printf("proxy: save routing state: %v", err)
		}
		return
	}
	id := status.ActiveID
	if id != "" {
		if _, err := m.store.Get(id); err != nil {
			id = ""
		}
	}
	if m.keptLoginAligned && id == "" {
		return
	}
	// This runs on read paths, which must not retry and log on every request
	// while the state cannot be written.
	if time.Now().Before(m.keptLoginRetry) {
		return
	}
	err := m.mutateStateLocked(func() {
		m.keptLoginAligned = true
		if id != "" && m.active[providerID] != id {
			m.active[providerID] = id
			m.selectionRevision[providerID]++
		}
	})
	if err != nil {
		m.keptLoginRetry = time.Now().Add(time.Minute)
		log.Printf("proxy: save routing state: %v", err)
	}
}

// RestartSelectionAtLogin is for the moment Claude Code gets connected to the
// relay: until then its login was the selection, so that is where it starts,
// as soon as the login is known.
func (m *Manager) RestartSelectionAtLogin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keptLoginWaiting = time.Time{}
	return m.mutateStateLocked(func() { m.keptLoginAligned = false })
}

// selectionWaitsForLogin reports a kept login that is not known yet with a
// selection that has not started there. The caller's own token is that login,
// so it answers until then, rather than a stale account for a while.
func (m *Manager) selectionWaitsForLogin(providerID string) bool {
	if !m.NativeLoginKept(providerID) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.keptLoginAligned
}

// lockProviderRefreshes takes every refresh lock of a provider's accounts, in
// the one order every native operation uses, so that nothing refreshing an
// account can save over what the native login recovers or writes.
func (m *Manager) lockProviderRefreshes(providerID string) (ids []string, unlock func(), err error) {
	accounts, err := m.store.List()
	if err != nil {
		return nil, nil, err
	}
	for _, a := range accounts {
		if a.Provider == providerID {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	locks := make([]*sync.Mutex, 0, len(ids))
	for _, key := range ids {
		lock := m.refreshLock(key)
		lock.Lock()
		locks = append(locks, lock)
	}
	return ids, func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}, nil
}

// keepLoginOnSwitch decides for one switch, on what is true right now rather
// than on the last status read. It also finishes an interrupted switch, which
// otherwise only a switch that writes the login would.
func (m *Manager) keepLoginOnSwitch(ctx context.Context, native provider.NativeLoginProvider) (bool, error) {
	if !m.routed() {
		return false, nil
	}
	if settler, ok := native.(nativeSettler); ok {
		return settler.SettleNative(ctx)
	}
	return native.NativeStatus().Condition != "logged_out", nil
}

// ActivateForClient switches an account for the proxy and, for Claude, Claude
// Code's own login unless that login is kept (NativeLoginKept). The user's
// switch and the automatic switch after an account runs out of usage
// (AutoSwitchNative) both come here. Proxy failover on a refused request
// selects only its route, which with a kept login is also the selection.
func (m *Manager) ActivateForClient(ctx context.Context, id string) (*claudecode.SwitchResult, error) {
	if m.HandedBack() {
		return nil, ErrHandedBack
	}
	// The user is in charge: an account they pick while it is at its limit
	// stays picked for a while.
	m.mu.Lock()
	if m.outOfUsageLocked(id) {
		m.manualHold[id] = time.Now().Add(manualHold)
	}
	m.mu.Unlock()
	return m.activateNative(ctx, id, nil)
}

// activateNative switches under the native activation lock. still, when
// given, runs under that lock first; false abandons the switch.
func (m *Manager) activateNative(ctx context.Context, id string, still func() bool) (*claudecode.SwitchResult, error) {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	if still != nil && !still() {
		return nil, errSwitchOvertaken
	}
	account, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	native, ok := m.providers[account.Provider].(provider.NativeLoginProvider)
	if !ok || !native.NativeEnabled() {
		return nil, m.Activate(id)
	}
	ids, unlock, err := m.lockProviderRefreshes(account.Provider)
	if err != nil {
		return nil, err
	}
	keep, err := m.keepLoginOnSwitch(ctx, native)
	if err != nil {
		unlock()
		// As when a switch of the login fails: nothing moved, and the caller
		// says why.
		return &claudecode.SwitchResult{}, err
	}
	if keep {
		unlock() // PrepareAccount takes this account's lock itself.
		if _, err := m.PrepareAccount(ctx, id); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrAccountUnusable, err)
		}
		m.mu.Lock()
		err := m.mutateStateLocked(func() { m.keptLoginAligned = true })
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, m.Activate(id)
	}
	defer unlock()
	result, err := native.SwitchNative(ctx, id, func(receipt string) error { return m.activateWithReceipt(id, receipt) })
	// Native switching may have adopted outgoing credentials even if a later
	// validation/write failed. Invalidate workers carrying older snapshots.
	m.mu.Lock()
	for _, key := range ids {
		m.generation[key]++
		m.accountRevision[key]++
		delete(m.health, key)
		delete(m.planChecked, key)
	}
	m.mu.Unlock()
	return &result, err
}

func (m *Manager) syncNative(ctx context.Context, prov provider.Provider, a *store.Account) error {
	native, ok := prov.(provider.NativeLoginProvider)
	if !ok || !native.NativeEnabled() {
		return nil
	}
	beforeAccess, beforeRefresh := a.Token.AccessToken, a.Token.RefreshToken
	changed, err := native.SyncNative(ctx, a)
	if err != nil {
		return err
	}
	if changed {
		if err := m.store.Save(*a); err != nil {
			return err
		}
		m.mu.Lock()
		m.accountRevision[a.ID]++
		m.quotaRevision[a.ID]++
		if beforeAccess != a.Token.AccessToken || beforeRefresh != a.Token.RefreshToken {
			h := m.healthLocked(a.ID)
			h.relogin = false
			h.lastChecked, h.lastSuccess, h.retryAt = time.Time{}, time.Time{}, time.Time{}
			delete(m.lastUsage, a.ID)
			delete(m.planChecked, a.ID)
		}
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) nativeForRequest(ctx context.Context, prov provider.Provider, picked store.Account) (store.Account, error) {
	if native, ok := prov.(provider.NativeLoginProvider); !ok || !native.NativeEnabled() {
		return picked, nil
	}
	lock := m.refreshLock(picked.ID)
	lock.Lock()
	defer lock.Unlock()
	a, err := m.store.Get(picked.ID)
	if err != nil {
		return picked, err
	}
	err = m.syncNative(ctx, prov, &a)
	return a, err
}

// Import keeps capture and persistence in the same activation/account/native
// lock span. A captured predecessor cannot be saved after an intervening
// switch and inactive refresh advanced that account's saved generation.
func (m *Manager) ImportNative(ctx context.Context, prov provider.Provider, target string) (store.Account, error) {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	capture, ok := prov.(interface {
		CaptureAndPersist(context.Context, func(*store.Account) error) (store.Account, error)
	})
	if !ok {
		return store.Account{}, provider.ErrUnsupported
	}
	accounts, err := m.store.List()
	if err != nil {
		return store.Account{}, err
	}
	ids := []string{}
	for _, a := range accounts {
		if a.Provider == prov.ID() {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	locks := []*sync.Mutex{}
	for _, id := range ids {
		lock := m.refreshLock(id)
		lock.Lock()
		locks = append(locks, lock)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	return capture.CaptureAndPersist(ctx, func(a *store.Account) error {
		if target != "" {
			return m.replaceReloginRecord(a, target, false)
		}
		return m.replaceAccountRecord(*a, false)
	})
}
