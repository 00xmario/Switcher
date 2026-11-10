package sysload

import (
	"testing"
	"time"
)

func TestParsePidStat(t *testing.T) {
	// A name with spaces and parentheses; flags 0x400100 (not a kernel
	// thread), utime 250, stime 50, starttime 777.
	raw := []byte("42 (my (odd) app) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 3 0 777 1000 200 18446744073709551615")
	comm, st, ok := parsePidStat(raw)
	if !ok || comm != "my (odd) app" || st.utime != 250 || st.stime != 50 || st.start != 777 || st.flags&pfKthread != 0 {
		t.Fatalf("%q %+v %v", comm, st, ok)
	}
	if _, _, ok := parsePidStat([]byte("42 (short) S 1")); ok {
		t.Fatal("a short line parsed")
	}
}

func TestParseDRMFdinfo(t *testing.T) {
	raw := []byte("pos:\t0\ndrm-driver:\tamdgpu\ndrm-client-id:\t253\ndrm-pdev:\t0000:65:00.0\n" +
		"drm-resident-vram:\t11064 KiB\ndrm-engine-gfx:\t5000000 ns\ndrm-engine-compute:\t100 ns\n" +
		"drm-engine-capacity-gfx:\t2\ndrm-cycles-gfx:\t99\n")
	c, ok := parseDRMFdinfo(raw)
	if !ok || !c.ok || c.key != "0000:65:00.0/253" || c.vram != 11064<<10 || c.busy["gfx"] != 5*time.Millisecond || c.busy["compute"] != 100 || len(c.busy) != 2 {
		t.Fatalf("%+v %v", c, ok)
	}
	if _, ok := parseDRMFdinfo([]byte("pos:\t0\nflags:\t02\n")); ok {
		t.Fatal("a file without a DRM client parsed")
	}
	if drmBytes("6 MiB") != 6<<20 || drmBytes("2 GiB") != 2<<30 || drmBytes("x KiB") != 0 || drmBytes("3 TiB") != 0 {
		t.Fatal("sizes")
	}
}

func TestProgramName(t *testing.T) {
	for exe, want := range map[string]string{
		"/home/u/.local/share/claude/versions/2.1.322":           "claude",
		"/home/u/.local/share/claude/versions/2.1.318 (deleted)": "claude",
		"/opt/chrome/chrome-headless-shell":                      "chrome-headless-shell",
		"/usr/bin/node":                                          "node",
		"/1.2":                                                   "1.2",
	} {
		if got := programName(exe); got != want {
			t.Errorf("programName(%q) = %q, want %q", exe, got, want)
		}
	}
}

// A name the kernel cut is completed from the one program that starts with
// it, then given the name people know.
func TestCompleteNames(t *testing.T) {
	procs := []procInfo{
		{name: "gnome-remote-desktop-daemon"},
		{name: "gnome-remote-de", cut: true},
		{name: "some-long-name-", cut: true}, // nothing to complete it
		{name: "chrome"},
		{name: "duplicate-name-", cut: true},
		{name: "duplicate-name-one"}, {name: "duplicate-name-two"},
	}
	completeNames(procs)
	for i, want := range []string{"gnome-remote-desktop-daemon", "gnome-remote-desktop-daemon", "some-long-name-", "Chrome", "duplicate-name-"} {
		if procs[i].name != want {
			t.Errorf("%d: %q, want %q", i, procs[i].name, want)
		}
	}
}

func TestParseDRMCyclesAndRegions(t *testing.T) {
	raw := []byte("drm-client-id:\t7\ndrm-pdev:\t0000:00:02.0\ndrm-cycles-rcs:\t500\ndrm-total-cycles-rcs:\t10000\n" +
		"drm-resident-vram0:\t2 MiB\ndrm-resident-local0:\t1 MiB\ndrm-resident-system0:\t9 MiB\n")
	c, ok := parseDRMFdinfo(raw)
	if !ok || !c.ok || c.cycles["rcs"] != [2]uint64{500, 10000} || c.vram != 3<<20 {
		t.Fatalf("%+v", c)
	}
	old, _ := parseDRMFdinfo([]byte("drm-client-id:\t1\ndrm-memory-vram:\t4 MiB\n"))
	if old.vram != 4<<20 {
		t.Fatalf("older amdgpu memory: %d", old.vram)
	}
	// Cycles give a share of the engine's own total.
	before := procSnapshot{gpuOK: true, procs: []procInfo{{pid: 1, name: "x"}}, gpu: []gpuClient{{key: "k", pid: 1, cycles: map[string][2]uint64{"rcs": {500, 10000}}}}}
	after := procSnapshot{gpuOK: true, procs: []procInfo{{pid: 1, name: "x"}}, gpu: []gpuClient{{key: "k", pid: 1, cycles: map[string][2]uint64{"rcs": {2500, 20000}}}}}
	got := breakdown(before, after, time.Second, 1)
	if len(got.Apps) != 1 || got.Apps[0].GPU == nil || *got.Apps[0].GPU != 20 {
		t.Fatalf("cycles share: %+v", got.Apps)
	}
}

// Another user's program goes by the kernel's name, which the process may
// have set: only a first word that looks like a name.
func TestKernelName(t *testing.T) {
	for comm, want := range map[string]string{
		"sshd": "sshd", "npm start --prod": "npm", "tok=hunter2 xyz": "Other programs",
		"host:5432": "Other programs", "": "Other programs", "a/b": "Other programs",
	} {
		if got := kernelName(comm); got != want {
			t.Errorf("kernelName(%q) = %q, want %q", comm, got, want)
		}
	}
}

// Engines of one kind share the time: two video engines busy 90% of a
// second each are 90% busy, not 180%.
func TestEngineCapacity(t *testing.T) {
	c, _ := parseDRMFdinfo([]byte("drm-client-id:\t3\ndrm-engine-video:\t0 ns\ndrm-engine-capacity-video:\t2\n"))
	if c.capacity["video"] != 2 || len(c.busy) != 1 {
		t.Fatalf("%+v", c)
	}
	before := procSnapshot{gpuOK: true, procs: []procInfo{{pid: 1, name: "x"}}, gpu: []gpuClient{{key: "k", pid: 1, busy: map[string]time.Duration{"video": 0}, capacity: c.capacity}}}
	after := procSnapshot{gpuOK: true, procs: []procInfo{{pid: 1, name: "x"}}, gpu: []gpuClient{{key: "k", pid: 1, busy: map[string]time.Duration{"video": 1800 * time.Millisecond}, capacity: c.capacity}}}
	if got := breakdown(before, after, time.Second, 1); *got.Apps[0].GPU != 90 {
		t.Fatalf("share %v", *got.Apps[0].GPU)
	}
}
