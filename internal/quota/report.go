package quota

import (
	"math"
	"sort"
	"strconv"
	"time"
)

// Account is an account as the report names it.
type Account struct {
	ID       string
	Provider string
	Name     string
	Plan     string
	Active   bool // the provider's selected account
}

// Report is the quota history of the last days, and what follows from it.
type Report struct {
	Now            int64           `json:"now"`
	Since          int64           `json:"since"`
	Days           int             `json:"days"`
	BurnHours      int             `json:"burn_hours"`
	RecordingSince int64           `json:"recording_since,omitempty"` // the oldest sample kept
	Pools          []Pool          `json:"pools"`
	Accounts       []AccountReport `json:"accounts"`
	Events         []EventReport   `json:"events"`
}

// Pool is one provider's accounts added up for one window: with three
// accounts the pool holds 300%.
type Pool struct {
	Provider   string             `json:"provider"`
	Window     string             `json:"window"`
	Accounts   int                `json:"accounts"`
	Capacity   int                `json:"capacity"`
	Remaining  int                `json:"remaining"`
	EmptyNow   int                `json:"empty_now"` // accounts with nothing left in this window
	Recorded   []PoolPoint        `json:"recorded"`
	Projected  []PoolPoint        `json:"projected"`
	Resets     []Moment           `json:"resets"` // past ones (from the history) and projected ones
	Burn       map[string]float64 `json:"burn"`   // percentage points per hour, over the last 1h, 3h, 6h and 24h
	FirstEmpty *Moment            `json:"first_empty,omitempty"`
	PoolEmpty  int64              `json:"pool_empty,omitempty"` // when every account would be empty, at this burn
	NextReset  *Moment            `json:"next_reset,omitempty"`
	LimitHits  int                `json:"limit_hits"`
}

// PoolPoint is the pool at one time.
type PoolPoint struct {
	T         int64 `json:"t"`
	Remaining int   `json:"remaining"`
	Capacity  int   `json:"capacity"`
}

// Moment is something at a time for an account: a reset and what it gives
// back, or the time it runs out.
type Moment struct {
	T         int64  `json:"t"`
	Account   string `json:"account"`
	Name      string `json:"name"`
	Gain      int    `json:"gain,omitempty"`
	Projected bool   `json:"projected,omitempty"`
}

// AccountReport is one account's windows and how fast they drain.
type AccountReport struct {
	ID        string         `json:"id"`
	Provider  string         `json:"provider"`
	Name      string         `json:"name"`
	Plan      string         `json:"plan,omitempty"`
	Active    bool           `json:"active,omitempty"`
	Removed   bool           `json:"removed,omitempty"`
	Windows   []WindowReport `json:"windows"`
	LimitHits int            `json:"limit_hits"`
	LastHit   int64          `json:"last_hit,omitempty"`
}

// WindowReport is one window of an account.
type WindowReport struct {
	Label      string     `json:"label"`
	Used       int        `json:"used"`
	ResetsAt   int64      `json:"resets_at,omitempty"`
	Known      bool       `json:"known"`
	Series     [][2]int64 `json:"series"` // [t, used]
	Burn       float64    `json:"burn"`   // over the report's burn hours
	Burn24h    float64    `json:"burn_24h"`
	ActiveBurn float64    `json:"active_burn"` // while in use, over the range
	EmptyIn    int64      `json:"empty_in,omitempty"`
	Cycles     int        `json:"cycles"`
	Hits       int        `json:"hits"`
	AvgToEmpty float64    `json:"avg_hours_to_empty,omitempty"` // from the window's start to its limit, when reached
	Fastest    float64    `json:"fastest_hours_to_empty,omitempty"`
}

// EventReport is an event with its accounts named.
type EventReport struct {
	Event
	Name   string `json:"name,omitempty"`
	ToName string `json:"to_name,omitempty"`
}

// period is how long a window lasts, to tell when it resets next: Session
// 5 hours, Weekly and a model's own weekly window 7 days, Monthly 30.
func period(label string) int64 {
	switch label {
	case "Session":
		return 5 * 3600
	case "Monthly":
		return 30 * 86400
	}
	if n := len(label); n > 1 && label[n-1] == 'd' {
		if d, err := strconv.Atoi(label[:n-1]); err == nil && d > 0 {
			return int64(d) * 86400
		}
	}
	return 7 * 86400
}

// horizon is how far ahead a pool is projected.
func horizon(label string) int64 {
	switch label {
	case "Session":
		return 24 * 3600
	case "Monthly":
		return 31 * 86400
	}
	return 7 * 86400
}

// usedAt is a window's share used at t: from the last sample before it,
// 0 once that sample's reset time has passed, -1 before any sample.
func usedAt(points []Point, t int64) int {
	i := sort.Search(len(points), func(i int) bool { return points[i].T > t }) - 1
	if i < 0 {
		return -1
	}
	p := points[i]
	if p.Resets > 0 && p.Resets <= t {
		return 0
	}
	return p.Used
}

// consumed adds up how much of a window was used from one time to another:
// rises only, a reset is not use.
func consumed(points []Point, from, to int64) float64 {
	prev := usedAt(points, from)
	var sum float64
	for _, p := range points {
		if p.T <= from || p.T > to {
			continue
		}
		if prev >= 0 && p.Used > prev {
			sum += float64(p.Used - prev)
		}
		prev = p.Used
	}
	return sum
}

func windowRank(label string) int {
	switch label {
	case "Weekly":
		return 0
	case "Session":
		return 1
	case "Monthly":
		return 2
	}
	return 3
}

// Report looks at the last days of history, projecting with the burn rate
// of the last burnHours. accounts are the accounts there are now.
func (r *Recorder) Report(now time.Time, days, burnHours int, accounts []Account) *Report {
	series, events, meta := r.snapshot()
	nowS := now.Unix()
	since := now.AddDate(0, 0, -days).Unix()
	out := &Report{Now: nowS, Since: since, Days: days, BurnHours: burnHours}
	for _, points := range series {
		if len(points) > 0 && (out.RecordingSince == 0 || points[0].T < out.RecordingSince) {
			out.RecordingSince = points[0].T
		}
	}
	known := map[string]Account{}
	for _, a := range accounts {
		known[a.ID] = a
	}
	name := func(id string) (string, string) {
		if a, ok := known[id]; ok {
			return a.Name, a.Provider
		}
		m := meta[id]
		return m.Name, m.Provider
	}
	hits := map[string][]int64{}
	for _, e := range events {
		if e.T >= since && (e.Kind == EventLimit || e.Kind == EventRefused) {
			hits[e.Account] = append(hits[e.Account], e.T)
		}
	}

	// Accounts: the ones there are, and removed ones with history in range.
	byAccount := map[string][]string{}
	for k, points := range series {
		if len(points) == 0 || points[len(points)-1].T < since && known[k.Account].ID == "" {
			continue
		}
		byAccount[k.Account] = append(byAccount[k.Account], k.Window)
	}
	ids := map[string]bool{}
	for _, a := range accounts {
		ids[a.ID] = true
	}
	for id := range byAccount {
		ids[id] = true
	}
	for id := range ids {
		a, ok := known[id]
		if !ok {
			n, p := name(id)
			a = Account{ID: id, Name: n, Provider: p}
		}
		ar := AccountReport{ID: id, Provider: a.Provider, Name: a.Name, Plan: a.Plan, Active: a.Active, Removed: !ok}
		labels := byAccount[id]
		sort.Slice(labels, func(i, j int) bool {
			if windowRank(labels[i]) != windowRank(labels[j]) {
				return windowRank(labels[i]) < windowRank(labels[j])
			}
			return labels[i] < labels[j]
		})
		for _, label := range labels {
			ar.Windows = append(ar.Windows, windowReport(series[seriesKey{id, label}], label, since, nowS, burnHours))
		}
		ar.LimitHits = len(hits[id])
		if n := len(hits[id]); n > 0 {
			ar.LastHit = hits[id][n-1]
		}
		if len(ar.Windows) == 0 && !ok {
			continue
		}
		out.Accounts = append(out.Accounts, ar)
	}
	sort.Slice(out.Accounts, func(i, j int) bool {
		a, b := out.Accounts[i], out.Accounts[j]
		if a.Removed != b.Removed {
			return !a.Removed
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})

	// Pools: each provider's accounts there are now, per window.
	type poolKey struct{ provider, window string }
	members := map[poolKey][]Account{}
	for _, a := range accounts {
		for _, label := range byAccount[a.ID] {
			k := poolKey{a.Provider, label}
			members[k] = append(members[k], a)
		}
	}
	for k, accs := range members {
		sort.Slice(accs, func(i, j int) bool { return accs[i].ID < accs[j].ID })
		out.Pools = append(out.Pools, pool(k.provider, k.window, accs, series, events, since, nowS, days, burnHours))
	}
	sort.Slice(out.Pools, func(i, j int) bool {
		a, b := out.Pools[i], out.Pools[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if windowRank(a.Window) != windowRank(b.Window) {
			return windowRank(a.Window) < windowRank(b.Window)
		}
		return a.Window < b.Window
	})

	for i := len(events) - 1; i >= 0 && len(out.Events) < 300; i-- {
		e := events[i]
		if e.T < since {
			break
		}
		n, p := name(e.Account)
		er := EventReport{Event: e, Name: n}
		if er.Provider == "" {
			er.Provider = p
		}
		if e.To != "" {
			er.ToName, _ = name(e.To)
		}
		out.Events = append(out.Events, er)
	}
	return out
}

func windowReport(points []Point, label string, since, now int64, burnHours int) WindowReport {
	w := WindowReport{Label: label}
	if u := usedAt(points, now); u >= 0 {
		w.Used, w.Known = u, true
		last := points[len(points)-1]
		if last.Resets > now {
			w.ResetsAt = last.Resets
		} else if last.Resets > 0 {
			w.ResetsAt = nextReset(last.Resets, label, now)
		}
	}
	// The samples in range, at most 400 of them, and where it stood at the
	// start.
	var inRange []Point
	if u := usedAt(points, since); u >= 0 {
		inRange = append(inRange, Point{T: since, Used: u})
	}
	for _, p := range points {
		if p.T > since {
			inRange = append(inRange, p)
		}
	}
	step := max(1, (len(inRange)+399)/400)
	for i, p := range inRange {
		if i%step == 0 || i == len(inRange)-1 {
			w.Series = append(w.Series, [2]int64{p.T, int64(p.Used)})
		}
	}
	if w.Known {
		w.Series = append(w.Series, [2]int64{now, int64(w.Used)})
	}
	hours := float64(burnHours)
	w.Burn = consumed(points, now-int64(burnHours)*3600, now) / hours
	w.Burn24h = consumed(points, now-86400, now) / 24
	if w.Known && w.Used < 100 && w.Burn > 0 {
		w.EmptyIn = int64(float64(100-w.Used) / w.Burn * 3600)
	}
	// While in use: the rises in range over the time they took, each gap
	// between samples counted up to an hour.
	var rise, busy float64
	prev, prevT := usedAt(points, since), since
	for _, p := range points {
		if p.T <= since || p.T > now {
			continue
		}
		if prev >= 0 && p.Used > prev {
			rise += float64(p.Used - prev)
			busy += math.Min(float64(p.T-prevT), 3600)
		}
		prev, prevT = p.Used, p.T
	}
	if busy > 0 {
		w.ActiveBurn = rise / (busy / 3600)
	}
	// Cycles: a window starts its period before its reset time. Each cycle
	// that reached its limit in range says how fast it drained.
	per := period(label)
	type cycle struct {
		resets, hit int64
	}
	var cycles []cycle
	for _, p := range points {
		if p.Resets <= 0 {
			continue
		}
		if n := len(cycles); n == 0 || abs64(p.Resets-cycles[n-1].resets) > 120 {
			cycles = append(cycles, cycle{resets: p.Resets})
		}
		c := &cycles[len(cycles)-1]
		if p.Used >= 100 && c.hit == 0 {
			c.hit = p.T
		}
	}
	var sum float64
	for _, c := range cycles {
		if c.resets < since || c.resets-per > now {
			continue
		}
		w.Cycles++
		if c.hit == 0 || c.hit < since {
			continue
		}
		h := float64(c.hit-(c.resets-per)) / 3600
		if h < 0 {
			continue
		}
		w.Hits++
		sum += h
		if w.Fastest == 0 || h < w.Fastest {
			w.Fastest = h
		}
	}
	if w.Hits > 0 {
		w.AvgToEmpty = sum / float64(w.Hits)
	}
	return w
}

// nextReset is the first reset after now of a window that last reset at t.
func nextReset(t int64, label string, now int64) int64 {
	per := period(label)
	if t > now {
		return t
	}
	return t + ((now-t)/per+1)*per
}

func pool(provider, window string, accs []Account, series map[seriesKey][]Point, events []Event, since, now int64, days, burnHours int) Pool {
	p := Pool{Provider: provider, Window: window, Accounts: len(accs), Burn: map[string]float64{}}
	states := make([]*state, 0, len(accs))
	for _, a := range accs {
		points := series[seriesKey{a.ID, window}]
		s := &state{Account: a}
		if u := usedAt(points, now); u >= 0 {
			s.known, s.left = true, float64(100-u)
			last := points[len(points)-1]
			if last.Resets > 0 {
				s.resets = nextReset(last.Resets, window, now)
			}
			p.Capacity += 100
			p.Remaining += 100 - u
			if u >= 100 {
				p.EmptyNow++
			}
			if u > 0 && s.resets > 0 && (p.NextReset == nil || s.resets < p.NextReset.T) {
				p.NextReset = &Moment{T: s.resets, Account: a.ID, Name: a.Name, Gain: u}
			}
		}
		states = append(states, s)
	}

	// Recorded: the pool every step over the range, each account as its
	// last sample had it.
	step := max(int64(15*60), int64(days)*86400/288)
	step -= step % 60
	for t := since; ; t += step {
		if t > now {
			t = now
		}
		pt := PoolPoint{T: t}
		for _, a := range accs {
			if u := usedAt(series[seriesKey{a.ID, window}], t); u >= 0 {
				pt.Remaining += 100 - u
				pt.Capacity += 100
			}
		}
		if pt.Capacity > 0 {
			p.Recorded = append(p.Recorded, pt)
		}
		if t == now {
			break
		}
	}
	for _, h := range []int{1, 3, 6, 24} {
		var sum float64
		for _, a := range accs {
			sum += consumed(series[seriesKey{a.ID, window}], now-int64(h)*3600, now)
		}
		p.Burn[strconv.Itoa(h)+"h"] = sum / float64(h)
	}
	ids := map[string]bool{}
	for _, a := range accs {
		ids[a.ID] = true
	}
	for _, e := range events {
		if e.T < since || !ids[e.Account] {
			continue
		}
		switch {
		case (e.Kind == EventReset || e.Kind == EventEarlyReset) && e.Window == window:
			p.Resets = append(p.Resets, Moment{T: e.T, Account: e.Account, Name: nameOf(accs, e.Account)})
		case e.Kind == EventLimit && e.Window == window, e.Kind == EventRefused:
			p.LimitHits++
		}
	}

	// Projected: the pool drains at the burn rate, from the selected account
	// and then, as Switcher does when one runs out, from the account with
	// the most left; each account fills up again at its reset.
	burn := p.Burn[strconv.Itoa(burnHours)+"h"]
	var live []*state
	for _, s := range states {
		if s.known {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return p
	}
	per := period(window)
	pick := func() *state {
		var best *state
		for _, s := range live {
			if s.left > 0 && (best == nil || s.left > best.left) {
				best = s
			}
		}
		return best
	}
	var current *state
	for _, s := range live {
		if s.Active && s.left > 0 {
			current = s
		}
	}
	if current == nil {
		current = pick()
	}
	total := func() int {
		var sum float64
		for _, s := range live {
			sum += s.left
		}
		return int(math.Round(sum))
	}
	end := now + horizon(window)
	pstep := max(int64(5*60), horizon(window)/168)
	p.Projected = append(p.Projected, PoolPoint{T: now, Remaining: total(), Capacity: p.Capacity})
	for t := now; t < end; {
		next := min(t+pstep, end)
		// Resets due in this step, in order.
		for {
			var due *state
			for _, s := range live {
				if s.resets > 0 && s.resets <= next && (due == nil || s.resets < due.resets) {
					due = s
				}
			}
			if due == nil {
				break
			}
			// Drain up to the reset first.
			t = drain(t, due.resets, burn, &current, pick, &p)
			if gain := int(math.Round(100 - due.left)); gain > 0 {
				p.Resets = append(p.Resets, Moment{T: due.resets, Account: due.ID, Name: due.Name, Gain: gain, Projected: true})
			}
			due.left = 100
			due.resets += per
			if current == nil {
				current = pick()
			}
			p.Projected = append(p.Projected, PoolPoint{T: t, Remaining: total(), Capacity: p.Capacity})
		}
		t = drain(t, next, burn, &current, pick, &p)
		p.Projected = append(p.Projected, PoolPoint{T: t, Remaining: total(), Capacity: p.Capacity})
	}
	// The chart looks as far ahead as it looks back (up to the horizon);
	// when the accounts run out is told however far ahead it is.
	shown := now + min(horizon(window), int64(days)*86400)
	cut := sort.Search(len(p.Projected), func(i int) bool { return p.Projected[i].T > shown })
	p.Projected = p.Projected[:cut]
	resets := p.Resets[:0]
	for _, m := range p.Resets {
		if !m.Projected || m.T <= shown {
			resets = append(resets, m)
		}
	}
	p.Resets = resets
	return p
}

// state is an account in a projection: what it has left and when it next
// resets.
type state struct {
	Account
	left   float64
	resets int64
	known  bool
}

// drain uses the pool from one time to another at the burn rate, from the
// current account and then the one pick gives, and notes when the first
// account and the whole pool run out. It returns the time it got to.
func drain(from, to int64, burn float64, current **state, pick func() *state, p *Pool) int64 {
	t := from
	for t < to && burn > 0 {
		if *current == nil {
			*current = pick()
			if *current == nil {
				if p.PoolEmpty == 0 {
					p.PoolEmpty = t
				}
				return to
			}
		}
		c := *current
		hours := float64(to-t) / 3600
		need := burn * hours
		if need < c.left {
			c.left -= need
			return to
		}
		// It runs out within this span.
		t += int64(c.left / burn * 3600)
		c.left = 0
		if p.FirstEmpty == nil {
			p.FirstEmpty = &Moment{T: t, Account: c.ID, Name: c.Name}
		}
		*current = pick()
		if *current == nil && p.PoolEmpty == 0 {
			p.PoolEmpty = t
		}
	}
	return to
}

func nameOf(accs []Account, id string) string {
	for _, a := range accs {
		if a.ID == id {
			return a.Name
		}
	}
	return ""
}
