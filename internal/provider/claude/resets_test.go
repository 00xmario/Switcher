package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// The shapes below follow Claude Code's own reading of the usage endpoint's
// cedar_ember block and of the reset_rate_limits claim.
const resetFixture = `{"five_hour":{"utilization":40},"cedar_ember":{"eligible":true,"at_limit":false,
 "next_grant_id":"weekly_gift",
 "grants":[
  {"id":"fable_gift","label":"Fable reset","resets_total":2,"resets_left":2,"ends_at":"2026-12-01T00:00:00Z","starts_at":"2026-11-01T00:00:00Z","usable_now":false,"use_requires_limit":true,"gives_back_weekly":true,"clears":["seven_day_omelette"]},
  {"id":"weekly_gift","label":"","type":"weekly","resets_total":1,"resets_left":1,"ends_at":"2026-12-20T00:00:00Z","usable_now":true,"use_requires_limit":false},
  {"id":"spent","label":"Old","resets_total":1,"resets_left":0,"usable_now":true},
  {"id":"paused_one","label":"Paused","resets_total":1,"resets_left":1,"paused":true},
  {"id":"gone","label":"Expired","resets_total":1,"resets_left":1,"ends_at":"2020-01-01T00:00:00Z"},
  {"id":"BAD ID","label":"Bad","resets_total":1,"resets_left":1},
  {"id":"cool","label":"","type":"session","resets_left":1,"usable_now":false,"unavailable_reason":"cooldown"}
 ]}}`

func TestParseResetCreditsOrdersAndNames(t *testing.T) {
	credits, err := parseResetCredits([]byte(resetFixture), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range credits {
		got = append(got, c.Kind+"|"+c.Unusable)
		grant, ok := creditGrant(c.ID)
		if !ok {
			t.Fatalf("credit id %q does not parse back", c.ID)
		}
		for _, r := range c.ID {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				t.Fatalf("credit id %q has %q, which the phone refuses", c.ID, r)
			}
		}
		if c.Kind == "Fable reset" && grant != "fable_gift" {
			t.Fatalf("fable credit points at %q", grant)
		}
	}
	want := []string{
		"Weekly reset|", // the server's suggestion first
		"Fable reset|Usable once you reach a limit it refills",
		"Fable reset|Usable once you reach a limit it refills",
		"5-hour reset|Cooling down, try again later",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("credits:\n got %v\nwant %v", got, want)
	}
	// Numbered in the order of use: neither Fable reset is used yet.
	if !strings.HasPrefix(credits[1].ID, "1-") || !strings.HasPrefix(credits[2].ID, "2-") {
		t.Fatalf("fable credits numbered %q, %q", credits[1].ID, credits[2].ID)
	}
	if credits[1].ExpiresAt != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("expiry %d", credits[1].ExpiresAt)
	}
}

func TestParseResetCreditsWithoutBlock(t *testing.T) {
	credits, err := parseResetCredits([]byte(`{"five_hour":{"utilization":1}}`), time.Now())
	if err != nil || len(credits) != 0 {
		t.Fatalf("credits %v, err %v", credits, err)
	}
}

func TestCreditGrantRejectsForgedIDs(t *testing.T) {
	for _, id := range []string{"", "fable", "1-zzzzzzzz-fable", "0-0011aabb-fable", "01-0011aabb-fable", "1-0011aabb-BAD", "1-0011aabb-a/b", "1-0011aab-fable"} {
		if _, ok := creditGrant(id); ok {
			t.Fatalf("%q accepted", id)
		}
	}
	if g, ok := creditGrant("12-0011aabb-fable_gift-x"); !ok || g != "fable_gift-x" {
		t.Fatalf("got %q %v", g, ok)
	}
}

func TestResetCreditsRoundTrip(t *testing.T) {
	var claims []map[string]string
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("%s %s without the account's token", req.Method, req.URL)
		}
		reply := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		}
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/api/oauth/usage" && req.URL.Query().Get("cedar_ember") == "1":
			return reply(200, resetFixture)
		case req.Method == http.MethodGet && req.URL.Path == "/api/oauth/profile":
			return reply(200, `{"account":{"uuid":"acct-1"},"organization":{"uuid":"org-1"}}`)
		case req.Method == http.MethodPost && req.URL.Path == "/api/organizations/org-1/reset_rate_limits":
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			claims = append(claims, body)
			return reply(200, `{"result":"reset","resets_left":1,"cleared":["seven_day_omelette"]}`)
		}
		t.Errorf("unexpected %s %s", req.Method, req.URL)
		return reply(404, `{}`)
	})}
	t.Cleanup(func() { http.DefaultClient = old })

	p := New()
	account := store.Account{ID: "claude-1", Provider: "claude", Token: store.Token{AccessToken: "fixture-token"}}
	credits, err := p.ListResetCredits(context.Background(), account)
	if err != nil || len(credits) != 4 {
		t.Fatalf("credits %v, err %v", credits, err)
	}
	for i := 0; i < 2; i++ { // a retry sends the same request id
		outcome, err := p.ConsumeResetCredit(context.Background(), account, credits[1].ID)
		if err != nil || outcome != "reset" {
			t.Fatalf("outcome %q, err %v", outcome, err)
		}
	}
	if len(claims) != 2 || claims[0]["program"] != "cedar_ember" || claims[0]["grant_id"] != "fable_gift" ||
		claims[0]["request_id"] == "" || claims[0]["request_id"] != claims[1]["request_id"] {
		t.Fatalf("claims %v", claims)
	}
	if id := claims[0]["request_id"]; len(id) > 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("request id %q", id)
	}
	other, _ := p.ConsumeResetCredit(context.Background(), account, credits[2].ID)
	if other != "reset" || claims[2]["request_id"] == claims[0]["request_id"] {
		t.Fatalf("the second Fable reset reused the first one's request id: %v", claims)
	}
}

func TestConsumeResetCreditOutcomes(t *testing.T) {
	for result, want := range map[string]string{"reset": "reset", "already_used": "already_redeemed", "not_limited": "nothing_to_reset",
		"cooldown": "cooldown", "ineligible": "unavailable", "unavailable": "unavailable", "something_new": "unavailable"} {
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
			body := `{"result":"` + result + `"}`
			if req.URL.Path == "/api/oauth/profile" {
				body = `{"account":{"uuid":"a"},"organization":{"uuid":"org-1"}}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})}
		got, err := New().ConsumeResetCredit(context.Background(), store.Account{Token: store.Token{AccessToken: "t"}}, "1-0011aabb-fable")
		http.DefaultClient = old
		if err != nil || got != want {
			t.Fatalf("%s: got %q, err %v", result, got, err)
		}
	}
}

func TestResetCreditsAuthFailureIsALeafError(t *testing.T) {
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	account := store.Account{Token: store.Token{AccessToken: "t"}}
	if _, err := New().ListResetCredits(context.Background(), account); err == nil || err.Error() != "list reset credits: http 401" {
		t.Fatalf("list: %v", err)
	}
	if _, err := New().ConsumeResetCredit(context.Background(), account, "1-0011aabb-fable"); err == nil || err.Error() != "consume reset credit: http 401" {
		t.Fatalf("consume: %v", err)
	}
}

func TestUsageAnswerServesTheResetListing(t *testing.T) {
	calls := 0
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Query().Get("cedar_ember") != "1" || req.URL.Query().Get("skip_spend") != "1" {
			t.Errorf("usage asked without the resets: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resetFixture)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	p := New()
	account := store.Account{ID: "claude-1", Token: store.Token{AccessToken: "t"}}
	if usage, err := p.Usage(context.Background(), account); err != nil || !usage.Available {
		t.Fatalf("usage %v, err %v", usage, err)
	}
	if credits, err := p.ListResetCredits(context.Background(), account); err != nil || len(credits) != 4 || calls != 1 {
		t.Fatalf("credits %d, err %v, calls %d", len(credits), err, calls)
	}
	rotated := account
	rotated.Token.AccessToken = "t2"
	if _, err := p.ListResetCredits(context.Background(), rotated); err != nil || calls != 2 {
		t.Fatalf("a new token reused the old answer: calls %d, err %v", calls, err)
	}
}

func TestCreditIDsAfterASpendAndATopUp(t *testing.T) {
	grants := func(total, left int) []byte {
		return []byte(`{"cedar_ember":{"eligible":true,"grants":[{"id":"fable","label":"Fable reset","resets_total":` +
			strconv.Itoa(total) + `,"resets_left":` + strconv.Itoa(left) + `,"usable_now":true}]}}`)
	}
	ids := func(raw []byte) []string {
		credits, err := parseResetCredits(raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range credits {
			out = append(out, c.ID)
		}
		return out
	}
	before, after, topped := ids(grants(2, 2)), ids(grants(2, 1)), ids(grants(3, 2))
	// After one is spent the other keeps its id; the spent one is gone.
	if len(after) != 1 || after[0] != before[1] {
		t.Fatalf("before %v, after %v", before, after)
	}
	// Topped up, the ids are new, so none is hidden as already spent.
	for _, id := range topped {
		for _, old := range before {
			if id == old {
				t.Fatalf("topped-up grant reused %s", id)
			}
		}
	}
}

func TestConsumeResetCreditNotNextGrant(t *testing.T) {
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"result":"unavailable","reason":"not_next_grant"}`
		if req.URL.Path == "/api/oauth/profile" {
			body = `{"account":{"uuid":"a"},"organization":{"uuid":"org-1"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	if got, err := New().ConsumeResetCredit(context.Background(), store.Account{Token: store.Token{AccessToken: "t"}}, "1-0011aabb-fable"); err != nil || got != "not_next_grant" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestResetListingBacksOffAfterTooManyRequests(t *testing.T) {
	calls := 0
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	p := New()
	account := store.Account{ID: "claude-1", Token: store.Token{AccessToken: "t"}}
	for i := 0; i < 3; i++ {
		if _, err := p.ListResetCredits(context.Background(), account); err == nil {
			t.Fatal("a refused listing succeeded")
		}
	}
	if calls != 1 {
		t.Fatalf("asked %d times during the backoff", calls)
	}
	// Spending asks anyway.
	_, _ = p.ListResetCredits(provider.FreshCredits(context.Background()), account)
	if calls != 2 {
		t.Fatalf("a fresh listing did not ask: %d", calls)
	}
}

// The answer Anthropic gave for a real Max account (ids and dates kept,
// nothing personal in it): one full reset of the weekly kind and two Fable
// resets on separate grants with long campaign labels.
const liveResets = `{"cedar_ember":{"eligible":true,"ineligible_reason":null,"at_limit":false,"next_grant_id":"full-reset-2611","grants":[
 {"id":"full-reset-2611","label":"One full limit reset (November 2026)","type":"weekly","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false,"paused":false,"unavailable_reason":null,"gives_back_weekly":false,"ends_at":"2026-12-10T22:00:00+00:00"},
 {"id":"launch-2610-fable","label":"New model launch: one Fable reset for Max, Team Premium and Enterprise Premium","type":"full","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false,"paused":false,"unavailable_reason":null,"gives_back_weekly":true,"ends_at":"2027-03-09T07:59:00+00:00"},
 {"id":"launch-2610-fable-6mo","label":"New model launch: one more Fable reset at 6 months' tenure","type":"full","resets_total":1,"resets_left":1,"usable_now":true,"use_requires_limit":false,"paused":false,"unavailable_reason":null,"gives_back_weekly":true,"ends_at":"2027-03-09T07:59:00+00:00"}]}}`

func TestLiveResetAnswer(t *testing.T) {
	credits, err := parseResetCredits([]byte(liveResets), time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range credits {
		grant, _ := creditGrant(c.ID)
		got = append(got, c.Kind+"|"+grant+"|"+c.Unusable)
	}
	want := "Weekly reset|full-reset-2611|,Fable reset|launch-2610-fable|,Fable reset|launch-2610-fable-6mo|"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
}

func TestResetRequestsIntroduceThemselvesAsClaudeCode(t *testing.T) {
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/profile" && !strings.HasPrefix(req.Header.Get("User-Agent"), "claude-cli/") {
			t.Errorf("%s %s sent as %q", req.Method, req.URL.Path, req.Header.Get("User-Agent"))
		}
		body := liveResets
		switch req.URL.Path {
		case "/api/oauth/profile":
			body = `{"account":{"uuid":"a"},"organization":{"uuid":"org-1"}}`
		case "/api/organizations/org-1/reset_rate_limits":
			body = `{"result":"not_limited"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	p := New()
	account := store.Account{ID: "claude-1", Token: store.Token{AccessToken: "t"}}
	if _, err := p.Usage(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	credits, err := p.ListResetCredits(provider.FreshCredits(context.Background()), account)
	if err != nil || len(credits) != 3 {
		t.Fatalf("credits %d, err %v", len(credits), err)
	}
	if _, err := p.ConsumeResetCredit(context.Background(), account, credits[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestResetIneligibility(t *testing.T) {
	for raw, want := range map[string]string{
		`{"cedar_ember":{"eligible":false,"ineligible_reason":"surface","grants":[]}}`: "surface",
		`{"cedar_ember":{"eligible":false}}`:                                           "unknown",
		liveResets:                                                                     "",
		`{"five_hour":{}}`:                                                             "",
	} {
		if got := resetIneligibility([]byte(raw)); got != want {
			t.Fatalf("%s: got %q want %q", raw[:30], got, want)
		}
	}
}
