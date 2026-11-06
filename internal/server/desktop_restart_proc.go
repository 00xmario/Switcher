package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// linuxDesktopName is the program of Anthropic's Claude Desktop package.
// Claude Code's own process is "claude" and Desktop's helpers have names of
// their own, so the name keeps them out.
const linuxDesktopName = "claude-desktop"

const desktopNoSessionMessage = "Switcher could not reach your desktop session to reopen Claude Desktop, so it was left as it is. Restart it yourself."

var (
	systemdRunPaths   = []string{"/usr/bin/systemd-run", "/bin/systemd-run"}
	systemctlPaths    = []string{"/usr/bin/systemctl", "/bin/systemctl"}
	linuxDesktopPaths = []string{"/usr/bin/claude-desktop"}
)

// linuxDesktop restarts Claude Desktop on Linux. It reads /proc rather than
// matching process names with pgrep: only there can the main process be told
// apart from its --type= children and from Claude Code.
type linuxDesktop struct {
	proc string // /proc
	uid  int
	home string
	// isDesktop says whether a running program is Claude Desktop.
	isDesktop func(exe string) bool
	// installed is the program to open when Desktop isn't running; "" if none.
	installed  func() string
	systemdRun string
	systemctl  string
	command    func(ctx context.Context, path string, args ...string) ([]byte, error)
	terminate  func(pid int) error
	// settle is how long a reopened Desktop must keep running to count as open.
	settle time.Duration
}

func newLinuxDesktop() *linuxDesktop {
	home, _ := os.UserHomeDir()
	return &linuxDesktop{
		proc: "/proc", uid: os.Getuid(), home: home,
		isDesktop: func(exe string) bool { return filepath.Base(exe) == linuxDesktopName },
		installed: func() string {
			if path := firstProgram(linuxDesktopPaths); path != "" {
				return path
			}
			path, err := exec.LookPath(linuxDesktopName)
			if err != nil {
				return ""
			}
			return path
		},
		systemdRun: firstProgram(systemdRunPaths),
		systemctl:  firstProgram(systemctlPaths),
		command:    runDesktopCommand,
		terminate:  terminateProcess,
		settle:     2 * time.Second,
	}
}

func firstProgram(paths []string) string {
	for _, path := range paths {
		if isProgram(path) {
			return path
		}
	}
	return ""
}

func isProgram(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func runDesktopCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(path), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func terminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

type desktopProcess struct {
	pid int
	exe string
}

// mains lists this user's Claude Desktop main processes. A process that is
// gone, a zombie, or another user's has no exe to read and is skipped.
func (d *linuxDesktop) mains() ([]desktopProcess, error) {
	entries, err := os.ReadDir(d.proc)
	if err != nil {
		return nil, err
	}
	var found []desktopProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(d.proc, entry.Name())
		exe, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil {
			continue
		}
		// An update replaces the program under a running app.
		exe = strings.TrimSuffix(exe, " (deleted)")
		if !d.isDesktop(exe) || !d.ownedByUser(dir) {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		// Chromium rewrites a child's command line into one string, so look
		// for the flag anywhere rather than for an argument starting with it.
		if err != nil || bytes.Contains(cmdline, []byte("--type=")) {
			continue
		}
		found = append(found, desktopProcess{pid: pid, exe: exe})
	}
	return found, nil
}

func (d *linuxDesktop) ownedByUser(dir string) bool {
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			// Real, effective, saved, filesystem.
			fields := strings.Fields(rest)
			return len(fields) > 1 && fields[1] == strconv.Itoa(d.uid)
		}
	}
	return false
}

// prepare finds what to reopen and refuses, before anything is closed, a
// restart that could not bring Desktop back.
func (d *linuxDesktop) prepare(ctx context.Context) (string, error) {
	running, err := d.mains()
	if err != nil {
		return "", err
	}
	program := ""
	for _, process := range running {
		if isProgram(process.exe) {
			program = process.exe
			break
		}
	}
	if program == "" {
		program = d.installed()
	}
	if program == "" {
		return "", restartFailure("Claude Desktop is not installed on this computer.", nil)
	}
	if d.systemdRun == "" || d.systemctl == "" {
		return "", restartFailure(desktopNoSessionMessage, errors.New("systemd-run or systemctl is missing"))
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	env, err := d.command(check, d.systemctl, "--user", "show-environment")
	if err != nil {
		return "", restartFailure(desktopNoSessionMessage, err)
	}
	for line := range strings.SplitSeq(string(env), "\n") {
		if strings.HasPrefix(line, "WAYLAND_DISPLAY=") || strings.HasPrefix(line, "DISPLAY=") {
			return program, nil
		}
	}
	return "", restartFailure(desktopNoSessionMessage, errors.New("the user manager has no display"))
}

func (d *linuxDesktop) quit(context.Context) error {
	running, err := d.mains()
	if err != nil {
		return err
	}
	for _, process := range running {
		// Children and helpers follow the main process, which shuts down
		// cleanly on SIGTERM.
		if err := d.terminate(process.pid); err != nil {
			return fmt.Errorf("signal process %d: %w", process.pid, err)
		}
	}
	return nil
}

func (d *linuxDesktop) running(context.Context) (bool, error) {
	running, err := d.mains()
	return len(running) > 0, err
}

// open starts Desktop as a unit of its own under the user manager. As a child
// of Switcher it would run in Switcher's service and die when that restarts,
// and it would inherit Switcher's environment instead of the desktop session's.
// Each start gets its own unit name because a name still loaded can't be reused.
func (d *linuxDesktop) open(ctx context.Context, program string) error {
	args := []string{"--user", "--collect", "--quiet", "--service-type=exec", "--slice=app.slice",
		"--unit=app-switcher-com.anthropic.Claude-" + rand.Text() + ".service"}
	if d.home != "" {
		args = append(args, "--working-directory="+d.home)
	}
	if _, err := d.command(ctx, d.systemdRun, append(args, "--", program)...); err != nil {
		return err
	}
	timer := time.NewTimer(d.settle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	active, err := d.running(ctx)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("it stopped right after starting")
	}
	return nil
}

func (d *linuxDesktop) restart(ctx context.Context) error {
	program, err := d.prepare(ctx)
	if err != nil {
		return err
	}
	return runDesktopRestart(ctx, desktopRestartOperations{
		quit:    d.quit,
		running: d.running,
		open:    func(ctx context.Context) error { return d.open(ctx, program) },
	})
}
