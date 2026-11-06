package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"switcher/internal/desktoprelay"
	"switcher/internal/remote"
)

// Screens that show account state wait on GET /api/state/changes between
// their reads of /api/state. The answer comes as soon as /api/state differs
// from the tag the screen saw last, so a switch made on another computer, or
// by automatic switching, shows at once instead of at the next poll.
const (
	stateChangesWait = 25 * time.Second
	// A host builds its state in well under a millisecond. A client's state
	// includes its host's, which comes over the network; the host also tells
	// the client when its state changes, so the client reads it again then.
	stateWatchHostEvery   = 500 * time.Millisecond
	stateWatchClientEvery = 2 * time.Second
	// A screen asks again right after each answer, so the watcher keeps
	// running a little after the last one left.
	stateWatchLinger     = 10 * time.Second
	stateWatchMaxWaiters = 64
	// An older host cannot say when it changes; it is asked again after a
	// while, since it may have been updated.
	stateWatchOldHostRetry = 5 * time.Minute
	stateWatchHostBackoff  = 30 * time.Second
	stateWatchHostMinGap   = 250 * time.Millisecond
)

var errTooManyWaiters = errors.New("too many screens are waiting for changes")

// stateWatch is the one watcher of a server. It runs only while screens wait:
// it reads the state the way /api/state answers it, now and then and after
// every change made here, and wakes every waiter when its tag changes.
type stateWatch struct {
	// render answers what /api/state would, for comparing. With read false
	// a client may reuse its host's state read last.
	render func(ctx context.Context, read bool) []byte
	// client reports whether the state comes from a host over the network.
	client func() bool
	// waitHost waits until the host's own state changes; nil without one.
	waitHost func(ctx context.Context, since string) (string, bool, error)

	hostEvery, clientEvery, linger time.Duration
	oldHostRetry, hostBackoff      time.Duration
	maxWaiters                     int

	nudge chan struct{}

	mu        sync.Mutex
	etag      string
	ready     bool          // etag was read since the watcher started
	changed   chan struct{} // closed, and replaced, when etag changes or gets ready
	waiters   int
	idleSince time.Time
	running   bool
}

func newStateWatch(render func(context.Context, bool) []byte, client func() bool,
	waitHost func(context.Context, string) (string, bool, error)) *stateWatch {
	return &stateWatch{render: render, client: client, waitHost: waitHost,
		hostEvery: stateWatchHostEvery, clientEvery: stateWatchClientEvery, linger: stateWatchLinger,
		oldHostRetry: stateWatchOldHostRetry, hostBackoff: stateWatchHostBackoff, maxWaiters: stateWatchMaxWaiters,
		nudge: make(chan struct{}, 1), changed: make(chan struct{})}
}

// stateTag is the opaque tag of one answer of /api/state.
func stateTag(answer []byte) string {
	sum := sha256.Sum256(answer)
	return hex.EncodeToString(sum[:16])
}

// wait answers the state's tag as soon as it differs from since, or since
// and false once timeout passes without a change.
func (s *stateWatch) wait(ctx context.Context, since string, timeout time.Duration) (string, bool, error) {
	s.mu.Lock()
	if s.waiters >= s.maxWaiters {
		s.mu.Unlock()
		return "", false, errTooManyWaiters
	}
	s.waiters++
	if !s.running {
		s.running, s.ready = true, false
		go s.run()
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.waiters--; s.waiters == 0 {
			s.idleSince = time.Now()
		}
		s.mu.Unlock()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	expired := false
	for {
		s.mu.Lock()
		etag, ready, changed := s.etag, s.ready, s.changed
		s.mu.Unlock()
		if ready && etag != since {
			return etag, true, nil
		}
		if expired {
			return since, false, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			expired = true
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
}

// poke has the watcher look again now, after a change made on this server.
// It never blocks; without waiters it costs one early look later.
func (s *stateWatch) poke() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

func (s *stateWatch) publish(etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready && etag == s.etag {
		return
	}
	s.etag, s.ready = etag, true
	close(s.changed)
	s.changed = make(chan struct{})
}

// stopIfIdle ends the watcher once nobody has waited for a while.
func (s *stateWatch) stopIfIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiters > 0 || time.Since(s.idleSince) < s.linger {
		return false
	}
	s.running, s.ready = false, false
	return true
}

func (s *stateWatch) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *stateWatch) run() {
	ctx, cancel := context.WithCancel(context.Background())
	var host *hostWatch
	defer func() {
		cancel()
		if host != nil {
			host.stop()
		}
	}()
	for {
		client := s.client != nil && s.client()
		switch {
		case client && host == nil && s.waitHost != nil:
			host = s.watchHost(ctx)
		case !client && host != nil:
			host.stop()
			host = nil
		}
		// A client reads its host's state again only when the host says it
		// changed, or while the host cannot say so; its own settings are
		// compared on every look.
		read := true
		if host != nil {
			read = host.dirty.Swap(false) || !host.live.Load()
		}
		s.publish(stateTag(s.render(ctx, read)))
		every := s.hostEvery
		if client {
			every = s.clientEvery
		}
		timer := time.NewTimer(every)
		select {
		case <-timer.C:
		case <-s.nudge:
			timer.Stop()
		}
		if s.stopIfIdle() {
			return
		}
	}
}

// hostWatch is a client's wait on its host's own changes.
type hostWatch struct {
	live   atomic.Bool // the host answers waits, so its state is read only on a change
	dirty  atomic.Bool // the host's state changed since it was read last
	cancel context.CancelFunc
	done   chan struct{}
}

func (h *hostWatch) stop() {
	h.cancel()
	<-h.done
}

func (s *stateWatch) watchHost(parent context.Context) *hostWatch {
	ctx, cancel := context.WithCancel(parent)
	h := &hostWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		since, pause, backoff := "", time.Duration(0), time.Second
		for {
			if pause > 0 {
				timer := time.NewTimer(pause)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			started := time.Now()
			etag, changed, err := s.waitHost(ctx, since)
			if ctx.Err() != nil {
				return
			}
			switch {
			case err == nil:
				h.live.Store(true)
				if changed {
					h.dirty.Store(true)
					s.poke()
				}
				since, backoff = etag, time.Second
				// A host that answers at once every time cannot make this spin.
				pause = stateWatchHostMinGap - time.Since(started)
			case errors.Is(err, remote.ErrHostCannotWait):
				h.live.Store(false)
				since, pause = "", s.oldHostRetry
			default:
				h.live.Store(false)
				since, pause = "", backoff
				backoff = min(2*backoff, s.hostBackoff)
			}
		}
	}()
	return h
}

// watch is this server's state watcher, made on first use.
func (a *API) watch() *stateWatch {
	a.watchOnce.Do(func() {
		var waitHost func(context.Context, string) (string, bool, error)
		if a.RemoteClient != nil {
			waitHost = a.RemoteClient.WaitHostChange
		}
		a.changes = newStateWatch(a.watchedState, a.usingHost, waitHost)
	})
	return a.changes
}

// StateChanged has screens waiting for a change look again now, after a
// change made outside a request, such as the background usage refresh.
func (a *API) StateChanged() { a.watch().poke() }

// changing marks a route that changes account state: once it answered, the
// watcher looks again, so other screens show the change without waiting for
// the watcher's next look.
func (a *API) changing(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r)
		a.watch().poke()
	}
}

func (a *API) usingHost() bool { return a.RemoteClient != nil && a.RemoteClient.Connected() }

// watchedState answers /api/state for the watcher. It asks as a paired
// device would, so the tag never covers the management key a local browser
// receives.
func (a *API) watchedState(ctx context.Context, read bool) []byte {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://switcher-watch/api/state", nil)
	r.RemoteAddr = "192.0.2.1:1"
	var status int
	var state any
	if a.usingHost() {
		status, state = http.StatusOK, a.RemoteClient.State(r, a.LocalStateFields, read)
	} else {
		status, state = a.stateView(r)
	}
	body, _ := json.Marshal(leaveOutActivity(state))
	return append(strconv.AppendInt(nil, int64(status), 10), body...)
}

// leaveOutActivity drops what changes without any account changing: the
// relay's count of replies in flight changes with every message Claude
// Desktop sends. The dashboard shows it from its regular reads.
func leaveOutActivity(state any) any {
	if fields, ok := state.(map[string]any); ok {
		if relay, ok := fields["desktop_relay"].(desktoprelay.Status); ok {
			relay.InFlight = 0
			fields["desktop_relay"] = relay
		}
	}
	return state
}

// handleStateChanges answers {"etag", "changed"}: changed as soon as the
// state differs from ?since (at once when since is empty or stale), or
// unchanged with the same etag after about 25 seconds.
func (a *API) handleStateChanges(w http.ResponseWriter, r *http.Request) {
	etag, changed, err := a.watch().wait(r.Context(), r.URL.Query().Get("since"), stateChangesWait)
	switch {
	case errors.Is(err, errTooManyWaiters):
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "too many screens are waiting for changes; try again shortly"})
	case err != nil:
		// The screen went away; nobody reads an answer.
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"etag": etag, "changed": changed})
	}
}
