package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"switcher/internal/autostart"
)

func TestAutostartRoutesWriteLoginItemsFromThisComputerOnly(t *testing.T) {
	api := &API{Autostart: autostart.NewAt("linux", t.TempDir(), "/home/u/.local/bin/switcher", nil)}
	mux := http.NewServeMux()
	api.Register(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/autostart", "192.168.1.20:5000", map[string]bool{"enabled": true}); w.Code != http.StatusForbidden {
		t.Fatalf("from the network: %d", w.Code)
	}
	w := phoneRequest(mux, http.MethodPost, "/api/autostart", "127.0.0.1:5000", map[string]bool{"enabled": true})
	var st autostart.Status
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || !st.Enabled {
		t.Fatalf("turn on: %d %s", w.Code, w.Body)
	}
	if fields := api.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil)); fields["autostart"] == nil {
		t.Fatal("state lacks autostart")
	}

	mac := &API{Autostart: autostart.NewAt("darwin", t.TempDir(), "/x", nil)}
	mux = http.NewServeMux()
	mac.Register(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/autostart", "127.0.0.1:5000", map[string]bool{"enabled": true}); w.Code != http.StatusNotImplemented {
		t.Fatalf("on a Mac: %d", w.Code)
	}
	if _, ok := mac.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil))["autostart"]; ok {
		t.Fatal("a Mac's state offers Linux login items")
	}
}

func TestQuitStopsSwitcherOnlyForThisComputer(t *testing.T) {
	quit := make(chan struct{}, 1)
	api := &API{Quit: func() { quit <- struct{}{} }}
	mux := http.NewServeMux()
	api.Register(mux)
	if w := phoneRequest(mux, http.MethodPost, "/api/quit", "100.101.102.103:443", nil); w.Code != http.StatusForbidden {
		t.Fatalf("from the network: %d", w.Code)
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/quit", "127.0.0.1:5000", nil); w.Code != http.StatusOK {
		t.Fatalf("quit: %d", w.Code)
	}
	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop")
	}
}
