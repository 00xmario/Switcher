package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/remote"
	"switcher/internal/usage"
)

type ratesRoundTrip func(*http.Request) (*http.Response, error)

func (f ratesRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// usageComputer is one Switcher with Claude Code logs of its own: a number
// of tokens, at a dollar per token.
func usageComputer(t *testing.T, name string, tokens int) (*API, *usage.Service) {
	t.Helper()
	config := t.TempDir()
	logs := filepath.Join(config, "projects")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"%s-session","message":{"id":"%s-1","model":"fixture-model","usage":{"input_tokens":0,"output_tokens":%d}}}`+"\n",
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), name, name, tokens)
	if err := os.WriteFile(filepath.Join(logs, "session.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	// This computer's logs only; never the user's own.
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("GROK_HOME", t.TempDir())
	rates := &http.Client{Transport: ratesRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"fixture-model":{"input_cost_per_token":1,"output_cost_per_token":1}}`))}, nil
	})}
	svc := usage.NewService(t.TempDir(), rates)
	if svc.Scan(30) == nil {
		t.Fatal("no scan")
	}
	api := &API{Usage: svc, MachineName: name}
	svc.SetIdentity(api.UsageIdentity)
	svc.SetOthers(api.UsageLedgers)
	return api, svc
}

// A host and the computers that use it all show the same usage: every
// computer's, added up. The host keeps a computer's usage while it is away,
// and a client shows the host's last numbers while the host is away.
func TestUsageIsCombinedAcrossHostAndClients(t *testing.T) {
	hostAPI, hostUsage := usageComputer(t, "k12", 100)
	hostAPI.UsageDevices = usage.OpenDeviceStore(filepath.Join(t.TempDir(), "usage-devices"))
	hostAPI.RemoteClient = remote.NewClient(t.TempDir(), "")
	mux := http.NewServeMux()
	hostAPI.Register(mux)
	host, err := remote.NewHost(t.TempDir(), 0, mux)
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	hostAPI.RemoteHost = host
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	pair := func(name string, tokens int) (*API, *usage.Service) {
		api, svc := usageComputer(t, name, tokens)
		code, err := host.NewPairingCode()
		if err != nil {
			t.Fatal(err)
		}
		api.RemoteClient = remote.NewClient(t.TempDir(), "")
		if _, err := api.RemoteClient.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, name); err != nil {
			t.Fatal(err)
		}
		return api, svc
	}
	laptopAPI, laptopUsage := pair("MacBook", 20)
	miniAPI, miniUsage := pair("Mini", 3)
	laptopAPI.syncUsage(context.Background())
	miniAPI.syncUsage(context.Background())
	laptopAPI.syncUsage(context.Background()) // now with Mini's too

	for name, svc := range map[string]*usage.Service{"k12": hostUsage, "MacBook": laptopUsage, "Mini": miniUsage} {
		s := svc.Get(30)
		if s.TotalTokens != 123 || s.TotalCost != 123 || s.Sessions != 3 || len(s.Devices) != 3 {
			t.Fatalf("%s sees tokens %d cost %v sessions %d devices %+v", name, s.TotalTokens, s.TotalCost, s.Sessions, s.Devices)
		}
		var this []string
		hosts := 0
		for _, d := range s.Devices {
			if d.This {
				this = append(this, d.Name)
			}
			if d.Host {
				hosts++
			}
		}
		if len(this) != 1 || this[0] != name || hosts != 1 {
			t.Fatalf("%s marks itself as %v, %d hosts: %+v", name, this, hosts, s.Devices)
		}
	}

	// The Usage page on a client says whose usage it adds up.
	w := httptest.NewRecorder()
	laptopAPI.handleUsage(w, httptest.NewRequest(http.MethodGet, "/api/tokens?days=30", nil))
	var answer struct {
		Summary usage.Summary  `json:"summary"`
		Sharing map[string]any `json:"sharing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.Summary.TotalTokens != 123 || answer.Sharing["host"] == nil || answer.Sharing["updated_ms"] == nil {
		t.Fatalf("the client's Usage answer: %v %s", err, w.Body.String())
	}

	// A paired computer may send only its own usage.
	stranger := httptest.NewRecorder()
	hostAPI.handleUsageDevice(stranger, httptest.NewRequest(http.MethodPut, "/api/tokens/device", strings.NewReader(`{}`)))
	if stranger.Code != http.StatusNotFound {
		t.Fatalf("this computer's own request was taken as a paired computer's: %d", stranger.Code)
	}

	// The Mini goes away: its usage still counts on the host, and the
	// MacBook keeps counting it.
	if err := host.Revoke(deviceNamed(t, host, "Mini")); err != nil {
		t.Fatal(err)
	}
	laptopAPI.syncUsage(context.Background())
	if s := laptopUsage.Get(30); s.TotalTokens != 123 {
		t.Fatalf("after the Mini left: %d tokens", s.TotalTokens)
	}
	// The host goes away: the MacBook shows the host's last numbers.
	host.Close()
	laptopAPI.syncUsage(context.Background())
	if s := laptopUsage.Get(30); s.TotalTokens != 123 {
		t.Fatalf("with the host away: %d tokens", s.TotalTokens)
	}
	if sharing := laptopAPI.usageSharing(); sharing["error"] == nil {
		t.Fatalf("the failed exchange is not reported: %v", sharing)
	}
	// Kept on disk: after a restart of the MacBook's Switcher too.
	reopened := remote.NewClient(laptopAPI.RemoteClient.Dir(), "")
	if ledgers, at := reopened.UsageLedgers(); len(ledgers) != 3 || at.IsZero() {
		t.Fatalf("after a restart: %d ledgers, at %v", len(ledgers), at)
	}
}

func deviceNamed(t *testing.T, host *remote.Host, name string) string {
	t.Helper()
	for _, d := range host.Status().Devices {
		if d.Name == name {
			return d.ID
		}
	}
	t.Fatalf("no device %s", name)
	return ""
}

// A host from before usage was combined answers with not found: the client
// shows its own usage and says why.
func TestOlderHostLeavesUsageAlone(t *testing.T) {
	api, svc := usageComputer(t, "MacBook", 7)
	old := http.NewServeMux() // no usage routes
	host, err := remote.NewHost(t.TempDir(), 0, old)
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	_ = host.SetEnabled(true)
	code, _ := host.NewPairingCode()
	api.RemoteClient = remote.NewClient(t.TempDir(), "")
	if _, err := api.RemoteClient.Connect(context.Background(), "127.0.0.1", host.Status().Port, code.Code, "MacBook"); err != nil {
		t.Fatal(err)
	}
	api.syncUsage(context.Background())
	if s := svc.Get(30); s.TotalTokens != 7 || s.Devices != nil {
		t.Fatalf("own usage: %d tokens, devices %+v", s.TotalTokens, s.Devices)
	}
	if sharing := api.usageSharing(); sharing["error"] == nil || !strings.Contains(sharing["error"].(string), "update") {
		t.Fatalf("why is not said: %v", sharing)
	}
}
