package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func TestProfileDaysReadsTheDailyTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("ChatGPT-Account-Id") != "acct" {
			t.Errorf("request: %s %v", r.Method, r.Header)
		}
		_, _ = w.Write([]byte(`{"profile":{"display_name":"Example User","username":"example.user"},"metadata":{"stats_as_of":"2026-09-09"},
			"stats":{"lifetime_tokens":142300000000,"daily_usage_buckets":[{"start_date":"2026-09-09","tokens":42},
			{"start_date":"2026-09-08T00:00:00Z","tokens":7},{"start_date":"nonsense","tokens":9},{"start_date":"2026-09-07","tokens":0}]}}`))
	}))
	defer srv.Close()
	old := profileURL
	profileURL = srv.URL
	defer func() { profileURL = old }()
	days, err := (&Provider{}).ProfileDays(context.Background(), store.Account{Token: store.Token{AccessToken: "tok", AccountID: "acct"}})
	if err != nil || len(days) != 2 || days[0] != (provider.DayTokens{Day: "2026-09-09", Tokens: 42}) || days[1] != (provider.DayTokens{Day: "2026-09-08", Tokens: 7}) {
		t.Fatalf("days %+v, %v", days, err)
	}
}
