package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// A Ledger is one computer's usage, condensed so that computers can share
// it: the token counts and cost of each hour, provider and model, and the
// sessions with the hours they ran. A host keeps its clients' ledgers and
// every computer adds them up in its own time zone, so each shows the same
// usage across all of them. Hours are counted in UTC; in a time zone half an
// hour off UTC, a record in the last half hour of a day can count for the
// next.
type Ledger struct {
	Entries     []LedgerEntry `json:"entries"`
	Sessions    []SessionSpan `json:"sessions"`
	ScannedAtMs int64         `json:"scanned_at_ms"`
}

// LedgerEntry is the usage of one hour, provider and model.
type LedgerEntry struct {
	Hour     int64   `json:"h"` // hours since the Unix epoch, UTC
	Provider string  `json:"p"`
	Model    string  `json:"m"`
	Totals   Totals  `json:"t"`
	Cost     float64 `json:"c"`
	Savings  float64 `json:"s"`
	Records  int64   `json:"r"`
	Unpriced int64   `json:"u"`
}

// SessionSpan is one session and the first and last hour it had usage in.
// Its ID is a hash: the session's own id stays on the computer.
type SessionSpan struct {
	Provider string `json:"p"`
	ID       string `json:"i"`
	First    int64  `json:"f"`
	Last     int64  `json:"l"`
}

// LedgerDays is how far back a ledger reaches: the longest window shown,
// and a day more for time zones ahead of UTC.
const LedgerDays = 91

// Limits on a ledger taken from another computer.
const (
	maxLedgerEntries  = 200_000
	maxLedgerSessions = 200_000
	maxLedgerName     = 128
)

func unixHour(t time.Time) int64 { return t.Unix() / 3600 }

// collectLedger scans the sources for records since start and condenses
// them, deduplicated and priced, into a ledger.
func collectLedger(sources []Source, cache *ScanCache, table RateTable, start time.Time) *Ledger {
	type key struct {
		hour            int64
		provider, model string
	}
	entries := map[key]*LedgerEntry{}
	sessions := map[[2]string]*SessionSpan{}
	seen := map[string]struct{}{}
	startUTC := start.UTC()
	sink := func(r Record) {
		if r.Timestamp.Before(startUTC) {
			return
		}
		if r.DedupeKey != "" {
			dk := r.SourceDir + "\x00" + r.DedupeKey
			if _, dup := seen[dk]; dup {
				return
			}
			seen[dk] = struct{}{}
		}
		hour := unixHour(r.Timestamp)
		cost, source := Price(table, r)
		k := key{hour, r.Provider, r.Model}
		e := entries[k]
		if e == nil {
			e = &LedgerEntry{Hour: hour, Provider: r.Provider, Model: r.Model}
			entries[k] = e
		}
		e.Totals = addTotals(e.Totals, r.Totals)
		e.Cost += cost
		e.Savings += CacheSavings(table, r)
		e.Records++
		if source == CostUnpriced {
			e.Unpriced++
		}
		if r.SessionID != "" {
			sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.SessionID))
			id := hex.EncodeToString(sum[:8])
			sk := [2]string{r.Provider, id}
			span := sessions[sk]
			if span == nil {
				span = &SessionSpan{Provider: r.Provider, ID: id, First: hour, Last: hour}
				sessions[sk] = span
			}
			span.First, span.Last = min(span.First, hour), max(span.Last, hour)
		}
	}
	began := time.Now()
	for _, src := range sources {
		scanSource(src, scanOptions{WindowStart: start}, cache, sink)
	}
	cache.Save()
	l := &Ledger{ScannedAtMs: began.UnixMilli(), Entries: make([]LedgerEntry, 0, len(entries)), Sessions: make([]SessionSpan, 0, len(sessions))}
	for _, e := range entries {
		l.Entries = append(l.Entries, *e)
	}
	for _, s := range sessions {
		l.Sessions = append(l.Sessions, *s)
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	sort.Slice(l.Sessions, func(i, j int) bool { return l.Sessions[i].ID < l.Sessions[j].ID })
	return l
}

// DeviceLedger is one computer's ledger, as the computers that add them up
// know it.
type DeviceLedger struct {
	Key       string  `json:"key"`  // stable for one Switcher install; not its pairing
	Name      string  `json:"name"` // the computer's name, for the breakdown
	Host      bool    `json:"host,omitempty"`
	UpdatedMs int64   `json:"updated_ms"`
	Ledger    *Ledger `json:"ledger"`
}

// DeviceUsage is one computer's share of a summary.
type DeviceUsage struct {
	Name      string  `json:"name"`
	This      bool    `json:"this,omitempty"`
	Host      bool    `json:"host,omitempty"`
	Cost      float64 `json:"cost_usd"`
	Tokens    int64   `json:"tokens"`
	UpdatedMs int64   `json:"updated_ms"`
}

// Combine adds up the ledgers of every computer for the local days since to
// until, inclusive, in loc. self is the key of the computer asking, whose
// share is marked as its own.
func Combine(devices []DeviceLedger, self string, since, until time.Time, loc *time.Location) *Summary {
	if loc == nil {
		loc = time.Local
	}
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, loc)
	end := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	firstHour, endHour := unixHour(start), unixHour(end)
	if start.Unix()%3600 != 0 {
		firstHour++ // an hour that began before the day is the previous day's
	}
	if end.Unix()%3600 != 0 {
		endHour++
	}

	buckets := map[bucketKey]*bucket{}
	var totals Totals
	var totalCost, savings float64
	var records, unpriced int64
	sessions := map[[2]string]struct{}{}
	providerSessions := map[string]map[string]struct{}{}
	var shares []DeviceUsage
	for _, d := range devices {
		if d.Ledger == nil {
			continue
		}
		share := DeviceUsage{Name: d.Name, This: d.Key == self, Host: d.Host, UpdatedMs: d.UpdatedMs}
		for _, e := range d.Ledger.Entries {
			if e.Hour < firstHour || e.Hour >= endHour {
				continue
			}
			day := time.Unix(e.Hour*3600, 0).In(loc).Format("2006-01-02")
			b := bucketFor(buckets, bucketKey{day: day, provider: e.Provider, model: e.Model})
			b.totals = addTotals(b.totals, e.Totals)
			b.cost += e.Cost
			b.savings += e.Savings
			b.records += e.Records
			b.unpricedRecords += e.Unpriced
			totals = addTotals(totals, e.Totals)
			totalCost += e.Cost
			savings += e.Savings
			records += e.Records
			unpriced += e.Unpriced
			share.Cost += e.Cost
			share.Tokens += e.Totals.Processed()
		}
		for _, s := range d.Ledger.Sessions {
			if s.Last < firstHour || s.First >= endHour {
				continue
			}
			sessions[[2]string{s.Provider, s.ID}] = struct{}{}
			set := providerSessions[s.Provider]
			if set == nil {
				set = map[string]struct{}{}
				providerSessions[s.Provider] = set
			}
			set[s.ID] = struct{}{}
		}
		shares = append(shares, share)
	}
	summary := buildSummary(buckets, totals, totalCost, savings, records, unpriced, int64(len(sessions)))
	for i := range summary.Providers {
		summary.Providers[i].Sessions = int64(len(providerSessions[summary.Providers[i].Provider]))
	}
	// One computer alone needs no breakdown.
	if len(shares) > 1 {
		sort.SliceStable(shares, func(i, j int) bool {
			if shares[i].Cost != shares[j].Cost {
				return shares[i].Cost > shares[j].Cost
			}
			return shares[i].Name < shares[j].Name
		})
		summary.Devices = shares
	}
	summary.Since = start.Format("2006-01-02")
	summary.Until = until.Format("2006-01-02")
	summary.TimeZone = loc.String()
	return summary
}

// Clean makes a ledger from another computer safe to keep and show: only
// the last LedgerDays and no future, plain short names, counts that are
// whole and not negative, and bounded sizes. It reports whether anything
// is left.
func (l *Ledger) Clean(now time.Time) bool {
	if l == nil {
		return false
	}
	oldest, newest := unixHour(now.AddDate(0, 0, -LedgerDays-1)), unixHour(now)+24
	entries := l.Entries[:0]
	for _, e := range l.Entries {
		if len(entries) >= maxLedgerEntries || e.Hour < oldest || e.Hour > newest {
			continue
		}
		e.Provider, e.Model = plainName(e.Provider, 32), plainName(e.Model, maxLedgerName)
		if e.Provider == "" {
			continue
		}
		e.Totals = Totals{UncachedInput: count(e.Totals.UncachedInput), CachedInput: count(e.Totals.CachedInput),
			CacheCreation: count(e.Totals.CacheCreation), Output: count(e.Totals.Output), Reasoning: count(e.Totals.Reasoning)}
		e.Cost, e.Savings = amount(e.Cost), amount(e.Savings)
		e.Records, e.Unpriced = count(e.Records), count(e.Unpriced)
		e.Unpriced = min(e.Unpriced, e.Records)
		entries = append(entries, e)
	}
	l.Entries = entries
	sessions := l.Sessions[:0]
	for _, s := range l.Sessions {
		if len(sessions) >= maxLedgerSessions || s.Last < oldest || s.First > newest || s.First > s.Last {
			continue
		}
		s.Provider = plainName(s.Provider, 32)
		if s.Provider == "" || len(s.ID) == 0 || len(s.ID) > 32 || strings.Trim(s.ID, "0123456789abcdef") != "" {
			continue
		}
		sessions = append(sessions, s)
	}
	l.Sessions = sessions
	return true
}

// count is a token or record count as it may be: whole, not negative, and
// short of what adding thousands of them could overflow.
func count(n int64) int64 { return min(max(n, 0), 1<<50) }

// amount is a cost as it may be: a finite, non-negative number of dollars.
func amount(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return min(v, 1e9)
}

// plainName keeps a name printable and short.
func plainName(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit])
	}
	return strings.TrimSpace(s)
}
