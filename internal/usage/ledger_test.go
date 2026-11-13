package usage

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entryAt(t time.Time, provider, model string, tokens int64, cost float64) LedgerEntry {
	return LedgerEntry{Hour: unixHour(t), Provider: provider, Model: model, Totals: Totals{Output: tokens}, Cost: cost, Records: 1}
}

// Every computer's usage is added up, each computer's share is kept, and
// sessions are counted once each.
func TestCombineAddsUpEveryComputer(t *testing.T) {
	day := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	host := &Ledger{
		Entries:  []LedgerEntry{entryAt(day, "claude", "fable", 100, 1.5), entryAt(day.Add(time.Hour), "codex", "gpt", 50, 0.5)},
		Sessions: []SessionSpan{{Provider: "claude", ID: "aa", First: unixHour(day), Last: unixHour(day)}},
	}
	laptop := &Ledger{
		Entries:  []LedgerEntry{entryAt(day, "claude", "fable", 300, 3)},
		Sessions: []SessionSpan{{Provider: "claude", ID: "bb", First: unixHour(day), Last: unixHour(day)}},
	}
	devices := []DeviceLedger{
		{Key: "host", Name: "k12", Host: true, Ledger: host},
		{Key: "laptop", Name: "MacBook", Ledger: laptop},
	}
	s := Combine(devices, "laptop", day, day, time.UTC)
	if s.TotalTokens != 450 || math.Abs(s.TotalCost-5) > 1e-9 || s.Records != 3 || s.Sessions != 2 {
		t.Fatalf("totals: tokens %d cost %v records %d sessions %d", s.TotalTokens, s.TotalCost, s.Records, s.Sessions)
	}
	if len(s.Devices) != 2 || s.Devices[0].Name != "MacBook" || !s.Devices[0].This || s.Devices[0].Cost != 3 ||
		s.Devices[1].Name != "k12" || !s.Devices[1].Host || s.Devices[1].Tokens != 150 {
		t.Fatalf("devices: %+v", s.Devices)
	}
	if len(s.Models) != 2 || s.Models[0].Model != "fable" || s.Models[0].Tokens != 400 {
		t.Fatalf("models: %+v", s.Models)
	}
	// One computer alone gets no breakdown.
	if alone := Combine(devices[:1], "host", day, day, time.UTC); alone.Devices != nil {
		t.Fatalf("a single computer has a breakdown: %+v", alone.Devices)
	}
}

// Each computer counts in UTC hours, and the one adding up puts each hour
// on its own local day: two computers in different time zones agree.
func TestCombineUsesTheViewersDays(t *testing.T) {
	amsterdam := time.FixedZone("UTC+1", 3600)
	india := time.FixedZone("UTC+5:30", 5*3600+1800)
	// 23:00 UTC on the 12th is the 13th in Amsterdam (00:00) and in India.
	late := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	l := &Ledger{Entries: []LedgerEntry{entryAt(late, "claude", "m", 10, 1)}}
	d := []DeviceLedger{{Key: "a", Ledger: l}}
	for _, c := range []struct {
		loc  *time.Location
		day  time.Time
		want int64
	}{
		{time.UTC, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), 10},
		{time.UTC, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), 0},
		{amsterdam, time.Date(2026, 11, 13, 0, 0, 0, 0, amsterdam), 10},
		{amsterdam, time.Date(2026, 11, 12, 0, 0, 0, 0, amsterdam), 0},
		{india, time.Date(2026, 11, 13, 0, 0, 0, 0, india), 10},
	} {
		s := Combine(d, "a", c.day, c.day, c.loc)
		if s.TotalTokens != c.want {
			t.Errorf("%s on %s: %d tokens, want %d", c.loc, c.day.Format("2006-01-02"), s.TotalTokens, c.want)
		}
		if c.want > 0 && (len(s.Days) != 1 || s.Days[0].Day != c.day.Format("2006-01-02")) {
			t.Errorf("%s: days %+v", c.loc, s.Days)
		}
	}
	// In a time zone half an hour off, the hour that starts before midnight
	// counts for the day it starts on: 18:00 UTC is 23:30 in India.
	evening := &Ledger{Entries: []LedgerEntry{entryAt(time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC), "claude", "m", 5, 1)}}
	got := Combine([]DeviceLedger{{Key: "a", Ledger: evening}}, "a", time.Date(2026, 11, 13, 0, 0, 0, 0, india), time.Date(2026, 11, 13, 0, 0, 0, 0, india), india)
	if got.TotalTokens != 5 {
		t.Errorf("half-hour zone: %d tokens", got.TotalTokens)
	}
}

// A ledger from another computer is kept only within its window, with plain
// names and numbers that cannot go negative, overflow or be NaN.
func TestCleanMakesAnotherComputersLedgerSafe(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	l := &Ledger{
		Entries: []LedgerEntry{
			{Hour: unixHour(now), Provider: "claude", Model: "fable\x1b[2J", Totals: Totals{Output: -5, CachedInput: 1 << 62}, Cost: math.NaN(), Savings: math.Inf(1), Records: 2, Unpriced: 9},
			{Hour: unixHour(now.AddDate(0, 0, -200)), Provider: "claude", Model: "old", Records: 1},
			{Hour: unixHour(now.AddDate(0, 0, 5)), Provider: "claude", Model: "future", Records: 1},
			{Hour: unixHour(now), Provider: "\x00\x01", Model: "no provider", Records: 1},
			{Hour: unixHour(now), Provider: "codex", Model: strings.Repeat("m", 500), Cost: -3, Records: 1},
		},
		Sessions: []SessionSpan{
			{Provider: "claude", ID: "0123abcd", First: unixHour(now), Last: unixHour(now)},
			{Provider: "claude", ID: "<script>", First: unixHour(now), Last: unixHour(now)},
			{Provider: "claude", ID: "ab", First: unixHour(now), Last: unixHour(now) - 5},
			{Provider: "claude", ID: strings.Repeat("a", 40), First: unixHour(now), Last: unixHour(now)},
		},
	}
	if !l.Clean(now) {
		t.Fatal("refused")
	}
	if len(l.Entries) != 2 {
		t.Fatalf("entries kept: %+v", l.Entries)
	}
	e := l.Entries[0]
	if e.Model != "fable[2J" || e.Totals.Output != 0 || e.Totals.CachedInput != 1<<50 || e.Cost != 0 || e.Savings != 0 || e.Unpriced != 2 {
		t.Fatalf("first entry: %+v", e)
	}
	if len([]rune(l.Entries[1].Model)) != maxLedgerName || l.Entries[1].Cost != 0 {
		t.Fatalf("second entry: model %d runes, cost %v", len([]rune(l.Entries[1].Model)), l.Entries[1].Cost)
	}
	if len(l.Sessions) != 1 || l.Sessions[0].ID != "0123abcd" {
		t.Fatalf("sessions kept: %+v", l.Sessions)
	}
	var none *Ledger
	if none.Clean(now) {
		t.Fatal("a missing ledger was kept")
	}
}

// The host keeps one ledger per install. A computer may write only the
// one it claimed while it stays paired; once it is gone, its install can be
// claimed again, as when the same computer pairs anew.
func TestDeviceStoreKeepsEachComputersLedger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage-devices")
	store := OpenDeviceStore(dir)
	now := time.Now()
	paired := map[string]bool{"dev-a": true, "dev-b": true}
	isPaired := func(id string) bool { return paired[id] }
	ledger := func(tokens int64) *Ledger {
		return &Ledger{Entries: []LedgerEntry{entryAt(now, "claude", "m", tokens, 1)}}
	}
	keyA, keyB := DeviceKey("install-a"), DeviceKey("install-b")
	if err := store.Put(keyA, "dev-a", "MacBook\x1b]0;x\a", ledger(10), isPaired, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(keyB, "dev-b", "Mini", ledger(20), isPaired, now); err != nil {
		t.Fatal(err)
	}
	// Another paired computer cannot write A's ledger.
	if err := store.Put(keyA, "dev-b", "Mini", ledger(999), isPaired, now); err != ErrClaimed {
		t.Fatalf("a claimed ledger was taken over: %v", err)
	}
	// A updates its own.
	if err := store.Put(keyA, "dev-a", "MacBook", ledger(11), isPaired, now); err != nil {
		t.Fatal(err)
	}
	// A pairs anew: its old pairing is gone, its new one takes the ledger.
	delete(paired, "dev-a")
	paired["dev-a2"] = true
	if err := store.Put(keyA, "dev-a2", "MacBook", ledger(12), isPaired, now); err != nil {
		t.Fatalf("a computer paired anew: %v", err)
	}
	// Kept on disk, privately, and read back after a restart.
	reopened := OpenDeviceStore(dir)
	got := map[string]int64{}
	for _, d := range reopened.Ledgers() {
		got[d.Name] = d.Ledger.Entries[0].Totals.Output
	}
	if len(got) != 2 || got["MacBook"] != 12 || got["Mini"] != 20 {
		t.Fatalf("after a restart: %v", got)
	}
	for _, key := range []string{keyA, keyB} {
		info, err := os.Stat(filepath.Join(dir, key+".json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", key, info, err)
		}
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("the folder is %o", info.Mode().Perm())
	}
	// Bad keys and ledgers are refused.
	for _, key := range []string{"", "../../etc/passwd", strings.Repeat("g", 32), keyA + "0"} {
		if err := store.Put(key, "dev-b", "x", ledger(1), isPaired, now); err == nil {
			t.Errorf("key %q was taken", key)
		}
	}
	if err := store.Put(DeviceKey("c"), "dev-b", "x", nil, isPaired, now); err == nil {
		t.Error("a missing ledger was taken")
	}
	// A file that is not a ledger, or under the wrong name, is not read.
	_ = os.WriteFile(filepath.Join(dir, strings.Repeat("a", 32)+".json"), []byte(`{"key":"`+keyB+`","ledger":{}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{}`), 0o600)
	if n := len(OpenDeviceStore(dir).Ledgers()); n != 2 {
		t.Fatalf("read %d ledgers", n)
	}
}

// The host keeps a bounded number of ledgers; room is made only from
// computers no longer paired.
func TestDeviceStoreIsBounded(t *testing.T) {
	store := OpenDeviceStore(t.TempDir())
	now := time.Now()
	paired := map[string]bool{}
	isPaired := func(id string) bool { return paired[id] }
	for i := 0; i < maxStoredDevices; i++ {
		id := "dev-" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		paired[id] = true
		if err := store.Put(DeviceKey(id), id, id, &Ledger{}, isPaired, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	paired["new"] = true
	if err := store.Put(DeviceKey("new"), "new", "new", &Ledger{}, isPaired, now); err == nil {
		t.Fatal("a ledger beyond the bound was kept while every computer is paired")
	}
	delete(paired, "dev-Aa")
	if err := store.Put(DeviceKey("new"), "new", "new", &Ledger{}, isPaired, now); err != nil {
		t.Fatalf("no room was made from a computer no longer paired: %v", err)
	}
	if n := len(store.Ledgers()); n != maxStoredDevices {
		t.Fatalf("%d ledgers kept", n)
	}
}

// The service adds the other computers' ledgers to its own, leaving out a
// copy of its own that came back from the host.
func TestServiceAddsOthersToItsOwn(t *testing.T) {
	s := &Service{now: time.Now}
	s.own, s.ownAt = &Ledger{Entries: []LedgerEntry{entryAt(time.Now(), "claude", "m", 10, 1)}}, time.Now()
	s.SetIdentity(func() (string, string, bool) { return "me", "k12", true })
	s.SetOthers(func() []DeviceLedger {
		return []DeviceLedger{
			{Key: "me", Name: "stale copy of me", Ledger: &Ledger{Entries: []LedgerEntry{entryAt(time.Now(), "claude", "m", 1000, 100)}}},
			{Key: "laptop", Name: "MacBook", Ledger: &Ledger{Entries: []LedgerEntry{entryAt(time.Now(), "codex", "g", 5, 0.5)}}},
		}
	})
	got := s.Get(1)
	if got.TotalTokens != 15 || len(got.Devices) != 2 || got.Devices[0].Name != "k12" || !got.Devices[0].This || !got.Devices[0].Host {
		t.Fatalf("summary: tokens %d devices %+v", got.TotalTokens, got.Devices)
	}
}
