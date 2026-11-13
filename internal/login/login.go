// Package login orchestrates OAuth logins: browser (PKCE callback) flows
// and device-code flows, where the provider polls upstream itself and the
// outcome simply takes longer to land. Results are kept briefly so a
// completed login is never lost to a poll race, and every successful
// exchange is persisted through the configured hook the moment it
// completes.
package login

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"log"
	"switcher/internal/provider"
	"switcher/internal/store"
)

const (
	// pendingTTL bounds how long a started-but-unfinished login stays alive.
	pendingTTL = 10 * time.Minute
	// resultTTL bounds how long a finished outcome stays pollable.
	resultTTL = 15 * time.Minute
	// pollWait is how long a single UI poll may block.
	pollWait = time.Second
)

// Handle identifies one started login attempt.
type Handle struct {
	State           string `json:"state"`
	URL             string `json:"url,omitempty"`
	Kind            string `json:"kind"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
}

// ErrUnknown is returned for unknown or expired logins.
var ErrUnknown = errors.New("unknown or expired login attempt")

type pending struct {
	provider      provider.Provider
	reloginTarget string
	kind          string
	pkce          bool // the code exchange proves this attempt, see LoginInfo.PKCE
	completing    bool
	finished      bool          // its outcome is recorded; done is closed
	tries         chan struct{} // turns for unnamed callbacks' exchanges
	// exchanging holds, per code, the exchange of that code under way:
	// the same code delivered twice is exchanged once, as a provider may
	// revoke what a code gave when it is used again.
	exchanging map[string]*exchange
	done       chan struct{}
	started    time.Time
}

// exchange is one code's exchange, and what came of it once done is closed.
type exchange struct {
	done chan struct{}
	err  error
}

// unnamedTries bounds how many exchanges for one sign-in run at once from
// callbacks that name no sign-in. More wait their turn.
const unnamedTries = 4

// result is the terminal outcome of one login flow, kept briefly so the UI
// can observe it even if it polls after the callback already landed.
type result struct {
	account store.Account
	err     error
	at      time.Time
}

// Manager tracks pending and recently-finished login flows across providers.
type Manager struct {
	persist func(*store.Account, string) error

	mu      sync.Mutex
	pending map[string]*pending
	results map[string]result
}

// New creates a login manager. persist is called exactly once per
// successful login, from the goroutine that completes it.
func New(persist func(*store.Account, string) error) *Manager {
	return &Manager{
		persist: persist,
		pending: map[string]*pending{},
		results: map[string]result{},
	}
}

// Start begins a browser (PKCE) login for the given provider.
func (m *Manager) Start(ctx context.Context, prov provider.Provider, reloginTarget string) (Handle, error) {
	info, err := prov.LoginStart(ctx)
	if err != nil {
		return Handle{}, fmt.Errorf("start login: %w", err)
	}
	// Only one browser sign-in per provider whose callback names none can
	// finish: a new one replaces that provider's still waiting, so a
	// sign-in that failed at the provider does not keep the next one from
	// finishing. Another provider's goes on. Only this computer's own
	// dashboard or command line starts one.
	if info.PKCE {
		m.mu.Lock()
		for state, p := range m.pending {
			if p.kind == "browser" && p.pkce && !p.completing && !p.finished && p.provider.ID() == prov.ID() {
				p.finished = true
				delete(m.pending, state)
				close(p.done)
			}
		}
		m.mu.Unlock()
	}
	m.track(info.State, prov, reloginTarget, "browser", info.PKCE)
	return Handle{State: info.State, URL: info.URL, Kind: info.Kind}, nil
}

// StartDevice begins a device-code login and spawns the background poller:
// when the user authorizes, the flow completes itself and persists. Like
// Start, reloginTarget adopts the existing account id on completion.
func (m *Manager) StartDevice(ctx context.Context, prov provider.Provider, reloginTarget string) (Handle, error) {
	info, poll, err := prov.DeviceStart(ctx)
	if err != nil {
		return Handle{}, fmt.Errorf("start device login: %w", err)
	}
	p := m.track(info.State, prov, reloginTarget, "device", false)
	go func() {
		account, err := poll(ctx)
		if err == nil && m.persist != nil {
			err = m.persist(&account, reloginTarget)
		}
		if err != nil {
			log.Printf("login: device flow for %s failed: %v", prov.ID(), err)
		} else {
			log.Printf("login: device flow for %s completed for %s", prov.ID(), account.Email)
		}
		m.mu.Lock()
		m.finishLocked(info.State, p, account, err)
		m.mu.Unlock()
	}()
	return Handle{State: info.State, Kind: info.Kind, VerificationURL: info.VerificationURL, UserCode: info.UserCode}, nil
}

// Complete finishes a browser flow identified by state. It is called by the
// OAuth callback endpoint. The exchanged account is persisted immediately
// so completion never depends on anyone polling.
func (m *Manager) Complete(ctx context.Context, state, code string) error {
	m.mu.Lock()
	m.gcLocked()
	if state == "" {
		state = m.singlePendingLocked("")
	}
	p, ok := m.pending[state]
	if !ok || p.kind != "browser" || p.completing {
		m.mu.Unlock()
		return ErrUnknown
	}
	// Claim the callback once, but leave the attempt pollable during exchange.
	p.completing = true
	m.mu.Unlock()
	log.Printf("login: completing flow for %s (state %.8s...)", p.provider.ID(), state)

	account, err := p.provider.LoginExchange(ctx, state, code)
	if err == nil && m.persist != nil {
		err = m.persist(&account, p.reloginTarget)
	}
	if err != nil {
		err = fmt.Errorf("login exchange: %w", err)
	}
	if err != nil {
		log.Printf("login: flow %.8s... failed: %v", state, err)
	} else {
		log.Printf("login: flow %.8s... completed for %s", state, account.Email)
	}
	m.mu.Lock()
	m.finishLocked(state, p, account, err)
	m.mu.Unlock()
	return err
}

// finishLocked records a sign-in's outcome, once.
func (m *Manager) finishLocked(state string, p *pending, account store.Account, err error) {
	if p.finished {
		return
	}
	p.finished = true
	m.gcResultsLocked()
	m.results[state] = result{account: account, err: err, at: time.Now()}
	if m.pending[state] == p {
		delete(m.pending, state)
	}
	close(p.done)
}

// ErrNoCode refuses a callback that brings no sign-in code.
var ErrNoCode = errors.New("the sign-in came back without a code")

// CompleteUnnamed finishes, from a callback that names no sign-in, the only
// browser sign-in under way whose code exchange proves it (PKCE): OpenAI's
// redirect names none, and Anthropic's names it only where the browser
// does not send it. Any web page can send such a callback while a sign-in
// is under way, so one does not claim the sign-in: a code that fails its
// exchange fails that callback alone, and the sign-in waits for the real
// one. Several run side by side, a few at a time. It returns the sign-in's
// state.
func (m *Manager) CompleteUnnamed(ctx context.Context, code string) (string, error) {
	return m.CompleteUnnamedFor(ctx, "", code)
}

// CompleteUnnamedFor is CompleteUnnamed for a callback that comes back
// where one provider's sign-ins do (providerID; "" for any): among that
// provider's sign-ins only.
func (m *Manager) CompleteUnnamedFor(ctx context.Context, providerID, code string) (string, error) {
	m.mu.Lock()
	m.gcLocked()
	state := m.singlePendingLocked(providerID)
	p := m.pending[state]
	m.mu.Unlock()
	if p == nil {
		return "", ErrUnknown
	}
	if code == "" {
		return state, ErrNoCode
	}
	// The same code already being exchanged: its outcome answers this
	// callback too.
	m.mu.Lock()
	if x := p.exchanging[code]; x != nil {
		m.mu.Unlock()
		select {
		case <-x.done:
			return state, x.err
		case <-ctx.Done():
			return state, ctx.Err()
		}
	}
	x := &exchange{done: make(chan struct{})}
	if p.exchanging == nil {
		p.exchanging = map[string]*exchange{}
	}
	p.exchanging[code] = x
	m.mu.Unlock()
	x.err = m.exchangeUnnamed(ctx, state, p, code)
	close(x.done)
	m.mu.Lock()
	delete(p.exchanging, code)
	m.mu.Unlock()
	return state, x.err
}

func (m *Manager) exchangeUnnamed(ctx context.Context, state string, p *pending, code string) error {
	select {
	case p.tries <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return m.recorded(state, p)
	}
	defer func() { <-p.tries }()
	m.mu.Lock()
	gone := p.finished || p.completing
	m.mu.Unlock()
	if gone {
		return m.recorded(state, p)
	}
	account, err := p.provider.LoginExchange(ctx, state, code)
	if err != nil {
		// This callback's code is not the sign-in's; the sign-in goes on.
		log.Printf("login: a callback for %s brought a code that did not exchange (state %.8s...): %v", p.provider.ID(), state, err)
		return fmt.Errorf("login exchange: %w", err)
	}
	m.mu.Lock()
	if p.finished || p.completing {
		// The sign-in ended, or another callback is saving, while this code
		// was exchanged: what is recorded is the answer, not this exchange.
		m.mu.Unlock()
		return m.recorded(state, p)
	}
	// Claimed now: one success is saved, once.
	p.completing = true
	m.mu.Unlock()
	if m.persist != nil {
		err = m.persist(&account, p.reloginTarget)
	}
	if err != nil {
		err = fmt.Errorf("login exchange: %w", err)
		log.Printf("login: flow %.8s... failed: %v", state, err)
	} else {
		log.Printf("login: flow %.8s... completed for %s", state, account.Email)
	}
	m.mu.Lock()
	m.finishLocked(state, p, account, err)
	m.mu.Unlock()
	return err
}

// recorded waits for a sign-in's outcome and answers with it: nil only for
// an account that was saved.
func (m *Manager) recorded(state string, p *pending) error {
	<-p.done
	m.mu.Lock()
	defer m.mu.Unlock()
	if res, ok := m.results[state]; ok {
		return res.err
	}
	return ErrUnknown
}

// Discard drops a sign-in that was started but will not be offered, such as
// one whose callback port another program holds. Left pending, it would
// make the next sign-in ambiguous for a callback that names no state.
func (m *Manager) Discard(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pending[state]; ok && !p.completing && !p.finished {
		p.finished = true
		delete(m.pending, state)
		close(p.done)
	}
}

// Outcome reports the current status of a login flow. finished=true means
// the outcome (success or failure) is being reported with this call.
func (m *Manager) Outcome(state string, wait time.Duration) (account store.Account, err error, finished bool) {
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		if res, ok := m.results[state]; ok {
			m.mu.Unlock()
			return res.account, res.err, true
		}
		p, ok := m.pending[state]
		m.mu.Unlock()
		if !ok {
			return store.Account{}, ErrUnknown, true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return store.Account{}, nil, false
		}
		select {
		case <-p.done:
			// The result is now in m.results; loop picks it up.
			continue
		case <-time.After(remaining):
			return store.Account{}, nil, false
		}
	}
}

// track records a new pending flow.
func (m *Manager) track(state string, prov provider.Provider, reloginTarget, kind string, pkce bool) *pending {
	m.mu.Lock()
	m.gcLocked()
	p := &pending{provider: prov, reloginTarget: reloginTarget, kind: kind, pkce: pkce, completing: kind == "device",
		tries: make(chan struct{}, unnamedTries), done: make(chan struct{}), started: time.Now()}
	m.pending[state] = p
	m.mu.Unlock()
	return p
}

// SinglePending returns the only tracked pending login state, or "" when
// none or more than one exist. OpenAI's redirect does not echo the state
// parameter back on the callback URL, so a single in-flight login resolves
// by being the only one (the Codex CLI works the same way). Only logins whose
// code PKCE binds to them count: for the others the state is the only proof,
// and any web page could otherwise deliver its own account's code here.
func (m *Manager) SinglePending() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	return m.singlePendingLocked("")
}

func (m *Manager) singlePendingLocked(providerID string) string {
	var found string
	for state, p := range m.pending {
		if p.kind == "browser" && p.pkce && !p.completing && (providerID == "" || p.provider.ID() == providerID) {
			if found != "" {
				return ""
			}
			found = state
		}
	}
	return found
}

// pendingStates lists the tracked pending login states.
func (m *Manager) pendingStates() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.pending))
	for state := range m.pending {
		out = append(out, state)
	}
	return out
}

// gcLocked drops pending logins older than the TTL.
func (m *Manager) gcLocked() {
	now := time.Now()
	for state, p := range m.pending {
		if !p.completing && now.Sub(p.started) > pendingTTL {
			delete(m.pending, state)
			if !p.finished {
				p.finished = true
				close(p.done)
			}
		}
	}
	m.gcResultsLocked()
}

// gcResultsLocked drops finished outcomes older than the TTL.
func (m *Manager) gcResultsLocked() {
	now := time.Now()
	for state, res := range m.results {
		if now.Sub(res.at) > resultTTL {
			delete(m.results, state)
		}
	}
}
