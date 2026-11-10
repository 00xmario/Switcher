package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switcher/internal/codexcfg"
	"switcher/internal/handback"
	"switcher/internal/remote"
	"switcher/internal/store"
)

func TestHandbackRoutesHandBackAndSetUpFromThisMachineOnly(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte(`model_provider = "openai"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexcfg.InstallAt(config, 8787); err != nil {
		t.Fatal(err)
	}
	api := &API{Handback: handback.New(t.TempDir(), handback.Deps{Codex: handback.CodexFiles(config, 8787)})}
	mux := http.NewServeMux()
	api.registerHandbackRoutes(mux)

	for _, remoteAddr := range []string{"192.168.1.20:5000", "100.101.102.103:443"} {
		if w := phoneRequest(mux, http.MethodPost, "/api/handback", remoteAddr, map[string]bool{"enabled": true}); w.Code != http.StatusForbidden {
			t.Fatalf("hand back from %s: %d", remoteAddr, w.Code)
		}
	}
	if api.handedBack() {
		t.Fatal("handed back from another device")
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]string{}); w.Code != http.StatusBadRequest {
		t.Fatalf("missing choice: %d", w.Code)
	}
	w := phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]bool{"enabled": true})
	var answer struct {
		Handback handback.Status `json:"handback"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &answer) != nil || !answer.Handback.HandedBack || len(answer.Handback.Steps) == 0 {
		t.Fatalf("hand back: %d %s", w.Code, w.Body)
	}
	if got := codexcfg.Check(config, 8787); got.Condition == "ready" {
		t.Fatal("Codex still uses Switcher")
	}
	if fields := api.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil)); fields["handed_back"] != true {
		t.Fatalf("state: %v", fields["handed_back"])
	}
	if w := phoneRequest(mux, http.MethodGet, "/api/handback", "127.0.0.1:5000", nil); w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	w = phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]bool{"enabled": false})
	if w.Code != http.StatusOK || api.handedBack() || codexcfg.Check(config, 8787).Condition != "ready" {
		t.Fatalf("set up again: %d %s", w.Code, w.Body)
	}
}

// keptAccounts is a Switcher account that cannot take its tokens back.
type keptAccounts struct{ account store.Account }

func (k keptAccounts) SelectedCodex(context.Context) (store.Account, bool, error) {
	return k.account, true, nil
}
func (k keptAccounts) Get(string) (store.Account, error)    { return k.account, nil }
func (k keptAccounts) AdoptToken(string, store.Token) error { return errors.New("disk full") }
func (k keptAccounts) SetHandedBack(bool)                   {}

// A set-up that stays handed back, because Codex's login could not be taken
// back, is an error: the dashboard and tray must not say it is set up.
func TestASetUpThatStaysHandedBackIsAConflict(t *testing.T) {
	codexDir := t.TempDir()
	config := filepath.Join(codexDir, "config.toml")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "handback.json"), []byte(`{"phase":"handed_back","since":"2026-11-01T12:00:00Z","codex_login":"codex-1"}`), 0o600)
	os.WriteFile(codexcfg.AuthPath(config), []byte(`{"tokens":{"access_token":"a2","refresh_token":"codex-r2","account_id":"acct"}}`), 0o600)
	account := store.Account{ID: "codex-1", Provider: "codex", Token: store.Token{AccessToken: "a", RefreshToken: "r", AccountID: "acct"}}
	api := &API{Handback: handback.New(dir, handback.Deps{Codex: handback.CodexFiles(config, 8787), Accounts: keptAccounts{account}})}
	mux := http.NewServeMux()
	api.registerHandbackRoutes(mux)
	w := phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]bool{"enabled": false})
	if w.Code != http.StatusConflict || !api.handedBack() {
		t.Fatalf("set up that stayed handed back: %d %s", w.Code, w.Body)
	}
}

func TestHandbackIsUnavailableWithoutAManager(t *testing.T) {
	api := &API{}
	mux := http.NewServeMux()
	api.registerHandbackRoutes(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]bool{"enabled": true}); w.Code != http.StatusNotImplemented {
		t.Fatalf("no manager: %d", w.Code)
	}
}

// While handed back, neither sharing nor another Switcher can be turned on.
func TestHandedBackRefusesSharingAndAHost(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(t.TempDir(), "config.toml")
	m := handback.New(dir, handback.Deps{Codex: handback.CodexFiles(config, 8787)})
	if _, err := m.HandBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	api := &API{Handback: m, RemoteClient: remote.NewClient(t.TempDir(), "k")}
	host, err := remote.NewHost(t.TempDir(), 0, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	api.RemoteHost = host
	mux := http.NewServeMux()
	api.Register(mux)
	for _, c := range []struct{ path, body string }{
		{"/api/remote/host", `{"enabled":true}`},
		{"/api/remote/connect", `{"address":"127.0.0.1","port":1,"code":"abc"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.RemoteAddr, req.Host = "127.0.0.1:1", "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "handed back") {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
	}
	if host.Status().Enabled {
		t.Fatal("sharing turned on while handed back")
	}
}
