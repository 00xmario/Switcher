package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingUI struct {
	mu       sync.Mutex
	renders  [][]Item
	notified []string
	quit     bool
}

func (u *recordingUI) Render(items []Item) {
	u.mu.Lock()
	u.renders = append(u.renders, items)
	u.mu.Unlock()
}
func (u *recordingUI) Notify(title, body string) {
	u.mu.Lock()
	u.notified = append(u.notified, title+": "+body)
	u.mu.Unlock()
}
func (u *recordingUI) Quit() { u.mu.Lock(); u.quit = true; u.mu.Unlock() }
func (u *recordingUI) last() []Item {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.renders) == 0 {
		return nil
	}
	return u.renders[len(u.renders)-1]
}
func (u *recordingUI) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.renders) }

// fakeServer is a Switcher API with a settable state; it records posts.
type fakeServer struct {
	mu    sync.Mutex
	state map[string]any
	calls []string
	down  bool
	// feed answers waits for changes; without it they get a 404, as from
	// a server older than them. A wait without a change lasts hold.
	feed    bool
	hold    time.Duration
	version int // the state's tag, raised by every set
	reads   int // state reads
	waits   int // waits for changes
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/state/changes" && r.Header.Get("Authorization") == "Bearer device-token" {
		f.changes(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer device-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/state" {
		f.reads++
		json.NewEncoder(w).Encode(f.state)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
	io.WriteString(w, `{}`)
}

// changes answers the way the server's /api/state/changes does: at once
// when the state differs from since, otherwise once it changes or hold
// passes, with the tag it was asked about.
func (f *fakeServer) changes(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.waits++
	feed, hold := f.feed, f.hold
	f.mu.Unlock()
	if !feed {
		http.NotFound(w, r)
		return
	}
	since := r.URL.Query().Get("since")
	deadline := time.Now().Add(hold)
	for {
		f.mu.Lock()
		tag := fmt.Sprint(f.version)
		f.mu.Unlock()
		if tag != since || time.Now().After(deadline) {
			json.NewEncoder(w).Encode(map[string]any{"etag": tag, "changed": tag != since})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (f *fakeServer) set(key string, value any) {
	f.mu.Lock()
	f.state[key] = value
	f.version++
	f.mu.Unlock()
}
func (f *fakeServer) counts() (reads, waits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.waits
}
func (f *fakeServer) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func baseState() map[string]any {
	return map[string]any{
		"version": "1.20.0", "order": []string{"claude", "codex", "grok"}, "hidden": []string{"grok"},
		"accounts": []map[string]any{
			{"id": "claude-1", "provider": "claude", "email": "work@example.com", "plan": "claude_max_20x", "native_switch_available": true, "native_active": true,
				"usage": map[string]any{"windows": []map[string]any{{"label": "Session", "used_percent": 34}, {"label": "Weekly", "used_percent": 61}}}},
			{"id": "claude-2", "provider": "claude", "email": "home@example.com", "plan": "claude_max_5x", "native_switch_available": true, "active": true},
			{"id": "codex-1", "provider": "codex", "email": "work@example.com", "plan": "prolite", "active": true},
			{"id": "grok-1", "provider": "grok", "email": "hidden@example.com", "active": true},
		},
		"awake":     map[string]any{"supported": true, "active": false},
		"autostart": map[string]any{"supported": true, "enabled": true},
		"update":    map[string]any{"latest": "v1.21.0", "update_available": true},
	}
}

func newFixture(t *testing.T) (*fakeServer, *recordingUI, *Controller, *time.Time) {
	t.Helper()
	f := &fakeServer{state: baseState()}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ui := &recordingUI{}
	now := time.Unix(1_800_000_000, 0)
	c := New(Options{Client: &Client{Base: srv.URL, Token: func() string { return "device-token" }}, Now: func() time.Time { return now }}, ui)
	return f, ui, c, &now
}

func find(items []Item, prefix string) *Item {
	for i := range items {
		if strings.HasPrefix(items[i].Title, prefix) {
			return &items[i]
		}
		if found := find(items[i].Children, prefix); found != nil {
			return found
		}
	}
	return nil
}

func titles(items []Item) string {
	var out []string
	for _, item := range items {
		if item.Separator {
			out = append(out, "—")
		} else {
			out = append(out, item.Title)
		}
	}
	return strings.Join(out, "\n")
}

func TestMenuShowsAccountsUsageAndTheSwitches(t *testing.T) {
	_, ui, c, _ := newFixture(t)
	c.Refresh(context.Background())
	items := ui.last()
	work := find(items, "work@example.com · Max 20x")
	if work == nil || !work.Checked || work.Title != "work@example.com · Max 20x" || work.Action != "activate:claude-1" {
		t.Fatalf("Claude Code's account:\n%s", titles(items))
	}
	// Its usage follows on dim lines, as what is left.
	if text := titles(items); !strings.Contains(text, "work@example.com · Max 20x\n    Session: ▰▰▰▱▱ 66% left\n    Weekly: ▰▰▱▱▱ 39% left\n") {
		t.Fatalf("usage lines:\n%s", text)
	}
	if line := find(items, "    Session:"); !line.Disabled || line.Action != "" {
		t.Fatalf("a usage line is clickable: %+v", line)
	}
	if head := find(items, "Claude"); head.Icon != "claude" || !head.Disabled {
		t.Fatalf("provider head: %+v", head)
	}
	// Claude marks Claude Code's login, not the proxy's selection.
	if home := find(items, "home@example.com"); home == nil || home.Checked {
		t.Fatalf("other Claude account: %+v", home)
	}
	if find(items, "hidden@example.com") != nil {
		t.Fatal("a hidden provider is shown")
	}
	text := titles(items)
	if strings.Index(text, "Claude") > strings.Index(text, "Codex") {
		t.Fatalf("providers out of order:\n%s", text)
	}
	if find(items, "Use the accounts of") != nil {
		t.Fatal("use-host offered without a pairing")
	}
	for _, want := range []string{"Hand back to the providers on this computer", "Only this computer's Switcher changes", "Start at login", "Install update 1.21.0", "Keep awake", "Quit Switcher"} {
		if find(items, want) == nil {
			t.Errorf("menu lacks %q:\n%s", want, text)
		}
	}
	if hb := find(items, "Hand back"); hb.Checked || hb.Action != "" || find(hb.Children, "Hand back now").Action != "hand-back:on" {
		t.Fatalf("hand-back must ask first: %+v", hb)
	}
}

func TestMenuFollowsTheHostAndTheHandBack(t *testing.T) {
	f, ui, c, _ := newFixture(t)
	f.set("remote", map[string]any{"role": "client", "host": "Studio", "connected": false})
	c.Refresh(context.Background())
	items := ui.last()
	if find(items, "Can't reach Studio") == nil {
		t.Fatalf("unreachable host:\n%s", titles(items))
	}
	if use := find(items, "Use the accounts of Studio"); use == nil || !use.Checked {
		t.Fatalf("use-host: %+v", use)
	}
	f.set("remote", map[string]any{"role": "local", "host": "Studio"})
	f.set("handed_back", true)
	c.Refresh(context.Background())
	items = ui.last()
	if use := find(items, "Use the accounts of Studio"); use == nil || use.Checked {
		t.Fatalf("use-host while local: %+v", use)
	}
	if hb := find(items, "✓ Hand back"); hb == nil || hb.Action != "" || find(hb.Children, "Set up again now").Action != "hand-back:off" {
		t.Fatalf("setting up again must ask first: %+v", hb)
	}
	if find(items, "Only this computer's Switcher changes; Studio's stays as it is") == nil {
		t.Fatalf("the paired host is not named:\n%s", titles(items))
	}
	if work := find(items, "work@example.com · Max 20x"); !work.Disabled {
		t.Fatal("accounts switchable while handed back")
	}
}

func TestClicksSendTheirActions(t *testing.T) {
	f, ui, c, _ := newFixture(t)
	f.set("remote", map[string]any{"role": "client", "host": "Studio", "connected": true})
	ctx := context.Background()
	c.Refresh(ctx)
	for _, action := range []string{"activate:claude-2", "use-host", "hand-back:on", "hand-back:off", "awake:60", "awake:off", "awake-display", "autostart", "refresh"} {
		c.Click(ctx, action)
	}
	want := []string{
		`POST /api/accounts/claude-2/activate`,
		`POST /api/remote/use-host {"enabled":false}`,
		`POST /api/handback {"enabled":true}`,
		`POST /api/handback {"enabled":false}`,
		`POST /api/awake {"minutes":60,"on":true}`,
		`POST /api/awake {"on":false}`,
		`PATCH /api/awake {"allow_display_sleep":true}`,
		`POST /api/autostart {"enabled":false}`,
		`POST /api/usage/refresh`,
	}
	got := f.posted()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	c.Click(ctx, "quit")
	if got := f.posted(); got[len(got)-1] != "POST /api/quit" || !ui.quit {
		t.Fatalf("quit: %v %v", got[len(got)-1], ui.quit)
	}
}

func TestAStoppedSwitcherIsStartedOnce(t *testing.T) {
	ui := &recordingUI{}
	starts := 0
	c := New(Options{Client: &Client{Base: "http://127.0.0.1:1"}, Start: func() error { starts++; return errors.New("could not start") }}, ui)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c.Refresh(ctx)
	c.Refresh(ctx)
	if starts != 1 {
		t.Fatalf("started %d times", starts)
	}
	if find(ui.last(), "Start Switcher") == nil || find(ui.last(), "could not start") == nil {
		t.Fatalf("menu while stopped:\n%s", titles(ui.last()))
	}
}

func TestResetAlertsArriveOnceWhenTheResetTimePasses(t *testing.T) {
	f, ui, c, now := newFixture(t)
	reset := now.Add(10 * time.Minute).Unix()
	accounts := baseState()["accounts"].([]map[string]any)
	accounts[0]["usage"] = map[string]any{"windows": []map[string]any{{"label": "Session", "used_percent": 100, "resets_at": reset}}}
	f.set("accounts", accounts)
	f.set("reset_notifications", true)
	ctx := context.Background()
	c.Refresh(ctx)
	if len(ui.notified) != 0 {
		t.Fatal("alerted early")
	}
	*now = now.Add(11 * time.Minute)
	c.Refresh(ctx)
	c.Refresh(ctx)
	if len(ui.notified) != 1 || !strings.Contains(ui.notified[0], "Claude usage window") || !strings.Contains(ui.notified[0], "Session") {
		t.Fatalf("alerts: %v", ui.notified)
	}
	// Turned off: no more alerts.
	f.set("reset_notifications", false)
	accounts[0]["usage"] = map[string]any{"windows": []map[string]any{{"label": "Session", "resets_at": now.Add(time.Minute).Unix()}}}
	f.set("accounts", accounts)
	c.Refresh(ctx)
	*now = now.Add(2 * time.Minute)
	c.Refresh(ctx)
	if len(ui.notified) != 1 {
		t.Fatalf("alerted while off: %v", ui.notified)
	}
}

func TestAnUpdatedServerRestartsTheTray(t *testing.T) {
	f, _, c, _ := newFixture(t)
	updated := 0
	c.opt.Updated = func() { updated++ }
	c.Refresh(context.Background())
	f.set("version", "1.21.0")
	c.Refresh(context.Background())
	c.Refresh(context.Background())
	if updated != 1 {
		t.Fatalf("restarted %d times", updated)
	}
}

func TestKeepAwakeTitleSaysUntilWhen(t *testing.T) {
	now := time.Date(2026, 11, 1, 14, 0, 0, 0, time.Local)
	item := awakeMenu(Awake{Supported: true, Active: true, Until: now.Add(90 * time.Minute).Unix()}, now)
	if item.Title != "Keep awake: until 15:30 (1 h 30 min left)" {
		t.Fatalf("title: %q", item.Title)
	}
	if item.Children[0].Checked {
		t.Fatal("Off checked while awake")
	}
	if got := awakeMenu(Awake{Supported: true, Active: true}, now).Title; got != "Keep awake: until turned off" {
		t.Fatalf("open-ended: %q", got)
	}
}

// A computer using its host offers to update the host's Switcher, after a
// second click, and asks its own server to have the host install it.
func TestTheHostIsUpdatedFromHere(t *testing.T) {
	f, ui, c, _ := newFixture(t)
	f.set("remote", map[string]any{"role": "client", "host": "Studio", "connected": true,
		"host_update": map[string]any{"current": "1.21.0", "latest": "v1.21.0", "update_available": true, "remote_update": true}})
	ctx := context.Background()
	c.Refresh(ctx)
	item := find(ui.last(), "Update Studio to 1.21.0")
	if item == nil || item.Action != "" || find(item.Children, "Update now").Action != "host-update" {
		t.Fatalf("host update: %+v\n%s", item, titles(ui.last()))
	}
	c.Click(ctx, "host-update")
	if got := f.posted(); len(got) == 0 || got[len(got)-1] != `POST /api/remote/host-update {"action":"install"}` {
		t.Fatalf("requests: %v", got)
	}
	f.set("remote", map[string]any{"role": "client", "host": "Studio", "connected": true,
		"host_update": map[string]any{"current": "1.21.0", "latest": "v1.21.0", "update_available": true, "remote_update": true, "install": map[string]any{"step": "updating"}}})
	c.Refresh(ctx)
	if find(ui.last(), "Updating Studio to 1.21.0") == nil || find(ui.last(), "Update now") != nil {
		t.Fatalf("while updating:\n%s", titles(ui.last()))
	}
	// A host too old to take updates from here is not offered one.
	f.set("remote", map[string]any{"role": "client", "host": "Studio", "connected": true,
		"host_update": map[string]any{"current": "1.19.0", "latest": "v1.21.0", "update_available": true, "remote_update": false}})
	c.Refresh(ctx)
	if find(ui.last(), "Update Studio") != nil {
		t.Fatalf("older host:\n%s", titles(ui.last()))
	}
}

// The menu says what is left, rounded and with the reset countdown, the way
// the Mac menu and the phone page do, so the three never disagree.
func TestUsageReadsAsWhatIsLeftWithItsReset(t *testing.T) {
	f, ui, c, now := newFixture(t)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }
	accounts := baseState()["accounts"].([]map[string]any)
	accounts[0]["usage"] = map[string]any{"windows": []map[string]any{
		{"label": "Session", "used_percent": 34, "resets_at": at(2*time.Hour + 20*time.Minute + 30*time.Second)},
		{"label": "Weekly", "used_percent": 69, "resets_at": at(56*time.Hour + 10*time.Minute)},
		{"label": "Fable", "used_percent": 92.4, "resets_at": at(30 * time.Second)},
		{"label": "Opus", "used_percent": 0, "resets_at": at(-time.Minute)},
	}}
	accounts[0]["reset_credits"] = map[string]any{"count": 2}
	accounts[1]["exhausted_until"] = at(time.Hour)
	accounts[1]["health"] = map[string]any{"condition": "needs_relogin"}
	accounts[1]["usage"] = map[string]any{"windows": []map[string]any{{"label": "Weekly", "used_percent": 100}, {"label": "Session", "used_percent": 3}}}
	f.set("accounts", accounts)
	c.Refresh(context.Background())
	want := strings.Join([]string{
		"work@example.com · Max 20x · ⚡ 2 banked",
		"    Session: ▰▰▰▱▱ 66% left · ↻ 2h 20m",
		"    Weekly: ▰▰▱▱▱ 31% left · ↻ 2d 8h",
		"    Fable: ▰▱▱▱▱ 8% left · ↻ 1m",
		"    Opus: ▰▰▰▰▰ 100% left",
		"home@example.com · Max 5x",
		"    Out of usage · back in 1h 0m",
		"    Sign in again",
		"    Weekly: ▱▱▱▱▱ 0% left",
		"    Session: ▰▰▰▰▱ 97% left",
		"—",
		"Codex",
		"work@example.com · Pro 5x",
		"    No usage reported yet",
	}, "\n")
	if text := titles(ui.last()); !strings.Contains(text, want) {
		t.Fatalf("menu:\n%s\nwant:\n%s", text, want)
	}

	// Without usage bars in the dashboard's settings, the menu has none.
	f.set("menu_usage_bars", false)
	c.Refresh(context.Background())
	if find(ui.last(), "    Weekly: 31% left · ↻ 2d 8h") == nil {
		t.Fatalf("without bars:\n%s", titles(ui.last()))
	}
}

func TestProvidersMissingFromTheOrderComeLast(t *testing.T) {
	st := State{Order: []string{"codex"}, Accounts: []Account{
		{ID: "o", Provider: "opencode", Email: "o@example.com"},
		{ID: "x", Provider: "codex", Email: "x@example.com"},
	}}
	text := titles(Build(st, Status{Running: true}, time.Unix(0, 0)))
	if strings.Index(text, "Codex") > strings.Index(text, "OpenCode") || !strings.Contains(text, "o@example.com") {
		t.Fatalf("menu:\n%s", text)
	}
}

// A countdown ticking or an account switching changes the menu in place;
// only new or removed lines rebuild it.
func TestMenuShapeDecidesBetweenInPlaceAndRebuild(t *testing.T) {
	st := State{Accounts: []Account{{ID: "a", Provider: "codex", Email: "a@example.com", Active: true}}}
	now := time.Unix(1_800_000_000, 0)
	st.Accounts[0].Usage = &struct {
		Windows []Window `json:"windows"`
	}{Windows: []Window{{Label: "Weekly", UsedPercent: 10, ResetsAt: float64(now.Add(3 * time.Hour).Unix())}}}
	before := Build(st, Status{Running: true}, now)
	later := Build(st, Status{Running: true}, now.Add(time.Minute))
	if titles(before) == titles(later) || !sameShape(before, later) {
		t.Fatal("a ticking countdown must change the menu in place")
	}
	st.Accounts[0].Active = false
	if switched := Build(st, Status{Running: true}, now); !sameShape(before, switched) {
		t.Fatal("switching accounts must change the menu in place")
	}
	st.Accounts = append(st.Accounts, Account{ID: "b", Provider: "codex", Email: "b@example.com"})
	if sameShape(before, Build(st, Status{Running: true}, now)) {
		t.Fatal("a new account must rebuild the menu")
	}
}

// run runs the controller until the test ends.
func run(t *testing.T, c *Controller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func switchClaudeToHome(f *fakeServer) {
	accounts := baseState()["accounts"].([]map[string]any)
	accounts[0]["native_active"], accounts[1]["native_active"] = false, true
	f.set("accounts", accounts)
}

func homeChecked(ui *recordingUI) bool {
	home := find(ui.last(), "home@example.com")
	return home != nil && home.Checked
}

// An account switched from another computer shows at once: the tray waits
// for the server to report a change rather than reading on a timer, and a
// wait that ends without one neither reads the state nor redraws the menu.
func TestTheMenuFollowsChangesAsTheyHappen(t *testing.T) {
	f, ui, c, _ := newFixture(t)
	f.mu.Lock()
	f.feed, f.hold = true, 40*time.Millisecond
	f.mu.Unlock()
	c.opt.Interval = time.Hour // no timer could show the switch in time
	run(t, c)
	waitFor(t, "the first waits", func() bool { _, waits := f.counts(); return waits >= 3 })
	reads, _ := f.counts()
	renders := ui.count()
	time.Sleep(200 * time.Millisecond)
	if again, waits := f.counts(); again != reads || ui.count() != renders || waits < 5 {
		t.Fatalf("without changes: %d reads (was %d), %d renders (was %d), %d waits", again, reads, ui.count(), renders, waits)
	}
	switchClaudeToHome(f)
	waitFor(t, "the switch to show", func() bool { return homeChecked(ui) })
	if find(ui.last(), "work@example.com · Max 20x").Checked {
		t.Fatal("the old account is still checked")
	}
}

// A server from before the changes feed answers 404; the tray then reads
// the state every Interval, still redrawing only what changed.
func TestAServerWithoutChangesIsReadEveryFewSeconds(t *testing.T) {
	f, ui, c, _ := newFixture(t)
	c.opt.Interval = 10 * time.Millisecond
	run(t, c)
	waitFor(t, "a few reads", func() bool { reads, _ := f.counts(); return reads >= 4 })
	if ui.count() != 1 {
		t.Fatalf("an unchanged state was drawn %d times", ui.count())
	}
	switchClaudeToHome(f)
	waitFor(t, "the switch to show", func() bool { return homeChecked(ui) })
	// It asks again after each round of reads, in case an update taught it.
	if reads, waits := f.counts(); waits < 1 || waits > reads/pollRounds+1 {
		t.Fatalf("asked for changes %d times in %d reads", waits, reads)
	}
}

func TestEveryProviderHasAMenuLogo(t *testing.T) {
	for provider := range providerNames {
		img, err := png.Decode(bytes.NewReader(logo(provider)))
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
			t.Fatalf("%s: %v", provider, b)
		}
	}
	if logo("unknown") != nil {
		t.Fatal("an unknown provider has a logo")
	}
}

func TestThePanelOpensInTheFirstAppBrowserFound(t *testing.T) {
	const url = "http://127.0.0.1:8787/panel"
	installed := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			if slices.Contains(names, name) {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		}
	}
	app := " --app=" + url + " --window-size=420,820"
	for _, tc := range []struct {
		installed []string
		want      string
	}{
		{[]string{"microsoft-edge", "chromium", "google-chrome"}, "/usr/bin/google-chrome" + app},
		{[]string{"microsoft-edge", "chromium-browser", "chromium"}, "/usr/bin/chromium" + app},
		{[]string{"microsoft-edge", "chromium-browser"}, "/usr/bin/chromium-browser" + app},
		{[]string{"firefox", "microsoft-edge"}, "/usr/bin/microsoft-edge" + app},
		// Without one, the default browser opens it as a page.
		{[]string{"firefox"}, "xdg-open " + url},
		{nil, "xdg-open " + url},
	} {
		if got := strings.Join(PanelCommand(url, installed(tc.installed...)), " "); got != tc.want {
			t.Errorf("with %v: %s, want %s", tc.installed, got, tc.want)
		}
	}
}

// Open Switcher… heads the menu and opens the server's panel; a stopped
// server starts first, and a browser that cannot open says so.
func TestOpenSwitcherShowsThePanel(t *testing.T) {
	_, ui, c, _ := newFixture(t)
	var opened []string
	var refuse error
	c.opt.Panel = func(url string) error { opened = append(opened, url); return refuse }
	ctx := context.Background()
	c.Refresh(ctx)
	if first := ui.last()[0]; first.Title != "Open Switcher…" || first.Action != "panel" || first.Icon != "switcher" {
		t.Fatalf("first item: %+v", first)
	}
	c.Click(ctx, "panel")
	if len(opened) != 1 || opened[0] != c.opt.Client.Base+"/panel" {
		t.Fatalf("opened %v", opened)
	}
	refuse = errors.New("no browser found to open Switcher in")
	c.Click(ctx, "panel")
	if find(ui.last(), "no browser found to open Switcher in") == nil {
		t.Fatalf("the failure is not shown:\n%s", titles(ui.last()))
	}

	starts := 0
	stopped := New(Options{Client: &Client{Base: "http://127.0.0.1:1"}, Start: func() error { starts++; return errors.New("could not start") },
		Panel: func(url string) error { opened = append(opened, url); return nil }}, &recordingUI{})
	stopped.Click(ctx, "panel")
	if starts != 1 || len(opened) != 2 {
		t.Fatalf("stopped server: %d starts, opened %v", starts, opened)
	}
}
