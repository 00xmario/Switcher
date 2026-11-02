package awake

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// afterStart is how long a fresh inhibitor must survive to count as held.
var afterStart = func() <-chan time.Time { return time.After(300 * time.Millisecond) }

// On Linux an inhibitor lock from systemd-logind keeps the computer awake.
// systemd-inhibit holds it for as long as its child runs; the child is a
// shell loop that ends when Switcher does, so a crash never leaves the
// computer awake. "sleep" blocks suspend, "idle" also keeps the screen from
// idling, and "handle-lid-switch" makes logind ignore a closed lid. None of
// them needs root: polkit grants them to the user's active session, so there
// is nothing to set up, unlike the sudoers rule on a Mac.
type linuxSystem struct {
	inhibit string // systemd-inhibit
	power   string // /sys/class/power_supply, a fixture in tests
	proc    string // /proc, to find a lid inhibitor left behind; "" skips that
	kill    func(pid int)

	mu  sync.Mutex
	lid func() // releases the closed-lid inhibitor
}

// inhibitPaths are where distributions install systemd-inhibit.
var inhibitPaths = []string{"/usr/bin/systemd-inhibit", "/bin/systemd-inhibit"}

func newLinuxSystem() system {
	for _, path := range inhibitPaths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return &linuxSystem{inhibit: path, power: "/sys/class/power_supply", proc: "/proc"}
		}
	}
	return nil
}

// inhibitArgs keeps the inhibitor until process pid exits.
func inhibitArgs(what string, pid int) []string {
	return []string{"--what=" + what, "--who=Switcher", "--why=Keeping the computer awake for your agents", "--mode=block",
		"/bin/sh", "-c", "while kill -0 " + strconv.Itoa(pid) + " 2>/dev/null; do sleep 5; done"}
}

func holdWhat(allowDisplaySleep bool) string {
	if allowDisplaySleep {
		return "sleep"
	}
	return "sleep:idle"
}

// start runs systemd-inhibit in its own process group, so releasing it ends
// the shell loop too.
func (s *linuxSystem) start(what string) (func(), int, error) {
	cmd := exec.Command(s.inhibit, inhibitArgs(what, os.Getpid())...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		})
	}
	// logind refuses an inhibitor polkit does not allow at once; notice that
	// instead of reporting a lock that is not held.
	select {
	case <-exited:
		return nil, 0, fmt.Errorf("systemd-inhibit stopped right away (%v); is this a desktop session that may inhibit sleep?", waitErr)
	case <-afterStart():
	}
	return release, cmd.Process.Pid, nil
}

func (s *linuxSystem) hold(allowDisplaySleep bool) (func(), int, error) {
	return s.start(holdWhat(allowDisplaySleep))
}

// sleepDisabled reports the closed-lid inhibitor Switcher holds. logind
// lists inhibitors, but another app's lid lock is not Switcher's to undo.
func (s *linuxSystem) sleepDisabled() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lid != nil, nil
}

func (s *linuxSystem) setSleepDisabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !on {
		if s.lid != nil {
			s.lid()
			s.lid = nil
		}
		s.endLeftoverLid(0)
		return nil
	}
	if s.lid != nil {
		return nil
	}
	release, pid, err := s.start("handle-lid-switch")
	if err != nil {
		return err
	}
	s.lid = release
	// The new one holds the lid first, so it is never free in between.
	s.endLeftoverLid(pid)
	return nil
}

// endLeftoverLid ends closed-lid inhibitors this process did not start but
// that watch it: a Switcher that updated restarts in place, with the same
// process id, so its inhibitor kept running unrecorded. keep is spared.
func (s *linuxSystem) endLeftoverLid(keep int) {
	if s.proc == "" {
		return
	}
	entries, err := os.ReadDir(s.proc)
	if err != nil {
		return
	}
	want := inhibitArgs("handle-lid-switch", os.Getpid())
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == keep {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.proc, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) == len(want)+1 && slices.Equal(args[1:], want) {
			if s.kill != nil {
				s.kill(pid)
			} else {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}
}

func (s *linuxSystem) lidRuleReady() bool    { return true }
func (s *linuxSystem) installLidRule() error { return nil }

// battery reads the kernel's power supplies: the lowest battery charge, and
// whether no charger is online.
func (s *linuxSystem) battery() (int, bool, error) { return readPowerSupplies(s.power) }

func readPowerSupplies(root string) (int, bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, false, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	read := func(name, field string) string {
		raw, _ := os.ReadFile(filepath.Join(root, name, field))
		return strings.TrimSpace(string(raw))
	}
	percent, found, charger, discharging := 101, false, false, false
	for _, name := range names {
		switch read(name, "type") {
		case "Battery":
			if read(name, "scope") == "Device" {
				continue // a mouse or headset, not this computer
			}
			n, err := strconv.Atoi(read(name, "capacity"))
			if err != nil {
				continue
			}
			found = true
			percent = min(percent, n)
			discharging = discharging || read(name, "status") == "Discharging"
		case "Mains", "USB", "USB_C", "USB_PD":
			charger = charger || read(name, "online") == "1"
		}
	}
	if !found {
		return 0, false, errors.New("no battery")
	}
	return percent, discharging && !charger, nil
}

// bridge needs nothing on Linux: an update restarts Switcher in place, with
// the same process id, so its inhibitors keep holding across it.
func (s *linuxSystem) bridge(bool) int { return 0 }

// stopLeftover ends an inhibitor an earlier Switcher left behind, and only
// that: the process must still be systemd-inhibit.
func (s *linuxSystem) stopLeftover(pid int) {
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err == nil && strings.TrimSpace(string(comm)) == "systemd-inhibit" {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
