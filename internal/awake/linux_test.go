package awake

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func powerSupply(t *testing.T, root, name string, fields map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for field, value := range fields {
		if err := os.WriteFile(filepath.Join(dir, field), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinuxBatteryReadsTheKernelsPowerSupplies(t *testing.T) {
	root := t.TempDir()
	if _, _, err := readPowerSupplies(root); err == nil {
		t.Fatal("a computer without a battery reports an error")
	}
	powerSupply(t, root, "hidpp_battery_0", map[string]string{"type": "Battery", "scope": "Device", "capacity": "3", "status": "Discharging"})
	if _, _, err := readPowerSupplies(root); err == nil {
		t.Fatal("a wireless mouse counted as this computer's battery")
	}
	powerSupply(t, root, "BAT0", map[string]string{"type": "Battery", "capacity": "9", "status": "Discharging"})
	percent, onBattery, err := readPowerSupplies(root)
	if err != nil || percent != 9 || !onBattery {
		t.Fatalf("discharging: %d %v %v", percent, onBattery, err)
	}
	powerSupply(t, root, "AC", map[string]string{"type": "Mains", "online": "1"})
	if _, onBattery, _ := readPowerSupplies(root); onBattery {
		t.Fatal("on battery while plugged in")
	}
}

func TestLinuxInhibitorEndsWithSwitcher(t *testing.T) {
	args := inhibitArgs("sleep:idle:handle-lid-switch", 4242)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--what=sleep:idle:handle-lid-switch", "--mode=block", "--who=Switcher", "kill -0 4242"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	if holdWhat(true) != "sleep" || holdWhat(false) != "sleep:idle" {
		t.Errorf("display choice: %q %q", holdWhat(true), holdWhat(false))
	}
}

// fakeInhibit is a systemd-inhibit that skips its options and runs the
// command, as the real one does once logind grants the lock.
func fakeInhibit(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "systemd-inhibit")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestLinuxHoldAndLidReleaseTheirInhibitors(t *testing.T) {
	afterStart = func() <-chan time.Time { return time.After(100 * time.Millisecond) }
	t.Cleanup(func() { afterStart = func() <-chan time.Time { return time.After(300 * time.Millisecond) } })
	s := &linuxSystem{inhibit: fakeInhibit(t, `while [ "${1#--}" != "$1" ]; do shift; done
exec "$@"
`)}
	release, pid, err := s.hold(false)
	if err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("the inhibitor is not running")
	}
	release()
	release() // twice is fine
	if alive(pid) {
		t.Fatal("the inhibitor outlived its release")
	}

	if on, _ := s.sleepDisabled(); on {
		t.Fatal("lid inhibitor held before asking")
	}
	if err := s.setSleepDisabled(true); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.sleepDisabled(); !on || !s.lidRuleReady() {
		t.Fatal("closed-lid inhibitor not held")
	}
	if err := s.setSleepDisabled(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.sleepDisabled(); on {
		t.Fatal("closed-lid inhibitor still held")
	}
}

func TestLinuxRefusedInhibitorIsAnError(t *testing.T) {
	s := &linuxSystem{inhibit: fakeInhibit(t, "echo 'Failed to inhibit: Access denied' >&2\nexit 1\n")}
	if _, _, err := s.hold(true); err == nil || !strings.Contains(err.Error(), "systemd-inhibit") {
		t.Fatalf("refused inhibitor: %v", err)
	}
}
