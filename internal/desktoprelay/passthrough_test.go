package desktoprelay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switcher/internal/desktoprelay"
)

// After Switcher hands Claude back to Anthropic, a Desktop that still runs
// with the relay keeps working on its own login: no conversation, picked or
// not, gets a Switcher account, and paired Switchers are refused.
func TestPassthroughSendsEveryRequestWithDesktopsOwnLogin(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(echoAuth)
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer token-A" {
		t.Fatalf("picked conversation before hand-back: %q", got)
	}

	m.SetPassthrough(true)
	for _, session := range []string{sessionA, sessionB} {
		if got := drain(t, send(t, c, br, "/v1/messages", `{}`, session, nil)); got != "Bearer caller-token" {
			t.Fatalf("%s after hand-back used %q", session, got)
		}
	}
	r := httptest.NewRequest("POST", "/remote/anthropic/v1/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	m.ServeAccount(w, r, "/v1/messages", "A")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "token-A") {
		t.Fatalf("paired Switcher after hand-back: %d %s", w.Code, w.Body)
	}

	m.SetPassthrough(false)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer token-A" {
		t.Fatalf("picked conversation after setting Switcher up again: %q", got)
	}
}

func TestPassthroughKeepsTheRelayStoppedOrRunning(t *testing.T) {
	cfg := fixtureConfig(t)
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPassthrough(true)
	if m.Status().Listening {
		t.Fatal("pass-through started the relay")
	}
}
