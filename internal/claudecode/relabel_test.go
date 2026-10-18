package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// profileJSON mirrors /api/oauth/profile for a fixture account.
func profileJSON(name string) []byte {
	identity := identityFor(name)
	return encoded(map[string]any{
		"account": map[string]any{"uuid": identity.UUID, "email": identity.Email, "display_name": "Fixture", "full_name": "Fixture Person", "created_at": "2026-01-01T00:00:00Z", "has_claude_max": true},
		"organization": map[string]any{"uuid": identity.OrganizationUUID, "name": identity.OrganizationName, "organization_type": "claude_max", "billing_type": "stripe_subscription",
			"rate_limit_tier": "default_claude_max_20x", "seat_tier": nil, "has_extra_usage_enabled": true, "subscription_created_at": "2026-01-02T00:00:00Z",
			"cc_onboarding_flags": map[string]any{}, "claude_code_trial_ends_at": nil, "claude_code_trial_duration_days": nil},
	})
}

// withProfileRecords makes the fixture profile return what the provider does:
// the identity plus Claude Code's oauthAccount record.
func withProfileRecords(h *harness) *int {
	calls := 0
	original := h.manager.profile
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		calls++
		identity, err := original(ctx, token)
		if err != nil {
			return identity, err
		}
		name := strings.TrimPrefix(identity.UUID, "uuid-")
		identity.Record, err = OAuthAccountRecord(profileJSON(name), time.Now())
		return identity, err
	}
	return &calls
}

// installLive puts a rotated generation of name's login in the keychain, the
// way Claude Code leaves it after refreshing on its own.
func installLive(t *testing.T, h *harness, name, access, refresh string) {
	t.Helper()
	installLiveUntil(t, h, name, access, refresh, time.Now().Add(time.Hour))
}

func installLiveUntil(t *testing.T, h *harness, name, access, refresh string, expires time.Time) {
	t.Helper()
	live, _ := object(rawCredential(name))
	oauth, _ := object(live["claudeAiOauth"])
	oauth["accessToken"] = encoded(access)
	oauth["refreshToken"] = encoded(refresh)
	oauth["expiresAt"] = encoded(expires.UnixMilli())
	live["claudeAiOauth"] = encoded(oauth)
	h.keys.values[h.paths.Service] = encoded(live)
}

// labelDesktop rewrites oauthAccount as Claude Desktop's Code sessions do,
// keeping the rest of the config.
func labelDesktop(t *testing.T, h *harness, name string) {
	t.Helper()
	current, _ := readPrivate(h.paths.ConfigFile)
	config, _ := object(current.Data)
	config["oauthAccount"] = encoded(identityFor(name))
	if err := writePrivate(h.paths.ConfigFile, fileValue{Data: encoded(config), Exists: true}); err != nil {
		t.Fatal(err)
	}
}

func configRecord(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	current, _ := readPrivate(h.paths.ConfigFile)
	config, _ := object(current.Data)
	record, err := object(config["oauthAccount"])
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// Claude Code re-fetches its profile when any of these is missing or the
// fetch is a day old, and then labels the config with its own token's
// account. Desktop's sessions hold Desktop's login.
func assertCompleteRecord(t *testing.T, record map[string]json.RawMessage, name string) {
	t.Helper()
	for _, key := range []string{"billingType", "accountCreatedAt", "subscriptionCreatedAt", "ccOnboardingFlags"} {
		if len(record[key]) == 0 {
			t.Fatalf("oauthAccount lacks %s, so Claude Code re-fetches it: %s", key, encoded(record))
		}
	}
	fetched, err := strconv.ParseInt(string(record["profileFetchedAt"]), 10, 64)
	if err != nil || time.Since(time.UnixMilli(fetched)) > time.Minute {
		t.Fatalf("profileFetchedAt %s is not fresh", record["profileFetchedAt"])
	}
	var uuid string
	_ = json.Unmarshal(record["accountUuid"], &uuid)
	if uuid != "uuid-"+name {
		t.Fatalf("label names %q, want %s", uuid, name)
	}
}

func TestDesktopLabelNeverBlocksAccountsAndFollowsTheLogin(t *testing.T) {
	h := newHarness(t, true)
	withProfileRecords(h)
	ctx := context.Background()
	// Claude Code holds B's login, refreshed by itself; a Desktop session
	// labeled the shared config with Desktop's account, A.
	installLive(t, h, "b", "at-b-rotated", "rt-b-current")
	labelDesktop(t, h, "a")

	a, _ := h.accounts.Get("claude-a")
	changed, err := h.manager.Synchronize(ctx, &a)
	if err != nil || changed || a.Token.RefreshToken != "rt-a" {
		t.Fatalf("a mislabeled config blocked or touched another account: changed=%v err=%v", changed, err)
	}
	if status := h.manager.Status(); status.ActiveID != "claude-b" || status.Condition != "ready" {
		t.Fatalf("status %+v; Claude Code runs as B", status)
	}

	b, _ := h.accounts.Get("claude-b")
	changed, err = h.manager.Synchronize(ctx, &b)
	if err != nil || !changed || b.Token.RefreshToken != "rt-b-current" || b.Token.AccessToken != "at-b-rotated" {
		t.Fatalf("B did not adopt its live generation: changed=%v err=%v token=%+v", changed, err, b.Token)
	}
	assertCompleteRecord(t, configRecord(t, h), "b")
	current, _ := readPrivate(h.paths.ConfigFile)
	config, _ := object(current.Data)
	for _, key := range []string{"projects", "mcpServers", "userID", "unknownMachineSetting"} {
		if len(config[key]) == 0 {
			t.Fatalf("relabeling dropped %s", key)
		}
	}
	stored, _ := object(b.ClaudeCode.OAuthAccount)
	if len(stored["billingType"]) == 0 {
		t.Fatal("the account kept an incomplete label for its next switch")
	}
}

// Switcher's refresh of Claude Code's own login requires a matching label, so
// the relabel must come first when the live token has expired.
func TestRelabelLetsSwitcherRefreshAnExpiredLogin(t *testing.T) {
	h := newHarness(t, true)
	withProfileRecords(h)
	ctx := context.Background()
	installLiveUntil(t, h, "b", "at-b-rotated", "rt-b-current", time.Now().Add(-time.Minute))
	labelDesktop(t, h, "a")
	b, _ := h.accounts.Get("claude-b")
	if _, err := h.manager.Synchronize(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.Refresh(ctx, &b, h.manager.grant); err != nil || h.grants != 1 {
		t.Fatalf("refresh after relabel: err=%v grants=%d", err, h.grants)
	}
	current, _ := h.manager.inspect(ctx)
	live, _ := parseOAuth(current.credential())
	if live.RefreshToken != "rt-successor" || b.Token.RefreshToken != "rt-successor" {
		t.Fatalf("successor not installed: live %q account %q", live.RefreshToken, b.Token.RefreshToken)
	}
}

func TestRelabelFetchesAProfileForAKnownLogin(t *testing.T) {
	h := newHarness(t, true)
	calls := withProfileRecords(h)
	ctx := context.Background()
	// The keychain holds exactly B's saved generation, so the owner is known
	// without asking Anthropic. The label still needs a fresh record.
	installLive(t, h, "b", "at-b", "rt-b")
	labelDesktop(t, h, "a")
	b, _ := h.accounts.Get("claude-b")
	if _, err := h.manager.Synchronize(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("profile fetched %d times, want once for the label", *calls)
	}
	assertCompleteRecord(t, configRecord(t, h), "b")
	// Once consistent, later polls neither fetch nor write.
	before, _ := readPrivate(h.paths.ConfigFile)
	if _, err := h.manager.Synchronize(ctx, &b); err != nil {
		t.Fatal(err)
	}
	after, _ := readPrivate(h.paths.ConfigFile)
	if *calls != 1 || string(before.Data) != string(after.Data) {
		t.Fatalf("a consistent label was rewritten (profile calls %d)", *calls)
	}
}

func TestUnreachableProfileStillRelabelsFromTheSavedRecord(t *testing.T) {
	h := newHarness(t, true)
	installLive(t, h, "b", "at-b", "rt-b")
	labelDesktop(t, h, "a")
	h.manager.profile = func(context.Context, string) (Identity, error) { return Identity{}, context.DeadlineExceeded }
	b, _ := h.accounts.Get("claude-b")
	if _, err := h.manager.Synchronize(context.Background(), &b); err != nil {
		t.Fatalf("an offline profile must not block the account: %v", err)
	}
	if identity, _ := identityOf(mustRead(t, h.paths.ConfigFile)); identity.UUID != "uuid-b" {
		t.Fatalf("label %q, want B's saved identity", identity.UUID)
	}
}

func TestSwitchWritesALabelClaudeCodeKeeps(t *testing.T) {
	h := newHarness(t, true)
	withProfileRecords(h)
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertCompleteRecord(t, configRecord(t, h), "b")
}

func TestOAuthAccountRecordCarriesClaudeCodesFields(t *testing.T) {
	fetched := time.UnixMilli(1792326931069)
	record, err := OAuthAccountRecord(profileJSON("b"), fetched)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"accountUuid": `"uuid-b"`, "emailAddress": `"b@example.test"`, "organizationUuid": `"org-b"`, "organizationName": `"Fixture b"`,
		"displayName": `"Fixture"`, "fullName": `"Fixture Person"`, "accountCreatedAt": `"2026-01-01T00:00:00Z"`,
		"subscriptionCreatedAt": `"2026-01-02T00:00:00Z"`, "billingType": `"stripe_subscription"`, "organizationType": `"claude_max"`,
		"organizationRateLimitTier": `"default_claude_max_20x"`, "hasExtraUsageEnabled": `true`, "ccOnboardingFlags": `{}`,
		"seatTier": `null`, "claudeCodeTrialEndsAt": `null`, "claudeCodeTrialDurationDays": `null`, "profileFetchedAt": `1792326931069`,
	}
	for key, value := range want {
		if string(record[key]) != value {
			t.Errorf("%s = %s, want %s", key, record[key], value)
		}
	}
	if len(record) != len(want) {
		t.Errorf("unexpected fields: %s", encoded(record))
	}
	if _, err := OAuthAccountRecord([]byte(`{"account":{},"organization":{}}`), fetched); err == nil {
		t.Fatal("a profile without an account produced a label")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	value, err := readPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	return value.Data
}

// An interrupted refresh of Claude Code's login must still finish after a
// Desktop session relabels the config, or every later switch stops on it.
func TestInterruptedRefreshFinishesUnderADesktopLabel(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	ctx := context.Background()
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture keychain write failed")
	if err := h.manager.Refresh(ctx, &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not interrupt the native refresh")
	}
	h.keys.writeErr = nil
	labelDesktop(t, h, "b")
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	a, _ = h.accounts.Get("claude-a")
	if _, err := restarted.Synchronize(ctx, &a); err != nil {
		t.Fatalf("recovery stopped on Desktop's label: %v", err)
	}
	current, _ := restarted.inspect(ctx)
	live, _ := parseOAuth(current.credential())
	if live.RefreshToken != "rt-successor" || h.grants != 1 {
		t.Fatalf("the received successor was not installed (live %q, grants %d)", live.RefreshToken, h.grants)
	}
	if identity, _ := identityOf(current.Config.Data); identity.UUID != "uuid-a" {
		t.Fatalf("label %q after recovery, want A", identity.UUID)
	}
	if _, err := restarted.Switch(ctx, "claude-b", func() error { return nil }); err != nil {
		t.Fatalf("switching stayed blocked: %v", err)
	}
}
