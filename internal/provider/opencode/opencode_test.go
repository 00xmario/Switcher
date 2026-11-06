package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUsageAuthStatus(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" || r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { http.DefaultClient = old }()
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		status = s
		_, err := New().Usage(context.Background(), store.Account{Token: store.Token{AccessToken: "secret-access"}})
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (s == http.StatusUnauthorized) || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("status %d: unexpected usage error: %v", s, err)
		}
	}
}

// An expired key says so, instead of only that usage needs authentication;
// free text from OpenCode stays out of the error.
func TestUsageNamesAnExpiredKey(t *testing.T) {
	body := `{"error":{"type":"authentication_error","code":"api_key_expired","message":"private text"}}`
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	defer func() { http.DefaultClient = old }()
	_, err := New().AddByKey(context.Background(), "sk-expired")
	if !errors.Is(err, provider.ErrUsageAuthRequired) || !strings.Contains(err.Error(), "this API key has expired") || strings.Contains(err.Error(), "private text") {
		t.Fatalf("unexpected error: %v", err)
	}
	body = `{"error":{"code":"Some <free> text that is not a code"}}`
	if _, err := New().AddByKey(context.Background(), "sk-x"); err == nil || strings.Contains(err.Error(), "free") {
		t.Fatalf("free text leaked: %v", err)
	}
}

func TestGoProxyPathAndChosenKey(t *testing.T) {
	p := New()
	for _, tc := range []struct{ path, want string }{
		{"/responses", "https://opencode.ai/zen/go/v1/responses"},
		{"/v1/responses", "https://opencode.ai/zen/go/v1/responses"},
		{"/v1/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"/v1/messages", "https://opencode.ai/zen/go/v1/messages"},
		{"/v1/models", "https://opencode.ai/zen/go/v1/models"},
		{"/v1x/responses", "https://opencode.ai/zen/go/v1/v1x/responses"},
	} {
		if got := p.UpstreamURL(tc.path); got != tc.want {
			t.Fatalf("UpstreamURL(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", nil)
	r.Header.Set("Authorization", "Bearer old-go-key")
	r.Header.Set("X-Api-Key", "old-go-api-key")
	r.Header.Set("X-OpenCode-Feature", "preserve")
	if err := p.ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-go-key"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer chosen-go-key" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-OpenCode-Feature") != "preserve" {
		t.Fatal("OpenCode proxy retained client credentials or removed a feature header")
	}
}
