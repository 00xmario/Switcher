package sysload

import (
	"context"
	"fmt"
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
	stats := []byte("    1   0:12.50  20480 Mon Nov 10 09:00:01 2026 launchd\n" +
		"  501   1:02:03.00 102400 Mon Nov 10 09:00:02 2026 Google Chrome\n" +
		"  502   2-00:00:01.00 1024 Mon Nov 10 09:00:03 2026 Google Chrome He\n" +
		"  600   0:00.10  4096 Mon Nov 10 09:00:04 2026 postgres\n" +
		"  601   0:00.00     0 Mon Nov 10 09:00:05 2026 <defunct>\n" +
		"  602   0:00.01  2048 Mon Nov 10 09:00:06 2026 zsh\n" +
		"  bad line\n")
	paths := []byte("    1 /sbin/launchd\n" +
		"  501 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome\n" +
		"  502 /Applications/Google Chrome.app/Contents/Frameworks/X.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper\n" +
		"  600 postgres: alice orders 127.0.0.1(54321) idle\n" +
		"  602 -zsh\n")
	snap := parsePS(stats, paths)
	if len(snap.procs) != 5 || snap.gpuOK {
		t.Fatalf("%+v", snap)
	}
	want := []struct {
		name string
		cpu  time.Duration
	}{{"launchd", 12500 * time.Millisecond}, {"Google Chrome", time.Hour + 2*time.Minute + 3*time.Second},
		{"Google Chrome", 48*time.Hour + time.Second}, {"PostgreSQL", 100 * time.Millisecond}, {"zsh", 10 * time.Millisecond}}
	for i, w := range want {
		if p := snap.procs[i]; p.name != w.name || p.cpu != w.cpu || p.start == 0 {
			t.Errorf("%d: %+v, want %+v", i, p, w)
		}
	}
	if snap.procs[0].mem != 20480<<10 || snap.procs[0].start == snap.procs[1].start {
		t.Fatalf("memory or start: %+v", snap.procs[:2])
	}
	// Without the paths, the kernel's names still serve.
	if snap := parsePS(stats, nil); snap.procs[1].name != "Google Chrome" || snap.procs[2].name != "Google Chrome He" {
		t.Fatalf("without paths: %+v", snap.procs)
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

// Only apps that lead some ranking with a use are kept, the same ones from
// one reading to the next: idle programs never fill the list.
func TestLeaders(t *testing.T) {
	var all []App
	for i := 0; i < 40; i++ {
		all = append(all, App{Name: string(rune('A' + i)), CPU: float64(i), Memory: uint64(40 - i)})
	}
	if got := leaders(all); len(got) != 2*appsTop {
		t.Fatalf("kept %d", len(got))
	}
	var idle []App
	for i := 0; i < 100; i++ {
		zero := 0.0
		idle = append(idle, App{Name: fmt.Sprintf("idle%03d", i), GPU: &zero})
	}
	busy := 5.0
	idle = append(idle, App{Name: "busy", CPU: 3, Memory: 10, GPU: &busy})
	got := leaders(idle)
	if len(got) != 1 || got[0].Name != "busy" {
		t.Fatalf("idle apps kept: %d, first %q", len(got), got[0].Name)
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
