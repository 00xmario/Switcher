package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The label Switcher writes must carry every field Claude Code checks before
// it re-fetches the profile, or Desktop's sessions relabel the config.
func TestNativeProfileCarriesClaudeCodesRecord(t *testing.T) {
	profile := `{"account":{"uuid":"uuid-b","email":"b@example.test","display_name":"B","full_name":"B Person","created_at":"2026-01-01T00:00:00Z","has_claude_max":true},
		"organization":{"uuid":"org-b","name":"B's Organization","organization_type":"claude_max","billing_type":"stripe_subscription","rate_limit_tier":"default_claude_max_20x",
		"seat_tier":null,"has_extra_usage_enabled":true,"subscription_status":"active","subscription_created_at":"2026-01-02T00:00:00Z","cc_onboarding_flags":{},
		"claude_code_trial_ends_at":null,"claude_code_trial_duration_days":null}}`
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/oauth/profile" || req.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Fatalf("unexpected fixture request %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(profile))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	identity, err := New().nativeProfile(context.Background(), "fixture-access")
	if err != nil {
		t.Fatal(err)
	}
	if identity.UUID != "uuid-b" || identity.OrganizationUUID != "org-b" || identity.Plan != "claude_max_20x" {
		t.Fatalf("identity %+v", identity)
	}
	for _, key := range []string{"billingType", "accountCreatedAt", "subscriptionCreatedAt", "ccOnboardingFlags", "profileFetchedAt"} {
		if len(identity.Record[key]) == 0 {
			t.Fatalf("record lacks %s: %v", key, identity.Record)
		}
	}
}
