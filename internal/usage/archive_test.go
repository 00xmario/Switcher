package usage

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func claudeLogLine(at time.Time, id string, output int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"s-%s","message":{"id":%q,"model":"fixture-model","usage":{"input_tokens":1,"output_tokens":%d}}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), id, id, output)
}

func offlineClient() *http.Client {
	return &http.Client{Transport: ratesTransport(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("offline")
	})}
}

// Usage stays counted after the CLI deletes its log (Claude Code deletes a
// transcript 30 days after it was last written), also after a restart.
func TestArchiveKeepsUsageWhoseLogIsGone(t *testing.T) {
	now := time.Now()
	s := fixtureService(t, offlineClient(), now)
	if s.Scan(1) == nil {
		t.Fatal("no summary")
	}
	before := s.Get(1).TotalTokens
	if before == 0 {
		t.Fatal("the log was not counted")
	}
	projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")
	if err := os.Remove(filepath.Join(projects, "fixture.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := s.Scan(1); got == nil || got.TotalTokens != before {
		t.Fatalf("after the log was deleted: %d tokens, want %d", got.TotalTokens, before)
	}
	again := NewService(s.dataDir, offlineClient())
	again.now = s.now
	if got := again.Scan(1); got == nil || got.TotalTokens != before {
		t.Fatalf("after a restart: %d tokens, want %d", got.TotalTokens, before)
	}
	// A new log counts too.
	writeUsageFile(t, filepath.Join(projects, "later.jsonl"), claudeLogLine(now.Add(-30*time.Minute), "later", 41))
	again.Scan(2)
	if got := again.Get(2); got == nil || got.TotalTokens != before+42 {
		t.Fatalf("a new log: %d tokens, want %d", got.TotalTokens, before+42)
	}
}

// The first scan with an archive reads every log on disk, however old, and
// keeps old usage per hour; later scans read only the recent window.
func TestFirstScanReadsEveryLog(t *testing.T) {
	now := time.Now()
	s := fixtureService(t, offlineClient(), now)
	projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")
	old := now.AddDate(-1, 0, -3)
	path := filepath.Join(projects, "old.jsonl")
	writeUsageFile(t, path, claudeLogLine(old, "a", 9)+claudeLogLine(old.Add(20*time.Minute), "b", 9))
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	s.Scan(1)
	ins := s.Insights(0)
	// Two old lines of 10 tokens each, and the fixture's one of today.
	if ins == nil || ins.Since != old.Format("2006-01-02") || ins.Totals.Tokens != 21 || ins.Totals.ActiveDays != 2 {
		t.Fatalf("all time: since %s, %d tokens on %d days", ins.Since, ins.Totals.Tokens, ins.Totals.ActiveDays)
	}
	var oldEntries []LedgerEntry
	for _, e := range s.Own().Entries {
		if e.Quarter < unixQuarter(now.AddDate(0, 0, -compactAfterDays)) {
			oldEntries = append(oldEntries, e)
		}
	}
	if len(oldEntries) == 0 {
		t.Fatal("old usage was not kept")
	}
	for _, e := range oldEntries {
		if e.Quarter%quartersPerBucket != 0 {
			t.Fatalf("old usage is not kept per half hour: %+v", e)
		}
	}
	if !s.backfilled {
		t.Fatal("the archive does not say it holds every log")
	}
}

// Merging: an entry with more records wins, one with fewer does not, an
// entry the scan lacks stays, old quarter hours are added up per hour, and
// sessions widen to cover both.
func TestMergeArchive(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	oldHour := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	archive := &Ledger{
		Entries: []LedgerEntry{
			{Quarter: unixQuarter(recent), Provider: "claude", Model: "m", Totals: Totals{Output: 50}, Records: 5},
			{Quarter: unixQuarter(recent) + 1, Provider: "claude", Model: "m", Totals: Totals{Output: 10}, Records: 1},
			{Quarter: unixQuarter(recent) + 2, Provider: "codex", Model: "gone", Totals: Totals{Output: 7}, Records: 1},
			{Quarter: unixQuarter(oldHour), Provider: "claude", Model: "m", Totals: Totals{Output: 1}, Records: 1},
			{Quarter: unixQuarter(oldHour) + 2, Provider: "claude", Model: "m", Totals: Totals{Output: 1}, Records: 1},
			{Quarter: unixQuarter(oldHour) + 3, Provider: "claude", Model: "m", Totals: Totals{Output: 1}, Records: 1},
		},
		Sessions: []SessionSpan{{Provider: "claude", ID: "aa", First: 10, Last: 20, Active: 6}, {Provider: "codex", ID: "cc", First: 1, Last: 5, Active: 4}},
	}
	scanned := &Ledger{
		Entries: []LedgerEntry{
			{Quarter: unixQuarter(recent), Provider: "claude", Model: "m", Totals: Totals{Output: 40}, Records: 4},
			{Quarter: unixQuarter(recent) + 1, Provider: "claude", Model: "m", Totals: Totals{Output: 30}, Records: 3},
		},
		// aa is the same span seen again; cc was resumed after the archive's
		// last quarter hour, so its active quarter hours add up.
		Sessions: []SessionSpan{{Provider: "claude", ID: "aa", First: 15, Last: 30, Active: 5}, {Provider: "codex", ID: "bb", First: 1, Last: 2, Active: 2},
			{Provider: "codex", ID: "cc", First: 900, Last: 902, Active: 3}},
	}
	merged, changed := mergeArchive(archive, scanned, now)
	if !changed {
		t.Fatal("not changed")
	}
	got := map[int64]int64{}
	for _, e := range merged.Entries {
		got[e.Quarter] += e.Totals.Output
	}
	want := map[int64]int64{unixQuarter(recent): 50, unixQuarter(recent) + 1: 30, unixQuarter(recent) + 2: 7, unixQuarter(oldHour): 1, unixQuarter(oldHour) + 2: 2}
	if len(got) != len(want) {
		t.Fatalf("entries: %+v", merged.Entries)
	}
	for q, n := range want {
		if got[q] != n {
			t.Errorf("quarter %d: %d tokens, want %d", q, got[q], n)
		}
	}
	if len(merged.Sessions) != 3 || merged.Sessions[0] != (SessionSpan{Provider: "claude", ID: "aa", First: 10, Last: 30, Active: 6}) ||
		merged.Sessions[2] != (SessionSpan{Provider: "codex", ID: "cc", First: 1, Last: 902, Active: 7}) {
		t.Fatalf("sessions: %+v", merged.Sessions)
	}
	if _, again := mergeArchive(merged, scanned, now); again {
		t.Fatal("merging the same scan again changed the archive")
	}
}

// The archive is this computer's own: it holds more than a ledger taken
// from another computer may, and reads back as written.
func TestArchiveHoldsMoreThanALedgerSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-archive.json")
	l := &Ledger{ScannedAtMs: 7}
	for i := 0; i < maxLedgerEntries+10; i++ {
		l.Entries = append(l.Entries, LedgerEntry{Quarter: int64(i) * quartersPerBucket, Provider: "claude", Model: "m", Totals: Totals{Output: 1}, Records: 1})
	}
	if err := saveArchive(path, l, true); err != nil {
		t.Fatal(err)
	}
	back, backfilled, err := loadArchive(path, time.Now())
	if err != nil || back == nil || !backfilled || len(back.Entries) != len(l.Entries) || back.ScannedAtMs != 7 {
		t.Fatalf("read back %v entries, backfilled %v", len(back.Entries), backfilled)
	}
	if sent := back.ForSending(); len(sent.Entries) != maxLedgerEntries || sent.Entries[len(sent.Entries)-1] != l.Entries[len(l.Entries)-1] {
		t.Fatal("what is sent is not the newest usage, cut to a ledger's size")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive file mode: %v %v", info.Mode(), err)
	}
	// Another version (a newer Switcher's) is set aside, never overwritten,
	// and the logs are read again; so is a damaged one.
	for i, body := range []string{`{"version":99,"backfilled":true,"entries":[]}`, `{"version":1,"entr`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(int64(1000+i), 0)
		back, backfilled, err := loadArchive(path, at)
		if err != nil || back != nil || backfilled {
			t.Fatalf("%s: %v %v %v", body, back, backfilled, err)
		}
		if kept, err := os.ReadFile(fmt.Sprintf("%s.set-aside-%d", path, at.Unix())); err != nil || string(kept) != body {
			t.Fatalf("%s was not kept: %q %v", body, kept, err)
		}
	}
	// None is no archive, and no error.
	if back, _, err := loadArchive(path, time.Now()); back != nil || err != nil {
		t.Fatalf("no archive: %v %v", back, err)
	}
}

// An archive that cannot be read this time is not overwritten: the scan
// shows what the logs have, and the next one reads it again.
func TestUnreadableArchiveIsNotOverwritten(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any file")
	}
	now := time.Now()
	s := fixtureService(t, offlineClient(), now)
	path := filepath.Join(s.dataDir, "usage-archive.json")
	if err := os.WriteFile(path, []byte("kept"), 0o000); err != nil {
		t.Fatal(err)
	}
	if s.Scan(1) == nil || s.Get(1).TotalTokens == 0 {
		t.Fatal("the logs were not counted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "kept" {
		t.Fatalf("the archive was overwritten: %q", data)
	}
}

// A record without a time (it reads as 1970) is not usage of 1970.
func TestRecordsWithoutATimeAreLeftOut(t *testing.T) {
	now := time.Now()
	s := fixtureService(t, offlineClient(), now)
	projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")
	writeUsageFile(t, filepath.Join(projects, "epoch.jsonl"), claudeLogLine(time.Unix(0, 0), "epoch", 5))
	s.Scan(1)
	if in := s.Insights(0); in.Since != now.Format("2006-01-02") {
		t.Fatalf("all time starts %s", in.Since)
	}
}
