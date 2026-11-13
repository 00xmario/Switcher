package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A callback with a wrong code fails at the token endpoint; the verifier
// stays for the real one, and goes once that has exchanged.
func TestFailedExchangeKeepsTheVerifier(t *testing.T) {
	p := New()
	p.verifier["the-verifier"] = verifierEntry{value: "the-verifier", startedAt: time.Now()}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		if fields["code_verifier"] != "the-verifier" {
			t.Errorf("exchange without the verifier: %s", raw)
		}
		if fields["code"] != "real-code" {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Header: http.Header{}}, nil
		}
		answer := `{"access_token":"a","refresh_token":"r","expires_in":3600,"account":{"uuid":"11111111-2222-3333-4444-555555555555","email_address":"you@example.com"}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	if _, err := p.LoginExchange(context.Background(), "the-verifier", "forged"); err == nil {
		t.Fatal("a wrong code exchanged")
	}
	p.mu.Lock()
	_, kept := p.verifier["the-verifier"]
	p.mu.Unlock()
	if !kept {
		t.Fatal("a failed exchange used up the verifier")
	}
	account, err := p.LoginExchange(context.Background(), "the-verifier", "real-code")
	if err != nil || account.Email != "you@example.com" {
		t.Fatalf("the real code after a wrong one: %+v %v", account, err)
	}
	p.mu.Lock()
	_, kept = p.verifier["the-verifier"]
	p.mu.Unlock()
	if kept {
		t.Fatal("the verifier outlived the sign-in")
	}
}

// Token requests carry a refresh token or a sign-in code in their body: a
// redirect to another host is not followed, so the body is not resent there.
func TestTokenRequestsDoNotFollowRedirectsElsewhere(t *testing.T) {
	var elsewhere int
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "platform.claude.com" {
			elsewhere++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: io.NopCloser(strings.NewReader("")),
			Header: http.Header{"Location": {"https://attacker.example/collect"}}, Request: r}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	if _, err := tokenRequest(context.Background(), tokenURL, []byte(`{"grant_type":"refresh_token","refresh_token":"secret"}`)); err == nil {
		t.Fatal("a redirect answered the token request")
	}
	if elsewhere != 0 {
		t.Fatalf("the token request was resent to another host %d times", elsewhere)
	}
}
