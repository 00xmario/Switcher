package usage

import (
	"encoding/json"
	"os"
	"sort"
	"time"
)

// The archive is this computer's usage for all time, kept by Switcher in
// usage-archive.json. The CLIs' logs are the source of truth, but they do
// not last: Claude Code deletes a transcript 30 days after it was last
// written, by default. So every scan's ledger is added to the archive, and
// usage the logs no longer hold stays in it: an entry is replaced only by
// one counting at least as many records. (A quarter hour whose logs were
// deleted gets no new usage later: records are dated when written.)
//
// Entries of the last compactAfterDays keep their quarter hours, as a
// ledger has them; older ones are kept per hour, which is all that
// insights about them need, so the archive stays small for years of use.
// The first scan with an archive reads every log on disk, however old.

// archiveVersion is raised when what the archive holds changes meaning; an
// archive of another version is rebuilt from the logs.
const archiveVersion = 1

// compactAfterDays is how old an entry gets before it is kept per hour. It
// is past what a regular scan reads (LedgerDays), so a scan only ever meets
// entries kept per quarter hour.
const compactAfterDays = 100

// quartersPerHour: a ledger's quarter hours are counted from the Unix
// epoch, so an hour starts at a quarter that is a multiple of four.
const quartersPerHour = 4

// maxArchiveSessions bounds the sessions the archive keeps: the ones whose
// last usage is newest stay.
const maxArchiveSessions = 200_000

// archiveFile is the archive on disk. Its entries are not bounded the way
// a ledger from another computer is: they are this computer's own.
type archiveFile struct {
	Version     int           `json:"version"`
	Backfilled  bool          `json:"backfilled"`
	ScannedAtMs int64         `json:"scanned_at_ms"`
	Entries     []LedgerEntry `json:"entries"`
	Sessions    []SessionSpan `json:"sessions"`
}

// loadArchive reads the archive, or nil when there is none of this version.
// backfilled says whether every log on disk has been read into it.
func loadArchive(path string) (ledger *Ledger, backfilled bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var f archiveFile
	if json.Unmarshal(data, &f) != nil || f.Version != archiveVersion {
		return nil, false
	}
	l := &Ledger{Entries: f.Entries, Sessions: f.Sessions, ScannedAtMs: f.ScannedAtMs}
	l.bound()
	return l, f.Backfilled
}

// saveArchive writes the archive in place of the old one, readable by this
// user only.
func saveArchive(path string, l *Ledger, backfilled bool) error {
	data, err := json.Marshal(archiveFile{Version: archiveVersion, Backfilled: backfilled,
		ScannedAtMs: l.ScannedAtMs, Entries: l.Entries, Sessions: l.Sessions})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

type entryKey struct {
	quarter         int64
	provider, model string
}

// compacted returns the entries with those before cut moved to the hour
// they fall in, the entries of one hour, provider and model added up.
func compacted(entries []LedgerEntry, cut int64) map[entryKey]LedgerEntry {
	out := make(map[entryKey]LedgerEntry, len(entries))
	for _, e := range entries {
		if e.Quarter < cut {
			e.Quarter -= ((e.Quarter % quartersPerHour) + quartersPerHour) % quartersPerHour
		}
		k := entryKey{e.Quarter, e.Provider, e.Model}
		if old, ok := out[k]; ok {
			e.Totals = boundedSum(old.Totals, e.Totals)
			e.Cost, e.Savings = old.Cost+e.Cost, old.Savings+e.Savings
			e.Records, e.Unpriced = count(old.Records+e.Records), count(old.Unpriced+e.Unpriced)
		}
		out[k] = e
	}
	return out
}

// mergeArchive adds a scan's ledger to the archive and reports whether the
// archive changed. For each quarter hour (or, when old, hour), provider and
// model, the entry counting more records wins, the scan's when they count
// as many (its prices are the latest); an entry the scan no longer has, its
// log gone, stays. Neither ledger is changed.
func mergeArchive(archive, scanned *Ledger, now time.Time) (*Ledger, bool) {
	cut := unixQuarter(now.AddDate(0, 0, -compactAfterDays))
	var old []LedgerEntry
	var oldSessions []SessionSpan
	if archive != nil {
		old, oldSessions = archive.Entries, archive.Sessions
	}
	merged := compacted(old, cut)
	changed := archive == nil || len(merged) != len(old)
	if scanned != nil {
		for k, e := range compacted(scanned.Entries, cut) {
			have, ok := merged[k]
			if ok && have.Records > e.Records {
				continue
			}
			if !ok || have != e {
				changed = true
			}
			merged[k] = e
		}
	}
	out := &Ledger{Entries: make([]LedgerEntry, 0, len(merged))}
	if archive != nil {
		out.ScannedAtMs = archive.ScannedAtMs
	}
	if scanned != nil {
		out.ScannedAtMs = scanned.ScannedAtMs
	}
	for _, e := range merged {
		out.Entries = append(out.Entries, e)
	}
	sortEntries(out.Entries)

	spans := make(map[[2]string]SessionSpan, len(oldSessions))
	for _, s := range oldSessions {
		spans[[2]string{s.Provider, s.ID}] = s
	}
	if len(spans) != len(oldSessions) {
		changed = true
	}
	if scanned != nil {
		for _, s := range scanned.Sessions {
			k := [2]string{s.Provider, s.ID}
			have, ok := spans[k]
			if ok {
				s.First, s.Last, s.Active = min(s.First, have.First), max(s.Last, have.Last), max(s.Active, have.Active)
			}
			if !ok || have != s {
				changed = true
			}
			spans[k] = s
		}
	}
	out.Sessions = make([]SessionSpan, 0, len(spans))
	for _, s := range spans {
		out.Sessions = append(out.Sessions, s)
	}
	if len(out.Sessions) > maxArchiveSessions {
		sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].Last > out.Sessions[j].Last })
		out.Sessions = out.Sessions[:maxArchiveSessions]
		changed = true
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Provider < b.Provider
	})
	out.bound()
	return out, changed
}

// sortEntries orders entries by time, then provider and model, as a ledger
// keeps them: ForSending relies on the newest coming last.
func sortEntries(entries []LedgerEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Quarter != b.Quarter {
			return a.Quarter < b.Quarter
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Model < b.Model
	})
}
