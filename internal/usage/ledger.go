package usage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// A Ledger is one computer's usage, condensed so that computers can share
// it: the token counts and cost of each quarter hour, provider and model,
// and the sessions with the quarter hours they ran. A host keeps its
// clients' ledgers and every computer adds them up in its own time zone, so
// each shows the same usage across all of them. Quarter hours are counted
// in UTC; every time zone is a whole number of them off UTC, so each falls
// in one local day.
type Ledger struct {
	Entries     []LedgerEntry `json:"entries"`
	Sessions    []SessionSpan `json:"sessions"`
	ScannedAtMs int64         `json:"scanned_at_ms"`
}

// LedgerEntry is the usage of one quarter hour, provider and model. It is
// sent as an array: [quarter, provider, model, [uncached input, cached
// input, cache creation, output, reasoning], cost, savings, records,
// unpriced].
type LedgerEntry struct {
	Quarter  int64 // quarter hours since the Unix epoch, UTC
	Provider string
	Model    string
	Totals   Totals
	Cost     float64
	Savings  float64
	Records  int64
	Unpriced int64
}

// SessionSpan is one session and the first and last quarter hour it had
// usage in. Its ID is a hash: the session's own id stays on the computer.
// It is sent as an array: [provider, id, first, last].
type SessionSpan struct {
	Provider string
	ID       string
	First    int64
	Last     int64
}

func (e LedgerEntry) MarshalJSON() ([]byte, error) {
	t := e.Totals
	return json.Marshal([]any{e.Quarter, e.Provider, e.Model,
		[5]int64{t.UncachedInput, t.CachedInput, t.CacheCreation, t.Output, t.Reasoning},
		amount(e.Cost), amount(e.Savings), e.Records, e.Unpriced})
}

func (e *LedgerEntry) UnmarshalJSON(data []byte) error {
	var raw [8]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var t [5]int64
	var out LedgerEntry
	for i, field := range []any{&out.Quarter, &out.Provider, &out.Model, &t, &out.Cost, &out.Savings, &out.Records, &out.Unpriced} {
		if err := json.Unmarshal(raw[i], field); err != nil {
			return errInvalidLedger
		}
	}
	out.Totals = Totals{UncachedInput: t[0], CachedInput: t[1], CacheCreation: t[2], Output: t[3], Reasoning: t[4]}
	*e = out
	return nil
}

func (s SessionSpan) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{s.Provider, s.ID, s.First, s.Last})
}

func (s *SessionSpan) UnmarshalJSON(data []byte) error {
	var raw [4]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var out SessionSpan
	for i, field := range []any{&out.Provider, &out.ID, &out.First, &out.Last} {
		if err := json.Unmarshal(raw[i], field); err != nil {
			return errInvalidLedger
		}
	}
	*s = out
	return nil
}

var errInvalidLedger = errors.New("invalid usage ledger")

// UnmarshalJSON reads a ledger one entry at a time and refuses one longer
// than a ledger may be, so a ledger from another computer takes no more
// memory than an honest one.
func (l *Ledger) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errInvalidLedger
	}
	var out Ledger
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return errInvalidLedger
		}
		switch tok {
		case "entries":
			err = decodeList(dec, func() int { return len(out.Entries) }, maxLedgerEntries, func(e LedgerEntry) { out.Entries = append(out.Entries, e) })
		case "sessions":
			err = decodeList(dec, func() int { return len(out.Sessions) }, maxLedgerSessions, func(s SessionSpan) { out.Sessions = append(out.Sessions, s) })
		case "scanned_at_ms":
			err = dec.Decode(&out.ScannedAtMs)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return errInvalidLedger
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return errInvalidLedger
	}
	*l = out
	return nil
}

// decodeList reads a JSON array (or null) one element at a time, refusing
// it once it would hold more than limit.
func decodeList[T any](dec *json.Decoder, held func() int, limit int, add func(T)) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if tok != json.Delim('[') {
		return errInvalidLedger
	}
	for dec.More() {
		if held() >= limit {
			return errInvalidLedger
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return err
		}
		add(v)
	}
	_, err = dec.Token()
	return err
}

// DeviceLedgers is a list of computers' ledgers, read one at a time and no
// more than a host keeps.
type DeviceLedgers []DeviceLedger

func (d *DeviceLedgers) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out DeviceLedgers
	if err := decodeList(dec, func() int { return len(out) }, MaxDevices, func(l DeviceLedger) { out = append(out, l) }); err != nil {
		return errInvalidLedger
	}
	*d = out
	return nil
}

// LedgerDays is how far back a ledger reaches: the longest window shown,
// and a day more for time zones ahead of UTC.
const LedgerDays = 91

// Limits on a ledger taken from another computer. A computer's own ledger
// is cut to them before it is sent: the oldest usage goes first.
const (
	// A quarter hour's entries are one per model used in it: three models
	// around the clock for 92 days are 26,500.
	maxLedgerEntries  = 50_000
	maxLedgerSessions = 50_000
	maxLedgerName     = 128
	// maxLedgerTotal bounds each count summed over one ledger, so the
	// ledgers of every computer a summary adds up cannot overflow.
	maxLedgerTotal = 1 << 54
)

// MaxDevices is how many computers' ledgers are added up at most: those a
// host keeps and the host's own.
const MaxDevices = maxStoredDevices + 1

// quarterSeconds is the span of a ledger entry.
const quarterSeconds = 15 * 60

func unixQuarter(t time.Time) int64 { return t.Unix() / quarterSeconds }

// knownProviders are the providers whose usage is counted: names from
// another computer are taken only from this list, as they are shown.
var knownProviders = map[string]bool{ProviderClaude: true, ProviderCodex: true, ProviderGrok: true}

// collectLedger scans the sources for records since start and condenses
// them, deduplicated and priced, into a ledger.
func collectLedger(sources []Source, cache *ScanCache, table RateTable, start time.Time) *Ledger {
	type key struct {
		quarter         int64
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
		quarter := unixQuarter(r.Timestamp)
		cost, source := Price(table, r)
		model := ledgerModel(r.Model)
		k := key{quarter, r.Provider, model}
		e := entries[k]
		if e == nil {
			e = &LedgerEntry{Quarter: quarter, Provider: r.Provider, Model: model}
			entries[k] = e
		}
		e.Totals = boundedSum(e.Totals, r.Totals)
		e.Cost += cost
		e.Savings += CacheSavings(table, r)
		e.Records = count(e.Records + 1)
		if source == CostUnpriced {
			e.Unpriced = count(e.Unpriced + 1)
		}
		if r.SessionID != "" {
			sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.SessionID))
			id := hex.EncodeToString(sum[:8])
			sk := [2]string{r.Provider, id}
			span := sessions[sk]
			if span == nil {
				span = &SessionSpan{Provider: r.Provider, ID: id, First: quarter, Last: quarter}
				sessions[sk] = span
			}
			span.First, span.Last = min(span.First, quarter), max(span.Last, quarter)
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
		if a.Quarter != b.Quarter {
			return a.Quarter < b.Quarter
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
	sort.Slice(l.Sessions, func(i, j int) bool { return l.Sessions[i].ID < l.Sessions[j].ID })
	l.bound()
	return l
}

// ForSending is the ledger cut to what another computer takes, the newest
// usage kept; this computer's own view keeps all of it. The ledger itself
// is not changed.
func (l *Ledger) ForSending() *Ledger {
	out := *l
	if len(out.Entries) > maxLedgerEntries {
		out.Entries = out.Entries[len(out.Entries)-maxLedgerEntries:]
	}
	if len(out.Sessions) > maxLedgerSessions {
		sessions := append([]SessionSpan(nil), out.Sessions...)
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].Last > sessions[j].Last })
		out.Sessions = sessions[:maxLedgerSessions]
	}
	return &out
}

// boundedSum adds two entries' counts, each bounded as a ledger's may be.
func boundedSum(a, b Totals) Totals {
	return Totals{
		UncachedInput: count(count(a.UncachedInput) + count(b.UncachedInput)),
		CachedInput:   count(count(a.CachedInput) + count(b.CachedInput)),
		CacheCreation: count(count(a.CacheCreation) + count(b.CacheCreation)),
		Output:        count(count(a.Output) + count(b.Output)),
		Reasoning:     count(count(a.Reasoning) + count(b.Reasoning)),
	}
}

// bound keeps every count whole, not negative and within what a ledger may
// hold in all: the entries that would take a total past it are left out,
// so adding up every computer's ledgers cannot overflow.
func (l *Ledger) bound() {
	var sum [7]int64
	entries := l.Entries[:0]
	for _, e := range l.Entries {
		e.Totals = Totals{UncachedInput: count(e.Totals.UncachedInput), CachedInput: count(e.Totals.CachedInput),
			CacheCreation: count(e.Totals.CacheCreation), Output: count(e.Totals.Output), Reasoning: count(e.Totals.Reasoning)}
		e.Cost, e.Savings = amount(e.Cost), amount(e.Savings)
		e.Records, e.Unpriced = count(e.Records), count(e.Unpriced)
		e.Unpriced = min(e.Unpriced, e.Records)
		counts := [7]int64{e.Totals.UncachedInput, e.Totals.CachedInput, e.Totals.CacheCreation, e.Totals.Output, e.Totals.Reasoning, e.Records, e.Unpriced}
		fits := true
		for i, n := range counts {
			fits = fits && sum[i]+n <= maxLedgerTotal
		}
		if !fits {
			continue
		}
		for i, n := range counts {
			sum[i] += n
		}
		entries = append(entries, e)
	}
	l.Entries = entries
}

// ledgerModel keeps a model name to characters JSON carries as they are,
// and short: no '<', '>', '&', quotes or backslashes, which it writes as
// several bytes each, and nothing that is not plain ASCII. Model names are
// such already.
func ledgerModel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLedgerName {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ._:/@+()[]=,-", r) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
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
	first, last := unixQuarter(start), unixQuarter(end)
	// Days start on a quarter hour in every time zone there is; in one that
	// did not, a quarter that began before the day is the previous day's.
	if start.Unix()%quarterSeconds != 0 {
		first++
	}
	if end.Unix()%quarterSeconds != 0 {
		last++
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
			if e.Quarter < first || e.Quarter >= last {
				continue
			}
			day := time.Unix(e.Quarter*quarterSeconds, 0).In(loc).Format("2006-01-02")
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
			if s.Last < first || s.First >= last {
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
// the last LedgerDays and no future, known providers, plain short model
// names, counts that are whole and not negative, totals that cannot
// overflow, and bounded sizes. It reports whether anything is left.
func (l *Ledger) Clean(now time.Time) bool {
	if l == nil {
		return false
	}
	oldest, newest := unixQuarter(now.AddDate(0, 0, -LedgerDays-1)), unixQuarter(now.Add(24*time.Hour))
	entries := l.Entries[:0]
	for _, e := range l.Entries {
		if len(entries) >= maxLedgerEntries || e.Quarter < oldest || e.Quarter > newest || !knownProviders[e.Provider] {
			continue
		}
		e.Model = ledgerModel(e.Model)
		entries = append(entries, e)
	}
	l.Entries = entries
	l.bound()
	sessions := l.Sessions[:0]
	for _, s := range l.Sessions {
		if len(sessions) >= maxLedgerSessions || s.Last < oldest || s.First > newest || s.First > s.Last || !knownProviders[s.Provider] {
			continue
		}
		if len(s.ID) == 0 || len(s.ID) > 32 || strings.Trim(s.ID, "0123456789abcdef") != "" {
			continue
		}
		sessions = append(sessions, s)
	}
	l.Sessions = sessions
	return true
}

// count is a token or record count as it may be: whole, not negative, and
// no more than a ledger may hold in all.
func count(n int64) int64 { return min(max(n, 0), maxLedgerTotal) }

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
