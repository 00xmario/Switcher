package codex

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"switcher/internal/store"
)

func idTokenWith(t *testing.T, auth map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"email": "a@example.com", "https://api.openai.com/auth": auth})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// The paid-through day comes from the id_token's ChatGPT claims; a Free
// account, a malformed day or token says nothing.
func TestPaidThroughReadsTheIDToken(t *testing.T) {
	p := &Provider{}
	for name, tc := range map[string]struct {
		token string
		want  int64
	}{
		"paid":      {idTokenWith(t, map[string]any{"chatgpt_plan_type": "go", "chatgpt_subscription_active_until": "2026-11-20T20:19:13+00:00"}), 1795205953},
		"fractions": {idTokenWith(t, map[string]any{"chatgpt_subscription_active_until": "2026-11-20T20:19:13.5+00:00"}), 1795205953},
		"free":      {idTokenWith(t, map[string]any{"chatgpt_plan_type": "free", "chatgpt_subscription_active_until": nil}), 0},
		"garbled":   {idTokenWith(t, map[string]any{"chatgpt_subscription_active_until": "next week"}), 0},
		"no token":  {"", 0},
	} {
		if got := p.PaidThrough(store.Account{Token: store.Token{IDToken: tc.token}}); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}
