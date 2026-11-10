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

func TestProcessName(t *testing.T) {
	for _, c := range []struct{ comm, exe, want string }{
		{"claude", "/home/u/.local/share/claude/versions/2.1.322", "claude"},
		{"2.1.318", "/home/u/.local/share/claude/versions/2.1.318", "claude"},
		{"2.1.318", "/home/u/.local/share/claude/versions/2.1.318 (deleted)", "claude"},
		{"chrome-headless", "/opt/chrome/chrome-headless-shell", "chrome-headless-shell"},
		{"node-MainThread", "/usr/bin/node", "node"},
		{"", "/usr/bin/worker", "worker"},
		{"bash", "", "bash"},
		{"gnome-remote-de", "/usr/libexec/gnome-remote-desktop-daemon", "gnome-remote-desktop-daemon"},
		{"1.2", "/1.2", "1.2"},
	} {
		if got := processName(c.comm, c.exe); got != c.want {
			t.Errorf("processName(%q, %q) = %q, want %q", c.comm, c.exe, got, c.want)
		}
	}
}
