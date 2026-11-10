package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A paired Mac shows its host's load: numbers kept in range, named after the
// host as paired, never what the host itself writes.
func TestAClientReadsItsHostsLoad(t *testing.T) {
	answer := map[string]any{"machine": "<img src=x>", "platform": "linux", "available": true, "reason": "<b>",
		"cpu": 250, "cores": 16, "memory": -3, "memory_used": 30, "memory_total": 20, "gpu": 140}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/system" {
			if _, ok := PairedDevice(r); !ok {
				t.Error("the load was asked without the paired device")
			}
			json.NewEncoder(w).Encode(answer)
			return
		}
		http.NotFound(w, r)
	})
	_, client := pairedWith(t, next)
	got := client.SystemLoad(context.Background())
	host := client.Status().HostName
	if !got.Remote || !got.Available || got.Machine != host || got.Reason != "" || got.Platform != "linux" {
		t.Fatalf("view: %+v (host %q)", got, host)
	}
	if got.CPU != 100 || got.Memory != 0 || got.Cores != 16 || got.MemUsed != 0 || got.MemTotal != 0 || got.GPU == nil || *got.GPU != 100 {
		t.Fatalf("readings out of range: %+v gpu=%v", got, got.GPU)
	}

	answer = map[string]any{"available": false, "reason": "unsupported", "cpu": 40}
	if got = client.SystemLoad(context.Background()); got.Available || got.Reason != "unsupported" || got.CPU != 0 {
		t.Fatalf("a host without readings: %+v", got)
	}
}

// An older host has no load route: the card says so instead of failing.
func TestAnOlderHostHasNoLoad(t *testing.T) {
	_, client := pairedWith(t, http.NotFoundHandler())
	if got := client.SystemLoad(context.Background()); got.Available || got.Reason != "update" || !got.Remote {
		t.Fatalf("older host: %+v", got)
	}
}

func TestOnlyTheLoadRouteItselfIsOpen(t *testing.T) {
	if !allowedRemotePath("/api/system") {
		t.Fatal("/api/system closed to paired devices")
	}
	for _, path := range []string{"/api/system/", "/api/systems", "/api/system/x", "/api/sys"} {
		if allowedRemotePath(path) {
			t.Errorf("%s open to paired devices", path)
		}
	}
}

// A host's usage limits reach this Mac's screens only as whole shares from
// 1 to 95: anything else, markup included, is dropped.
func TestAHostsUsageLimitsAreOnlyNumbers(t *testing.T) {
	state := map[string]any{"accounts": []any{
		map[string]any{"id": "a", "provider": "claude", "usage_limit": `"><img src=x onerror=alert(1)>`, "at_limit": true},
		map[string]any{"id": "b", "provider": "claude", "usage_limit": 10.5, "at_limit": "yes"},
		map[string]any{"id": "c", "provider": "claude", "usage_limit": 96.0},
		map[string]any{"id": "d", "provider": "claude", "usage_limit": 20.0, "at_limit": "true"},
		map[string]any{"id": "e", "provider": "claude", "usage_limit": 30.0, "at_limit": true},
	}}
	sanitizeHostState(state)
	accounts := state["accounts"].([]any)
	for i, want := range []map[string]any{{}, {}, {}, {"usage_limit": 20.0}, {"usage_limit": 30.0, "at_limit": true}} {
		account := accounts[i].(map[string]any)
		for _, key := range []string{"usage_limit", "at_limit"} {
			if account[key] != want[key] {
				t.Errorf("account %v: %s = %v, want %v", account["id"], key, account[key], want[key])
			}
		}
	}
}

func TestOnlyTheSkillRouteItselfIsOpen(t *testing.T) {
	if !allowedRemotePath("/api/agent-skill") {
		t.Fatal("/api/agent-skill closed to paired devices")
	}
	for _, path := range []string{"/api/agent-skill/", "/api/agent-skills", "/api/agent"} {
		if allowedRemotePath(path) {
			t.Errorf("%s open to paired devices", path)
		}
	}
}

// An older host has no skill route: reading says so, installing explains.
func TestAnOlderHostHasNoSkill(t *testing.T) {
	_, client := pairedWith(t, http.NotFoundHandler())
	if got := client.HostSkill(context.Background()); got.Reason != "update" || got.Skill != nil {
		t.Fatalf("older host: %+v", got)
	}
	if _, err := client.SetHostSkill(context.Background(), true); err == nil || !strings.Contains(err.Error(), "1.27.0") {
		t.Fatalf("install on an older host: %v", err)
	}
}

// The host's refusal reaches this Mac as plain text, and its paths too.
func TestAHostsSkillAnswerIsPlainText(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PairedDevice(r); !ok {
			t.Error("asked without the paired device")
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "edited\x1b[31m by hand"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "current": true, "path": "/home/x/SKILL.md\x07", "claude_link": "a\nb"})
	})
	_, client := pairedWith(t, next)
	got := client.HostSkill(context.Background())
	if got.Skill == nil || !got.Skill.Current || got.Skill.Path != "/home/x/SKILL.md" || got.Skill.ClaudeLink != "ab" {
		t.Fatalf("status: %+v", got.Skill)
	}
	if _, err := client.SetHostSkill(context.Background(), true); err == nil || strings.ContainsAny(err.Error(), "\x1b") || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("refusal: %v", err)
	}
}

// The settings page never waits: the first reading is taken in the
// background, and later ones answer from it while fresh.
func TestTheHostsSkillIsReadInTheBackground(t *testing.T) {
	var reads atomic.Int32
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		<-release
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "current": true})
	})
	_, client := pairedWith(t, next)
	start := time.Now()
	if got := client.CachedHostSkill(); got.Reason != "checking" || got.Skill != nil || time.Since(start) > time.Second {
		t.Fatalf("first: %+v after %v", got, time.Since(start))
	}
	client.CachedHostSkill() // one reading at a time
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := client.CachedHostSkill(); got.Skill != nil && got.Skill.Current {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background reading never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := client.HostSkill(context.Background()); got.Skill == nil || reads.Load() != 1 {
		t.Fatalf("a fresh reading was not reused: %+v, %d reads", got, reads.Load())
	}
}

// A reading that started before an install cannot undo what the install
// showed, an old reading says it is old, and readings on their way are
// shared.
func TestTheHostsSkillKeepsTheLatestWrite(t *testing.T) {
	var reads atomic.Int32
	installed := atomic.Bool{}
	gate := make(chan struct{}, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			installed.Store(true)
		} else {
			reads.Add(1)
			now := installed.Load()
			<-gate // answered as it was when asked
			json.NewEncoder(w).Encode(map[string]any{"installed": now, "current": now, "managed": now})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"installed": true, "current": true, "managed": true})
	})
	_, client := pairedWith(t, next)
	client.CachedHostSkill() // a reading starts, not installed
	client.CachedHostSkill() // shared, not a second one
	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := client.SetHostSkill(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	gate <- struct{}{} // the old reading lands after the install
	time.Sleep(100 * time.Millisecond)
	if got := client.CachedHostSkill(); got.Skill == nil || !got.Skill.Installed || got.Stale || reads.Load() != 1 {
		t.Fatalf("after the install: %+v stale=%v, %d reads", got.Skill, got.Stale, reads.Load())
	}
	// A host change forgets the reading.
	client.forgetSkill()
	if got := client.CachedHostSkill(); got.Reason != "checking" {
		t.Fatalf("after a host change: %+v", got)
	}
	gate <- struct{}{}
}

// A host's apps reach this computer as short plain names and numbers in
// range, named after the host as paired.
func TestAClientReadsItsHostsApps(t *testing.T) {
	long := strings.Repeat("x", 200)
	answer := map[string]any{"machine": "<b>", "available": true, "cpu": 20, "memory": 50, "memory_total": 100, "memory_used": 50,
		"processes": 300, "gpu_apps": true, "apps": []any{
			map[string]any{"name": "Chrome\x1b[2J‮", "processes": 12, "cpu": 340.0, "memory": 9e18, "gpu": -4.0, "gpu_memory": 1 << 20},
			map[string]any{"name": long, "processes": -1, "cpu": 2.5, "memory": 1 << 30},
			map[string]any{"name": "\x07", "cpu": 1.0},
		}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/system/apps" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(answer)
	})
	_, client := pairedWith(t, next)
	got := client.SystemApps(context.Background())
	if !got.Remote || !got.Available || got.Machine != client.Status().HostName || got.Processes != 300 || len(got.Apps) != 2 {
		t.Fatalf("%+v", got)
	}
	chrome := got.Apps[0]
	if chrome.Name != "Chrome[2J" || chrome.CPU != 100 || chrome.Memory != 0 || chrome.GPU == nil || *chrome.GPU != 0 || chrome.GPUMemory != 1<<20 {
		t.Fatalf("chrome: %+v", chrome)
	}
	if other := got.Apps[1]; len([]rune(other.Name)) != 64 || other.Processes != 0 || other.Memory != 1<<30 || other.GPU != nil {
		t.Fatalf("other: %+v", other)
	}
	answer["gpu_apps"] = false
	if got := client.SystemApps(context.Background()); got.Apps[0].GPU != nil || got.Apps[0].GPUMemory != 0 {
		t.Fatalf("graphics kept without per-app readings: %+v", got.Apps[0])
	}
}

func TestAnOlderHostHasNoApps(t *testing.T) {
	_, client := pairedWith(t, http.NotFoundHandler())
	if got := client.SystemApps(context.Background()); got.Available || got.Reason != "update" || got.Apps != nil {
		t.Fatalf("older host: %+v", got)
	}
	if !allowedRemotePath("/api/system/apps") || allowedRemotePath("/api/system/apps/x") {
		t.Fatal("the apps route is not open exactly")
	}
}
