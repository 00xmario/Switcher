package usage

import (
	"sort"
	"strconv"
	"time"
)

// Insights is the long view of usage: every computer's, for all time or the
// last days asked for, looked at from many sides. Days are local days in
// the time zone of the computer asking, as on the Usage page.
type Insights struct {
	Range     string `json:"range"` // "all" or a number of days
	Since     string `json:"since"` // the first day counted
	Until     string `json:"until"` // today
	TimeZone  string `json:"time_zone"`
	FirstDay  string `json:"first_day,omitempty"` // the first day with usage, in the range
	ScannedMs int64  `json:"scanned_at_ms"`
	// ProfileTokens is how much of the total is known only from the Codex
	// accounts' profiles at OpenAI: day totals, without cost, model or hour.
	ProfileTokens int64 `json:"codex_profile_tokens,omitempty"`

	Totals         InsightTotals   `json:"totals"`
	Streaks        Streaks         `json:"streaks"`
	PeakDay        *InsightDay     `json:"peak_day,omitempty"`      // most tokens
	PeakCostDay    *InsightDay     `json:"peak_cost_day,omitempty"` // most cost
	PeakHour       *PeakHour       `json:"peak_hour,omitempty"`     // the busiest single hour
	LongestSession *LongestSession `json:"longest_session,omitempty"`
	Sessions       SessionStats    `json:"sessions"`

	Days      []InsightDay      `json:"days"`     // every day of the range, idle ones too
	Weekdays  []WeekdayStat     `json:"weekdays"` // Monday first
	Hours     []HourStat        `json:"hours"`    // local hours of the day, 0 to 23
	Grid      [][]int64         `json:"grid"`     // tokens by weekday (Monday first) and hour
	Months    []MonthStat       `json:"months"`
	Models    []ModelInsight    `json:"models"`
	Providers []ProviderInsight `json:"providers"`
	Devices   []DeviceUsage     `json:"devices,omitempty"`
	Compare   Compare           `json:"compare"`
}

// InsightTotals adds up the range.
type InsightTotals struct {
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost_usd"`
	Totals       Totals  `json:"breakdown"`
	CacheSavings float64 `json:"cache_savings_usd"`
	CachedShare  float64 `json:"cached_share"` // of input, how much was read from cache
	Records      int64   `json:"records"`
	Unpriced     int64   `json:"unpriced_records"`
	Sessions     int64   `json:"sessions"`
	ActiveDays   int     `json:"active_days"`
	Days         int     `json:"days"` // from the first day with usage (or the range's first) to today
	AvgTokens    float64 `json:"avg_tokens_per_active_day"`
	AvgCost      float64 `json:"avg_cost_per_active_day"`
}

// Streaks are runs of days with usage.
type Streaks struct {
	Current     int    `json:"current"` // ending today, or yesterday when today has none yet
	Longest     int    `json:"longest"`
	LongestFrom string `json:"longest_from,omitempty"`
	LongestTo   string `json:"longest_to,omitempty"`
}

// InsightDay is one local day.
type InsightDay struct {
	Day       string             `json:"day"`
	Tokens    int64              `json:"tokens"`
	Cost      float64            `json:"cost_usd"`
	Sessions  int64              `json:"sessions"`
	Providers map[string]dayCell `json:"providers,omitempty"`
}

// WeekdayStat is one day of the week over the range.
type WeekdayStat struct {
	Weekday    string  `json:"weekday"`
	Tokens     int64   `json:"tokens"`
	Cost       float64 `json:"cost_usd"`
	Days       int     `json:"days"`        // how many of them the range has
	ActiveDays int     `json:"active_days"` // how many of them had usage
	AvgTokens  float64 `json:"avg_tokens"`  // per such day in the range
	AvgCost    float64 `json:"avg_cost_usd"`
}

// HourStat is one hour of the day over the range.
type HourStat struct {
	Hour   int     `json:"hour"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost_usd"`
}

// PeakHour is the single hour with the most tokens.
type PeakHour struct {
	Start  int64   `json:"start_ms"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost_usd"`
}

// MonthStat is one calendar month.
type MonthStat struct {
	Month      string  `json:"month"` // 2026-11
	Tokens     int64   `json:"tokens"`
	Cost       float64 `json:"cost_usd"`
	ActiveDays int     `json:"active_days"`
	Sessions   int64   `json:"sessions"`
}

// ModelInsight is one provider and model over the range.
type ModelInsight struct {
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Tokens     int64   `json:"tokens"`
	Cost       float64 `json:"cost_usd"`
	TokenShare float64 `json:"token_share"`
	CostShare  float64 `json:"cost_share"`
	FirstDay   string  `json:"first_day"`
	LastDay    string  `json:"last_day"`
	ActiveDays int     `json:"active_days"`
	Unpriced   bool    `json:"unpriced"`
}

// ProviderInsight is one provider (coding tool) over the range.
type ProviderInsight struct {
	Provider   string  `json:"provider"`
	Tokens     int64   `json:"tokens"`
	Cost       float64 `json:"cost_usd"`
	TokenShare float64 `json:"token_share"`
	CostShare  float64 `json:"cost_share"`
	Sessions   int64   `json:"sessions"`
	FirstDay   string  `json:"first_day"`
	LastDay    string  `json:"last_day"`
	ActiveDays int     `json:"active_days"`
}

// LongestSession is the session that was active the longest.
type LongestSession struct {
	Provider string `json:"provider"`
	Minutes  int64  `json:"minutes"`
	Day      string `json:"day"` // the day it started
}

// SessionStats describes the sessions that started in the range. A
// session's length is the quarter hours it had usage in (SessionSpan.Minutes),
// so it is known to 15 minutes, and a session resumed days later does not
// count the days between.
type SessionStats struct {
	Count         int64   `json:"count"`
	AvgMinutes    float64 `json:"avg_minutes"`
	MedianMinutes int64   `json:"median_minutes"`
	PerActiveDay  float64 `json:"per_active_day"`
}

// Compare sets this week and month so far against the same span before.
type Compare struct {
	ThisWeek  Period `json:"this_week"`
	LastWeek  Period `json:"last_week"`
	ThisMonth Period `json:"this_month"`
	LastMonth Period `json:"last_month"`
}

// Period is the usage of a span of days, inclusive.
type Period struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost_usd"`
}

var weekdayNames = [7]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// mondayFirst numbers the days of the week from Monday.
func mondayFirst(d time.Weekday) int { return (int(d) + 6) % 7 }

// BuildInsights looks at every computer's ledger for the last days local
// days up to now (inclusive), or for all time when days is 0. self is the
// key of the computer asking.
func BuildInsights(devices []DeviceLedger, self string, days int, now time.Time, loc *time.Location) *Insights {
	return BuildInsightsWith(devices, self, days, now, loc, nil)
}

// ProfileModel names, among the models, the usage known only as a day's
// total from the Codex accounts' profiles at OpenAI.
const ProfileModel = "Codex account history (day totals)"

// BuildInsightsWith is BuildInsights with the Codex accounts' own daily
// token totals, as OpenAI counts them (day to tokens, all accounts added
// up). They count usage on every computer and in the cloud, also where the
// logs are gone, but only as a day's total: no cost, model or hour. A day
// counts whichever is more, the logs' Codex tokens or OpenAI's total, so
// nothing is counted twice.
func BuildInsightsWith(devices []DeviceLedger, self string, days int, now time.Time, loc *time.Location, codexDays map[string]int64) *Insights {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	end := today.AddDate(0, 0, 1)
	var start time.Time
	if days > 0 {
		start = today.AddDate(0, 0, -(days - 1))
	} else {
		// All time: from the earliest usage of any computer, never later
		// than today.
		start = today
		for _, d := range devices {
			if d.Ledger == nil {
				continue
			}
			for _, e := range d.Ledger.Entries {
				if t := time.Unix(e.Quarter*quarterSeconds, 0).In(loc); t.Before(start) {
					start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
				}
			}
		}
	}
	if days == 0 {
		for day, tokens := range codexDays {
			if d, err := time.ParseInLocation("2006-01-02", day, loc); err == nil && tokens > 0 && d.Before(start) && !d.Before(historyFloor) {
				start = d
			}
		}
	}
	first, last := unixQuarter(start), unixQuarter(end)
	if start.Unix()%quarterSeconds != 0 {
		first++
	}
	if end.Unix()%quarterSeconds != 0 {
		last++
	}

	type dayAgg struct {
		tokens    int64
		cost      float64
		sessions  int64
		providers map[string]*dayCell
	}
	type modelAgg struct {
		ModelInsight
		days              map[string]bool
		unpriced, records int64
	}
	type providerAgg struct {
		ProviderInsight
		days     map[string]bool
		sessions map[string]bool
	}
	dayMap := map[string]*dayAgg{}
	var hours [24]HourStat
	var grid [7][24]int64
	hourTotals := map[int64]*PeakHour{}
	models := map[[2]string]*modelAgg{}
	providers := map[string]*providerAgg{}
	var shares []DeviceUsage
	out := &Insights{Range: "all", TimeZone: loc.String(), Until: today.Format("2006-01-02")}
	if days > 0 {
		out.Range = strconv.Itoa(days)
	}
	// Local time of a quarter hour, worked out once per quarter.
	type when struct {
		day        string
		hour, wday int
		hourStart  int64
	}
	whens := map[int64]when{}
	at := func(q int64) when {
		if w, ok := whens[q]; ok {
			return w
		}
		t := time.Unix(q*quarterSeconds, 0).In(loc)
		w := when{day: t.Format("2006-01-02"), hour: t.Hour(), wday: mondayFirst(t.Weekday()),
			hourStart: time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).UnixMilli()}
		whens[q] = w
		return w
	}
	t := &out.Totals
	for _, d := range devices {
		if d.Ledger == nil {
			continue
		}
		share := DeviceUsage{Name: d.Name, This: d.Key == self, Host: d.Host, UpdatedMs: d.UpdatedMs}
		for _, e := range d.Ledger.Entries {
			if e.Quarter < first || e.Quarter >= last {
				continue
			}
			w := at(e.Quarter)
			tokens := e.Totals.Processed()
			da := dayMap[w.day]
			if da == nil {
				da = &dayAgg{providers: map[string]*dayCell{}}
				dayMap[w.day] = da
			}
			da.tokens += tokens
			da.cost += e.Cost
			cell := da.providers[e.Provider]
			if cell == nil {
				cell = &dayCell{}
				da.providers[e.Provider] = cell
			}
			cell.Tokens += tokens
			cell.Cost += e.Cost

			hours[w.hour].Tokens += tokens
			hours[w.hour].Cost += e.Cost
			grid[w.wday][w.hour] += tokens
			ph := hourTotals[w.hourStart]
			if ph == nil {
				ph = &PeakHour{Start: w.hourStart}
				hourTotals[w.hourStart] = ph
			}
			ph.Tokens += tokens
			ph.Cost += e.Cost

			mk := [2]string{e.Provider, e.Model}
			ma := models[mk]
			if ma == nil {
				ma = &modelAgg{ModelInsight: ModelInsight{Provider: e.Provider, Model: e.Model, FirstDay: w.day, LastDay: w.day}, days: map[string]bool{}}
				models[mk] = ma
			}
			ma.Tokens += tokens
			ma.Cost += e.Cost
			ma.records += e.Records
			ma.unpriced += e.Unpriced
			ma.days[w.day] = true
			ma.FirstDay, ma.LastDay = minDay(ma.FirstDay, w.day), maxDay(ma.LastDay, w.day)

			pa := providers[e.Provider]
			if pa == nil {
				pa = &providerAgg{ProviderInsight: ProviderInsight{Provider: e.Provider, FirstDay: w.day, LastDay: w.day}, days: map[string]bool{}, sessions: map[string]bool{}}
				providers[e.Provider] = pa
			}
			pa.Tokens += tokens
			pa.Cost += e.Cost
			pa.days[w.day] = true
			pa.FirstDay, pa.LastDay = minDay(pa.FirstDay, w.day), maxDay(pa.LastDay, w.day)

			t.Tokens += tokens
			t.Cost += e.Cost
			t.Totals = addTotals(t.Totals, e.Totals)
			t.CacheSavings += e.Savings
			t.Records += e.Records
			t.Unpriced += e.Unpriced
			share.Tokens += tokens
			share.Cost += e.Cost
		}
		shares = append(shares, share)
	}

	// What OpenAI counted on a day beyond the logs' Codex tokens.
	startDay, endDay := start.Format("2006-01-02"), today.Format("2006-01-02")
	for day, tokens := range codexDays {
		if day < startDay || day > endDay || len(day) != 10 {
			continue
		}
		da := dayMap[day]
		if da == nil {
			da = &dayAgg{providers: map[string]*dayCell{}}
			dayMap[day] = da
		}
		cell := da.providers[ProviderCodex]
		if cell == nil {
			cell = &dayCell{}
			da.providers[ProviderCodex] = cell
		}
		extra := min(tokens, maxLedgerTotal) - cell.Tokens
		if extra <= 0 {
			continue
		}
		cell.Tokens += extra
		da.tokens += extra
		mk := [2]string{ProviderCodex, ProfileModel}
		ma := models[mk]
		if ma == nil {
			ma = &modelAgg{ModelInsight: ModelInsight{Provider: ProviderCodex, Model: ProfileModel, FirstDay: day, LastDay: day}, days: map[string]bool{}}
			models[mk] = ma
		}
		ma.Tokens += extra
		ma.records++
		ma.unpriced++
		ma.days[day] = true
		ma.FirstDay, ma.LastDay = minDay(ma.FirstDay, day), maxDay(ma.LastDay, day)
		pa := providers[ProviderCodex]
		if pa == nil {
			pa = &providerAgg{ProviderInsight: ProviderInsight{Provider: ProviderCodex, FirstDay: day, LastDay: day}, days: map[string]bool{}, sessions: map[string]bool{}}
			providers[ProviderCodex] = pa
		}
		pa.Tokens += extra
		pa.days[day] = true
		pa.FirstDay, pa.LastDay = minDay(pa.FirstDay, day), maxDay(pa.LastDay, day)
		t.Tokens += extra
		out.ProfileTokens += extra
	}

	// Sessions, each counted once on the day it started; a session seen by
	// two computers' ledgers (it cannot be, but a copy could) once.
	sessions := map[[2]string]SessionSpan{}
	for _, d := range devices {
		if d.Ledger == nil {
			continue
		}
		for _, s := range d.Ledger.Sessions {
			if s.First < first || s.First >= last || s.Last < s.First {
				continue
			}
			k := [2]string{s.Provider, s.ID}
			if have, ok := sessions[k]; ok {
				s.First, s.Last, s.Active = min(s.First, have.First), max(s.Last, have.Last), max(s.Active, have.Active)
			}
			sessions[k] = s
		}
	}
	var lengths []int64
	for k, s := range sessions {
		w := at(s.First)
		if da := dayMap[w.day]; da != nil {
			da.sessions++
		} else {
			dayMap[w.day] = &dayAgg{sessions: 1, providers: map[string]*dayCell{}}
		}
		if pa := providers[k[0]]; pa != nil {
			pa.sessions[k[1]] = true
		}
		minutes := s.Minutes()
		lengths = append(lengths, minutes)
		if out.LongestSession == nil || minutes > out.LongestSession.Minutes {
			out.LongestSession = &LongestSession{Provider: k[0], Minutes: minutes, Day: w.day}
		}
	}
	t.Sessions = int64(len(sessions))

	// Every day of the range, from the first with usage when it is all time.
	if days == 0 {
		var earliest string
		for day := range dayMap {
			if earliest == "" || day < earliest {
				earliest = day
			}
		}
		if earliest != "" {
			if s, err := time.ParseInLocation("2006-01-02", earliest, loc); err == nil {
				start = s
			}
		}
	}
	out.Since = start.Format("2006-01-02")
	var weekdays [7]WeekdayStat
	for i := range weekdays {
		weekdays[i].Weekday = weekdayNames[i]
	}
	months := map[string]*MonthStat{}
	run, runFrom := 0, ""
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		day := InsightDay{Day: key}
		da := dayMap[key]
		if da != nil {
			day.Tokens, day.Cost, day.Sessions = da.tokens, da.cost, da.sessions
			if len(da.providers) > 0 {
				day.Providers = map[string]dayCell{}
				for p, c := range da.providers {
					day.Providers[p] = *c
				}
			}
		}
		active := day.Tokens > 0
		out.Days = append(out.Days, day)
		t.Days++
		wd := &weekdays[mondayFirst(d.Weekday())]
		wd.Days++
		wd.Tokens += day.Tokens
		wd.Cost += day.Cost
		mk := d.Format("2006-01")
		m := months[mk]
		if m == nil {
			m = &MonthStat{Month: mk}
			months[mk] = m
		}
		m.Tokens += day.Tokens
		m.Cost += day.Cost
		m.Sessions += day.Sessions
		if active {
			t.ActiveDays++
			wd.ActiveDays++
			m.ActiveDays++
			if out.FirstDay == "" {
				out.FirstDay = key
			}
			if out.PeakDay == nil || day.Tokens > out.PeakDay.Tokens {
				p := day
				out.PeakDay = &p
			}
			if out.PeakCostDay == nil || day.Cost > out.PeakCostDay.Cost {
				p := day
				out.PeakCostDay = &p
			}
			if run == 0 {
				runFrom = key
			}
			run++
			if run > out.Streaks.Longest {
				out.Streaks.Longest, out.Streaks.LongestFrom, out.Streaks.LongestTo = run, runFrom, key
			}
		} else if key != out.Until {
			// Today without usage yet does not end the current streak.
			run = 0
		}
	}
	out.Streaks.Current = run
	if t.ActiveDays > 0 {
		t.AvgTokens = float64(t.Tokens) / float64(t.ActiveDays)
		t.AvgCost = t.Cost / float64(t.ActiveDays)
		out.Sessions.PerActiveDay = float64(t.Sessions) / float64(t.ActiveDays)
	}
	if input := t.Totals.UncachedInput + t.Totals.CachedInput + t.Totals.CacheCreation; input > 0 {
		t.CachedShare = float64(t.Totals.CachedInput) / float64(input)
	}
	for i := range weekdays {
		if weekdays[i].Days > 0 {
			weekdays[i].AvgTokens = float64(weekdays[i].Tokens) / float64(weekdays[i].Days)
			weekdays[i].AvgCost = weekdays[i].Cost / float64(weekdays[i].Days)
		}
	}
	out.Weekdays = weekdays[:]
	for h := range hours {
		hours[h].Hour = h
	}
	out.Hours = hours[:]
	out.Grid = make([][]int64, 7)
	for i := range grid {
		out.Grid[i] = append([]int64(nil), grid[i][:]...)
	}
	for _, ph := range hourTotals {
		if out.PeakHour == nil || ph.Tokens > out.PeakHour.Tokens || ph.Tokens == out.PeakHour.Tokens && ph.Start < out.PeakHour.Start {
			p := *ph
			out.PeakHour = &p
		}
	}
	for _, m := range months {
		out.Months = append(out.Months, *m)
	}
	sort.Slice(out.Months, func(i, j int) bool { return out.Months[i].Month < out.Months[j].Month })

	if len(lengths) > 0 {
		sort.Slice(lengths, func(i, j int) bool { return lengths[i] < lengths[j] })
		var sum int64
		for _, l := range lengths {
			sum += l
		}
		out.Sessions.Count = int64(len(lengths))
		out.Sessions.AvgMinutes = float64(sum) / float64(len(lengths))
		out.Sessions.MedianMinutes = lengths[len(lengths)/2]
	}

	for _, ma := range models {
		m := ma.ModelInsight
		m.ActiveDays = len(ma.days)
		m.Unpriced = ma.records > 0 && ma.unpriced*2 >= ma.records
		if t.Tokens > 0 {
			m.TokenShare = float64(m.Tokens) / float64(t.Tokens)
		}
		if t.Cost > 0 {
			m.CostShare = m.Cost / t.Cost
		}
		out.Models = append(out.Models, m)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		a, b := out.Models[i], out.Models[j]
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	for _, pa := range providers {
		p := pa.ProviderInsight
		p.ActiveDays = len(pa.days)
		p.Sessions = int64(len(pa.sessions))
		if t.Tokens > 0 {
			p.TokenShare = float64(p.Tokens) / float64(t.Tokens)
		}
		if t.Cost > 0 {
			p.CostShare = p.Cost / t.Cost
		}
		out.Providers = append(out.Providers, p)
	}
	sort.Slice(out.Providers, func(i, j int) bool {
		if out.Providers[i].Tokens != out.Providers[j].Tokens {
			return out.Providers[i].Tokens > out.Providers[j].Tokens
		}
		return out.Providers[i].Provider < out.Providers[j].Provider
	})
	if len(shares) > 1 {
		sort.SliceStable(shares, func(i, j int) bool {
			if shares[i].Tokens != shares[j].Tokens {
				return shares[i].Tokens > shares[j].Tokens
			}
			return shares[i].Name < shares[j].Name
		})
		out.Devices = shares
	}
	out.Compare = comparePeriods(devices, today, loc)
	return out
}

// comparePeriods adds up this week (from Monday) and this month so far,
// and the same number of days of the week and month before.
func comparePeriods(devices []DeviceLedger, today time.Time, loc *time.Location) Compare {
	weekStart := today.AddDate(0, 0, -mondayFirst(today.Weekday()))
	sinceWeek := int(today.Sub(weekStart).Hours()/24+0.5) + 1
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc)
	prevMonth := monthStart.AddDate(0, -1, 0)
	prevMonthEnd := prevMonth.AddDate(0, 0, today.Day()-1)
	if !prevMonthEnd.Before(monthStart) {
		prevMonthEnd = monthStart.AddDate(0, 0, -1)
	}
	return Compare{
		ThisWeek:  periodUsage(devices, weekStart, today, loc),
		LastWeek:  periodUsage(devices, weekStart.AddDate(0, 0, -7), weekStart.AddDate(0, 0, sinceWeek-8), loc),
		ThisMonth: periodUsage(devices, monthStart, today, loc),
		LastMonth: periodUsage(devices, prevMonth, prevMonthEnd, loc),
	}
}

// periodUsage adds up every computer's usage from one local day to another,
// both included.
func periodUsage(devices []DeviceLedger, from, to time.Time, loc *time.Location) Period {
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	first, last := unixQuarter(start), unixQuarter(end)
	if start.Unix()%quarterSeconds != 0 {
		first++
	}
	if end.Unix()%quarterSeconds != 0 {
		last++
	}
	p := Period{From: start.Format("2006-01-02"), To: to.Format("2006-01-02")}
	for _, d := range devices {
		if d.Ledger == nil {
			continue
		}
		for _, e := range d.Ledger.Entries {
			if e.Quarter >= first && e.Quarter < last {
				p.Tokens += e.Totals.Processed()
				p.Cost += e.Cost
			}
		}
	}
	return p
}

func minDay(a, b string) string {
	if a == "" || b < a {
		return b
	}
	return a
}

func maxDay(a, b string) string {
	if b > a {
		return b
	}
	return a
}
