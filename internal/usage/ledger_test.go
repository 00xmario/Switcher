package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entryAt(t time.Time, provider, model string, tokens int64, cost float64) LedgerEntry {
	return LedgerEntry{Quarter: unixQuarter(t), Provider: provider, Model: model, Totals: Totals{Output: tokens}, Cost: cost, Records: 1}
}

// Every computer's usage is added up, each computer's share is kept, and
// sessions are counted once each.
func TestCombineAddsUpEveryComputer(t *testing.T) {
	day := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	host := &Ledger{
		Entries:  []LedgerEntry{entryAt(day, "claude", "fable", 100, 1.5), entryAt(day.Add(time.Hour), "codex", "gpt", 50, 0.5)},
		Sessions: []SessionSpan{{Provider: "claude", ID: "aa", First: unixQuarter(day), Last: unixQuarter(day)}},
	}
	laptop := &Ledger{
		Entries:  []LedgerEntry{entryAt(day, "claude", "fable", 300, 3)},
		Sessions: []SessionSpan{{Provider: "claude", ID: "bb", First: unixQuarter(day), Last: unixQuarter(day)}},
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
	// In time zones a half or three quarters of an hour off UTC, usage just
	// before local midnight counts for that day and usage from midnight on
	// for the next: 18:15 UTC is 23:45 in India, 18:30 is midnight there,
	// and 18:15 is midnight in Nepal.
	nepal := time.FixedZone("UTC+5:45", 5*3600+45*60)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 13, h, m, 0, 0, time.UTC) }
	evening := &Ledger{Entries: []LedgerEntry{
		entryAt(at(18, 15), "claude", "m", 5, 1),
		entryAt(at(18, 30), "claude", "m", 7, 1),
		entryAt(at(18, 0), "codex", "m", 11, 1),
	}}
	for _, c := range []struct {
		loc        *time.Location
		day        int
		want       int64
		whatsCount string
	}{
		{india, 13, 16, "18:00 and 18:15 UTC"},
		{india, 14, 7, "18:30 UTC"},
		{nepal, 13, 11, "18:00 UTC"},
		{nepal, 14, 12, "18:15 and 18:30 UTC"},
	} {
		day := time.Date(2026, 11, c.day, 0, 0, 0, 0, c.loc)
		got := Combine([]DeviceLedger{{Key: "a", Ledger: evening}}, "a", day, day, c.loc)
		if got.TotalTokens != c.want {
			t.Errorf("%s on the %d: %d tokens, want %d (%s)", c.loc, c.day, got.TotalTokens, c.want, c.whatsCount)
		}
	}
}

// A ledger from another computer is kept only within its window, with plain
// names and numbers that cannot go negative, overflow or be NaN.
func TestCleanMakesAnotherComputersLedgerSafe(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	l := &Ledger{
		Entries: []LedgerEntry{
			{Quarter: unixQuarter(now), Provider: "claude", Model: "fable\x1b[2J", Totals: Totals{Output: -5, CachedInput: 1 << 62}, Cost: math.NaN(), Savings: math.Inf(1), Records: 2, Unpriced: 9},
			{Quarter: unixQuarter(now.AddDate(0, 0, -200)), Provider: "claude", Model: "old", Records: 1},
			{Quarter: unixQuarter(now.AddDate(0, 0, 5)), Provider: "claude", Model: "future", Records: 1},
			{Quarter: unixQuarter(now), Provider: "\x00\x01", Model: "no provider", Records: 1},
			{Quarter: unixQuarter(now), Provider: `"><style>*{display:none}</style>`, Model: "unknown provider", Records: 1},
			{Quarter: unixQuarter(now), Provider: "codex", Model: strings.Repeat("m", 500), Cost: -3, Records: 1},
		},
		Sessions: []SessionSpan{
			{Provider: "claude", ID: "0123abcd", First: unixQuarter(now), Last: unixQuarter(now)},
			{Provider: "claude", ID: "<script>", First: unixQuarter(now), Last: unixQuarter(now)},
			{Provider: "claude", ID: "ab", First: unixQuarter(now), Last: unixQuarter(now) - 5},
			{Provider: "claude", ID: strings.Repeat("a", 40), First: unixQuarter(now), Last: unixQuarter(now)},
			{Provider: "<b>", ID: "0123abce", First: unixQuarter(now), Last: unixQuarter(now)},
		},
	}
	if !l.Clean(now) {
		t.Fatal("refused")
	}
	if len(l.Entries) != 2 {
		t.Fatalf("entries kept: %+v", l.Entries)
	}
	e := l.Entries[0]
	if e.Model != "fable[2J" || e.Totals.Output != 0 || e.Totals.CachedInput != maxLedgerTotal || e.Cost != 0 || e.Savings != 0 || e.Unpriced != 2 {
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

// The host keeps one ledger per install and pairing: a pairing that sends
// another install's ledger gives up its earlier one; the pairing that sends
// an install's id last holds its ledger, as when a computer pairs again
// after an unpairing that never reached the host; and a ledger counts only
// while its pairing is in place.
func TestDeviceStoreKeepsEachComputersLedger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage-devices")
	store := OpenDeviceStore(dir)
	now := time.Now()
	paired := map[string]bool{"dev-a": true, "dev-b": true}
	isPaired := func(id string) bool { return paired[id] }
	ledger := func(tokens int64) *Ledger {
		return &Ledger{Entries: []LedgerEntry{entryAt(now, "claude", "m", tokens, 1)}}
	}
	counted := func(s *DeviceStore) map[string]int64 {
		got := map[string]int64{}
		for _, d := range s.Ledgers(isPaired) {
			got[d.Name] = d.Ledger.Entries[0].Totals.Output
		}
		return got
	}
	keyA, keyB, keyC := DeviceKey("install-a"), DeviceKey("install-b"), DeviceKey("install-c")
	if err := store.Put(keyA, "dev-a", "MacBook\x1b]0;x\a", ledger(10), isPaired, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(keyB, "dev-b", "Mini", ledger(20), isPaired, now); err != nil {
		t.Fatal(err)
	}
	// A updates its own.
	if err := store.Put(keyA, "dev-a", "MacBook", ledger(11), isPaired, now); err != nil {
		t.Fatal(err)
	}
	// A pairs again while its old pairing is still on the host: the new
	// pairing holds A's ledger.
	paired["dev-a2"] = true
	if err := store.Put(keyA, "dev-a2", "MacBook", ledger(12), isPaired, now); err != nil {
		t.Fatalf("a computer paired again: %v", err)
	}
	if got := counted(store); len(got) != 2 || got["MacBook"] != 12 || got["Mini"] != 20 {
		t.Fatalf("after pairing again: %v", got)
	}
	// One pairing, one ledger: B sending another install's gives up its own.
	if err := store.Put(keyC, "dev-b", "Mini", ledger(30), isPaired, now); err != nil {
		t.Fatal(err)
	}
	if got := counted(store); len(got) != 2 || got["Mini"] != 30 {
		t.Fatalf("one pairing kept two ledgers: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, keyB+".json")); !os.IsNotExist(err) {
		t.Fatalf("the ledger given up is still on disk: %v", err)
	}
	if err := store.Put(keyB, "dev-b", "Mini", ledger(20), isPaired, now); err != nil {
		t.Fatal(err)
	}
	// Kept on disk, privately, and read back after a restart (Switcher
	// writes what it kept in memory only as it stops).
	store.Flush()
	reopened := OpenDeviceStore(dir)
	if got := counted(reopened); len(got) != 2 || got["MacBook"] != 12 || got["Mini"] != 20 {
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
	// A removed pairing's ledger no longer counts, and goes with the next
	// ledger kept.
	delete(paired, "dev-a2")
	if got := counted(store); len(got) != 1 || got["Mini"] != 20 {
		t.Fatalf("a removed pairing still counts: %v", got)
	}
	if err := store.Put(keyB, "dev-b", "Mini", ledger(21), isPaired, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyA+".json")); !os.IsNotExist(err) {
		t.Fatalf("a removed pairing's ledger is still on disk: %v", err)
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
	if n := len(OpenDeviceStore(dir).Ledgers(isPaired)); n != 1 {
		t.Fatalf("read %d ledgers", n)
	}
}

// The host keeps a bounded number of ledgers, of computers still paired.
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
	if err := store.Put(DeviceKey("new"), "new", "new", &Ledger{}, isPaired, now); err != ErrStoreFull {
		t.Fatalf("a ledger beyond the bound, with every computer paired: %v", err)
	}
	delete(paired, "dev-Aa")
	if err := store.Put(DeviceKey("new"), "new", "new", &Ledger{}, isPaired, now); err != nil {
		t.Fatalf("no room was made from a computer no longer paired: %v", err)
	}
	if n := len(store.Ledgers(isPaired)); n != maxStoredDevices {
		t.Fatalf("%d ledgers kept", n)
	}
}

// A ledger travels as compact arrays and is read one entry at a time: one
// longer than a ledger may be, or with entries of the wrong shape, is
// refused before it takes memory.
func TestLedgerIsReadWithinItsLimits(t *testing.T) {
	now := time.Now()
	l := &Ledger{
		Entries:     []LedgerEntry{{Quarter: unixQuarter(now), Provider: "claude", Model: "fable", Totals: Totals{UncachedInput: 1, CachedInput: 2, CacheCreation: 3, Output: 4, Reasoning: 5}, Cost: 1.25, Savings: 0.5, Records: 6, Unpriced: 1}},
		Sessions:    []SessionSpan{{Provider: "claude", ID: "0123abcd", First: unixQuarter(now) - 4, Last: unixQuarter(now)}},
		ScannedAtMs: now.UnixMilli(),
	}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entries":[[`) || !strings.Contains(string(raw), `[1,2,3,4,5]`) {
		t.Fatalf("not compact: %s", raw)
	}
	var back Ledger
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != 1 || back.Entries[0] != l.Entries[0] || len(back.Sessions) != 1 || back.Sessions[0] != l.Sessions[0] || back.ScannedAtMs != l.ScannedAtMs {
		t.Fatalf("round trip: %+v", back)
	}
	many := func(field, element string, n int) []byte {
		return []byte(`{"` + field + `":[` + strings.TrimSuffix(strings.Repeat(element+",", n), ",") + `]}`)
	}
	entry := `[1,"claude","m",[0,0,0,0,0],0,0,0,0]`
	session := `["claude","ab",1,1]`
	if err := json.Unmarshal(many("entries", entry, maxLedgerEntries), &back); err != nil || len(back.Entries) != maxLedgerEntries {
		t.Fatalf("a ledger at the limit: %v", err)
	}
	for name, body := range map[string][]byte{
		"too many entries":      many("entries", entry, maxLedgerEntries+1),
		"too many sessions":     many("sessions", session, maxLedgerSessions+1),
		"entries in two halves": []byte(`{"entries":[` + strings.TrimSuffix(strings.Repeat(entry+",", maxLedgerEntries), ",") + `],"entries":[` + entry + `]}`),
		"empty objects":         many("entries", `{}`, 3),
		"short entries":         many("entries", `[1,"claude"]`, 1),
		"wrong types":           many("entries", `[1,"claude","m",[0,0,0,0,0],"x",0,0,0]`, 1),
		"not an object":         []byte(`[]`),
	} {
		var got Ledger
		if err := json.Unmarshal(body, &got); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	devices := `[` + strings.TrimSuffix(strings.Repeat(`{"key":"k","ledger":null},`, MaxDevices+1), ",") + `]`
	var list DeviceLedgers
	if err := json.Unmarshal([]byte(devices), &list); err == nil {
		t.Errorf("%d computers' ledgers were taken", MaxDevices+1)
	}
}

// Every ledger's totals are bounded, so even the most computers a summary
// adds up, each with a ledger at the bound, cannot overflow.
func TestTotalsCannotOverflow(t *testing.T) {
	now := time.Now()
	big := Totals{UncachedInput: 1 << 62, CachedInput: 1 << 62, CacheCreation: 1 << 62, Output: 1 << 62, Reasoning: 1 << 62}
	var devices []DeviceLedger
	for i := 0; i < MaxDevices+1; i++ {
		l := &Ledger{}
		for j := 0; j < 50; j++ {
			l.Entries = append(l.Entries, LedgerEntry{Quarter: unixQuarter(now) - int64(j), Provider: "claude", Model: "m", Totals: big, Cost: 1e300, Records: 1 << 62, Unpriced: 1 << 62})
		}
		if !l.Clean(now) {
			t.Fatal("refused")
		}
		devices = append(devices, DeviceLedger{Key: strings.Repeat(string(rune('a'+i%26)), 1) + string(rune('a'+i/26)), Name: "c", Ledger: l})
	}
	s := Combine(devices, "", now.AddDate(0, 0, -1), now, time.UTC)
	if s.TotalTokens <= 0 || s.Records <= 0 || s.Totals.Output <= 0 || math.IsInf(s.TotalCost, 0) || math.IsNaN(s.TotalCost) {
		t.Fatalf("overflowed: tokens %d records %d output %d cost %v", s.TotalTokens, s.Records, s.Totals.Output, s.TotalCost)
	}
	for _, d := range s.Devices {
		if d.Tokens <= 0 {
			t.Fatalf("a computer's share overflowed: %+v", d)
		}
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

// Model names keep to what JSON carries byte for byte, so a ledger is no
// larger written out than read in; a ledger that is anyway larger than a
// host keeps is refused; and an unchanged one is not written again at once.
func TestModelNamesAndStoredSizesAreBounded(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-4-1":        "claude-opus-4-1",
		"gpt-5.1-codex-max":      "gpt-5.1-codex-max",
		"<synthetic>":            "synthetic",
		`a"b\c&d<e>f` + " ":      "abcdef",
		strings.Repeat("<", 500): "",
		strings.Repeat("m", 500): strings.Repeat("m", maxLedgerName),
	} {
		if got := ledgerModel(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	now := time.Now()
	big := &Ledger{}
	for i := 0; i < 1000; i++ {
		big.Entries = append(big.Entries, LedgerEntry{Quarter: unixQuarter(now) - int64(i), Provider: "claude", Model: strings.Repeat("m", 120), Records: 1})
	}
	was := keptLedgerLimit
	keptLedgerLimit = 64 << 10
	defer func() { keptLedgerLimit = was }()
	dir := t.TempDir()
	store := OpenDeviceStore(dir)
	paired := func(string) bool { return true }
	if err := store.Put(DeviceKey("big"), "dev", "x", big, paired, now); err != ErrLedgerTooLarge {
		t.Fatalf("a ledger larger than a host keeps: %v", err)
	}
	small := func() *Ledger { return &Ledger{Entries: []LedgerEntry{entryAt(now, "claude", "m", 5, 1)}} }
	if err := store.Put(DeviceKey("small"), "dev", "x", small(), paired, now); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, DeviceKey("small")+".json")
	before, _ := os.Stat(file)
	if err := store.Put(DeviceKey("small"), "dev", "x", small(), paired, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(file)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged ledger was written again at once")
	}
	if got := store.Ledgers(paired); len(got) != 1 || got[0].UpdatedMs != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("the time it was sent: %+v", got)
	}
}

// This computer's own view keeps all of its usage; only what is sent is cut
// to what another computer takes. Its totals are bounded as anyone's.
func TestOwnLedgerIsKeptWholeAndBounded(t *testing.T) {
	now := time.Now()
	l := &Ledger{}
	for i := 0; i < maxLedgerEntries+10; i++ {
		l.Entries = append(l.Entries, LedgerEntry{Quarter: unixQuarter(now) - int64(maxLedgerEntries+10-i), Provider: "claude", Model: "m", Records: 1})
	}
	sent := l.ForSending()
	if len(l.Entries) != maxLedgerEntries+10 || len(sent.Entries) != maxLedgerEntries || sent.Entries[len(sent.Entries)-1] != l.Entries[len(l.Entries)-1] {
		t.Fatalf("kept %d, sent %d", len(l.Entries), len(sent.Entries))
	}
	huge := &Ledger{Entries: []LedgerEntry{
		{Quarter: 1, Provider: "claude", Totals: Totals{Output: 1 << 62}, Records: 1 << 62},
		{Quarter: 2, Provider: "claude", Totals: Totals{Output: 1 << 62}, Records: 1 << 62},
	}}
	huge.bound()
	if len(huge.Entries) != 1 || huge.Entries[0].Totals.Output != maxLedgerTotal {
		t.Fatalf("bounded: %+v", huge.Entries)
	}
	if got := boundedSum(Totals{Output: maxLedgerTotal}, Totals{Output: maxLedgerTotal}); got.Output != maxLedgerTotal {
		t.Fatalf("a sum past the bound: %d", got.Output)
	}
}

// A changed ledger is written at once the first time and then at most every
// few minutes, the latest kept in memory meanwhile; a rescan that found no
// new usage is no change.
func TestChangedLedgersAreWrittenNowAndThen(t *testing.T) {
	dir := t.TempDir()
	store := OpenDeviceStore(dir)
	now := time.Now()
	paired := func(string) bool { return true }
	key := DeviceKey("a")
	ledger := func(tokens int64, scanned int64) *Ledger {
		return &Ledger{Entries: []LedgerEntry{entryAt(now, "claude", "m", tokens, 1)}, ScannedAtMs: scanned}
	}
	onDisk := func() int64 {
		var stored struct {
			Ledger *Ledger `json:"ledger"`
		}
		raw, _ := os.ReadFile(filepath.Join(dir, key+".json"))
		if json.Unmarshal(raw, &stored) != nil || stored.Ledger == nil || len(stored.Ledger.Entries) == 0 {
			return -1
		}
		return stored.Ledger.Entries[0].Totals.Output
	}
	v := store.Version()
	if err := store.Put(key, "dev", "x", ledger(1, 1), paired, now); err != nil || onDisk() != 1 {
		t.Fatalf("the first ledger: %v, on disk %d", err, onDisk())
	}
	if store.Version() == v {
		t.Fatal("a kept ledger did not change the version")
	}
	if err := store.Put(key, "dev", "x", ledger(2, 2), paired, now.Add(time.Minute)); err != nil || onDisk() != 1 {
		t.Fatalf("a change a minute later was written at once: %v, on disk %d", err, onDisk())
	}
	if got := store.Ledgers(paired)[0].Ledger.Entries[0].Totals.Output; got != 2 {
		t.Fatalf("the latest is not what counts: %d", got)
	}
	store.Flush()
	if onDisk() != 2 {
		t.Fatalf("after a flush: %d", onDisk())
	}
	if err := store.Put(key, "dev", "x", ledger(3, 3), paired, now.Add(writeEvery+time.Minute)); err != nil || onDisk() != 3 {
		t.Fatalf("a change after the wait: %v, on disk %d", err, onDisk())
	}
	// Scanned again, the same usage: no change.
	if ledgerSum(ledger(3, 3)) != ledgerSum(ledger(3, 99)) {
		t.Fatal("the time of the scan counts as a change")
	}
}

// A ledger whose write failed stays to be written, and a clock that ran ahead
// holds no write up.
func TestFailedAndMisdatedWritesAreRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	dir := filepath.Join(t.TempDir(), "usage-devices")
	store := OpenDeviceStore(dir)
	now := time.Now()
	paired := func(string) bool { return true }
	key := DeviceKey("a")
	ledger := func(tokens int64) *Ledger {
		return &Ledger{Entries: []LedgerEntry{entryAt(now, "claude", "m", tokens, 1)}}
	}
	onDisk := func() int64 {
		var stored struct {
			Ledger *Ledger `json:"ledger"`
		}
		raw, _ := os.ReadFile(filepath.Join(dir, key+".json"))
		if json.Unmarshal(raw, &stored) != nil || stored.Ledger == nil || len(stored.Ledger.Entries) == 0 {
			return -1
		}
		return stored.Ledger.Entries[0].Totals.Output
	}
	if err := store.Put(key, "dev", "x", ledger(1), paired, now); err != nil {
		t.Fatal(err)
	}
	// The disk refuses: the host says so, keeps the ledger, and writes it
	// once it can, though the same usage is sent again meanwhile.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.devices[key].savedMs = now.Add(-writeEvery).UnixMilli()
	store.mu.Unlock()
	if err := store.Put(key, "dev", "x", ledger(2), paired, now); err == nil {
		t.Fatal("a failed write was reported as kept")
	}
	_ = os.Chmod(dir, 0o700)
	if err := store.Put(key, "dev", "x", ledger(2), paired, now.Add(time.Minute)); err != nil || onDisk() != 2 {
		t.Fatalf("the same usage again: %v, on disk %d", err, onDisk())
	}
	// A last write dated ahead, by a clock that ran ahead: the next change
	// is written at once.
	store.mu.Lock()
	store.devices[key].savedMs = now.Add(12 * time.Hour).UnixMilli()
	store.mu.Unlock()
	if err := store.Put(key, "dev", "x", ledger(3), paired, now.Add(2*time.Minute)); err != nil || onDisk() != 3 {
		t.Fatalf("after a clock ran ahead: %v, on disk %d", err, onDisk())
	}
	// Once flushed for good, every ledger is written at once.
	store.Flush()
	if err := store.Put(key, "dev", "x", ledger(4), paired, now.Add(3*time.Minute)); err != nil || onDisk() != 4 {
		t.Fatalf("after the last flush: %v, on disk %d", err, onDisk())
	}
}
