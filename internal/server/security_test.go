package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/login"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/remote"
	"switcher/internal/settings"
	"switcher/internal/store"
	"switcher/internal/update"
)

func TestLANStaysClosedDuringAndAfterDisabledAuthRestart(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&API{Settings: prefs}).registerAuthRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("private state and management key"))
	})
	lan := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).WrapLAN(mux))
	request := func(path, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://192.0.2.5:9123"+path, nil)
		r.RemoteAddr = "192.0.2.6:1234"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		lan.ServeHTTP(w, r)
		return w
	}
	if w := request("/api/state", session); w.Code != 200 {
		t.Fatalf("authenticated LAN: %d", w.Code)
	}
	restarted := make(chan struct{}, 1)
	original := restartForTest
	restartForTest = func() { restarted <- struct{}{} } // failed/no-op restart, never exec the test binary
	defer func() { restartForTest = original }()
	r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/auth/disable", strings.NewReader(`{"password":"fixture-password"}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	assertClosed := func() {
		for _, path := range []string{"/", "/api/state", "/api/auth/status", "/codex/v1/responses", "/v0/management/auth-files"} {
			for _, cookie := range []string{"", session} {
				w := request(path, cookie)
				if w.Code != 503 || strings.Contains(w.Body.String(), "management key") {
					t.Fatalf("LAN reopened at %s: %d", path, w.Code)
				}
			}
		}
	}
	assertClosed()
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart hook was not called")
	}
	assertClosed()
	local := (&AuthGate{Store: prefs}).Wrap(mux)
	w = httptest.NewRecorder()
	local.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:9123/", nil))
	if w.Code != 200 {
		t.Fatal("local no-auth install did not recover after disable")
	}
}

func TestDamagedSettingsNeverOpenDashboardOrLAN(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("private")) })
	gate := &AuthGate{Store: prefs}
	if err := os.WriteFile(prefs.Path(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.Handler{gate.Wrap(next), gate.WrapLAN(next)} {
		for _, path := range []string{"/api/state", "/app.js", "/api/auth/login"} {
			r := httptest.NewRequest("GET", path, nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("damaged settings exposed %s: %d", path, w.Code)
			}
		}
	}
	w := httptest.NewRecorder()
	gate.WrapLAN(next).ServeHTTP(w, httptest.NewRequest("GET", "/codex/v1/responses", nil))
	if w.Code != 503 {
		t.Fatal("damaged settings left LAN native traffic open")
	}
	w = httptest.NewRecorder()
	gate.Wrap(next).ServeHTTP(w, httptest.NewRequest("GET", "/codex/v1/responses", nil))
	if w.Code != 200 {
		t.Fatal("local independently authenticated CLI route was browser-gated")
	}
}

func TestPreauthStatusDoesNotTrustHostOrRevealNativeMetadata(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	native := &nativeClaudeFixture{statusOverride: &claudecode.Status{Email: "private@example.test", ActiveID: "claude-private", ConfigPath: "/fixture/private-home/.claude.json"}}
	api := &API{Settings: prefs, Providers: map[string]provider.Provider{"claude": native}}
	mux := http.NewServeMux()
	api.registerAuthRoutes(mux)
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).Wrap(mux))
	for _, tc := range []struct {
		name, remote, cookie, bearer string
		details, csrf                bool
	}{
		{"anonymous local", "127.0.0.1:1234", "", "", false, false},
		{"spoofed LAN", "192.0.2.6:1234", "", "", false, false},
		{"authenticated LAN", "192.0.2.6:1234", session, "", false, true},
		{"local cookie", "127.0.0.1:1234", session, "", true, true},
		{"local device", "127.0.0.1:1234", "", "Bearer " + device, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://127.0.0.1:9123/api/auth/status", nil)
			r.RemoteAddr = tc.remote
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			r.Header.Set("Authorization", tc.bearer)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
				t.Fatalf("status: %d %v", w.Code, err)
			}
			if _, ok := body["claude_code"]; ok != tc.details {
				t.Fatalf("wrong metadata visibility: %s", w.Body.String())
			}
			if _, ok := body["csrf"]; ok != tc.csrf {
				t.Fatalf("wrong CSRF visibility: %s", w.Body.String())
			}
			if !tc.details {
				want := 2
				if tc.csrf {
					want++
				}
				if len(body) != want || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("preauth disclosure: %s", w.Body.String())
				}
			}
		})
	}
}

type machineImporter struct {
	*recheckProvider
	calls atomic.Int32
}

func (*machineImporter) ID() string { return "copilot" }
func (p *machineImporter) ImportFromKeychain(context.Context) (store.Account, error) {
	p.calls.Add(1)
	return store.Account{ID: "copilot-fixture", Provider: "copilot", Email: "fixture@example.test"}, nil
}

func TestEveryMachineImportRequiresLoopbackIncludingCopilot(t *testing.T) {
	p := &machineImporter{recheckProvider: &recheckProvider{}}
	st := store.New(t.TempDir())
	providers := map[string]provider.Provider{"copilot": p}
	manager, err := proxy.New(st, providers, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: st, Proxy: manager, Providers: providers, Settings: prefs}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, tc := range []struct {
		remote       string
		cookie, csrf bool
		status       int
	}{
		{"127.0.0.1:1234", false, false, 401},
		{"127.0.0.1:1234", true, false, 403},
		{"192.0.2.6:1234", true, true, 403},
		{"127.0.0.1:1234", true, true, 200},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/login/import", strings.NewReader(`{"provider":"copilot"}`))
		r.RemoteAddr = tc.remote
		if tc.cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if tc.csrf {
			r.Header.Set(csrfHeader, prefs.CSRFToken())
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("import from %s: %d %s", tc.remote, w.Code, w.Body.String())
		}
		if tc.status != 200 && p.calls.Load() != 0 {
			t.Fatal("rejected import accessed machine credentials")
		}
	}
	if p.calls.Load() != 1 {
		t.Fatal("local fixture import was not called exactly once")
	}
}

type fakeUpdateChecker struct {
	refreshes, installs, unattended atomic.Int32
	checkErr                        error
}

func (c *fakeUpdateChecker) Check(context.Context) (update.State, error) {
	c.refreshes.Add(1)
	return c.State(), c.checkErr
}
func (c *fakeUpdateChecker) State() update.State {
	state := update.State{Latest: "v9.0.0", Available: c.refreshes.Load() > 0}
	if c.installs.Load() > 0 {
		state.Install = &update.Progress{Step: "downloading", Fraction: 0.4, Done: 4, Total: 8}
	}
	return state
}
func (c *fakeUpdateChecker) InstallAndRestart() error {
	c.installs.Add(1)
	return errors.New("fake install blocked")
}
func (c *fakeUpdateChecker) InstallAndRestartUnattended() error {
	c.unattended.Add(1)
	return errors.New("fake install blocked")
}

func TestUpdateCheckRefreshesOnlyMetadataAndKeepsAuthAndCSRF(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeUpdateChecker{}
	api := &API{Settings: prefs, Updater: checker, Version: "8.0.0"}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	call := func(method, path string, cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(nil))
		if cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if csrf {
			r.Header.Set(csrfHeader, prefs.CSRFToken())
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/api/update/check", false, false); w.Code != 401 {
		t.Fatalf("anonymous check: %d", w.Code)
	}
	if w := call("POST", "/api/update/check", true, false); w.Code != 403 {
		t.Fatalf("CSRF-less check: %d", w.Code)
	}
	if checker.refreshes.Load() != 0 || checker.installs.Load() != 0 {
		t.Fatal("rejected request invoked updater")
	}
	w := call("POST", "/api/update/check", true, true)
	var body struct {
		Update update.State `json:"update"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || !body.Update.Available || body.Update.Latest != "v9.0.0" || checker.refreshes.Load() != 1 || checker.installs.Load() != 0 {
		t.Fatalf("metadata check installed or failed to refresh: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/api/update", true, true); w.Code != 500 || checker.installs.Load() != 1 {
		t.Fatal("install route no longer uses the separate install operation")
	}
	if w := call("GET", "/api/update", false, false); w.Code != 401 {
		t.Fatalf("anonymous progress read: %d", w.Code)
	}
	w = call("GET", "/api/update", true, false)
	var progress update.State
	if err := json.Unmarshal(w.Body.Bytes(), &progress); err != nil || w.Code != 200 || progress.Install == nil ||
		progress.Install.Step != "downloading" || progress.Install.Fraction != 0.4 || checker.refreshes.Load() != 1 || checker.installs.Load() != 1 {
		t.Fatalf("progress read did not answer the cached state alone: %d %s", w.Code, w.Body.String())
	}
	if state := api.UpdateState(); state.Install != nil || !state.Available {
		t.Fatalf("/api/state's update must keep the check and leave the install progress out: %+v", state)
	}
	checker.checkErr = errors.New("fixture metadata unavailable")
	if w := call("POST", "/api/update/check", true, true); w.Code != 503 || checker.installs.Load() != 1 {
		t.Fatal("failed metadata refresh was reported as a successful check or invoked installation")
	}
	api.Updater = nil
	if w := call("POST", "/api/update/check", true, true); w.Code != 503 {
		t.Fatal("missing updater did not return unavailable")
	}
}

func TestProviderDisplayRoutesReportPersistenceFailureAndPreserveState(t *testing.T) {
	for _, tc := range []struct {
		path, method, body string
		initiallyHidden    bool
	}{
		{"/api/providers/order", "PATCH", `{"order":["other","fake"]}`, false},
		{"/api/providers/fake/hide", "POST", "", false},
		{"/api/providers/fake/show", "POST", "", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			st := store.New(root)
			providers := map[string]provider.Provider{"fake": &recheckProvider{}, "other": &recheckProvider{}}
			manager, err := proxy.New(st, providers, []string{"fake", "other"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.initiallyHidden {
				if err := manager.HideProvider("fake"); err != nil {
					t.Fatal(err)
				}
			}
			order, hidden := manager.Providers()
			statePath := filepath.Join(root, "state.json")
			if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Mkdir(statePath, 0700); err != nil {
				t.Fatal(err)
			}
			api := &API{Store: st, Proxy: manager, Providers: providers}
			mux := http.NewServeMux()
			api.Register(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			nextOrder, nextHidden := manager.Providers()
			if w.Code != 500 || !reflect.DeepEqual(order, nextOrder) || !reflect.DeepEqual(hidden, nextHidden) {
				t.Fatalf("failed preference write reported success or changed state: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLogoutReportsPersistentFailureAndAllowsOnlyCSRFProtectedRetry(t *testing.T) {
	root := t.TempDir()
	prefs := settings.New(root)
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	csrf := prefs.CSRFToken()
	mux := http.NewServeMux()
	(&API{Settings: prefs}).registerAuthRoutes(mux)
	privateCalls := 0
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		privateCalls++
	})
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	call := func(method, path, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		r.Header.Set(csrfHeader, csrf)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := filepath.Join(root, "sessions.json")
	backup := filepath.Join(root, "readable-sessions.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, path); err != nil {
		t.Fatal(err)
	}
	w := call("POST", "/api/auth/logout", csrf)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"status":"ok"`) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("blocked logout reported success or discarded the retry cookie: %d %s", w.Code, w.Body.String())
	}
	if prefs.ValidateSession(token) {
		t.Fatal("failed logout persistence restored in-memory authentication")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if !settings.New(root).ValidateSession(token) {
		t.Fatal("fixture did not retain the old disk session across a fresh store")
	}
	if w := call("GET", "/api/state", csrf); w.Code != http.StatusUnauthorized || privateCalls != 0 {
		t.Fatal("logout retry authentication exposed another protected route")
	}
	if w := call("POST", "/api/auth/logout", ""); w.Code != http.StatusForbidden {
		t.Fatalf("logout retry bypassed CSRF: %d", w.Code)
	}
	if w := call("POST", "/api/auth/logout", "wrong-csrf"); w.Code != http.StatusForbidden {
		t.Fatalf("logout retry accepted incorrect CSRF: %d", w.Code)
	}
	w = call("POST", "/api/auth/logout", csrf)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != sessionCookie || cookies[0].MaxAge >= 0 {
		t.Fatalf("repaired logout did not persist and expire its cookie: %d %s", w.Code, w.Body.String())
	}
	if settings.New(root).ValidateSession(token) {
		t.Fatal("successful logout retry left a valid persistent session")
	}
}

// The provider routes spend the accounts' subscriptions. On the network they
// take the management key where a CLI sends its API key; on this computer
// they stay as open as the CLIs' own logins.
func TestLANProviderRoutesNeedTheManagementKey(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	const key = "fixture-management-key"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("upstream answer")) })
	gate := &AuthGate{Store: prefs, DesktopManagementKey: key}
	lan := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, gate.WrapLAN(next))
	request := func(path string, header http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://192.0.2.5:9123"+path, strings.NewReader("{}"))
		r.RemoteAddr = "192.0.2.6:1234"
		for name, values := range header {
			r.Header[name] = values
		}
		w := httptest.NewRecorder()
		lan.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/codex/v1/responses", "/claude/v1/messages", "/grok/v1/chat/completions", "/opencode/x", "/antigravity/x", "/gemini/x", "/copilot/x"} {
		for name, header := range map[string]http.Header{
			"no key":        {},
			"wrong key":     {"Authorization": {"Bearer wrong"}},
			"empty bearer":  {"Authorization": {"Bearer "}},
			"key prefix":    {"X-Api-Key": {key[:len(key)-1]}},
			"session only":  {"Cookie": {sessionCookie + "=" + session}},
			"own cli login": {"Authorization": {"Bearer sk-ant-oat01-own"}, "Chatgpt-Account-Id": {"own"}},
		} {
			if w := request(path, header); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "upstream answer") {
				t.Fatalf("%s with %s reached the proxy from the network: %d", path, name, w.Code)
			}
		}
		for _, header := range []http.Header{
			{"Authorization": {"Bearer " + key}},
			{"X-Api-Key": {key}},
			{"Api-Key": {key}},
			{"X-Goog-Api-Key": {key}},
			{"X-Management-Key": {key}},
		} {
			if w := request(path, header); w.Code != http.StatusOK {
				t.Fatalf("%s with the management key in %v: %d", path, header, w.Code)
			}
		}
	}
	// A gate that was given no key lets nothing through.
	closed := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).WrapLAN(next))
	r := httptest.NewRequest("POST", "https://192.0.2.5:9123/codex/v1/responses", nil)
	r.RemoteAddr = "192.0.2.6:1234"
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	closed.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a gate without a key let the network use the proxy: %d", w.Code)
	}
	// This computer's own listener keeps the CLIs working without a key.
	w = httptest.NewRecorder()
	gate.Wrap(next).ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:9123/codex/v1/responses", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("the local CLI route now needs a key: %d", w.Code)
	}
}

// Wrong passwords from the network must not keep the owner out at this
// computer, where the password is also changed and turned off.
func TestNetworkPasswordGuessesDoNotLockOutThisComputer(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&API{Settings: prefs}).registerAuthRoutes(mux)
	login := func(remote, host, password string) int {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"`+password+`"}`))
		r.RemoteAddr, r.Host = remote, host
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 5; i++ {
		if code := login("192.0.2.6:4000", "192.0.2.5:9123", "wrong-guess"); code != http.StatusUnauthorized {
			t.Fatalf("network guess %d: %d", i, code)
		}
	}
	if code := login("192.0.2.7:4000", "192.0.2.5:9123", "fixture-password"); code != http.StatusTooManyRequests {
		t.Fatalf("the network kept guessing after its lockout, from another address: %d", code)
	}
	// A network client that claims to be this computer is still the network.
	if code := login("192.0.2.6:4000", "127.0.0.1:9123", "fixture-password"); code != http.StatusTooManyRequests {
		t.Fatalf("a spoofed Host escaped the network's lockout: %d", code)
	}
	if code := login("127.0.0.1:5000", "127.0.0.1:9123", "fixture-password"); code != http.StatusOK {
		t.Fatalf("network guesses locked out this computer: %d", code)
	}
	if code := login("192.0.2.6:4000", "192.0.2.5:9123", "fixture-password"); code != http.StatusTooManyRequests {
		t.Fatalf("a local sign-in lifted the network's lockout: %d", code)
	}
}

// A refused request to a provider route must not hold its connection open by
// sending its body slowly: those routes have no read deadline of their own.
func TestRefusedProviderRequestClosesItsConnection(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("a refused request reached the proxy") })
	gate := &AuthGate{Store: prefs, DesktopManagementKey: "fixture-management-key"}
	srv := httptest.NewServer(ControlRequests(LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, gate.WrapLAN(next))))
	defer srv.Close()
	for name, host := range map[string]string{"no key": "192.0.2.5:9123", "foreign host": "attacker.example"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		// A megabyte announced, a few bytes sent, and then nothing more.
		fmt.Fprintf(conn, "POST /codex/v1/responses HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1048576\r\n\r\n{\"a\":", host)
		start := time.Now()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		if !resp.Close {
			t.Fatalf("%s: the refusal keeps the connection", name)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		// The server hangs up by itself, without the rest of the body.
		if _, err := conn.Read(make([]byte, 1)); err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("%s: connection still open after %s (%v)", name, time.Since(start), err)
		}
		conn.Close()
	}
}

type browserLoginProvider struct{ *recheckProvider }

func (*browserLoginProvider) LoginStart(context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{Kind: "browser", State: "state-1", URL: "https://auth.example/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback"}, nil
}

// A browser sign-in starts only while Switcher answers where it comes back
// to; otherwise whoever holds that port would be handed the sign-in code.
func TestBrowserSignInNeedsItsCallbackPort(t *testing.T) {
	var asked []string
	var refuse error
	api := &API{Providers: map[string]provider.Provider{"fake": &browserLoginProvider{&recheckProvider{}}},
		Logins:        login.New(nil),
		CallbackReady: func(signIn string) error { asked = append(asked, signIn); return refuse }}
	start := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"provider":"fake"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.handleLoginStart(w, r)
		return w
	}
	if w := start(); w.Code != http.StatusOK || len(asked) != 1 || !strings.Contains(asked[0], "redirect_uri") {
		t.Fatalf("a sign-in with its port held: %d %v", w.Code, asked)
	}
	if got := api.Logins.SinglePending(); got != "" {
		t.Fatalf("fixture sign-ins are not PKCE-bound, yet one is pending by itself: %q", got)
	}
	refuse = errors.New("another program is using port 1455")
	w := start()
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "auth.example") || !strings.Contains(w.Body.String(), "another program") {
		t.Fatalf("a sign-in whose port another program holds: %d %s", w.Code, w.Body.String())
	}
	// The refused sign-in is gone: it does not stay around to confuse the
	// next one's callback.
	if _, err, finished := api.Logins.Outcome("state-1", 0); !finished || !errors.Is(err, login.ErrUnknown) {
		t.Fatalf("a refused sign-in is still pending: finished=%v err=%v", finished, err)
	}
}

// With a password set, the dashboard's files need a session however their
// path is spelled: a provider prefix followed by dot segments is not a
// provider route.
func TestDotSegmentsDoNotPassForAProviderRoute(t *testing.T) {
	for _, path := range []string{"/codex/../app.js", "/claude/v1/../../index.html", "/codex/./x", "/grok/..", "/codex//app.js", "/gemini/."} {
		if nativeRoute(path) {
			t.Errorf("%s counts as a provider route", path)
		}
	}
	for _, path := range []string{"/codex/v1/responses", "/claude/v1/messages", "/codex/responses..json", "/v0/management/auth-files"} {
		if !nativeRoute(path) {
			t.Errorf("%s no longer counts as a provider route", path)
		}
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("dashboard file")) })
	mux.HandleFunc("/codex/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("proxy")) })
	gated := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, target := range []string{"/codex%2F..%2Fapp.js", "/codex/..%2Fapp.js", "/codex%2f%2e%2e%2fapp.js"} {
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:9123"+target, nil))
		if strings.Contains(w.Body.String(), "dashboard file") {
			t.Errorf("%s served a dashboard file without a session (%d)", target, w.Code)
		}
	}
}

// A paired device may use the host's accounts, never delete them: one that
// was lost or turned against its owner could otherwise wipe them all.
func TestPairedDevicesCannotRemoveTheHostsAccounts(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.Save(store.Account{ID: "keep-me", Provider: "fake", Email: "you@example.com"}); err != nil {
		t.Fatal(err)
	}
	m, err := proxy.New(st, map[string]provider.Provider{"fake": &recheckProvider{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: st, Proxy: m}
	mux := http.NewServeMux()
	api.Register(mux)
	host, err := remote.NewHost(t.TempDir(), 0, mux)
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
	client := remote.NewClient(t.TempDir(), "")
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, "Laptop"); err != nil {
		t.Fatal(err)
	}
	through := client.Middleware(func(*http.Request) map[string]any { return nil }, http.NotFoundHandler())
	w := httptest.NewRecorder()
	through.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/accounts/keep-me", nil))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "removed on") {
		t.Fatalf("a paired device removing an account: %d %s", w.Code, w.Body.String())
	}
	if _, err := st.Get("keep-me"); err != nil {
		t.Fatal("the paired device's request removed the account")
	}
	// This computer removes its own accounts as before.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "http://127.0.0.1:8787/api/accounts/keep-me", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("removing an account here: %d %s", w.Code, w.Body.String())
	}
}
