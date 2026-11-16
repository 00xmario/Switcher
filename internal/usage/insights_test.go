package usage

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func insightEntry(t time.Time, provider, model string, tokens int64, cost float64) LedgerEntry {
	return LedgerEntry{Quarter: unixQuarter(t), Provider: provider, Model: model, Totals: Totals{Output: tokens}, Cost: cost, Records: 1}
}

func insightDevices() []DeviceLedger {
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, time.UTC) }
	this := &Ledger{
		Entries: []LedgerEntry{
			insightEntry(at(11, 9, 9), "claude", "m1", 20, 0),
			insightEntry(at(11, 10, 9), "claude", "m1", 10, 0),
			insightEntry(at(11, 14, 22), "codex", "m2", 300, 3),
			insightEntry(at(11, 15, 10), "claude", "m1", 50, 0.5),
			insightEntry(at(11, 16, 10), "claude", "m1", 100, 1),
		},
		Sessions: []SessionSpan{
			{Provider: "claude", ID: "a1", First: unixQuarter(at(11, 16, 10)), Last: unixQuarter(at(11, 16, 10)) + 7},
			{Provider: "codex", ID: "a2", First: unixQuarter(at(11, 14, 22)), Last: unixQuarter(at(11, 14, 22))},
		},
	}
	other := &Ledger{
		Entries: []LedgerEntry{
			insightEntry(at(10, 1, 12), "claude", "m1", 1000, 10),
			insightEntry(at(11, 15, 11), "codex", "m2", 400, 4),
		},
		Sessions: []SessionSpan{
			{Provider: "codex", ID: "b1", First: unixQuarter(at(11, 15, 11)), Last: unixQuarter(at(11, 15, 11)) + 3},
		},
	}
	return []DeviceLedger{{Key: "this", Name: "MacBook", Ledger: this}, {Key: "other", Name: "Studio", Host: true, Ledger: other}}
}

// All time, every computer: totals, streaks, peaks, the week and the day,
// months, models, tools, sessions, computers and the week and month so far
// against the ones before.
func TestInsightsAllTime(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC) // a Monday
	in := BuildInsights(insightDevices(), "this", 0, now, time.UTC)
	if in.Since != "2026-10-01" || in.Until != "2026-11-16" || in.FirstDay != "2026-10-01" || len(in.Days) != 47 {
		t.Fatalf("range: %s to %s, first %s, %d days", in.Since, in.Until, in.FirstDay, len(in.Days))
	}
	tot := in.Totals
	if tot.Tokens != 1880 || math.Abs(tot.Cost-18.5) > 1e-9 || tot.ActiveDays != 6 || tot.Days != 47 || tot.Sessions != 3 {
		t.Fatalf("totals: %+v", tot)
	}
	if in.Streaks != (Streaks{Current: 3, Longest: 3, LongestFrom: "2026-11-14", LongestTo: "2026-11-16"}) {
		t.Fatalf("streaks: %+v", in.Streaks)
	}
	if in.PeakDay == nil || in.PeakDay.Day != "2026-10-01" || in.PeakCostDay == nil || in.PeakCostDay.Day != "2026-10-01" {
		t.Fatalf("peaks: %+v %+v", in.PeakDay, in.PeakCostDay)
	}
	if in.PeakHour == nil || in.PeakHour.Tokens != 1000 || in.PeakHour.Start != time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("peak hour: %+v", in.PeakHour)
	}
	byName := map[string]WeekdayStat{}
	for _, w := range in.Weekdays {
		byName[w.Weekday] = w
	}
	if in.Weekdays[0].Weekday != "Monday" || byName["Monday"].Tokens != 120 || byName["Saturday"].Tokens != 300 || byName["Sunday"].Tokens != 450 || byName["Sunday"].ActiveDays != 1 {
		t.Fatalf("weekdays: %+v", in.Weekdays)
	}
	if mondays := byName["Monday"].Days; mondays != 7 || math.Abs(byName["Monday"].AvgTokens-120.0/7) > 1e-9 {
		t.Fatalf("Mondays: %d, average %v", mondays, byName["Monday"].AvgTokens)
	}
	if in.Hours[10].Tokens != 150 || in.Hours[22].Tokens != 300 || in.Hours[12].Tokens != 1000 || in.Grid[0][10] != 100 || in.Grid[6][11] != 400 {
		t.Fatalf("hours %+v grid %+v", in.Hours, in.Grid)
	}
	if len(in.Months) != 2 || in.Months[0] != (MonthStat{Month: "2026-10", Tokens: 1000, Cost: 10, ActiveDays: 1}) || in.Months[1].Tokens != 880 || in.Months[1].Sessions != 3 {
		t.Fatalf("months: %+v", in.Months)
	}
	if len(in.Models) != 2 || in.Models[0].Model != "m1" || in.Models[0].Tokens != 1180 || in.Models[0].FirstDay != "2026-10-01" || in.Models[0].LastDay != "2026-11-16" || in.Models[0].ActiveDays != 5 {
		t.Fatalf("models: %+v", in.Models)
	}
	if len(in.Providers) != 2 || in.Providers[0].Provider != "claude" || in.Providers[0].Sessions != 1 || in.Providers[1].Sessions != 2 {
		t.Fatalf("providers: %+v", in.Providers)
	}
	if in.Sessions.Count != 3 || in.Sessions.MedianMinutes != 60 || math.Abs(in.Sessions.AvgMinutes-65) > 1e-9 {
		t.Fatalf("sessions: %+v", in.Sessions)
	}
	if in.LongestSession == nil || *in.LongestSession != (LongestSession{Provider: "claude", Minutes: 120, Day: "2026-11-16"}) {
		t.Fatalf("longest session: %+v", in.LongestSession)
	}
	if len(in.Devices) != 2 || in.Devices[0].Name != "Studio" || in.Devices[0].Tokens != 1400 || !in.Devices[1].This || in.Devices[1].Tokens != 480 {
		t.Fatalf("devices: %+v", in.Devices)
	}
	c := in.Compare
	if c.ThisWeek.Tokens != 100 || c.LastWeek.Tokens != 20 || c.LastWeek.From != "2026-11-09" || c.LastWeek.To != "2026-11-09" ||
		c.ThisMonth.Tokens != 880 || c.LastMonth.Tokens != 1000 || c.LastMonth.To != "2026-10-16" {
		t.Fatalf("compare: %+v", c)
	}
}

// A window of days counts only those days; idle days are in the series.
func TestInsightsWindow(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	in := BuildInsights(insightDevices(), "this", 7, now, time.UTC)
	if in.Range != "7" || in.Since != "2026-11-10" || len(in.Days) != 7 || in.Totals.Tokens != 860 || in.PeakDay.Day != "2026-11-15" || in.Totals.ActiveDays != 4 {
		t.Fatalf("7 days: since %s, %d days, %d tokens, peak %+v, %d active", in.Since, len(in.Days), in.Totals.Tokens, in.PeakDay, in.Totals.ActiveDays)
	}
	if in.Days[1].Tokens != 0 || in.Days[1].Day != "2026-11-11" {
		t.Fatalf("an idle day: %+v", in.Days[1])
	}
	// Today without usage yet keeps the streak that ended yesterday.
	later := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	if in := BuildInsights(insightDevices(), "this", 0, later, time.UTC); in.Streaks.Current != 3 {
		t.Fatalf("current streak the next morning: %d", in.Streaks.Current)
	}
	if in := BuildInsights(insightDevices(), "this", 0, later.AddDate(0, 0, 1), time.UTC); in.Streaks.Current != 0 {
		t.Fatalf("current streak after a day off: %d", in.Streaks.Current)
	}
}

// Days are local: an hour of usage late in the evening in UTC falls on the
// next day further east.
func TestInsightsUseTheLocalDay(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*3600)
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, tokyo)
	in := BuildInsights(insightDevices(), "this", 0, now, tokyo)
	for _, d := range in.Days {
		if d.Day == "2026-11-15" && d.Tokens != 300+50+400 {
			t.Fatalf("15 Nov in Tokyo: %d tokens", d.Tokens)
		}
	}
	if in.Hours[7].Tokens != 300 || in.Hours[19].Tokens != 150 {
		t.Fatalf("hours in Tokyo: %+v", in.Hours)
	}
}

// Nothing yet: an empty report, not a crash.
func TestInsightsWithoutUsage(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	in := BuildInsights([]DeviceLedger{{Key: "this", Ledger: &Ledger{}}}, "this", 0, now, time.UTC)
	if in.Totals.Tokens != 0 || in.PeakDay != nil || in.Streaks.Current != 0 || len(in.Days) != 1 || len(in.Weekdays) != 7 || len(in.Hours) != 24 {
		t.Fatalf("empty: %+v", in)
	}
}

// A session resumed days later counts the quarter hours it was active, not
// the days between; a Switcher that sends four fields (no active count) is
// still read, and one that reads four takes the five sent now.
func TestSessionLengthIsActiveTime(t *testing.T) {
	s := SessionSpan{Provider: "codex", ID: "ab", First: 1000, Last: 1000 + 4*96, Active: 4}
	if s.Minutes() != 60 {
		t.Fatalf("resumed session: %d minutes", s.Minutes())
	}
	if (SessionSpan{First: 10, Last: 13}).Minutes() != 60 {
		t.Fatal("a session without an active count is first to last")
	}
	raw, err := json.Marshal(s)
	if err != nil || string(raw) != `["codex","ab",1000,1384,4]` {
		t.Fatalf("sent as %s %v", raw, err)
	}
	var back SessionSpan
	if err := json.Unmarshal(raw, &back); err != nil || back != s {
		t.Fatalf("read back %+v %v", back, err)
	}
	var old SessionSpan
	if err := json.Unmarshal([]byte(`["codex","ab",1000,1384]`), &old); err != nil || old.Active != 0 || old.Last != 1384 {
		t.Fatalf("four fields: %+v %v", old, err)
	}
	var four [4]json.RawMessage // what a Switcher before active reads into
	if err := json.Unmarshal(raw, &four); err != nil || string(four[3]) != "1384" {
		t.Fatalf("an older Switcher cannot read it: %v", err)
	}
	l := &Ledger{Sessions: []SessionSpan{{Provider: "codex", ID: "ab", First: 10, Last: 20, Active: 50}}}
	if !l.Clean(time.Unix(20*quarterSeconds, 0)) || l.Sessions[0].Active != 11 {
		t.Fatalf("an active count past the span: %+v", l.Sessions)
	}
}
