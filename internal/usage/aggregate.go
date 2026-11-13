package usage

import (
	"sort"
	"time"
)

// Summary is the API response for one window.
type Summary struct {
	Since         string          `json:"since"`
	Until         string          `json:"until"`
	TimeZone      string          `json:"time_zone"`
	Providers     []ProviderUsage `json:"providers"`
	Models        []ModelUsage    `json:"models"`
	Days          []DayUsage      `json:"days"`
	Totals        Totals          `json:"totals"`
	TotalCost     float64         `json:"total_cost_usd"`
	TotalTokens   int64           `json:"total_tokens"`
	Sessions      int64           `json:"sessions"`
	Records       int64           `json:"records"`
	Unpriced      int64           `json:"unpriced_records"`
	UnpricedShare float64         `json:"unpriced_share"`
	CacheSavings  float64         `json:"cache_savings_usd"`
	ScannedAtMs   int64           `json:"scanned_at_ms"`
	ScanMs        int64           `json:"scan_ms"`
	// Devices is each computer's share, when more than one is counted:
	// a host and the Macs that use it.
	Devices []DeviceUsage `json:"devices,omitempty"`
}

// ProviderUsage aggregates one provider.
type ProviderUsage struct {
	Provider   string  `json:"provider"`
	Sessions   int64   `json:"sessions"`
	Cost       float64 `json:"cost_usd"`
	Tokens     int64   `json:"tokens"`
	CostShare  float64 `json:"cost_share"`
	TokenShare float64 `json:"token_share"`
}

// ModelUsage aggregates one provider+model pair.
type ModelUsage struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Cost     float64 `json:"cost_usd"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
	Unpriced bool    `json:"unpriced"`
}

// DayUsage is one local calendar day with per-provider values.
type DayUsage struct {
	Day       string             `json:"day"`
	Providers map[string]dayCell `json:"providers"`
}

type dayCell struct {
	Cost   float64 `json:"cost_usd"`
	Tokens int64   `json:"tokens"`
}

type bucketKey struct {
	day      string
	provider string
	model    string
}

type bucket struct {
	totals          Totals
	cost            float64
	savings         float64
	records         int64
	unpricedRecords int64
}

// SummaryFor runs the pipeline for this computer alone: scan every source,
// dedupe, price, and bucket by local day. since/until are inclusive local
// days.
func SummaryFor(sources []Source, cache *ScanCache, table RateTable, since, until time.Time, loc *time.Location) *Summary {
	if loc == nil {
		loc = time.Local
	}
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, loc)
	began := time.Now()
	ledger := collectLedger(sources, cache, table, start)
	summary := Combine([]DeviceLedger{{Key: "this", Ledger: ledger}}, "this", since, until, loc)
	summary.ScannedAtMs = began.UnixMilli()
	summary.ScanMs = time.Since(began).Milliseconds()
	return summary
}

func bucketFor(buckets map[bucketKey]*bucket, k bucketKey) *bucket {
	b, ok := buckets[k]
	if !ok {
		b = &bucket{}
		buckets[k] = b
	}
	return b
}

func addTotals(a, b Totals) Totals {
	return Totals{
		UncachedInput: a.UncachedInput + b.UncachedInput,
		CachedInput:   a.CachedInput + b.CachedInput,
		CacheCreation: a.CacheCreation + b.CacheCreation,
		Output:        a.Output + b.Output,
		Reasoning:     a.Reasoning + b.Reasoning,
	}
}

func buildSummary(buckets map[bucketKey]*bucket, totals Totals, totalCost, savings float64,
	records, unpriced, sessions int64) *Summary {
	type providerAgg struct {
		cost     float64
		tokens   int64
		sessions map[string]struct{}
	}
	type modelAgg struct {
		cost     float64
		tokens   int64
		records  int64
		unpriced int64
	}
	providers := map[string]*providerAgg{}
	models := map[[2]string]*modelAgg{}
	days := map[string]map[string]*dayCell{}

	for k, b := range buckets {
		tokens := b.totals.Processed()
		pa, ok := providers[k.provider]
		if !ok {
			pa = &providerAgg{sessions: map[string]struct{}{}}
			providers[k.provider] = pa
		}
		pa.cost += b.cost
		pa.tokens += tokens
		modelKey := [2]string{k.provider, k.model}
		ma := models[modelKey]
		if ma == nil {
			ma = &modelAgg{}
			models[modelKey] = ma
		}
		ma.cost += b.cost
		ma.tokens += tokens
		ma.records += b.records
		ma.unpriced += b.unpricedRecords
		dayCells := days[k.day]
		if dayCells == nil {
			dayCells = map[string]*dayCell{}
			days[k.day] = dayCells
		}
		cell := dayCells[k.provider]
		if cell == nil {
			cell = &dayCell{}
			dayCells[k.provider] = cell
		}
		cell.Cost += b.cost
		cell.Tokens += tokens
	}

	out := &Summary{
		Totals:       totals,
		TotalCost:    totalCost,
		TotalTokens:  totals.Processed(),
		CacheSavings: savings,
		Records:      records,
		Unpriced:     unpriced,
		Sessions:     sessions,
	}
	if records > 0 {
		out.UnpricedShare = float64(unpriced) / float64(records)
	}

	for provider, pa := range providers {
		out.Providers = append(out.Providers, ProviderUsage{
			Provider: provider, Cost: pa.cost, Tokens: pa.tokens, Sessions: int64(len(pa.sessions)),
		})
	}
	sort.Slice(out.Providers, func(i, j int) bool {
		if out.Providers[i].Cost != out.Providers[j].Cost {
			return out.Providers[i].Cost > out.Providers[j].Cost
		}
		return out.Providers[i].Provider < out.Providers[j].Provider
	})
	for i := range out.Providers {
		if totalCost > 0 {
			out.Providers[i].CostShare = out.Providers[i].Cost / totalCost
		}
		if out.TotalTokens > 0 {
			out.Providers[i].TokenShare = float64(out.Providers[i].Tokens) / float64(out.TotalTokens)
		}
	}

	keys := make([][2]string, 0, len(models))
	for k := range models {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := models[keys[i]], models[keys[j]]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		if a.tokens != b.tokens {
			return a.tokens > b.tokens
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		ma := models[k]
		out.Models = append(out.Models, ModelUsage{
			Provider: k[0], Model: k[1], Cost: ma.cost, Tokens: ma.tokens,
			Unpriced: ma.records > 0 && ma.unpriced >= ma.records,
		})
	}
	if totalCost > 0 {
		for i := range out.Models {
			out.Models[i].Share = out.Models[i].Cost / totalCost
		}
	}

	dayList := make([]string, 0, len(days))
	for day := range days {
		dayList = append(dayList, day)
	}
	sort.Strings(dayList)
	for _, day := range dayList {
		cells := days[day]
		du := DayUsage{Day: day, Providers: map[string]dayCell{}}
		for provider, cell := range cells {
			du.Providers[provider] = *cell
		}
		out.Days = append(out.Days, du)
	}
	return out
}
