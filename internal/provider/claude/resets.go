package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Claude banks limit resets as grants: one per kind (a Fable reset, a weekly
// reset, ...), each with a number of resets left. Claude Code reads them from
// the usage endpoint with cedar_ember=1 and spends one with a claim on the
// organization; Switcher does the same.
// claudeCodeUserAgent is how Claude Code introduces itself. Anthropic hands
// out an account's banked resets only to Claude Code (any other client is
// told "surface": ineligible), so the usage check and the claim say so, as
// the Codex provider does for the Codex CLI.
const claudeCodeUserAgent = "claude-cli/2.1.325 (external, cli)"

var (
	claudeAPIBase   = "https://api.anthropic.com"
	resetGrantID    = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	resetProgram    = "cedar_ember"
	resetClaimLimit = 25 * time.Second
)

type resetGrant struct {
	ID                string   `json:"id"`
	Label             string   `json:"label"`
	ResetsTotal       int      `json:"resets_total"`
	ResetsLeft        int      `json:"resets_left"`
	StartsAt          *string  `json:"starts_at"`
	EndsAt            *string  `json:"ends_at"`
	Paused            bool     `json:"paused"`
	UsableNow         bool     `json:"usable_now"`
	UseRequiresLimit  *bool    `json:"use_requires_limit"`
	Type              string   `json:"type"`
	GivesBackWeekly   bool     `json:"gives_back_weekly"`
	UnavailableReason *string  `json:"unavailable_reason"`
	Clears            []string `json:"clears"`
}

// resetSnapshot is the last usage answer for an account, which carries its
// banked resets; a listing soon after reuses it instead of asking again.
type resetSnapshot struct {
	token  string
	raw    []byte
	at     time.Time
	reason string
}

// The usage check runs every few minutes and refreshes the snapshot; the
// account view lists from it rather than asking Claude's rate-limited usage
// endpoint again. Spending a reset always asks afresh.
const (
	resetSnapshotTTL = 10 * time.Minute
	resetBackoff     = 5 * time.Minute
)

func (p *Provider) rememberResets(a store.Account, raw []byte) {
	if a.ID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resets == nil {
		p.resets = map[string]resetSnapshot{}
	}
	reason := resetIneligibility(raw)
	if old, ok := p.resets[a.ID]; !ok || old.reason != reason {
		if reason != "" {
			// Says why no reset shows, such as a client Anthropic no
			// longer accepts ("surface", "cli_version").
			log.Printf("claude: banked resets not offered for %s: %s", a.Email, reason)
		}
	}
	p.resets[a.ID] = resetSnapshot{token: a.Token.AccessToken, raw: raw, at: time.Now(), reason: reason}
}

// resetIneligibility is the server's reason for offering no resets, or "".
func resetIneligibility(raw []byte) string {
	var parsed struct {
		CedarEmber *struct {
			Eligible bool    `json:"eligible"`
			Reason   *string `json:"ineligible_reason"`
		} `json:"cedar_ember"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.CedarEmber == nil || parsed.CedarEmber.Eligible {
		return ""
	}
	if r := deref(parsed.CedarEmber.Reason); r != "" && len(r) <= 40 {
		return r
	}
	return "unknown"
}

func (p *Provider) recentResets(a store.Account) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	snap, ok := p.resets[a.ID]
	if !ok || a.ID == "" || snap.token != a.Token.AccessToken || time.Since(snap.at) > resetSnapshotTTL {
		return nil
	}
	return snap.raw
}

func (p *Provider) backOffResets(a store.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resetsWait == nil {
		p.resetsWait = map[string]time.Time{}
	}
	p.resetsWait[a.ID] = time.Now().Add(resetBackoff)
}

func (p *Provider) resetsBackingOff(a store.Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.resetsWait[a.ID])
}

func (p *Provider) forgetResets(a store.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resets, a.ID)
}

// ListResetCredits implements provider.ResetCreditProvider. Each reset left
// on a grant is one credit; the server's suggested grant comes first, then
// the earliest to expire.
func (p *Provider) ListResetCredits(ctx context.Context, a store.Account) ([]provider.ResetCredit, error) {
	if !provider.WantsFreshCredits(ctx) {
		if raw := p.recentResets(a); raw != nil {
			return parseResetCredits(raw, time.Now())
		}
		if p.resetsBackingOff(a) {
			return nil, errors.New("list reset credits: http 429")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeAPIBase+"/api/oauth/usage?cedar_ember=1&skip_spend=1", nil)
	if err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	req.Header.Set("User-Agent", claudeCodeUserAgent)
	raw, status, err := readLimited(req)
	if err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	if status != http.StatusOK {
		if status == http.StatusTooManyRequests {
			p.backOffResets(a)
		}
		return nil, fmt.Errorf("list reset credits: http %d", status)
	}
	p.rememberResets(a, raw)
	return parseResetCredits(raw, time.Now())
}

func parseResetCredits(raw []byte, now time.Time) ([]provider.ResetCredit, error) {
	var parsed struct {
		CedarEmber *struct {
			Eligible    bool              `json:"eligible"`
			Grants      []json.RawMessage `json:"grants"`
			NextGrantID *string           `json:"next_grant_id"`
		} `json:"cedar_ember"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	status := parsed.CedarEmber
	if status == nil {
		return nil, nil
	}
	next := ""
	if status.NextGrantID != nil {
		next = *status.NextGrantID
	}
	type ranked struct {
		credit provider.ResetCredit
		next   bool
	}
	var out []ranked
	for _, item := range status.Grants {
		var g resetGrant
		// A grant Switcher cannot read is left out, as Claude Code does.
		if json.Unmarshal(item, &g) != nil || !resetGrantID.MatchString(g.ID) || g.ResetsLeft <= 0 || g.ResetsLeft > 100 || g.Paused {
			continue
		}
		expires := parseTimeOrZero(g.EndsAt)
		if expires > 0 && expires <= now.Unix() {
			continue
		}
		total := max(g.ResetsTotal, g.ResetsLeft)
		period := sha256.Sum256([]byte(deref(g.StartsAt) + "|" + deref(g.EndsAt) + "|" + strconv.Itoa(total)))
		kind, note := grantKind(g), grantNote(g)
		// Numbered in the order of use: the next to spend is use number
		// total-left+1. A list that lags behind a spend still offers the
		// right one once the spent number is filtered, a claim whose answer
		// was lost is retried under the same number (and request id), and
		// a grant topped up gets a new total and so new ids.
		for n := total - g.ResetsLeft + 1; n <= total; n++ {
			out = append(out, ranked{next: g.ID == next, credit: provider.ResetCredit{
				ID:        strconv.Itoa(n) + "-" + hex.EncodeToString(period[:4]) + "-" + g.ID,
				ExpiresAt: expires,
				Kind:      kind,
				Unusable:  note,
			}})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].next != out[j].next {
			return out[i].next
		}
		if (out[i].credit.Unusable == "") != (out[j].credit.Unusable == "") {
			return out[i].credit.Unusable == ""
		}
		ei, ej := out[i].credit.ExpiresAt, out[j].credit.ExpiresAt
		if ei == 0 || ej == 0 {
			return ei != 0
		}
		return ei < ej
	})
	credits := make([]provider.ResetCredit, len(out))
	for i, r := range out {
		credits[i] = r.credit
	}
	return credits, nil
}

// grantKind names a grant as Claude Code does, by what it refills; the
// server's label is a campaign title ("New model launch: one Fable reset
// for Max, ..."), so it is not the name.
func grantKind(g resetGrant) string {
	if g.GivesBackWeekly {
		return "Fable reset"
	}
	switch g.Type {
	case "session":
		return "5-hour reset"
	case "weekly":
		return "Weekly reset"
	case "full":
		return "Full reset"
	}
	return "Limit reset"
}

func grantNote(g resetGrant) string {
	if g.UsableNow {
		return ""
	}
	switch deref(g.UnavailableReason) {
	case "cooldown":
		return "Cooling down, try again later"
	case "blocked_five_hour":
		return "Can't be used while you're at your 5-hour limit"
	case "blocked_seven_day":
		return "Can't be used while you're at your weekly limit"
	case "blocked_opus", "blocked_sonnet", "blocked_other":
		return "Can't be used while you're at another limit it doesn't refill"
	case "cannot_clear":
		return "Doesn't refill the limit you've reached"
	case "needs_limit":
		return "Usable once you reach a limit it refills"
	}
	if g.UseRequiresLimit == nil || *g.UseRequiresLimit {
		return "Usable once you reach a limit it refills"
	}
	return "Can't be used right now"
}

// ConsumeResetCredit implements provider.ResetCreditProvider. The request id
// is fixed per account and credit, so a retried claim cannot spend twice.
// Outcomes: "reset" and "already_redeemed" (spent), "nothing_to_reset",
// "cooldown", "not_next_grant" and "unavailable" (nothing spent).
func (p *Provider) ConsumeResetCredit(ctx context.Context, a store.Account, creditID string) (string, error) {
	grant, ok := creditGrant(creditID)
	if !ok {
		return "", errors.New("consume reset credit: invalid credit")
	}
	ctx, cancel := context.WithTimeout(ctx, resetClaimLimit)
	defer cancel()
	// Whatever the answer, the next listing asks Claude again.
	defer p.forgetResets(a)
	profile, err := p.fetchProfile(ctx, a.Token.AccessToken)
	if err != nil {
		if strings.Contains(err.Error(), "http 401") {
			return "", errors.New("consume reset credit: http 401")
		}
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	org := profile.Organization.UUID
	if org == "" || strings.ContainsAny(org, "/?#%") {
		return "", errors.New("consume reset credit: no organization")
	}
	sum := sha256.Sum256([]byte("switcher-claude-reset|" + profile.Account.UUID + "|" + a.ID + "|" + creditID))
	body, err := json.Marshal(map[string]string{
		"program":    resetProgram,
		"grant_id":   grant,
		"request_id": hex.EncodeToString(sum[:16]),
	})
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeAPIBase+"/api/organizations/"+org+"/reset_rate_limits", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeCodeUserAgent)
	raw, status, err := readLimited(req)
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("consume reset credit: http %d", status)
	}
	var out struct {
		Result string  `json:"result"`
		Reason *string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if deref(out.Reason) == "not_next_grant" {
		return "not_next_grant", nil
	}
	switch out.Result {
	case "reset", "already_used":
		// The usage ParseRateLimit remembers is from before the reset: a
		// refusal right after must not park the account on it.
		p.mu.Lock()
		delete(p.limits, a.ID)
		p.mu.Unlock()
		if out.Result == "already_used" {
			// Claude Code counts it as spent: a retried claim that went through.
			return "already_redeemed", nil
		}
		return out.Result, nil
	case "cooldown":
		return out.Result, nil
	case "not_limited":
		return "nothing_to_reset", nil
	case "":
		return "", errors.New("consume reset credit: missing result")
	default:
		return "unavailable", nil
	}
}

// creditGrant reads the grant out of a credit id: "<n>-<period>-<grant>",
// letters, digits, - and _ only, as the phone's check allows.
func creditGrant(id string) (string, bool) {
	parts := strings.SplitN(id, "-", 3)
	if len(id) > 80 || len(parts) != 3 || len(parts[1]) != 8 || !resetGrantID.MatchString(parts[2]) {
		return "", false
	}
	if n, err := strconv.Atoi(parts[0]); err != nil || n < 1 || n > 100 || strconv.Itoa(n) != parts[0] {
		return "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", false
	}
	return parts[2], true
}

func readLimited(req *http.Request) ([]byte, int, error) {
	resp, err := provider.Client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > 1<<20 {
		return nil, 0, errors.New("response body too large")
	}
	return raw, resp.StatusCode, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
