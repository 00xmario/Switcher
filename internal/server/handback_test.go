package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"switcher/internal/codexcfg"
	"switcher/internal/handback"
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

func TestHandbackIsUnavailableWithoutAManager(t *testing.T) {
	api := &API{}
	mux := http.NewServeMux()
	api.registerHandbackRoutes(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/handback", "127.0.0.1:5000", map[string]bool{"enabled": true}); w.Code != http.StatusNotImplemented {
		t.Fatalf("no manager: %d", w.Code)
	}
}
