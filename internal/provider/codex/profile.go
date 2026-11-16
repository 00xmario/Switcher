package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// profileURL is where Codex's own clients read an account's profile: its
// token statistics, as its desktop app and /usage show them.
var profileURL = "https://chatgpt.com/backend-api/wham/profiles/me"

// ProfileDays implements provider.ProfileProvider: it reads the account's daily token totals from its Codex
// profile. It only reads; the days are OpenAI's own (their dates, their
// count), for usage this computer's logs no longer hold.
func (p *Provider) ProfileDays(ctx context.Context, a store.Account) ([]provider.DayTokens, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("codex profile: %w", err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return nil, fmt.Errorf("codex profile: %w", err)
	}
	resp, err := provider.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex profile: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("codex profile: %w", err)
	}
	if len(raw) > 4<<20 {
		return nil, errors.New("codex profile: response body too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex profile: http %d", resp.StatusCode)
	}
	var parsed struct {
		Stats struct {
			Buckets []struct {
				Start  string `json:"start_date"`
				Tokens int64  `json:"tokens"`
			} `json:"daily_usage_buckets"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("codex profile: invalid response")
	}
	days := make([]provider.DayTokens, 0, len(parsed.Stats.Buckets))
	for _, b := range parsed.Stats.Buckets {
		// A date, or a time that starts with one.
		if len(b.Start) < 10 || b.Tokens <= 0 {
			continue
		}
		if _, err := time.Parse("2006-01-02", b.Start[:10]); err != nil {
			continue
		}
		days = append(days, provider.DayTokens{Day: b.Start[:10], Tokens: b.Tokens})
	}
	return days, nil
}
