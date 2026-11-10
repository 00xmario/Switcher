package sysload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProcStat(t *testing.T) {
	got, err := parseProcStat("cpu  100 20 30 800 50 5 5 10 7 0\n")
	if err != nil || got.busy != 170 || got.total != 1020 {
		t.Fatalf("parse: %+v %v", got, err)
	}
	for _, bad := range []string{"", "cpu0 1 2 3 4 5", "cpu 1 x 3 4 5", "intr 1 2 3 4"} {
		if _, err := parseProcStat(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestMeminfo(t *testing.T) {
	used, total, err := parseMeminfo([]byte("MemTotal:       16000 kB\nMemFree:  1000 kB\nMemAvailable:    4000 kB\n"))
	if err != nil || total != 16000*1024 || used != 12000*1024 {
		t.Fatalf("parse: %d %d %v", used, total, err)
	}
	if _, _, err := parseMeminfo([]byte("MemTotal: 16000 kB\n")); err == nil {
		t.Fatal("meminfo without MemAvailable parsed")
	}
}

// The busiest card counts; connectors and unreadable files do not.
func TestSysfsGPU(t *testing.T) {
	root := t.TempDir()
	write := func(card, value string) {
		dir := filepath.Join(root, card, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gpu_busy_percent"), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := sysfsGPU(root)(); ok {
		t.Fatal("a computer without cards has a reading")
	}
	write("card0", "12\n")
	write("card1", "48\n")
	write("card1-HDMI-A-1", "99\n")
	write("card2", "busy\n")
	if v, ok := sysfsGPU(root)(); !ok || v != 48 {
		t.Fatalf("busiest card: %v %v", v, ok)
	}
}

// A powered-down card is idle and never asked, so a look at the load does
// not wake a laptop's graphics.
func TestSuspendedCardsAreIdle(t *testing.T) {
	root := t.TempDir()
	card := func(name, vendor, busy, status string) {
		device := filepath.Join(root, name, "device")
		os.MkdirAll(filepath.Join(device, "power"), 0o755)
		os.WriteFile(filepath.Join(device, "vendor"), []byte(vendor+"\n"), 0o644)
		if busy != "" {
			os.WriteFile(filepath.Join(device, "gpu_busy_percent"), []byte(busy), 0o644)
		}
		os.WriteFile(filepath.Join(device, "power", "runtime_status"), []byte(status+"\n"), 0o644)
	}
	card("card0", "0x1002", "77", "suspended")
	if v, ok := sysfsGPU(root)(); !ok || v != 0 {
		t.Fatalf("suspended AMD card: %v %v", v, ok)
	}
	asked := 0
	nvidia := func(context.Context) (float64, bool) { asked++; return 50, true }
	card("card1", "0x10de", "", "suspended")
	if v, ok := sleepingNvidia(root, nvidia)(context.Background()); !ok || v != 0 || asked != 0 {
		t.Fatalf("sleeping NVIDIA card was asked: %v %v %d", v, ok, asked)
	}
	card("card1", "0x10de", "", "active")
	if v, _ := sleepingNvidia(root, nvidia)(context.Background()); v != 50 || asked != 1 {
		t.Fatalf("awake NVIDIA card: %v %d", v, asked)
	}
	if v, _ := sleepingNvidia(t.TempDir(), nvidia)(context.Background()); v != 50 {
		t.Fatal("without drm cards nvidia-smi is still asked")
	}
}

func TestNvidiaAndFallback(t *testing.T) {
	if v, ok := parseNvidia([]byte("12\n 87 \n[N/A]\n")); !ok || v != 87 {
		t.Fatalf("nvidia: %v %v", v, ok)
	}
	none := func() (float64, bool) { return 0, false }
	nvidia := func(context.Context) (float64, bool) { return 9, true }
	if v, ok := linuxGPU(none, nvidia)(context.Background()); !ok || v != 9 {
		t.Fatalf("fallback: %v %v", v, ok)
	}
	sysfs := func() (float64, bool) { return 3, true }
	if v, _ := linuxGPU(sysfs, nvidia)(context.Background()); v != 3 {
		t.Fatalf("sysfs first: %v", v)
	}
}

// This computer's own readings, as the server takes them.
func TestThisComputer(t *testing.T) {
	s := New()
	got, err := s.Read(context.Background())
	if err != nil || got.Cores < 1 || got.MemTotal == 0 || got.MemUsed > got.MemTotal || got.CPU < 0 || got.CPU > 100 {
		t.Fatalf("reading: %+v %v", got, err)
	}
}
