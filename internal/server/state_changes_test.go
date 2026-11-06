package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/remote"
	"switcher/internal/settings"
	"switcher/internal/store"
)

// fakeState is a state the test changes; the watcher reads it like /api/state.
type fakeState struct {
	mu    sync.Mutex
	value string
	reads []bool // the read flag of every look
}

func (f *fakeState) set(v string) {
	f.mu.Lock()
	f.value = v
	f.mu.Unlock()
}

func (f *fakeState) render(_ context.Context, read bool) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, read)
	return []byte(f.value)
}

func (f *fakeState) looks() (all, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, read := range f.reads {
		if read {
			reads++
		}
	}
	return len(f.reads), reads
}

// here is a waiter on this computer, such as its menu bar or dashboard.
var here = waiter{local: true}

func quickWatch(f *fakeState) *stateWatch {
	s := newStateWatch(f.render, nil, nil)
	s.hostEvery, s.clientEvery, s.linger = 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	return s
}

// stopped waits until the watcher has stopped for want of waiters. It pokes
// the watcher, which may sleep for a long time in a test, so it looks again
// and sees that nobody waits.
func stopped(t *testing.T, s *stateWatch) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.isRunning() {
		if time.Now().After(deadline) {
			t.Fatal("the watcher kept running without waiters")
		}
		s.poke()
		time.Sleep(5 * time.Millisecond)
	}
}

type waitResult struct {
	etag    string
	changed bool
	err     error
}

func waitAsync(s *stateWatch, ctx context.Context, since string, timeout time.Duration) chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		etag, changed, err := s.wait(ctx, here, since, timeout)
		out <- waitResult{etag, changed, err}
	}()
	return out
}

func TestStateChangesAnswerAtOnceForAnEmptyOrStaleTag(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	defer stopped(t, s)
	for _, since := range []string{"", "a tag from before a restart"} {
		started := time.Now()
		etag, changed, err := s.wait(context.Background(), here, since, time.Minute)
		if err != nil || !changed || etag != stateTag([]byte("one")) || time.Since(started) > time.Second {
			t.Fatalf("since %q: %q %v %v after %s", since, etag, changed, err, time.Since(started))
		}
	}
}

func TestStateChangesWaitForAChange(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	result := waitAsync(s, context.Background(), etag, time.Minute)
	select {
	case r := <-result:
		t.Fatalf("answered without a change: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	f.set("two")
	select {
	case r := <-result:
		if r.err != nil || !r.changed || r.etag != stateTag([]byte("two")) {
			t.Fatalf("after the change: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the change was not answered")
	}
}

func TestStateChangesTimeOutUnchangedWithTheSameTag(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	started := time.Now()
	again, changed, err := s.wait(context.Background(), here, etag, 80*time.Millisecond)
	if err != nil || changed || again != etag || time.Since(started) < 80*time.Millisecond {
		t.Fatalf("timeout: %q %v %v after %s", again, changed, err, time.Since(started))
	}
}

func TestStateChangesEndWithTheRequest(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	result := waitAsync(s, ctx, etag, time.Minute)
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case r := <-result:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("cancelled wait: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled wait kept waiting")
	}
}

func TestStateChangesWakeEveryWaiterAndAreBounded(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	s.maxWaiters = 40
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	var results []chan waitResult
	for range s.maxWaiters {
		results = append(results, waitAsync(s, context.Background(), etag, time.Minute))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		waiting := s.waiters
		s.mu.Unlock()
		if waiting == s.maxWaiters {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d waiters arrived", waiting, s.maxWaiters)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, err := s.wait(context.Background(), here, etag, time.Minute); !errors.Is(err, errTooManyWaiters) {
		t.Fatalf("one waiter too many: %v", err)
	}
	f.set("two")
	for i, result := range results {
		select {
		case r := <-result:
			if r.err != nil || !r.changed || r.etag != stateTag([]byte("two")) {
				t.Fatalf("waiter %d: %+v", i, r)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d was not woken", i)
		}
	}
}

// waitersOf starts n waits by who and returns once they all wait.
func waitersOf(t *testing.T, s *stateWatch, ctx context.Context, who waiter, etag string, n int) []chan waitResult {
	t.Helper()
	s.mu.Lock()
	want := s.waiters + n
	s.mu.Unlock()
	var results []chan waitResult
	for range n {
		results = append(results, waitAsyncAs(s, ctx, who, etag, time.Minute))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		waiting := s.waiters
		s.mu.Unlock()
		if waiting == want {
			return results
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d waiters, want %d", waiting, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitAsyncAs(s *stateWatch, ctx context.Context, who waiter, since string, timeout time.Duration) chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		etag, changed, err := s.wait(ctx, who, since, timeout)
		out <- waitResult{etag, changed, err}
	}()
	return out
}

// One paired device, perhaps another person's Mac, cannot take the waits of
// this computer's own screens or of other devices.
func TestEachCallerKeepsItsShareOfTheWaits(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	s.maxWaiters, s.perDevice, s.localReserve = 8, 2, 3
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	refused := func(who waiter) bool {
		t.Helper()
		_, _, err := s.wait(context.Background(), who, etag, time.Millisecond)
		return errors.Is(err, errTooManyWaiters)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mac := waiter{device: "device-a"}
	first := waitersOf(t, s, ctx, mac, etag, 2)
	if !refused(mac) {
		t.Fatal("a paired device held more than its share")
	}
	if refused(here) || refused(waiter{device: "device-b"}) {
		t.Fatal("a device at its share kept this computer or another device out")
	}
	// Devices and other callers from elsewhere leave the share kept here.
	all := append(first, waitersOf(t, s, ctx, waiter{device: "device-b"}, etag, 2)...)
	all = append(all, waitersOf(t, s, ctx, waiter{}, etag, 1)...) // a browser on the LAN listener
	if !refused(waiter{device: "device-c"}) || !refused(waiter{}) {
		t.Fatal("callers from elsewhere took the share kept for this computer")
	}
	all = append(all, waitersOf(t, s, ctx, here, etag, 3)...)
	if !refused(here) {
		t.Fatal("the overall bound no longer holds")
	}
	// A wait that ends gives its slot back.
	cancel()
	for _, result := range all {
		<-result
	}
	solo, stop := context.WithCancel(context.Background())
	again := waitAsyncAs(s, solo, mac, etag, time.Minute)
	time.Sleep(30 * time.Millisecond)
	select {
	case r := <-again:
		t.Fatalf("a device whose waits ended was refused: %+v", r)
	default:
	}
	stop()
	if r := <-again; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the device's new wait: %+v", r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.away != 0 || len(s.devices) != 0 {
		t.Fatalf("slots left after every wait ended: %d away, devices %v", s.away, s.devices)
	}
}

func TestStateWatcherStopsAndLeavesNoGoroutines(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	var results []chan waitResult
	for range 10 {
		results = append(results, waitAsync(s, ctx, "", time.Minute))
	}
	for _, result := range results {
		<-result
	}
	for range 10 {
		results = append(results, waitAsync(s, ctx, stateTag([]byte("one")), time.Minute))
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	for _, result := range results[10:] {
		if r := <-result; !errors.Is(r.err, context.Canceled) {
			t.Fatalf("cancelled wait: %+v", r)
		}
	}
	stopped(t, s)
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A screen that comes back later starts the watcher again, with a fresh look.
	f.set("two")
	if etag, changed, err := s.wait(context.Background(), here, stateTag([]byte("one")), time.Minute); err != nil || !changed || etag != stateTag([]byte("two")) {
		t.Fatalf("after a restart: %q %v %v", etag, changed, err)
	}
	stopped(t, s)
}

func TestStateWatcherLooksAgainAfterAChangeMadeHere(t *testing.T) {
	f := &fakeState{value: "one"}
	s := quickWatch(f)
	s.hostEvery = time.Hour // only a poke can make it look again
	defer stopped(t, s)
	etag, _, _ := s.wait(context.Background(), here, "", time.Minute)
	result := waitAsync(s, context.Background(), etag, time.Minute)
	f.set("two")
	select {
	case r := <-result:
		t.Fatalf("looked again without a poke: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	s.poke()
	select {
	case r := <-result:
		if !r.changed || r.etag != stateTag([]byte("two")) {
			t.Fatalf("after the poke: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("a poke did not make the watcher look again")
	}
}

// A state that fails to build ends one look, not the server: the waiter
// times out unchanged and the next look that works answers.
func TestABrokenStateEndsOnlyOneLook(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	f := &fakeState{value: "one"}
	s := newStateWatch(func(ctx context.Context, read bool) []byte {
		if broken.Load() {
			panic("fixture state failure")
		}
		return f.render(ctx, read)
	}, nil, nil)
	s.hostEvery, s.linger = 10*time.Millisecond, 20*time.Millisecond
	defer stopped(t, s)
	if etag, changed, err := s.wait(context.Background(), here, "", 50*time.Millisecond); err != nil || changed || etag != "" {
		t.Fatalf("while broken: %q %v %v", etag, changed, err)
	}
	result := waitAsync(s, context.Background(), "", time.Minute)
	broken.Store(false)
	select {
	case r := <-result:
		if !r.changed || r.etag != stateTag([]byte("one")) {
			t.Fatalf("once it works: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not recover")
	}
}

// fakeHost stands in for a paired host's own changes route.
type fakeHost struct {
	answers chan func() (string, bool, error)
	sinces  chan string
}

func (h *fakeHost) wait(ctx context.Context, since string) (string, bool, error) {
	h.sinces <- since
	select {
	case answer := <-h.answers:
		return answer()
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

func TestAClientReadsItsHostOnlyWhenTheHostChanged(t *testing.T) {
	f := &fakeState{value: "one"}
	host := &fakeHost{answers: make(chan func() (string, bool, error)), sinces: make(chan string, 16)}
	s := newStateWatch(f.render, func() bool { return true }, host.wait)
	s.hostEvery, s.clientEvery, s.linger = time.Hour, 10*time.Millisecond, 20*time.Millisecond
	s.oldHostRetry = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		stopped(t, s)
	}()
	// A screen waits on the state, which does not change here, all along.
	go func() { _, _, _ = s.wait(ctx, here, stateTag([]byte("one")), time.Hour) }()
	answer := func(etag string, changed bool, err error) {
		t.Helper()
		select {
		case host.answers <- func() (string, bool, error) { return etag, changed, err }:
		case <-time.After(2 * time.Second):
			t.Fatal("the client did not wait on its host")
		}
	}
	nextSince := func(want string) {
		t.Helper()
		select {
		case since := <-host.sinces:
			if since != want {
				t.Fatalf("waited since %q, want %q", since, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the client did not wait on its host since %q", want)
		}
	}
	nextSince("")
	// Until the host answers a wait, the client reads its state on every look.
	time.Sleep(50 * time.Millisecond)
	if all, reads := f.looks(); reads < 2 || reads != all {
		t.Fatalf("before the host answered: %d looks, %d reads", all, reads)
	}
	answer("h1", true, nil)
	nextSince("h1")
	// The host answers waits: only this Mac's own settings are compared.
	time.Sleep(30 * time.Millisecond)
	all0, reads0 := f.looks()
	time.Sleep(100 * time.Millisecond)
	all1, reads1 := f.looks()
	if all1-all0 < 2 || reads1 != reads0 {
		t.Fatalf("while the host is quiet: %d looks, %d reads", all1-all0, reads1-reads0)
	}
	answer("h2", true, nil)
	nextSince("h2")
	time.Sleep(30 * time.Millisecond)
	if _, reads := f.looks(); reads != reads1+1 {
		t.Fatalf("after the host changed: %d reads, want %d", reads, reads1+1)
	}
	// An older host cannot wait; its state is read on every look again.
	answer("", false, remote.ErrHostCannotWait)
	time.Sleep(60 * time.Millisecond)
	all2, reads2 := f.looks()
	time.Sleep(100 * time.Millisecond)
	all3, reads3 := f.looks()
	if all3-all2 < 2 || reads3-reads2 != all3-all2 {
		t.Fatalf("with an older host: %d looks, %d reads", all3-all2, reads3-reads2)
	}
}

// liveProvider answers usage at once, without an upstream.
type liveProvider struct{ *recheckProvider }

func (liveProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{Available: true}, nil
}

// liveAPI is a server with two fake accounts, the first one active.
func liveAPI(t *testing.T) *API {
	t.Helper()
	st := store.New(t.TempDir())
	for _, id := range []string{"fake-a", "fake-b"} {
		if err := st.Save(store.Account{ID: id, Provider: "fake", Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	p := liveProvider{&recheckProvider{}}
	m, err := proxy.New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("fake-a"); err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, Proxy: m, Providers: map[string]provider.Provider{"fake": p}, Settings: settings.New(t.TempDir())}
	a.watch().linger = 20 * time.Millisecond
	t.Cleanup(func() { stopped(t, a.watch()) })
	return a
}

// getChanges asks a changes route; it may run off the test's goroutine, so
// a failure is an answer of status 0.
func getChanges(client *http.Client, url string) (int, map[string]any) {
	res, err := client.Get(url)
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestASwitchShowsAtOnceOnTheChangesRoute(t *testing.T) {
	a := liveAPI(t)
	a.watch().hostEvery = time.Hour // only the switch itself can wake the waiter
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	status, first := getChanges(srv.Client(), srv.URL+"/api/state/changes")
	etag, _ := first["etag"].(string)
	if status != http.StatusOK || first["changed"] != true || len(etag) != 32 {
		t.Fatalf("first answer: %d %v", status, first)
	}
	type answer struct {
		status int
		body   map[string]any
		after  time.Duration
	}
	waiting := make(chan answer, 1)
	started := time.Now()
	go func() {
		status, body := getChanges(srv.Client(), srv.URL+"/api/state/changes?since="+etag)
		waiting <- answer{status, body, time.Since(started)}
	}()
	time.Sleep(100 * time.Millisecond)
	res, err := srv.Client().Post(srv.URL+"/api/accounts/fake-b/activate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("switch: %d", res.StatusCode)
	}
	select {
	case got := <-waiting:
		if got.status != http.StatusOK || got.body["changed"] != true || got.body["etag"] == etag {
			t.Fatalf("after the switch: %d %v", got.status, got.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiting screen did not see the switch")
	}
	var state struct {
		Active map[string]string `json:"active"`
	}
	res, err = srv.Client().Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if json.NewDecoder(res.Body).Decode(&state) != nil || state.Active["fake"] != "fake-b" {
		t.Fatalf("state after the switch: %+v", state)
	}
}

func TestTheChangesTagLeavesOutSecretsAndRelayActivity(t *testing.T) {
	a := liveAPI(t)
	a.ManagementKey = "management-secret"
	local := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	local.RemoteAddr, local.Host = "127.0.0.1:50000", "127.0.0.1:8787"
	if _, state := a.stateView(local); state.(map[string]any)["hub_management_key"] != "management-secret" {
		t.Fatal("a local browser no longer receives the management key")
	}
	body := string(a.watchedState(context.Background(), true))
	if strings.Contains(body, "management-secret") || !strings.HasPrefix(body, "200{") {
		t.Fatalf("the watcher compares the management key: %s", body)
	}
	busy := leaveOutActivity(map[string]any{"desktop_relay": desktoprelay.Status{Listening: true, InFlight: 3, OpenRequests: 5}})
	quiet := map[string]any{"desktop_relay": desktoprelay.Status{Listening: true}}
	if b, q := mustJSON(t, busy), mustJSON(t, quiet); b != q {
		t.Fatalf("replies in flight change the tag: %s against %s", b, q)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The changes route is guarded exactly like /api/state: the local-origin
// checks first, then the optional login, with a device token for the menu
// bar app.
func TestTheChangesRouteNeedsWhatTheStateNeeds(t *testing.T) {
	a := liveAPI(t)
	if err := a.Settings.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	token, err := a.Settings.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	gate := &AuthGate{Store: a.Settings}
	handler := ControlRequests(LocalOnlyWith(LocalOptions{Port: 8787}, gate.Wrap(mux)))
	request := func(host, auth, site string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/state/changes?since=", nil)
		r.Host, r.RemoteAddr = host, "127.0.0.1:50000"
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name, host, auth, site string
		want                   int
	}{
		{"no login", "127.0.0.1:8787", "", "", http.StatusUnauthorized},
		{"wrong token", "127.0.0.1:8787", "not-the-token", "", http.StatusUnauthorized},
		{"rebinding", "evil.example:8787", token, "", http.StatusForbidden},
		{"another site", "127.0.0.1:8787", token, "cross-site", http.StatusForbidden},
		{"device token", "127.0.0.1:8787", token, "", http.StatusOK},
	} {
		w := request(tc.host, tc.auth, tc.site)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d: %s", tc.name, w.Code, tc.want, w.Body)
		}
		if tc.want == http.StatusOK && !strings.Contains(w.Body.String(), `"changed":true`) {
			t.Errorf("%s: %s", tc.name, w.Body)
		}
	}
}

// The control deadline also ends a request's context once it passes, which
// would cut a wait for changes off long before its 25 seconds.
func TestTheControlDeadlineLeavesRoomForTheWait(t *testing.T) {
	waited := make(chan error, 2)
	srv := httptest.NewServer(controlRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			waited <- fmt.Errorf("%s ended after the control deadline", r.URL.Path)
			return
		case <-time.After(300 * time.Millisecond):
			waited <- nil
		}
		_, _ = io.WriteString(w, "waited")
	}), 50*time.Millisecond))
	defer srv.Close()
	get := func(path string) {
		t.Helper()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, srv.Listener.Addr())
		if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
			resp.Body.Close()
		}
	}
	get("/api/state/changes?since=x")
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	// Any other control request keeps the short deadline.
	get("/api/state")
	if err := <-waited; err == nil {
		t.Fatal("the control deadline no longer bounds other requests")
	}
}

// A paired device reaches the host's waits through the host listener, which
// names the device: at its share it is refused, while this computer's own
// screens still get theirs, and a wait that ends frees its slot.
func TestAPairedDeviceAtItsShareLeavesTheHostItsWaits(t *testing.T) {
	hostAPI := liveAPI(t)
	hostMux := http.NewServeMux()
	hostAPI.Register(hostMux)
	host, err := remote.NewHost(t.TempDir(), 0, hostMux)
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
	t.Cleanup(func() { _ = client.Disconnect() })
	hostSrv := httptest.NewServer(hostMux)
	defer hostSrv.Close()

	etag, _, err := client.WaitHostChange(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var held []chan error
	for range stateWatchPerDevice {
		done := make(chan error, 1)
		go func() { _, _, err := client.WaitHostChange(ctx, etag); done <- err }()
		held = append(held, done)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		hostAPI.watch().mu.Lock()
		holding := hostAPI.watch().devices
		count := 0
		for _, n := range holding {
			count += n
		}
		hostAPI.watch().mu.Unlock()
		if count == stateWatchPerDevice {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the device holds %d waits, want %d", count, stateWatchPerDevice)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, err := client.WaitHostChange(context.Background(), etag); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a device past its share: %v", err)
	}
	if status, body := getChanges(hostSrv.Client(), hostSrv.URL+"/api/state/changes"); status != http.StatusOK || body["changed"] != true {
		t.Fatalf("this computer's own screen was refused: %d %v", status, body)
	}
	cancel()
	for _, done := range held {
		<-done
	}
	again, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	if _, _, err := client.WaitHostChange(again, etag); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("after its waits ended, the device's next wait: %v", err)
	}
}

// A paired client's dashboard and menu see a switch made on its host at once,
// and the host's own screens see a switch made on the client at once.
func TestASwitchOnEitherSideShowsOnTheOther(t *testing.T) {
	hostAPI := liveAPI(t)
	hostMux := http.NewServeMux()
	hostAPI.Register(hostMux)
	host, err := remote.NewHost(t.TempDir(), 0, hostMux)
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
	clientAPI := &API{Store: store.New(t.TempDir()), Settings: settings.New(t.TempDir()), RemoteClient: client}
	clientAPI.watch().linger = 20 * time.Millisecond
	clientAPI.watch().clientEvery = time.Hour // only the host's own wait can wake the client
	// Cleanups run last first: the watchers stop, then the pairing is
	// forgotten, so no address refresh writes into a removed directory.
	t.Cleanup(func() { _ = client.Disconnect() })
	t.Cleanup(func() { stopped(t, clientAPI.watch()) })
	clientMux := http.NewServeMux()
	clientAPI.Register(clientMux)
	clientSrv := httptest.NewServer(client.Middleware(clientAPI.LocalStateFields, clientMux))
	defer clientSrv.Close()
	hostSrv := httptest.NewServer(hostMux)
	defer hostSrv.Close()

	since := func(srv *httptest.Server) string {
		t.Helper()
		status, body := getChanges(srv.Client(), srv.URL+"/api/state/changes")
		etag, _ := body["etag"].(string)
		if status != http.StatusOK || etag == "" {
			t.Fatalf("first answer: %d %v", status, body)
		}
		return etag
	}
	waitOn := func(srv *httptest.Server, etag string) chan map[string]any {
		out := make(chan map[string]any, 1)
		go func() {
			_, body := getChanges(srv.Client(), srv.URL+"/api/state/changes?since="+etag)
			out <- body
		}()
		return out
	}
	switchOn := func(srv *httptest.Server, id string) {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/accounts/"+id+"/activate", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("switch to %s: %d", id, res.StatusCode)
		}
	}
	seen := func(what string, wait chan map[string]any) {
		t.Helper()
		select {
		case body := <-wait:
			if body["changed"] != true {
				t.Fatalf("%s: %v", what, body)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not show", what)
		}
	}

	// The client's switch reaches the host, whose waiting screens see it.
	hostWait := waitOn(hostSrv, since(hostSrv))
	clientEtag := since(clientSrv)
	time.Sleep(100 * time.Millisecond)
	clientWait := waitOn(clientSrv, clientEtag)
	time.Sleep(100 * time.Millisecond)
	switchOn(clientSrv, "fake-b")
	seen("the client's switch on the host", hostWait)
	seen("the client's switch on the client", clientWait)

	// A switch made on the host shows on the client, through the host's wait.
	clientWait = waitOn(clientSrv, since(clientSrv))
	time.Sleep(100 * time.Millisecond)
	switchOn(hostSrv, "fake-a")
	seen("the host's switch on the client", clientWait)
}
