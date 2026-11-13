package codex

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A callback with a wrong code, which any web page can send while a sign-in
// is under way, fails at the token endpoint. The verifier stays, so the real
// code still exchanges; once it has, the verifier is gone.
func TestFailedExchangeKeepsTheVerifier(t *testing.T) {
	p := New()
	p.verifier["live"] = verifierEntry{value: "the-verifier", startedAt: time.Now()}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"you@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct"}}`))
	old := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("code_verifier") != "the-verifier" {
			t.Errorf("exchange without the verifier: %v", form)
		}
		if form.Get("code") != "real-code" {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)), Header: http.Header{}}, nil
		}
		answer := `{"access_token":"a","refresh_token":"r","id_token":"x.` + claims + `.y","expires_in":3600}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(answer)), Header: http.Header{"Content-Type": {"application/json"}}}, nil
	})}
	t.Cleanup(func() { oauthHTTPClient = old })
	if _, err := p.LoginExchange(context.Background(), "live", "forged"); err == nil {
		t.Fatal("a wrong code exchanged")
	}
	if _, ok := p.verifier["live"]; !ok {
		t.Fatal("a failed exchange used up the verifier")
	}
	account, err := p.LoginExchange(context.Background(), "live", "real-code")
	if err != nil || account.Email != "you@example.com" {
		t.Fatalf("the real code after a wrong one: %+v %v", account, err)
	}
	if _, ok := p.verifier["live"]; ok {
		t.Fatal("the verifier outlived the sign-in")
	}
}
