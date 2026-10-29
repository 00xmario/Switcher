package login

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

type reloginProvider struct {
	account store.Account
}

func (*reloginProvider) ID() string          { return "fake" }
func (*reloginProvider) DisplayName() string { return "Fake" }
func (*reloginProvider) LoginStart(context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{State: "browser-state", Kind: "browser"}, nil
}
func (p *reloginProvider) LoginExchange(context.Context, string, string) (store.Account, error) {
	return p.account, nil
}
func (p *reloginProvider) DeviceStart(context.Context) (provider.LoginInfo, func(context.Context) (store.Account, error), error) {
	return provider.LoginInfo{State: "device-state", Kind: "device"},
		func(context.Context) (store.Account, error) { return p.account, nil }, nil
}
func (*reloginProvider) AddByKey(context.Context, string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}
func (*reloginProvider) Refresh(context.Context, *store.Account) error { return nil }
func (*reloginProvider) IsExpired(store.Account) bool                  { return false }
func (*reloginProvider) UpstreamURL(path string) string                { return path }
func (*reloginProvider) ApplyAuth(*http.Request, store.Account) error  { return nil }
func (*reloginProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{}, nil
}
func (*reloginProvider) ParseRateLimit(context.Context, store.Account, int, []byte) (time.Time, bool) {
	return time.Time{}, false
}

func TestReloginOnlyPersistsSelectedIdentity(t *testing.T) {
	for _, mode := range []string{"browser", "device"} {
		for _, tc := range []struct {
			name       string
			email      string
			lookupGone bool
			wantErr    error
		}{
			{name: "matching identity", email: "A@Example.com"},
			{name: "different identity", email: "b@example.com", wantErr: proxy.ErrReloginIdentityMismatch},
			{name: "deleted target", email: "a@example.com", lookupGone: true, wantErr: proxy.ErrReloginTargetUnavailable},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p := &reloginProvider{account: store.Account{ID: "new-id", Provider: "fake", Email: tc.email}}
				st := store.New(t.TempDir())
				if !tc.lookupGone {
					if err := st.Save(store.Account{ID: "existing-id", Provider: "fake", Email: "a@example.com"}); err != nil {
						t.Fatal(err)
					}
				}
				proxyManager, err := proxy.New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
				if err != nil {
					t.Fatal(err)
				}
				m := New(func(a *store.Account, target string) error {
					return proxyManager.ReplaceReloginAccount(a, target)
				})
				var handle Handle
				if mode == "device" {
					handle, err = m.StartDevice(context.Background(), p, "existing-id")
				} else {
					handle, err = m.Start(context.Background(), p, "existing-id")
				}
				if err != nil {
					t.Fatal(err)
				}
				if mode == "browser" {
					if err := m.Complete(context.Background(), handle.State, "code"); (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || (tc.wantErr == nil && err != nil) {
						t.Fatalf("callback error = %v, want %v", err, tc.wantErr)
					}
				}
				account, outcomeErr, finished := m.Outcome(handle.State, time.Second)
				if !finished || (tc.wantErr != nil && !errors.Is(outcomeErr, tc.wantErr)) || (tc.wantErr == nil && outcomeErr != nil) {
					t.Fatalf("outcome: finished=%v, error=%v, want %v", finished, outcomeErr, tc.wantErr)
				}
				if tc.wantErr != nil {
					if _, err := st.Get("new-id"); err == nil {
						t.Fatal("mismatched relogin persisted a new account")
					}
				} else if saved, err := st.Get("existing-id"); err != nil || saved.Email != tc.email || account.ID != "existing-id" {
					t.Fatalf("matching relogin failed to adopt existing ID: %+v, %+v, %v", saved, account, err)
				}
			})
		}
	}
}

// stateProvider proves an attempt with its state alone (Google's flow, no
// PKCE); pkceProvider binds the code to the attempt's verifier.
type stateProvider struct {
	reloginProvider
	pkce      bool
	exchanged chan string
}

func (p *stateProvider) LoginStart(context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{State: "browser-state", Kind: "browser", PKCE: p.pkce}, nil
}

func (p *stateProvider) LoginExchange(_ context.Context, _, code string) (store.Account, error) {
	p.exchanged <- code
	return store.Account{ID: "attacker", Provider: "fake", Email: "attacker@example.com"}, nil
}

// A callback without the state may complete only a login whose code PKCE
// binds to it. Otherwise any page could deliver its own account's code to
// the one pending login and have this Mac save that account.
func TestStatelessCallbackNeedsPKCE(t *testing.T) {
	for _, pkce := range []bool{false, true} {
		p := &stateProvider{pkce: pkce, exchanged: make(chan string, 1)}
		saved := 0
		m := New(func(*store.Account, string) error { saved++; return nil })
		handle, err := m.Start(context.Background(), p, "")
		if err != nil {
			t.Fatal(err)
		}
		err = m.Complete(context.Background(), "", "attacker-code")
		if !pkce {
			if !errors.Is(err, ErrUnknown) || saved != 0 || len(p.exchanged) != 0 {
				t.Fatalf("stateless callback completed a login without PKCE: err=%v saved=%d", err, saved)
			}
			if state := m.SinglePending(); state != "" {
				t.Fatalf("login without PKCE offered to stateless callbacks: %q", state)
			}
			// The provider's own redirect carries the state and still works.
			if err := m.Complete(context.Background(), handle.State, "real-code"); err != nil || <-p.exchanged != "real-code" {
				t.Fatalf("callback with state: %v", err)
			}
			continue
		}
		if err != nil || saved != 1 || <-p.exchanged != "attacker-code" {
			t.Fatalf("stateless PKCE callback: err=%v saved=%d", err, saved)
		}
	}
}
