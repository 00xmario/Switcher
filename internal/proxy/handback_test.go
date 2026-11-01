package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// countingProvider counts every upstream call and token refresh.
type countingProvider struct {
	fakeProvider
	refreshes atomic.Int32
}

func (c *countingProvider) Refresh(ctx context.Context, a *store.Account) error {
	c.refreshes.Add(1)
	return c.fakeProvider.Refresh(ctx, a)
}

func (c *countingProvider) IsExpired(store.Account) bool { return true }

// While handed back nothing may use, refresh or rotate a Switcher account:
// the tools' logins are theirs alone.
func TestHandedBackStopsEveryUseOfTheAccounts(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	st := store.New(t.TempDir())
	prov := &countingProvider{fakeProvider: fakeProvider{id: "fake", upstream: upstream, refreshToken: "rotated"}}
	if err := st.Save(account("a")); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, map[string]provider.Provider{prov.ID(): prov}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	m.SetHandedBack(true)
	if !m.HandedBack() {
		t.Fatal("not handed back")
	}
	if rec := doRequest(t, m, "/v1/responses"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxy request while handed back: %d %s", rec.Code, rec.Body)
	}
	m.RefreshUsageAll(context.Background())
	a, _ := st.Get("a")
	m.RefreshUsage(context.Background(), a)
	if _, err := m.PrepareAccount(context.Background(), "a"); !errors.Is(err, ErrHandedBack) {
		t.Fatalf("prepare while handed back: %v", err)
	}
	if _, err := m.ActivateForClient(context.Background(), "a"); !errors.Is(err, ErrHandedBack) {
		t.Fatalf("native switch while handed back: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the poll's follow-up switch runs on its own goroutine
	if n := prov.refreshes.Load(); n != 0 {
		t.Fatalf("%d token refreshes while handed back", n)
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Fatalf("%d upstream calls while handed back", n)
	}
	if got, _ := st.Get("a"); got.Token.AccessToken != "a-token" {
		t.Fatalf("token changed while handed back: %q", got.Token.AccessToken)
	}

	m.SetHandedBack(false)
	if rec := doRequest(t, m, "/v1/responses"); rec.Code != http.StatusOK {
		t.Fatalf("proxy request after setting up again: %d %s", rec.Code, rec.Body)
	}
}

func TestAdoptTokenReplacesOnlyTheCredential(t *testing.T) {
	a := account("a")
	a.Email = "keep@example.com"
	a.Token.Extra = map[string]any{"k": "v"}
	m := newManager(t, httptest.NewServer(http.NotFoundHandler()), a)
	if err := m.AdoptToken("a", store.Token{AccessToken: "newer", RefreshToken: "newer-r", AccountID: "acct"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.store.Get("a")
	if err != nil || got.Token.AccessToken != "newer" || got.Token.RefreshToken != "newer-r" || got.Email != "keep@example.com" || got.Token.Extra["k"] != "v" {
		t.Fatalf("adopted: %+v %v", got, err)
	}
}
