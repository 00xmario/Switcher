package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/claudesync"
	"switcher/internal/settings"
	"switcher/internal/store"
)

func TestClaudeSyncGuardsAndNames(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.Save(store.Account{ID: "claude-test", Provider: "claude", Email: "account@example.com", Token: store.Token{AccountID: "desktop-uuid"}}); err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("test-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a := &API{Store: st, Settings: prefs, syncClaudeForTest: func(ctx context.Context, labels map[string]string) (claudesync.Result, error) {
		calls++
		if labels["desktop-uuid"] != "account@example.com" {
			t.Fatal("account UUID was not mapped to a label")
		}
		return claudesync.Result{Added: 2, Backup: "fixture-backup", Accounts: []claudesync.AccountResult{{ID: "desktop-uuid", Label: labels["desktop-uuid"], Added: 2, Total: 4}}}, nil
	}}
	mux := http.NewServeMux()
	a.Register(mux)
	gate := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, tc := range []struct {
		name, remote, cookie, csrf string
		status                     int
	}{
		{"unauthenticated", "127.0.0.1:8888", "", "", 401},
		{"missing CSRF", "127.0.0.1:8888", token, "", 403},
		{"LAN spoofing Host", "192.168.1.2:8888", token, prefs.CSRFToken(), 403},
		{"local authorized", "127.0.0.1:8888", token, prefs.CSRFToken(), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/claude/sync", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Switcher-CSRF", tc.csrf)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "switcher_session", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if w.Code == 200 {
				var body struct {
					Result claudesync.Result `json:"result"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Result.Added != 2 {
					t.Fatalf("counts missing: %v", err)
				}
				if strings.Contains(w.Body.String(), "access_token") {
					t.Fatal("token disclosure")
				}
			}
		})
	}
	if calls != 1 {
		t.Fatalf("runner called %d times", calls)
	}
}

func TestClaudeSyncCompletesPastRequestReadDeadline(t *testing.T) {
	api := &API{
		Store: store.New(t.TempDir()),
		syncClaudeForTest: func(ctx context.Context, _ map[string]string) (claudesync.Result, error) {
			select {
			case <-ctx.Done():
				return claudesync.Result{}, ctx.Err()
			case <-time.After(150 * time.Millisecond):
				return claudesync.Result{Added: 1}, nil
			}
		},
	}
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(controlRequests(mux, 25*time.Millisecond))
	defer srv.Close()

	client := srv.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Post(srv.URL+"/api/claude/sync", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Detail  string `json:"detail"`
		Details string `json:"details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body.Detail, "1 new session added") {
		t.Fatalf("status %d, result %q, error %q", resp.StatusCode, body.Detail, body.Details)
	}
}

func TestClaudeSyncSurvivesClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &API{
		Store: store.New(t.TempDir()),
		syncClaudeForTest: func(syncCtx context.Context, _ map[string]string) (claudesync.Result, error) {
			cancel()
			return claudesync.Result{Added: 1}, syncCtx.Err()
		},
	}
	mux := http.NewServeMux()
	api.Register(mux)
	req := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/claude/sync", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:8888"
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d after client cancellation: %s", w.Code, w.Body.String())
	}
}
