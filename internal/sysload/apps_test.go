package sysload

import (
	"context"
	"testing"
	"time"
)

func TestAppNames(t *testing.T) {
	for path, want := range map[string]string{
		"/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Versions/1/Helpers/Google Chrome Helper (Renderer).app/Contents/MacOS/Google Chrome Helper (Renderer)": "Google Chrome",
		"/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder": "Finder",
		"/usr/bin/python3.12":             "python3.12",
		"/opt/thing/bin/worker (deleted)": "worker",
		"kernel_task":                     "kernel_task",
		"/Applications/Slack.app":         "Slack",
	} {
		if got := appName(path); got != want {
			t.Errorf("appName(%q) = %q, want %q", path, got, want)
		}
	}
	if friendlyName("python3.12") != "Python" || friendlyName("claude") != "Claude Code" || friendlyName("mystery") != "mystery" {
		t.Fatal("friendly names")
	}
}

func TestParsePS(t *testing.T) {
	out := []byte("    1   0:12.50  20480 /sbin/launchd\n" +
		"  501   1:02:03.00 102400 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome\n" +
		"  502   2-00:00:01.00 1024 /Applications/Google Chrome.app/Contents/Frameworks/X.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper\n" +
		"  bad line\n")
	snap := parsePS(out)
	if len(snap.procs) != 3 || snap.gpuOK {
		t.Fatalf("%+v", snap)
	}
	if p := snap.procs[0]; p.name != "launchd" || p.cpu != 12500*time.Millisecond || p.mem != 20480<<10 {
		t.Fatalf("launchd: %+v", p)
	}
	if p := snap.procs[1]; p.name != "Google Chrome" || p.cpu != time.Hour+2*time.Minute+3*time.Second {
		t.Fatalf("chrome: %+v", p)
	}
	if p := snap.procs[2]; p.name != "Google Chrome" || p.cpu != 48*time.Hour+time.Second {
		t.Fatalf("helper: %+v", p)
	}
	for _, bad := range []string{"", "1:2:3:4", "x:10", "1-x:00.0"} {
		if _, ok := psTime(bad); ok {
			t.Errorf("psTime(%q) parsed", bad)
		}
	}
}

// Shares come from the difference between two listings, grouped by app; a
// reused process id starts over, and graphics busy time is a share of the
// time between them.
func TestBreakdown(t *testing.T) {
	before := procSnapshot{gpuOK: true, procs: []procInfo{
		{pid: 1, start: 10, name: "Chrome", cpu: time.Second, mem: 100},
		{pid: 2, start: 10, name: "Chrome", cpu: time.Second, mem: 100},
		{pid: 3, start: 10, name: "old", cpu: 50 * time.Second},
	}, gpu: []gpuClient{{key: "d/1", pid: 1, busy: map[string]time.Duration{"gfx": 0, "compute": 0}}}}
	after := procSnapshot{gpuOK: true, procs: []procInfo{
		{pid: 1, start: 10, name: "Chrome", cpu: 3 * time.Second, mem: 100}, // +2s
		{pid: 2, start: 10, name: "Chrome", cpu: 2 * time.Second, mem: 100}, // +1s
		{pid: 3, start: 99, name: "reused", cpu: 60 * time.Second, mem: 50}, // a new process: no share yet
		{pid: 4, start: 10, name: "Linux kernel", mem: 0},
	}, gpu: []gpuClient{{key: "d/1", pid: 1, vram: 1 << 20, busy: map[string]time.Duration{"gfx": 500 * time.Millisecond, "compute": 900 * time.Millisecond}}}}
	got := breakdown(before, after, 10*time.Second, 4) // 40 core-seconds
	if got.Processes != 4 || !got.GPUApps {
		t.Fatalf("%+v", got)
	}
	apps := map[string]App{}
	for _, a := range got.Apps {
		apps[a.Name] = a
	}
	chrome := apps["Chrome"]
	if chrome.Processes != 2 || chrome.CPU != 7.5 || chrome.Memory != 200 || chrome.GPU == nil || *chrome.GPU != 9 || chrome.GPUMemory != 1<<20 {
		t.Fatalf("chrome: %+v gpu=%v", chrome, chrome.GPU)
	}
	if r := apps["reused"]; r.CPU != 0 || r.GPU == nil || *r.GPU != 0 {
		t.Fatalf("reused: %+v", r)
	}
	if got.Apps[0].Name != "Chrome" {
		t.Fatalf("not led by processor: %+v", got.Apps)
	}
}

// Only the leaders of some ranking are kept.
func TestLeaders(t *testing.T) {
	var all []App
	for i := 0; i < 40; i++ {
		all = append(all, App{Name: string(rune('A' + i)), CPU: float64(i), Memory: uint64(40 - i)})
	}
	got := leaders(all)
	if len(got) != 2*appsTop {
		t.Fatalf("kept %d", len(got))
	}
}

// The first breakdown measures over a short window; later ones answer from
// the previous listing, and a repeated request within a moment is the same.
func TestAppsSampler(t *testing.T) {
	clock := time.Unix(1000, 0)
	calls := 0
	s := &Sampler{now: func() time.Time { return clock }, r: reader{cores: func() int { return 2 }, procs: func() (procSnapshot, error) {
		calls++
		clock = clock.Add(appsWindow)
		return procSnapshot{procs: []procInfo{{pid: 1, name: "x", cpu: time.Duration(calls) * appsWindow}}}, nil
	}}}
	got, err := s.Apps(context.Background())
	if err != nil || calls != 2 || len(got.Apps) != 1 || got.Apps[0].CPU != 50 {
		t.Fatalf("first: %+v %v calls=%d", got, err, calls)
	}
	if _, err := s.Apps(context.Background()); err != nil || calls != 2 {
		t.Fatalf("a fresh breakdown was taken again: calls=%d", calls)
	}
	clock = clock.Add(2 * time.Second)
	if _, err := s.Apps(context.Background()); err != nil || calls != 3 {
		t.Fatalf("later: calls=%d", calls)
	}
	if _, err := (&Sampler{}).Apps(context.Background()); err != ErrUnsupported {
		t.Fatal("a sampler without listings answered")
	}
}
