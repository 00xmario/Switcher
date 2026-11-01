package proxy

import (
	"errors"
	"net/http"
	"time"

	"switcher/internal/store"
)

// ErrHandedBack refuses work that would use, refresh or install a Switcher
// account while Switcher has handed the tools back to their providers.
var ErrHandedBack = errors.New("Switcher has handed your tools back to their providers; turn that off in Switcher to use its accounts again")

// SetHandedBack stops (true) or resumes (false) every use of Switcher's own
// accounts: proxy requests, token refreshes and usage polls, the Desktop
// relay's credentials and Claude Code login switches. While handed back the
// tools' own logins are theirs alone, so nothing here may rotate a token.
func (m *Manager) SetHandedBack(on bool) { m.handedBack.Store(on) }

// HandedBack reports whether Switcher has handed the tools back.
func (m *Manager) HandedBack() bool { return m.handedBack.Load() }

// AdoptToken replaces an account's credential with a newer generation of the
// same login, such as the one a tool kept refreshing while Switcher had
// handed it back. Only the credential changes; usage is checked afresh.
func (m *Manager) AdoptToken(id string, token store.Token) error {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	lock := m.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()
	a, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if token.Extra == nil {
		token.Extra = a.Token.Extra
	}
	a.Token = token
	a.LastRefresh = time.Now().Unix()
	return m.saveAccount(a)
}

func writeHandedBack(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, ErrHandedBack.Error())
}
