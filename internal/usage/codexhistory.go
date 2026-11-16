package usage

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// CodexHistory keeps the Codex accounts' daily token totals as OpenAI
// reports them (codex-history.json), on the Switcher that holds the
// accounts. A day keeps the largest total seen, and an account's days stay
// after OpenAI stops reporting them or the account is removed.
type CodexHistory struct {
	mu        sync.Mutex
	path      string
	Accounts  map[string]map[string]int64 `json:"accounts"` // account id, day, tokens
	FetchedMs int64                       `json:"fetched_ms"`
	fetching  bool
}

// maxHistoryDays bounds the days kept per account: thirty years of them.
const maxHistoryDays = 11_000

// OpenCodexHistory reads the history kept at path, or starts one.
func OpenCodexHistory(path string) *CodexHistory {
	h := &CodexHistory{path: path, Accounts: map[string]map[string]int64{}}
	if data, err := os.ReadFile(path); err == nil {
		var read CodexHistory
		if json.Unmarshal(data, &read) == nil && read.Accounts != nil {
			h.Accounts, h.FetchedMs = read.Accounts, read.FetchedMs
		}
	}
	return h
}

// Merge adds an account's days as just read.
func (h *CodexHistory) Merge(account string, days map[string]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	have := h.Accounts[account]
	if have == nil {
		have = map[string]int64{}
		h.Accounts[account] = have
	}
	for day, tokens := range days {
		if len(day) != 10 || tokens <= 0 || len(have) >= maxHistoryDays && have[day] == 0 {
			continue
		}
		have[day] = max(have[day], min(tokens, maxLedgerTotal))
	}
}

// Fetched notes that every account was read at now, and writes the file.
func (h *CodexHistory) Fetched(now time.Time) error {
	h.mu.Lock()
	h.FetchedMs = now.UnixMilli()
	data, err := json.Marshal(h)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// Days is every account's days added up, and when they were last read.
func (h *CodexHistory) Days() (map[string]int64, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int64{}
	for _, days := range h.Accounts {
		for day, tokens := range days {
			out[day] = min(out[day]+tokens, maxLedgerTotal)
		}
	}
	return out, h.FetchedMs
}

// Begin reports whether a read should start now: none is running and the
// last is older than every. The caller calls End when it is done.
func (h *CodexHistory) Begin(now time.Time, every time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fetching || now.UnixMilli()-h.FetchedMs < every.Milliseconds() && h.FetchedMs <= now.UnixMilli() {
		return false
	}
	h.fetching = true
	return true
}

// End ends a read begun with Begin.
func (h *CodexHistory) End() {
	h.mu.Lock()
	h.fetching = false
	h.mu.Unlock()
}

// CleanCodexDays keeps, of days from another computer, the ones that are
// days with a count: plain dates, bounded tokens, a bounded number.
func CleanCodexDays(days map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for day, tokens := range days {
		if len(out) >= maxHistoryDays {
			break
		}
		if _, err := time.Parse("2006-01-02", day); err != nil || tokens <= 0 {
			continue
		}
		out[day] = min(tokens, maxLedgerTotal)
	}
	return out
}
