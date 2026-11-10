package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"switcher/internal/store"
	"switcher/internal/sysload"
)

func TestAccountUsageLimitRoundTrip(t *testing.T) {
	h := newAutoResetHarness(t)
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	read := func(rec *httptest.ResponseRecorder) accountView {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Account accountView `json:"account"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Account
	}
	if got := read(h.patch(t, "fake-aaaa", `{"usage_limit":10}`)); got.UsageLimit != 10 || got.AtLimit {
		t.Fatalf("limit 10: %+v", got)
	}
	if got := h.api.Proxy.UsageLimit("fake-aaaa"); got != 10 {
		t.Fatalf("stored %d", got)
	}
	// Both preferences in one request, and the other one left alone.
	got := read(h.patch(t, "fake-aaaa", `{"usage_limit":25,"auto_use_reset":"on"}`))
	if got.UsageLimit != 25 || got.AutoUseReset != "on" {
		t.Fatalf("both: %+v", got)
	}
	if got = read(h.patch(t, "fake-aaaa", `{"auto_use_reset":"global"}`)); got.UsageLimit != 25 {
		t.Fatalf("an auto-use change dropped the limit: %+v", got)
	}
	if got = read(h.patch(t, "fake-aaaa", `{"usage_limit":0}`)); got.UsageLimit != 0 {
		t.Fatalf("off: %+v", got)
	}
}

// Nothing is saved from a request with any value out of range.
func TestAccountUsageLimitRejectsWhatALimitCannotBe(t *testing.T) {
	h := newAutoResetHarness(t)
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"usage_limit":-1}`, `{"usage_limit":96}`, `{"usage_limit":"10"}`, `{"usage_limit":10.5}`,
		`{"usage_limit":10,"auto_use_reset":"maybe"}`} {
		if rec := h.patch(t, "fake-aaaa", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH %s: %d, want 400", body, rec.Code)
		}
	}
	if got := h.api.Proxy.UsageLimit("fake-aaaa"); got != 0 {
		t.Fatalf("a rejected request saved %d", got)
	}
	if rec := h.patch(t, "missing-account", `{"usage_limit":10}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d, want 404", rec.Code)
	}
}

func TestSystemLoadAnswersThisComputer(t *testing.T) {
	get := func(a *API) sysload.View {
		t.Helper()
		mux := http.NewServeMux()
		a.Register(mux)
		req := httptest.NewRequest(http.MethodGet, "/api/system", nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var v sysload.View
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("GET /api/system: %d %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("a load reading may be cached")
		}
		return v
	}
	h := newAutoResetHarness(t)
	if v := get(h.api); v.Available || v.Reason != "unsupported" || v.Remote {
		t.Fatalf("without a sampler: %+v", v)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	h.api.Load = sysload.New()
	v := get(h.api)
	if !v.Available || v.Remote || v.Platform != runtime.GOOS || v.Cores < 1 || v.MemTotal == 0 || v.CPU < 0 || v.CPU > 100 {
		t.Fatalf("this computer: %+v", v)
	}
}

// The load page's breakdown lists this computer's apps, by name only.
func TestSystemAppsListsThisComputersApps(t *testing.T) {
	a := &API{Load: sysload.New()}
	mux := http.NewServeMux()
	a.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/system/apps", nil)
	req.RemoteAddr, req.Host = "127.0.0.1:1", "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var v sysload.AppsView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !a.Load.Supported() {
		if v.Available || v.Reason != "unsupported" {
			t.Fatalf("unsupported platform: %+v", v)
		}
		return
	}
	if !v.Available || v.Processes == 0 || len(v.Apps) == 0 || v.Apps == nil {
		t.Fatalf("no apps: %+v", v)
	}
	for _, app := range v.Apps {
		if app.Name == "" || strings.Contains(app.Name, " --") {
			t.Fatalf("an app name carries more than a name: %q", app.Name)
		}
	}
}
