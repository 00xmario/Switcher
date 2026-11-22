package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
)

// appLoginTestProvider keeps its apps' sign-in in memory.
type appLoginTestProvider struct {
	*recheckProvider
	mu    sync.Mutex
	email string
	given []store.Account
}

func (p *appLoginTestProvider) AppLogin(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.email, nil
}

func (p *appLoginTestProvider) UseInApps(_ context.Context, a store.Account) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.email = a.Email
	p.given = append(p.given, a)
	return nil
}

// Use in Antigravity signs the apps in with the account, marks it, and
// answers with what happened; the proxy route of such a provider is closed.
func TestUseInAppsSignsTheAppsIn(t *testing.T) {
	accountStore := store.New(t.TempDir())
	prov := &appLoginTestProvider{recheckProvider: &recheckProvider{}, email: "old@example.com"}
	m, err := proxy.New(accountStore, map[string]provider.Provider{"fake": prov}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Settings: settings.New(t.TempDir()), Store: accountStore, Proxy: m, Providers: map[string]provider.Provider{"fake": prov}}
	mux := http.NewServeMux()
	a.Register(mux)
	for _, id := range []string{"fake-aaaa", "fake-bbbb"} {
		if err := accountStore.Save(store.Account{ID: id, Provider: "fake", Email: id + "@example.com",
			Token: store.Token{AccessToken: "at-" + id, RefreshToken: "rt-" + id, ExpiresAt: time.Now().Add(time.Hour).Unix()}}); err != nil {
			t.Fatal(err)
		}
	}
	if v := a.viewOf(mustGet(t, accountStore, "fake-bbbb")); !v.AppLoginAvailable || v.AppLoginActive {
		t.Fatalf("before: %+v", v)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/fake-bbbb/activate", nil)
	req.RemoteAddr, req.Host = "127.0.0.1:1234", "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Native struct {
			Changed bool   `json:"changed"`
			Message string `json:"message"`
		} `json:"native"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &answer) != nil || !answer.Native.Changed || answer.Native.Message == "" {
		t.Fatalf("answer %s", rec.Body.String())
	}
	if len(prov.given) != 1 || prov.given[0].ID != "fake-bbbb" || prov.given[0].Token.RefreshToken != "rt-fake-bbbb" {
		t.Fatalf("handed %+v", prov.given)
	}
	if v := a.viewOf(mustGet(t, accountStore, "fake-bbbb")); !v.AppLoginActive {
		t.Fatalf("after: %+v", v)
	}
	if m.ActiveID("fake") != "fake-bbbb" {
		t.Fatalf("selection %q", m.ActiveID("fake"))
	}
	// The proxy does not route this provider.
	proxied := httptest.NewRecorder()
	m.ServeHTTP(proxied, httptest.NewRequest(http.MethodPost, "/fake/v1internal:generateContent", nil))
	if proxied.Code != http.StatusForbidden {
		t.Fatalf("proxy answered %d, want 403", proxied.Code)
	}
}

func mustGet(t *testing.T, st *store.Store, id string) store.Account {
	t.Helper()
	a, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
