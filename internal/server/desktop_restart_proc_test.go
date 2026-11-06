package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Nothing here may reach a real Claude Desktop on the machine running the
// tests: fixtures read a fake /proc, and the one test on the real /proc matches
// a single throwaway program by its exact path.

type fakeProcess struct {
	exe, cmdline string
	uid          int
}

type fakeDesktop struct {
	*linuxDesktop
	t         *testing.T
	program   string
	env       string
	envErr    error
	runErr    error
	reopens   bool
	signalled []int
	runs      [][]string
}

func newFakeDesktop(t *testing.T) *fakeDesktop {
	t.Helper()
	root := t.TempDir()
	program := filepath.Join(root, "bin", linuxDesktopName)
	if err := os.MkdirAll(filepath.Dir(program), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeDesktop{t: t, program: program, env: "HOME=/home/test\nWAYLAND_DISPLAY=wayland-0\n", reopens: true}
	f.linuxDesktop = &linuxDesktop{
		proc: filepath.Join(root, "proc"), uid: 1000, home: "/home/test",
		isDesktop:  func(exe string) bool { return filepath.Base(exe) == linuxDesktopName },
		installed:  func() string { return program },
		systemdRun: "/fixture/systemd-run", systemctl: "/fixture/systemctl",
		settle: time.Millisecond,
	}
	if err := os.MkdirAll(f.proc, 0o700); err != nil {
		t.Fatal(err)
	}
	f.command = func(_ context.Context, path string, args ...string) ([]byte, error) {
		f.runs = append(f.runs, append([]string{path}, args...))
		if path == f.systemctl {
			return []byte(f.env), f.envErr
		}
		if f.runErr != nil {
			return nil, f.runErr
		}
		if f.reopens {
			f.add(500, fakeProcess{exe: program, cmdline: program + "\x00", uid: 1000})
		}
		return nil, nil
	}
	f.terminate = func(pid int) error {
		f.signalled = append(f.signalled, pid)
		return os.RemoveAll(filepath.Join(f.proc, strconv.Itoa(pid)))
	}
	return f
}

func (f *fakeDesktop) add(pid int, p fakeProcess) {
	f.t.Helper()
	dir := filepath.Join(f.proc, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if p.exe != "" {
		if err := os.Symlink(p.exe, filepath.Join(dir, "exe")); err != nil {
			f.t.Fatal(err)
		}
	}
	status := fmt.Sprintf("Name:\tx\nUid:\t%d\t%d\t%d\t%d\n", p.uid, p.uid, p.uid, p.uid)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(p.cmdline), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeDesktop) started() [][]string {
	var runs [][]string
	for _, run := range f.runs {
		if run[0] == f.systemdRun {
			runs = append(runs, run)
		}
	}
	return runs
}

func TestLinuxDesktopFindsOnlyItsMainProcess(t *testing.T) {
	f := newFakeDesktop(t)
	exe := "/usr/lib/claude-desktop/claude-desktop"
	f.add(100, fakeProcess{exe: exe, cmdline: exe + "\x00", uid: 1000})
	// Chromium rewrites a child's command line into one string with spaces.
	f.add(101, fakeProcess{exe: exe, cmdline: exe + " --type=renderer --lang=en-US\x00", uid: 1000})
	f.add(102, fakeProcess{exe: exe, cmdline: exe + "\x00--type=zygote\x00", uid: 1000})
	f.add(103, fakeProcess{exe: "/usr/lib/claude-desktop/resources/chrome-native-host", cmdline: "host\x00", uid: 1000})
	f.add(104, fakeProcess{exe: "/home/test/.local/bin/claude", cmdline: "claude\x00", uid: 1000})
	f.add(105, fakeProcess{exe: exe, cmdline: exe + "\x00", uid: 1001})
	f.add(106, fakeProcess{cmdline: "", uid: 1000}) // a zombie has no exe
	f.add(107, fakeProcess{exe: exe + " (deleted)", cmdline: exe + "\x00claude://code/new\x00", uid: 1000})
	if err := os.MkdirAll(filepath.Join(f.proc, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	found, err := f.mains()
	if err != nil {
		t.Fatal(err)
	}
	want := []desktopProcess{{100, exe}, {107, exe}}
	if !slices.Equal(found, want) {
		t.Fatalf("found %+v, want %+v", found, want)
	}
}

func TestLinuxDesktopRestartSignalsTheMainProcessAndReopensItOutsideSwitcher(t *testing.T) {
	f := newFakeDesktop(t)
	f.add(100, fakeProcess{exe: f.program, cmdline: f.program + "\x00", uid: 1000})
	f.add(101, fakeProcess{exe: f.program, cmdline: f.program + " --type=renderer\x00", uid: 1000})
	if err := f.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.signalled, []int{100}) {
		t.Fatalf("signalled %v, want only the main process", f.signalled)
	}
	runs := f.started()
	if len(runs) != 1 {
		t.Fatalf("started %d times", len(runs))
	}
	got := strings.Join(runs[0], " ")
	for _, want := range []string{"--user", "--collect", "--service-type=exec", "--slice=app.slice", "--working-directory=/home/test", "--unit=app-switcher-com.anthropic.Claude-"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if !strings.HasSuffix(got, "-- "+f.program) {
		t.Fatalf("program is not the final argument after --: %s", got)
	}
	// Restart again: a unit name that is still loaded can't be used twice.
	f.reopens = false
	if err := f.open(context.Background(), f.program); err != nil {
		t.Fatal(err)
	}
	unit := func(run []string) string {
		for _, arg := range run {
			if strings.HasPrefix(arg, "--unit=") {
				return arg
			}
		}
		return ""
	}
	if runs := f.started(); unit(runs[0]) == "" || unit(runs[0]) == unit(runs[1]) {
		t.Fatalf("unit names repeat: %s, %s", unit(runs[0]), unit(runs[1]))
	}
}

func TestLinuxDesktopRestartOpensAStoppedApp(t *testing.T) {
	f := newFakeDesktop(t)
	if err := f.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.signalled) != 0 || len(f.started()) != 1 {
		t.Fatalf("signalled %v, started %d", f.signalled, len(f.started()))
	}
}

func TestLinuxDesktopRestartKeepsTheProgramItWasRunning(t *testing.T) {
	f := newFakeDesktop(t)
	f.installed = func() string { return "/usr/bin/some-other-install" }
	f.add(100, fakeProcess{exe: f.program + " (deleted)", cmdline: f.program + "\x00", uid: 1000})
	if err := f.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs := f.started(); !strings.HasSuffix(strings.Join(runs[0], " "), "-- "+f.program) {
		t.Fatalf("reopened something else: %v", runs[0])
	}
}

// A restart that couldn't bring Desktop back must not close it.
func TestLinuxDesktopRestartRefusesBeforeClosingAnything(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		change  func(*fakeDesktop)
		want    string
	}{
		{"not installed", false, func(f *fakeDesktop) { f.installed = func() string { return "" } }, "not installed"},
		{"no systemd-run", true, func(f *fakeDesktop) { f.systemdRun = "" }, "desktop session"},
		{"no systemctl", true, func(f *fakeDesktop) { f.systemctl = "" }, "desktop session"},
		{"no user manager", true, func(f *fakeDesktop) { f.envErr = errors.New("fixture bus failure") }, "desktop session"},
		{"no display", true, func(f *fakeDesktop) { f.env = "HOME=/home/test\nXDISPLAY=:0\n" }, "desktop session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDesktop(t)
			if tc.running {
				f.add(100, fakeProcess{exe: f.program, cmdline: f.program + "\x00", uid: 1000})
			}
			tc.change(f)
			err := f.restart(context.Background())
			var failure *desktopRestartError
			if !errors.As(err, &failure) || !strings.Contains(failure.message, tc.want) {
				t.Fatalf("error %v, want one saying %q", err, tc.want)
			}
			if len(f.signalled) != 0 || len(f.started()) != 0 {
				t.Fatalf("signalled %v, started %d", f.signalled, len(f.started()))
			}
		})
	}
}

func TestLinuxDesktopRestartSaysWhenTheAppDidNotComeBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeDesktop)
	}{
		{"systemd-run failed", func(f *fakeDesktop) { f.runErr = errors.New("fixture start failure") }},
		{"stopped right after starting", func(f *fakeDesktop) { f.reopens = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDesktop(t)
			f.add(100, fakeProcess{exe: f.program, cmdline: f.program + "\x00", uid: 1000})
			tc.change(f)
			err := f.restart(context.Background())
			var failure *desktopRestartError
			if !errors.As(err, &failure) || !strings.Contains(failure.message, "did not come back") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

// TestDummyDesktopProcess is the program TestLinuxDesktopControlsARealProcess
// runs: this test binary under Desktop's name, waiting to be signalled.
func TestDummyDesktopProcess(t *testing.T) {
	if os.Getenv("SWITCHER_TEST_DUMMY_DESKTOP") != "1" {
		t.Skip("only a helper for TestLinuxDesktopControlsARealProcess")
	}
	time.Sleep(time.Minute)
}

// The real /proc, a real process and a real signal, on a throwaway copy of this
// test binary that only its exact path can match.
func TestLinuxDesktopControlsARealProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), linuxDesktopName)
	if err = os.WriteFile(program, data, 0o700); err != nil {
		t.Fatal(err)
	}
	var live []*exec.Cmd
	start := func() error {
		for attempt := 0; ; attempt++ {
			cmd := exec.Command(program, "-test.run=^TestDummyDesktopProcess$")
			cmd.Env = append(os.Environ(), "SWITCHER_TEST_DUMMY_DESKTOP=1")
			err := cmd.Start()
			if errors.Is(err, syscall.ETXTBSY) && attempt < 50 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil {
				return err
			}
			// Stays a zombie until reaped, as under a parent that is slow to.
			live = append(live, cmd)
			return nil
		}
	}
	t.Cleanup(func() {
		for _, cmd := range live {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if err = start(); err != nil {
		t.Skipf("cannot run a program from the temp dir: %v", err)
	}
	d := newLinuxDesktop()
	d.isDesktop = func(exe string) bool { return exe == program }
	d.installed = func() string { return "" }
	d.systemdRun, d.systemctl = "/fixture/systemd-run", "/fixture/systemctl"
	d.settle = time.Millisecond
	d.command = func(_ context.Context, path string, _ ...string) ([]byte, error) {
		if path == d.systemctl {
			return []byte("DISPLAY=:0\n"), nil
		}
		return nil, start()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		found, err := d.mains()
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 1 && found[0].pid == live[0].Process.Pid && found[0].exe == program {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not find the process: %+v", found)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := live[0].Wait(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("first process did not end on SIGTERM: %v", err)
	}
	found, err := d.mains()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || len(found) != 1 || found[0].pid != live[1].Process.Pid {
		t.Fatalf("not reopened: %+v, %d started", found, len(live))
	}
}

func TestDesktopRestartFailuresNameTheStepThatFailed(t *testing.T) {
	quit, gone, open := func(context.Context) error { return nil }, func(context.Context) (bool, error) { return false, nil }, func(context.Context) error { return nil }
	broken := errors.New("fixture-cause")
	for _, tc := range []struct {
		name string
		ops  desktopRestartOperations
		want string
	}{
		{"quit", desktopRestartOperations{func(context.Context) error { return broken }, gone, open}, "left running"},
		{"exit", desktopRestartOperations{quit, func(context.Context) (bool, error) { return false, broken }, open}, "did not quit in time"},
		{"open", desktopRestartOperations{quit, gone, func(context.Context) error { return broken }}, "did not come back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runDesktopRestart(context.Background(), tc.ops)
			w := httptest.NewRecorder()
			writeDesktopRestartError(w, err)
			body := w.Body.String()
			if !errors.Is(err, broken) || w.Code != http.StatusServiceUnavailable || !strings.Contains(body, tc.want) ||
				!strings.Contains(body, `"error_code":"desktop_restart_failed"`) || strings.Contains(body, "fixture-cause") || strings.Contains(body, "relay") {
				t.Fatalf("%v -> %d %s", err, w.Code, body)
			}
		})
	}
}

func TestDesktopRestartEndpointReportsTheFailure(t *testing.T) {
	f := newDesktopSetupFixture(t)
	f.request(t, "POST", "/configure", `{}`, 200)
	f.api.RestartDesktopForTest = func(context.Context) error {
		return restartFailure("Claude Desktop is not installed on this computer.", nil)
	}
	w := f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "not installed") || strings.Contains(string(body), "relay") {
		t.Fatalf("reply: %s", body)
	}
	f.api.RestartDesktopForTest = func(context.Context) error { return errors.New("fixture-provider-secret") }
	w = f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	body, _ = io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), "could not be restarted") || strings.Contains(string(body), "fixture-provider-secret") {
		t.Fatalf("unknown failure reply: %s", body)
	}
}
