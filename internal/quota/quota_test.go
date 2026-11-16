package quota

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func recorderAt(t *testing.T, at time.Time) (*Recorder, *clock) {
	t.Helper()
	c := &clock{t: at}
	r := Open(filepath.Join(t.TempDir(), "quota-history.json"))
	r.now = c.now
	return r, c
}

func kinds(r *Recorder) []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e.Kind+":"+e.Window)
	}
	return out
}

// A window is sampled when it changes (or now and then when it does not);
// reaching 100% is a limit, a reset time moving on a reset, and a drop
// before the reset time an early reset (a banked reset spent).
func TestRecorderSamplesAndEvents(t *testing.T) {
	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	r, c := recorderAt(t, start)
	weekly := func(used int, resets time.Time) []Window {
		return []Window{{Label: "Weekly", Used: used, ResetsAt: resets.Unix()}}
	}
	reset := start.Add(48 * time.Hour)
	r.Usage("a", weekly(40, reset))
	c.add(time.Minute)
	r.Usage("a", weekly(40, reset)) // unchanged: not kept
	c.add(time.Minute)
	r.Usage("a", weekly(70, reset))
	if n := len(r.series[seriesKey{"a", "Weekly"}].Points); n != 2 {
		t.Fatalf("%d samples, want 2", n)
	}
	c.add(4 * time.Hour)
	r.Usage("a", weekly(70, reset)) // unchanged for longer than the heartbeat
	c.add(time.Hour)
	r.Usage("a", weekly(100, reset))
	r.Usage("a", weekly(100, reset)) // still at the limit: one limit
	c.t = reset.Add(10 * time.Minute)
	r.Usage("a", weekly(3, reset.Add(7*24*time.Hour)))
	c.add(time.Hour)
	r.Usage("a", weekly(20, reset.Add(7*24*time.Hour)))
	c.add(time.Hour)
	r.Usage("a", weekly(0, reset.Add(7*24*time.Hour))) // before its reset time
	got := kinds(r)
	want := []string{"limit:Weekly", "reset:Weekly", "early_reset:Weekly"}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	if r.events[1].T != reset.Unix() {
		t.Fatalf("a reset is dated at its reset time: %d, want %d", r.events[1].T, reset.Unix())
	}
	if n := len(r.series[seriesKey{"a", "Weekly"}].Points); n != 7 {
		t.Fatalf("%d samples, want 7", n)
	}

	// Refusals minutes apart are the same limit.
	r.Refused("a")
	c.add(5 * time.Minute)
	r.Refused("a")
	// One switch told twice is kept once.
	r.Switched("claude", "a", "b")
	r.Switched("claude", "a", "b")
	if got := kinds(r); len(got) != 5 || got[3] != "refused:" || got[4] != "switch:" {
		t.Fatalf("events %v", got)
	}

	// Written and read back, readable by this user only.
	r.Remember("a", "claude", "work@example.com")
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	back := Open(r.path)
	if len(back.events) != 5 || len(back.series[seriesKey{"a", "Weekly"}].Points) != 7 || back.meta["a"].Name != "work@example.com" {
		t.Fatalf("read back %d events, %+v", len(back.events), back.meta)
	}
}

// Old samples go, keeping the last one before the cut, which says where
// the window stood.
func TestRecorderPrunes(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r, c := recorderAt(t, start)
	r.Usage("a", []Window{{Label: "Session", Used: 10}})
	c.add(time.Hour)
	r.Usage("a", []Window{{Label: "Session", Used: 20}})
	c.add(keepSamples + 24*time.Hour)
	r.Usage("a", []Window{{Label: "Session", Used: 30}})
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	points := r.series[seriesKey{"a", "Session"}].Points
	if len(points) != 2 || points[0].Used != 20 {
		t.Fatalf("kept %+v", points)
	}
}

func near(a, b, slack int64) bool { return a-b <= slack && b-a <= slack }

// The pool adds up the accounts; it drains at the recent burn rate from the
// selected account, then from the one with the most left, until each fills
// up again at its reset; and each account says how fast it drains.
func TestReportProjectsThePool(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	r, c := recorderAt(t, now.Add(-6*time.Hour))
	resetA, resetB := now.Add(48*time.Hour).Unix(), now.Add(5*24*time.Hour).Unix()
	for h := 0; h <= 6; h++ {
		r.Usage("a", []Window{{Label: "Weekly", Used: 40 + 3*h, ResetsAt: resetA}})
		r.Usage("b", []Window{{Label: "Weekly", Used: 20, ResetsAt: resetB}})
		if h < 6 {
			c.add(time.Hour)
		}
	}
	accounts := []Account{
		{ID: "a", Provider: "claude", Name: "work@example.com", Active: true},
		{ID: "b", Provider: "claude", Name: "home@example.com"},
	}
	rep := r.Report(now, 7, 3, accounts)
	if len(rep.Pools) != 1 {
		t.Fatalf("pools: %+v", rep.Pools)
	}
	p := rep.Pools[0]
	if p.Provider != "claude" || p.Window != "Weekly" || p.Capacity != 200 || p.Remaining != 122 || p.Accounts != 2 {
		t.Fatalf("pool: %+v", p)
	}
	if p.Burn["3h"] != 3 || p.Burn["6h"] != 3 || p.Burn["1h"] != 3 {
		t.Fatalf("burn: %+v", p.Burn)
	}
	if p.FirstEmpty == nil || p.FirstEmpty.Account != "a" || !near(p.FirstEmpty.T, now.Add(14*time.Hour).Unix(), 60) {
		t.Fatalf("first empty: %+v, want a in 14h", p.FirstEmpty)
	}
	// Then b's 80 at 3 an hour: 26h40m more, before a's reset.
	if !near(p.PoolEmpty, now.Add(40*time.Hour+40*time.Minute).Unix(), 120) {
		t.Fatalf("pool empty at %v, want in 40h40m", time.Unix(p.PoolEmpty, 0).Sub(now))
	}
	if p.NextReset == nil || p.NextReset.Account != "a" || p.NextReset.T != resetA || p.NextReset.Gain != 58 {
		t.Fatalf("next reset: %+v", p.NextReset)
	}
	var projectedResets int
	for _, m := range p.Resets {
		if m.Projected {
			projectedResets++
		}
	}
	if projectedResets < 2 || len(p.Projected) < 100 || p.Projected[0].Remaining != 122 {
		t.Fatalf("projection: %d points, %d resets, starting at %+v", len(p.Projected), projectedResets, p.Projected[0])
	}
	if last := p.Recorded[len(p.Recorded)-1]; last.T != now.Unix() || last.Remaining != 122 {
		t.Fatalf("recorded ends at %+v", last)
	}
	if len(rep.Accounts) != 2 {
		t.Fatalf("accounts: %+v", rep.Accounts)
	}
	for _, a := range rep.Accounts {
		w := a.Windows[0]
		switch a.ID {
		case "a":
			if w.Used != 58 || w.Burn != 3 || w.EmptyIn != 14*3600 || !a.Active || w.ResetsAt != resetA {
				t.Fatalf("account a: %+v", w)
			}
		case "b":
			if w.Used != 20 || w.Burn != 0 || w.EmptyIn != 0 {
				t.Fatalf("account b: %+v", w)
			}
		}
	}
}

// How fast an account drains: from its window's start (a period before its
// reset time) to the limit, in each cycle that reached it; limits count,
// also a refusal without a window.
func TestReportDrainSpeed(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cycleStart := now.Add(-10 * 24 * time.Hour)
	r, c := recorderAt(t, cycleStart)
	first := cycleStart.Add(7 * 24 * time.Hour).Unix()
	r.Usage("a", []Window{{Label: "Weekly", Used: 0, ResetsAt: first}})
	c.add(50 * time.Hour)
	r.Usage("a", []Window{{Label: "Weekly", Used: 100, ResetsAt: first}})
	c.t = time.Unix(first, 0).Add(time.Minute)
	second := first + 7*86400
	r.Usage("a", []Window{{Label: "Weekly", Used: 1, ResetsAt: second}})
	c.add(30 * time.Hour)
	r.Usage("a", []Window{{Label: "Weekly", Used: 100, ResetsAt: second}})
	c.add(time.Hour)
	r.Refused("a")
	rep := r.Report(now, 30, 3, []Account{{ID: "a", Provider: "codex", Name: "pro@example.com"}})
	w := rep.Accounts[0].Windows[0]
	if w.Hits != 2 || w.Cycles != 2 || math.Abs(w.Fastest-30) > 0.1 || math.Abs(w.AvgToEmpty-40) > 0.1 {
		t.Fatalf("drain: %+v", w)
	}
	if rep.Accounts[0].LimitHits != 2 {
		t.Fatalf("limit hits %d, want 2 (the refusal right after a limit is the same one)", rep.Accounts[0].LimitHits)
	}
	if len(rep.Events) == 0 || rep.Events[0].Name != "pro@example.com" {
		t.Fatalf("events: %+v", rep.Events)
	}
}

// A removed account keeps its name in its history.
func TestReportNamesRemovedAccounts(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	r, _ := recorderAt(t, now.Add(-time.Hour))
	r.Usage("gone", []Window{{Label: "Session", Used: 50}})
	r.Remember("gone", "claude", "old@example.com")
	rep := r.Report(now, 7, 3, nil)
	if len(rep.Accounts) != 1 || !rep.Accounts[0].Removed || rep.Accounts[0].Name != "old@example.com" || len(rep.Pools) != 0 {
		t.Fatalf("report: %+v", rep)
	}
}
