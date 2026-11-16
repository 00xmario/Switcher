// Package quota keeps the history of the accounts' quota: each account's
// windows (Session, Weekly, a model's own) as their used share changed, and
// what happened to them: limits reached, requests refused for one, resets,
// and switches to another account. The Switcher that holds the accounts
// records it (the computers that use it ask that one), from what the proxy
// learns, and reports how fast accounts drain and when they will run out.
package quota

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Event kinds.
const (
	EventLimit      = "limit"       // a window reached 100% used
	EventRefused    = "refused"     // the provider refused a request for the limit
	EventReset      = "reset"       // a window started again at its reset time
	EventEarlyReset = "early_reset" // a window started again before it: a banked reset, or the provider's
	EventSwitch     = "switch"      // Switcher moved a provider to another account
)

// Retention: samples for keepSamples, events for keepEvents and at most
// maxEvents of them.
const (
	keepSamples = 120 * 24 * time.Hour
	keepEvents  = 2 * 365 * 24 * time.Hour
	maxEvents   = 20_000
	// heartbeat: an unchanged window is sampled again after this, so a
	// chart knows the value held.
	heartbeat = 3 * time.Hour
	// writeEvery: changes are written to disk at most this often.
	writeEvery = 2 * time.Minute
)

const fileVersion = 1

// Point is one sample of a window: when, the share used, and when the
// window resets (unix seconds, 0 unknown). It is stored as [t, used, resets].
type Point struct {
	T      int64
	Used   int
	Resets int64
}

func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal([3]int64{p.T, int64(p.Used), p.Resets})
}

func (p *Point) UnmarshalJSON(data []byte) error {
	var raw [3]int64
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = Point{T: raw[0], Used: int(min(max(raw[1], 0), 100)), Resets: raw[2]}
	return nil
}

// Event is something that happened to an account.
type Event struct {
	T        int64  `json:"t"`
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
	Account  string `json:"account,omitempty"`
	Window   string `json:"window,omitempty"`
	To       string `json:"to,omitempty"` // a switch's new account
}

type seriesKey struct{ Account, Window string }

// Series is one account's window over time.
type Series struct {
	Account string  `json:"account"`
	Window  string  `json:"window"`
	Points  []Point `json:"points"`
}

// Meta is what the recorder remembers of an account, so its history keeps
// a name after the account is removed.
type Meta struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type file struct {
	Version int             `json:"version"`
	Series  []*Series       `json:"series"`
	Events  []Event         `json:"events"`
	Meta    map[string]Meta `json:"meta"`
}

// Recorder keeps the history in memory and in a file. Its methods are
// safe to call from any goroutine and never block on disk: the proxy calls
// them while it holds its own lock.
type Recorder struct {
	mu      sync.Mutex
	path    string
	series  map[seriesKey]*Series
	events  []Event
	meta    map[string]Meta
	dirty   bool
	timer   *time.Timer
	writeMu sync.Mutex
	now     func() time.Time
}

// Open reads the history kept at path (quota-history.json), or starts one.
func Open(path string) *Recorder {
	r := &Recorder{path: path, series: map[seriesKey]*Series{}, meta: map[string]Meta{}, now: time.Now}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var f file
	if json.Unmarshal(data, &f) != nil || f.Version != fileVersion {
		return r
	}
	for _, s := range f.Series {
		if s == nil || s.Account == "" || s.Window == "" {
			continue
		}
		r.series[seriesKey{s.Account, s.Window}] = s
	}
	r.events = f.Events
	for id, m := range f.Meta {
		r.meta[id] = m
	}
	return r
}

// Usage records a fresh usage reading of an account's windows.
func (r *Recorder) Usage(account string, windows []Window) {
	if r == nil || account == "" {
		return
	}
	now := r.now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, w := range windows {
		if w.Label == "" {
			continue
		}
		used := min(max(w.Used, 0), 100)
		k := seriesKey{account, w.Label}
		s := r.series[k]
		if s == nil {
			s = &Series{Account: account, Window: w.Label}
			r.series[k] = s
		}
		p := Point{T: now, Used: used, Resets: w.ResetsAt}
		if n := len(s.Points); n > 0 {
			last := s.Points[n-1]
			if now < last.T {
				continue // a reading from before the last one
			}
			// A new cycle: the reset time moved on, or the share dropped.
			newCycle := last.Resets > 0 && w.ResetsAt > last.Resets+120 || used+5 < last.Used
			if newCycle && last.Used > 0 {
				kind := EventReset
				if last.Resets > 0 && now < last.Resets-60 {
					kind = EventEarlyReset
				}
				at := now
				if kind == EventReset && last.Resets > 0 && last.Resets < now {
					at = last.Resets
				}
				r.addEventLocked(Event{T: at, Kind: kind, Account: account, Window: w.Label})
			}
			if used >= 100 && (last.Used < 100 || newCycle) {
				r.addEventLocked(Event{T: now, Kind: EventLimit, Account: account, Window: w.Label})
			}
			same := last.Used == used && abs64(last.Resets-w.ResetsAt) <= 120
			if same && now-last.T < int64(heartbeat/time.Second) {
				continue
			}
		} else if used >= 100 {
			r.addEventLocked(Event{T: now, Kind: EventLimit, Account: account, Window: w.Label})
		}
		s.Points = append(s.Points, p)
		changed = true
	}
	if changed {
		r.changedLocked()
	}
}

// Window is one quota window as the proxy reads it.
type Window struct {
	Label    string
	Used     int
	ResetsAt int64
}

// Refused records that a provider refused a request for an account's
// limit, unless it is a limit already recorded: one of its windows is at
// 100% now, or it was refused minutes ago.
func (r *Recorder) Refused(account string) {
	if r == nil || account == "" {
		return
	}
	now := r.now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, s := range r.series {
		if n := len(s.Points); k.Account == account && n > 0 {
			if last := s.Points[n-1]; last.Used >= 100 && (last.Resets == 0 || last.Resets > now) {
				return
			}
		}
	}
	for i := len(r.events) - 1; i >= 0 && now-r.events[i].T < 15*60; i-- {
		if e := r.events[i]; e.Account == account && (e.Kind == EventLimit || e.Kind == EventRefused) {
			return
		}
	}
	r.addEventLocked(Event{T: now, Kind: EventRefused, Account: account})
	r.changedLocked()
}

// Switched records that a provider moved from one account to another.
func (r *Recorder) Switched(provider, from, to string) {
	if r == nil || to == "" || from == to {
		return
	}
	now := r.now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	// One switch is told from more than one place (the route, and Claude
	// Code's login moving with it): keep it once.
	for i := len(r.events) - 1; i >= 0 && now-r.events[i].T < 60; i-- {
		if e := r.events[i]; e.Kind == EventSwitch && e.Provider == provider && e.Account == from && e.To == to {
			return
		}
	}
	r.addEventLocked(Event{T: now, Kind: EventSwitch, Provider: provider, Account: from, To: to})
	r.changedLocked()
}

// Remember keeps an account's provider and name for its history.
func (r *Recorder) Remember(account, provider, name string) {
	if r == nil || account == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := Meta{Provider: provider, Name: name}
	if r.meta[account] != m {
		r.meta[account] = m
		r.changedLocked()
	}
}

func (r *Recorder) addEventLocked(e Event) {
	r.events = append(r.events, e)
	// Events arrive in order, except a reset dated at its reset time.
	for i := len(r.events) - 1; i > 0 && r.events[i].T < r.events[i-1].T; i-- {
		r.events[i], r.events[i-1] = r.events[i-1], r.events[i]
	}
}

// changedLocked schedules a write, and drops what is past retention.
func (r *Recorder) changedLocked() {
	r.dirty = true
	if r.timer == nil && r.path != "" {
		r.timer = time.AfterFunc(writeEvery, func() { _ = r.Flush() })
	}
}

func (r *Recorder) pruneLocked(now int64) {
	oldest := now - int64(keepSamples/time.Second)
	for k, s := range r.series {
		i := sort.Search(len(s.Points), func(i int) bool { return s.Points[i].T >= oldest })
		// Keep the last point before the cut: it says where the window stood.
		if i > 0 {
			i--
		}
		s.Points = append([]Point(nil), s.Points[i:]...)
		if len(s.Points) == 0 || len(s.Points) == 1 && s.Points[0].T < oldest {
			delete(r.series, k)
		}
	}
	oldestEvent := now - int64(keepEvents/time.Second)
	i := sort.Search(len(r.events), func(i int) bool { return r.events[i].T >= oldestEvent })
	if len(r.events)-i > maxEvents {
		i = len(r.events) - maxEvents
	}
	r.events = append([]Event(nil), r.events[i:]...)
}

// Flush writes the history now when it changed.
func (r *Recorder) Flush() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	r.timer = nil
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	r.pruneLocked(r.now().Unix())
	f := file{Version: fileVersion, Events: append([]Event(nil), r.events...), Meta: map[string]Meta{}}
	for _, s := range r.series {
		f.Series = append(f.Series, &Series{Account: s.Account, Window: s.Window, Points: append([]Point(nil), s.Points...)})
	}
	for id, m := range r.meta {
		f.Meta[id] = m
	}
	r.dirty = false
	r.mu.Unlock()
	sort.Slice(f.Series, func(i, j int) bool {
		if f.Series[i].Account != f.Series[j].Account {
			return f.Series[i].Account < f.Series[j].Account
		}
		return f.Series[i].Window < f.Series[j].Window
	})
	data, err := json.Marshal(f)
	if err == nil {
		tmp := r.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			if err = os.Rename(tmp, r.path); err != nil {
				_ = os.Remove(tmp)
			}
		}
	}
	if err != nil {
		r.mu.Lock()
		r.dirty = true
		if r.timer == nil {
			r.timer = time.AfterFunc(writeEvery, func() { _ = r.Flush() })
		}
		r.mu.Unlock()
	}
	return err
}

// snapshot copies what a report reads.
func (r *Recorder) snapshot() (map[seriesKey][]Point, []Event, map[string]Meta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	series := make(map[seriesKey][]Point, len(r.series))
	for k, s := range r.series {
		series[k] = append([]Point(nil), s.Points...)
	}
	meta := make(map[string]Meta, len(r.meta))
	for id, m := range r.meta {
		meta[id] = m
	}
	return series, append([]Event(nil), r.events...), meta
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
