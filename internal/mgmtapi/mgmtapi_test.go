package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/remote"
	"switcher/internal/store"
)

type managementTransport func(*http.Request) (*http.Response, error)

func (f managementTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := f(r)
	if response != nil {
		response.Request = r
	}
	return response, err
}

type managementProvider struct{ provider.Provider }

func (*managementProvider) IsExpired(a store.Account) bool { return a.Token.AccessToken == "expired" }
func (*managementProvider) Refresh(_ context.Context, a *store.Account) error {
	a.Token.AccessToken, a.Token.ExpiresAt = "fresh", time.Now().Add(time.Hour).Unix()
	return nil
}

type managementNativeProvider struct{ *managementProvider }

func (*managementNativeProvider) NativeEnabled() bool { return true }
func (*managementNativeProvider) NativeStatus() claudecode.Status {
	return claudecode.Status{Available: true}
}
func (*managementNativeProvider) SwitchNative(context.Context, string, func(string) error) (claudecode.SwitchResult, error) {
	return claudecode.SwitchResult{}, provider.ErrUnsupported
}
func (*managementNativeProvider) SyncNative(_ context.Context, a *store.Account) (bool, error) {
	if a.Token.AccessToken != "saved-predecessor" {
		return false, nil
	}
	a.Token.AccessToken = "native-current"
	return true, nil
}
func (*managementNativeProvider) RefreshAfter401(_ context.Context, a *store.Account, rejected string) error {
	if rejected != "native-current" || a.Token.AccessToken != rejected {
		return errors.New("wrong rejected generation")
	}
	a.Token.AccessToken = "native-successor"
	return nil
}

func managementFixture(t *testing.T, prov provider.Provider, access string) *API {
	t.Helper()
	st := store.New(t.TempDir())
	for _, id := range []string{"a", "b"} {
		if err := st.Save(store.Account{ID: id, Provider: "codex", Token: store.Token{AccessToken: access}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Save(store.Account{ID: "c", Provider: "claude", Token: store.Token{AccessToken: access}}); err != nil {
		t.Fatal(err)
	}
	m, err := proxy.New(st, map[string]provider.Provider{"codex": prov, "claude": prov}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	return &API{Store: st, Proxy: m, ManagementKey: "fixture-key"}
}

// previousTransport is the transport the tests found, for calls that are not
// the provider's.
var previousTransport = http.DefaultTransport

func mockManagementNetwork(t *testing.T, transport managementTransport) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func managementResponse(status int, location string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}
	if location != "" {
		response.Header.Set("Location", location)
	}
	return response
}

func managementCall(api *API, target string) *httptest.ResponseRecorder {
	return managementCallWith(api, "", target, nil)
}

func managementCallWith(api *API, method, target string, data json.RawMessage) *httptest.ResponseRecorder {
	return managementCallFor(api, "a", method, target, data)
}

func managementCallFor(api *API, account, method, target string, data json.RawMessage) *httptest.ResponseRecorder {
	body, _ := json.Marshal(apiCallRequest{AuthIndex: account, Method: method, URL: target, Data: data, Header: map[string]string{"Authorization": "Bearer $TOKEN$", "X-Token": "$TOKEN$"}})
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer fixture-key")
	response := httptest.NewRecorder()
	mux := http.NewServeMux()
	api.Register(mux)
	mux.ServeHTTP(response, request)
	return response
}

func TestAPICallRejectsPlaintextAndRedirectDowngrades(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial HTTP", true: "redirect HTTP"}[redirect], func(t *testing.T) {
			api := managementFixture(t, &managementProvider{}, "fresh")
			calls := 0
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme == "http" {
					t.Error("account token sent over plaintext HTTP")
					return managementResponse(200, ""), nil
				}
				return managementResponse(302, "http://chatgpt.com/backend-api/wham/usage"), nil
			})
			target, wantCode, wantCalls := "http://chatgpt.com/backend-api/wham/usage", 400, 0
			if redirect {
				target, wantCode, wantCalls = "https://chatgpt.com/backend-api/wham/usage", 502, 1
			}
			response := managementCall(api, target)
			if response.Code != wantCode || calls != wantCalls {
				t.Fatalf("unsafe destination accepted: status=%d calls=%d", response.Code, calls)
			}
		})
	}
}

func TestAPICallPreparesExpiredAndNativeCredentialsWithoutSwitching(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "native rejected generation"}[native], func(t *testing.T) {
			var prov provider.Provider = &managementProvider{}
			access, want := "expired", "fresh"
			if native {
				// Claude Code rotated the login; Switcher's stored copy is rejected
				// and the retry adopts Claude Code's token without a new grant.
				prov, access, want = &managementNativeProvider{managementProvider: &managementProvider{}}, "saved-predecessor", "native-current"
			}
			api := managementFixture(t, prov, access)
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				if native && r.Header.Get("Authorization") == "Bearer saved-predecessor" {
					return managementResponse(401, ""), nil
				}
				if r.Header.Get("Authorization") != "Bearer "+want {
					t.Errorf("management used stale credentials: %q", r.Header.Get("Authorization"))
				}
				return managementResponse(200, ""), nil
			})
			response := managementCall(api, "https://chatgpt.com/backend-api/wham/usage")
			var result struct {
				StatusCode int `json:"status_code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || result.StatusCode != 200 || api.Proxy.ActiveID("codex") != "b" {
				t.Fatalf("management credential preparation changed route or failed: status=%d body=%s active=%s", response.Code, response.Body.String(), api.Proxy.ActiveID("codex"))
			}
		})
	}
}

func TestAPICallRedirectCredentialsStayOnOriginalOrigin(t *testing.T) {
	for _, crossOrigin := range []bool{false, true} {
		t.Run(map[bool]string{false: "same origin", true: "cross origin"}[crossOrigin], func(t *testing.T) {
			api := managementFixture(t, &managementProvider{}, "fresh")
			calls := 0
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					location := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
					if crossOrigin {
						location = "https://api.anthropic.com/api/oauth/usage"
					}
					return managementResponse(302, location), nil
				}
				for _, key := range []string{"Authorization", "X-Token"} {
					if crossOrigin && r.Header.Get(key) != "" || !crossOrigin && r.Header.Get(key) == "" {
						t.Errorf("redirect credential policy violated for %s", key)
					}
				}
				return managementResponse(200, ""), nil
			})
			if response := managementCall(api, "https://chatgpt.com/backend-api/wham/usage"); response.Code != 200 || calls != 2 {
				t.Fatalf("redirect failed: status=%d calls=%d", response.Code, calls)
			}
		})
	}
}

// Paired Macs reach api-call with this Mac's key, and an account's token can
// read far more than usage, such as its ChatGPT conversations. Only the four
// requests T3 Code makes, with their methods, may leave.
func TestAPICallReachesOnlyTheRequestsT3CodeMakes(t *testing.T) {
	api := managementFixture(t, &managementProvider{}, "fresh")
	var reached []string
	mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
		call := r.Method + " " + r.URL.String()
		if r.Body != nil {
			if body, _ := io.ReadAll(r.Body); len(body) > 0 {
				call += " " + string(body)
			}
		}
		reached = append(reached, call)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("%s %s went without the account token", r.Method, r.URL)
		}
		return managementResponse(200, ""), nil
	})
	// T3 Code sends data as CLIProxyAPI does, a string holding the JSON body.
	for _, call := range []struct{ method, url, data, want string }{
		{"", "https://chatgpt.com/backend-api/wham/usage", "", "GET https://chatgpt.com/backend-api/wham/usage"},
		{"GET", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", "", "GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"},
		{"POST", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", `"{\"credit_id\":\"c\"}"`, `POST https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume {"credit_id":"c"}`},
		{"POST", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", `{"credit_id":"c"}`, `POST https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume {"credit_id":"c"}`},
		{"get", "https://ChatGPT.com/backend-api/wham/usage", "", "GET https://ChatGPT.com/backend-api/wham/usage"},
	} {
		reached = nil
		var data json.RawMessage
		if call.data != "" {
			data = json.RawMessage(call.data)
		}
		if response := managementCallWith(api, call.method, call.url, data); response.Code != 200 || len(reached) != 1 || reached[0] != call.want {
			t.Errorf("%s %s: status=%d reached=%v", call.method, call.url, response.Code, reached)
		}
	}
	for _, call := range []struct{ method, url string }{
		{"GET", "https://chatgpt.com/backend-api/conversations"},
		{"POST", "https://chatgpt.com/backend-api/codex/responses"},
		{"GET", "https://chatgpt.com/backend-api/wham/usage?limit=1"},
		{"GET", "https://chatgpt.com/backend-api/wham/usage/"},
		{"GET", "https://chatgpt.com/backend-api/wham/%75sage"},
		{"GET", "https://chatgpt.com/backend-api/wham/usage/../../me"},
		{"POST", "https://chatgpt.com/backend-api/wham/usage"},
		{"GET", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"},
		{"DELETE", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"},
		{"POST", "https://api.anthropic.com/v1/messages"},
		// Account "a" is a ChatGPT account: its token is not Anthropic's to see.
		{"GET", "https://api.anthropic.com/api/oauth/usage"},
		{"GET", "https://api.anthropic.com:8443/api/oauth/usage"},
		{"GET", "https://example.com/api/oauth/usage"},
		// "İ" lowercases to "i", but the transport dials its punycode,
		// api.xn--anthropic-dkf.com, another domain.
		{"GET", "https://api.anthropİc.com/api/oauth/usage"},
		{"GET", "https://chatgpt.com./backend-api/wham/usage"},
		{"GET", "https://user@chatgpt.com/backend-api/wham/usage"},
	} {
		reached = nil
		if response := managementCallWith(api, call.method, call.url, nil); response.Code != 400 || len(reached) != 0 {
			t.Errorf("%s %s was allowed: status=%d reached=%v", call.method, call.url, response.Code, reached)
		}
	}
}

// An account's token goes to its own provider and to no other company.
func TestAPICallKeepsATokenWithItsOwnProvider(t *testing.T) {
	api := managementFixture(t, &managementProvider{}, "fresh")
	var reached []string
	mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.URL.Host)
		return managementResponse(200, ""), nil
	})
	if response := managementCallFor(api, "c", "GET", "https://api.anthropic.com/api/oauth/usage", nil); response.Code != 200 || len(reached) != 1 || reached[0] != "api.anthropic.com" {
		t.Fatalf("a Claude account's usage: %d %v", response.Code, reached)
	}
	reached = nil
	for _, call := range [][2]string{
		{"c", "https://chatgpt.com/backend-api/wham/usage"},
		{"c", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"},
		{"a", "https://api.anthropic.com/api/oauth/usage"},
		{"b", "https://api.anthropic.com/api/oauth/usage"},
	} {
		if response := managementCallFor(api, call[0], "GET", call[1], nil); response.Code != 400 || len(reached) != 0 {
			t.Errorf("account %s was sent to %s: %d %v", call[0], call[1], response.Code, reached)
		}
	}
	if response := managementCallFor(api, "missing", "GET", "https://chatgpt.com/backend-api/wham/usage", nil); response.Code != 404 {
		t.Errorf("unknown account: %d", response.Code)
	}
}

// A redirect is held to the same endpoints and methods, so a hop cannot
// carry the token anywhere else on an allowed host.
func TestAPICallRedirectStaysOnT3CodesEndpoints(t *testing.T) {
	for _, hop := range []struct {
		method, url, data string
		status            int
		location          string
	}{
		{"GET", "https://chatgpt.com/backend-api/wham/usage", "", 302, "https://chatgpt.com/backend-api/conversations"},
		// A 307 keeps the method: the credit POST must not reach usage.
		{"POST", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", `"{}"`, 307, "https://chatgpt.com/backend-api/wham/usage"},
	} {
		api := managementFixture(t, &managementProvider{}, "fresh")
		calls := 0
		mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
			calls++
			return managementResponse(hop.status, hop.location), nil
		})
		var data json.RawMessage
		if hop.data != "" {
			data = json.RawMessage(hop.data)
		}
		if response := managementCallWith(api, hop.method, hop.url, data); response.Code != 502 || calls != 1 {
			t.Errorf("%s %s: redirect to %s was followed: status=%d calls=%d", hop.method, hop.url, hop.location, response.Code, calls)
		}
	}
}

// Spending a banked reset cannot be taken back: through a paired device, the
// consume call goes out only for an account that is out of usage.
func TestPairedDevicesConsumeResetsOnlyWhenOutOfUsage(t *testing.T) {
	api := managementFixture(t, &managementProvider{}, "fresh")
	mux := http.NewServeMux()
	api.Register(mux)
	// As main.go shares it: a paired device's management calls carry the
	// host's own key.
	shared := http.NewServeMux()
	shared.Handle("/v0/management/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer fixture-key")
		mux.ServeHTTP(w, r)
	}))
	host, err := remote.NewHost(t.TempDir(), 0, shared)
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	code, err := host.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	client := remote.NewClient(t.TempDir(), "laptop-key")
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, "Laptop"); err != nil {
		t.Fatal(err)
	}
	through := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	var reached []string
	mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "chatgpt.com" {
			reached = append(reached, r.Method+" "+r.URL.Path)
			return managementResponse(200, ""), nil
		}
		return previousTransport.RoundTrip(r)
	})
	call := func(target string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(apiCallRequest{AuthIndex: "a", Method: "POST", URL: target, Data: json.RawMessage(`{"credit_id":"c"}`), Header: map[string]string{"Authorization": "Bearer $TOKEN$"}})
		request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
		request.Header.Set("Authorization", "Bearer laptop-key")
		w := httptest.NewRecorder()
		through.ServeHTTP(w, request)
		return w
	}
	consume := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	if w := call(consume); w.Code != http.StatusConflict || len(reached) != 0 {
		t.Fatalf("a paired device consumed a reset for an account with usage left: %d %s %v", w.Code, w.Body.String(), reached)
	}
	// Here, on the host itself, the call goes out as before.
	if w := managementCallWith(api, "POST", consume, json.RawMessage(`{"credit_id":"c"}`)); w.Code != http.StatusOK || len(reached) != 1 {
		t.Fatalf("consuming a reset here: %d %s %v", w.Code, w.Body.String(), reached)
	}
}
