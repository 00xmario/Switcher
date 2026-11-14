package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"switcher/internal/phone"
	"switcher/internal/sysload"
)

// An approved phone may switch Claude Code's login through the phone
// listener; the same request from the network may not.
func TestApprovedPhoneSwitchesClaudeCode(t *testing.T) {
	api, manager, paths, _, _, _ := nativeClaudeFixtureAPI(t)
	mux := http.NewServeMux()
	api.Register(mux)

	// Without a phone, a request that does not come from this Mac is refused.
	lan := httptest.NewRequest("POST", "http://switcher-phone/api/accounts/claude-b/activate", nil)
	lan.RemoteAddr = "192.0.2.3:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, lan)
	if w.Code != http.StatusForbidden || manager.ActiveID("claude") != "claude-a" {
		t.Fatalf("non-local switch without a phone: %d", w.Code)
	}

	access := phone.New(filepath.Join(t.TempDir(), "phone.json"))
	if err := access.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	h := &phone.Handler{Access: access, Key: func() string { return "add-on-key" }, Inner: mux, Static: fstest.MapFS{}}
	send := func(method, path string, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
		r.Header.Set("X-Switcher-Phone-Key", "add-on-key")
		r.Header.Set("X-Switcher-Phone-Node", "nPhone")
		r.Header.Set("X-Switcher-Phone-Device", "phone")
		r.Header.Set("X-Switcher-Phone-Origin", "https://switcher-mac.tail1.ts.net")
		if method != http.MethodGet {
			r.Header.Set("Origin", "https://switcher-mac.tail1.ts.net")
		}
		if csrf != "" {
			r.Header.Set("X-Switcher-CSRF", csrf)
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w = send(http.MethodPost, "/api/pair", nil, "")
	var pairing struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &pairing)
	poll := w.Result().Cookies()
	if _, err := access.Approve(pairing.Code); err != nil {
		t.Fatal(err)
	}
	w = send(http.MethodGet, "/api/pair", poll, "")
	var approved struct{ CSRF string }
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	var session []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-switcher_phone" {
			session = append(session, c)
		}
	}
	if len(session) != 1 || approved.CSRF == "" {
		t.Fatalf("pairing failed: %s", w.Body.String())
	}

	w = send(http.MethodPost, "/api/accounts/claude-b/use", session, approved.CSRF)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"claude_code_switched":true`) {
		t.Fatalf("phone switch: %d %s", w.Code, w.Body.String())
	}
	if manager.ActiveID("claude") != "claude-b" {
		t.Fatal("the phone's switch did not select the account")
	}
	credentials, err := os.ReadFile(paths.CredentialsFile)
	if err != nil || !strings.Contains(string(credentials), "fixture-token-b") {
		t.Fatal("Claude Code's login was not switched")
	}
}

// Through the phone listener, an approved phone reads this computer's load
// from the real API: the read is not one kept to this Mac.
func TestApprovedPhoneReadsTheLoad(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no load readings here")
	}
	mux := http.NewServeMux()
	(&API{Load: sysload.New()}).Register(mux)
	access := phone.New(filepath.Join(t.TempDir(), "phone.json"))
	if err := access.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	h := &phone.Handler{Access: access, Key: func() string { return "add-on-key" }, Inner: mux, Static: fstest.MapFS{}}
	send := func(method, path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
		r.Header.Set("X-Switcher-Phone-Key", "add-on-key")
		r.Header.Set("X-Switcher-Phone-Node", "nPhone")
		r.Header.Set("X-Switcher-Phone-Device", "phone")
		r.Header.Set("X-Switcher-Phone-Origin", "https://switcher-mac.tail1.ts.net")
		if method != http.MethodGet {
			r.Header.Set("Origin", "https://switcher-mac.tail1.ts.net")
		}
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := send(http.MethodPost, "/api/pair", nil)
	var pairing struct{ Code string }
	_ = json.Unmarshal(w.Body.Bytes(), &pairing)
	poll := w.Result().Cookies()
	if _, err := access.Approve(pairing.Code); err != nil {
		t.Fatal(err)
	}
	var session []*http.Cookie
	for _, c := range send(http.MethodGet, "/api/pair", poll).Result().Cookies() {
		if c.Name == "__Host-switcher_phone" {
			session = append(session, c)
		}
	}
	w = send(http.MethodGet, "/api/system/apps", session)
	var v sysload.AppsView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.Available || v.Cores < 1 {
		t.Fatalf("load through the phone: %d %s", w.Code, w.Body.String())
	}
}
