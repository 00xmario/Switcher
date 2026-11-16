package usage

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Service owns the scan cache, the rate table, and this computer's ledger.
// A summary adds that ledger up with the other computers' (see SetOthers).
// Scans are done in the background; readers always get the last summary
// without blocking.
type Service struct {
	mu       sync.Mutex
	ratesMu  sync.Mutex
	own      *Ledger // never changed once made: a new scan makes a new one
	ownAt    time.Time
	scanMs   int64
	scanning bool
	identity func() (key, name string, host bool)
	others   func() []DeviceLedger
	cache    *ScanCache
	rates    RateTable
	ratesOK  bool
	ratesAt  time.Time
	dataDir  string
	client   *http.Client
	now      func() time.Time

	// The archive: this computer's usage for all time (archive.go). It is
	// read on the first scan and written after each scan that changed it.
	archive       *Ledger
	archiveRead   bool
	backfilled    bool
	archiveUnsent bool // the last write failed: write it again
}

// historyFloor is the oldest a usage record may be: records dated before
// it (a missing timestamp reads as 1970) are left out.
var historyFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// NewService builds the service; dataDir holds usage-scan-cache.json,
// usage-archive.json and model-rates.json.
func NewService(dataDir string, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		// continue: cache writes will fail but scans still work
	}
	s := &Service{
		cache:   LoadScanCache(filepath.Join(dataDir, "usage-scan-cache.json")),
		dataDir: dataDir,
		client:  client,
		now:     time.Now,
	}
	return s
}

// SetIdentity says who this computer is among the ones whose usage is
// added up: a key stable for this install, its name, and whether it hosts.
func (s *Service) SetIdentity(identity func() (key, name string, host bool)) {
	s.mu.Lock()
	s.identity = identity
	s.mu.Unlock()
}

// SetOthers gives the other computers' ledgers to add to this one's: a
// host's clients, as they last sent them, or, on a client, its host's and
// the other clients', as the host last gave them.
func (s *Service) SetOthers(others func() []DeviceLedger) {
	s.mu.Lock()
	s.others = others
	s.mu.Unlock()
}

// Get returns the summary for a window of days, all computers' usage added
// up, or nil before this computer's first scan. The caller decides
// staleness with Age.
func (s *Service) Get(days int) *Summary {
	devices, key, own, scanMs := s.devices()
	if own == nil {
		return nil
	}
	until := s.now()
	summary := Combine(devices, key, until.AddDate(0, 0, -(days-1)), until, time.Local)
	summary.ScannedAtMs, summary.ScanMs = own.ScannedAtMs, scanMs
	return summary
}

// Insights returns the long view of every computer's usage over the last
// days, or all time when days is 0; nil before this computer's first scan.
func (s *Service) Insights(days int) *Insights {
	devices, key, own, _ := s.devices()
	if own == nil {
		return nil
	}
	insights := BuildInsights(devices, key, days, s.now(), time.Local)
	insights.ScannedMs = own.ScannedAtMs
	return insights
}

// devices is every computer's ledger, this one's first, with this one's key.
func (s *Service) devices() (devices []DeviceLedger, key string, own *Ledger, scanMs int64) {
	s.mu.Lock()
	own, ownAt, scanMs, identity, others := s.own, s.ownAt, s.scanMs, s.identity, s.others
	s.mu.Unlock()
	if own == nil {
		return nil, "", nil, 0
	}
	key, name, host := "this", "This computer", false
	if identity != nil {
		key, name, host = identity()
	}
	devices = []DeviceLedger{{Key: key, Name: name, Host: host, UpdatedMs: ownAt.UnixMilli(), Ledger: own}}
	if others != nil {
		for _, d := range others() {
			if d.Key != key {
				devices = append(devices, d)
			}
		}
	}
	return devices, key, own, scanMs
}

// Own is this computer's ledger from its latest scan, or nil before one.
// It is never changed afterwards.
func (s *Service) Own() *Ledger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own
}

// OwnLedger is Own with when it was made.
func (s *Service) OwnLedger() (*Ledger, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own, s.ownAt
}

// Age reports how old this computer's latest scan is.
func (s *Service) Age(days int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.own == nil {
		return 1<<63 - 1
	}
	return s.now().Sub(s.ownAt)
}

// Scan rescans this computer's logs, over every window at once, and returns
// the summary for a window of days; nil while another scan runs. Callers
// should run it in a goroutine; Get does not wait for scan I/O.
func (s *Service) Scan(days int) *Summary {
	if !s.scanOwn() {
		return nil
	}
	return s.Get(days)
}

// Rescan is Scan for a caller that has no use for the summary: it only
// rescans, and asks nothing of the other computers.
func (s *Service) Rescan() { s.scanOwn() }

func (s *Service) scanOwn() bool {
	s.mu.Lock()
	if s.scanning {
		s.mu.Unlock()
		return false
	}
	s.scanning = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
	}()
	rates := s.currentRates()
	now := s.now()
	began := time.Now()
	path := filepath.Join(s.dataDir, "usage-archive.json")
	if !s.archiveRead {
		archive, backfilled, err := loadArchive(path, now)
		// An archive that could not be read this time is left as it is:
		// this scan shows what the logs have, and the next reads it again.
		s.archive, s.backfilled, s.archiveRead = archive, backfilled, err == nil
	}
	// Until the archive holds every log on disk, read them all, however
	// old (no AI coding tool wrote one before historyFloor); then the
	// window the Usage page shows is enough.
	start := now.AddDate(0, 0, -LedgerDays)
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
	if !s.backfilled {
		from = historyFloor
	}
	scanned := collectLedger(ResolveSources(), s.cache, rates, from)
	ledger, changed := mergeArchive(s.archive, scanned, now)
	if s.archiveRead {
		if changed || !s.backfilled || s.archiveUnsent {
			err := saveArchive(path, ledger, true)
			s.archiveUnsent = err != nil
		}
		s.archive, s.backfilled = ledger, true
	}
	s.mu.Lock()
	s.own, s.ownAt, s.scanMs = ledger, s.now(), time.Since(began).Milliseconds()
	s.mu.Unlock()
	return true
}

func (s *Service) currentRates() RateTable {
	// Different windows share one price refresh, without blocking Get.
	s.ratesMu.Lock()
	defer s.ratesMu.Unlock()
	now := s.now()
	s.mu.Lock()
	age := now.Sub(s.ratesAt)
	if s.ratesOK && age >= 0 && age < ratesTTL {
		table := s.rates
		s.mu.Unlock()
		return table
	}
	s.mu.Unlock()
	table, _, fetchedAt := loadRatesAt(filepath.Join(s.dataDir, "model-rates.json"), s.client, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(table) > 0 {
		s.rates = table
		s.ratesAt = fetchedAt
		s.ratesOK = true
	}
	return s.rates
}

// RefreshStale rescans when this computer's ledger is older than olderThan,
// and returns the windows that refreshed: all of them, since one scan serves
// every window. Intended for a background goroutine.
func (s *Service) RefreshStale(windows []int, olderThan time.Duration) []int {
	s.mu.Lock()
	stale := s.own == nil || (s.now().Sub(s.ownAt) > olderThan && !s.scanning)
	s.mu.Unlock()
	if stale && s.scanOwn() {
		return windows
	}
	return nil
}
