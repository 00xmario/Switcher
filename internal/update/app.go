package update

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// appBundle returns the Switcher.app that contains exe, if any.
func appBundle(exe string) (string, bool) {
	i := strings.Index(exe, ".app/Contents/MacOS/")
	if i < 0 {
		return "", false
	}
	return exe[:i+len(".app")], true
}

// appOps keeps every disk image, file system and process effect of an app
// install at one boundary. Tests supply all of them.
type appOps struct {
	tempDir  func(dir, pattern string) (string, error)
	download func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error
	attach   func(ctx context.Context, image, mountpoint string) error
	detach   func(mountpoint string) error
	// version reads CFBundleShortVersionString of an app.
	version func(ctx context.Context, app string) (string, error)
	// verify checks the app's code signature, so a damaged copy is refused.
	verify    func(ctx context.Context, app string) error
	smoke     func(ctx context.Context, server string) ([]byte, error)
	copyApp   func(ctx context.Context, from, to string) error
	writable  func(dir string) bool
	rename    func(from, to string) error
	removeAll func(path string) error
	// adminSwap replaces current with from in a folder this user cannot
	// write, after macOS asks for an administrator password.
	adminSwap func(ctx context.Context, from, current string) error
	restart   func(bundle, exe string) error
}

// installApp downloads the release's disk image, checks it and puts its
// Switcher.app in place of the running one, then restarts Switcher.
//
// The new app is copied next to the current one first, so the swap itself is
// two renames on one file system. If the second rename fails, the first is
// undone and the running app stays as it was.
func (c *Checker) installApp(bundle, exe string, ops appOps) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	// This process lives on for a few seconds after a finished install,
	// until the relaunch stops it. Another install of the same release
	// would be cut off by that stop.
	if c.restarting() {
		return errors.New("the update is installed and Switcher is restarting")
	}
	tag := c.Latest()
	if tag == "" {
		return errors.New("no update known; check for updates first")
	}
	if !validTag.MatchString(tag) {
		return fmt.Errorf("refusing to install malformed release tag %q", tag)
	}
	if compare(tag, c.Current) <= 0 {
		return fmt.Errorf("release %s is not newer than current version %s", tag, c.Current)
	}
	latest := strings.TrimPrefix(tag, "v")
	if strings.Contains(bundle, "/AppTranslocation/") {
		return errors.New("macOS runs this Switcher from a temporary read-only copy. Move Switcher.app to Applications, open it from there and update again")
	}
	parent := filepath.Dir(bundle)
	writable := ops.writable(parent)
	if !writable && strings.HasPrefix(bundle, "/Volumes/") {
		return errors.New("Switcher runs from its disk image. Drag Switcher.app to Applications, open it from there and update again")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	work, err := ops.tempDir("", ".switcher-update-")
	if err != nil {
		return fmt.Errorf("prepare download: %w", err)
	}
	defer ops.removeAll(work)
	asset := "Switcher_" + latest + ".dmg"
	c.setStep("downloading", 0)
	installed := false
	defer func() {
		if !installed { // a finished install keeps reporting its restart
			c.setStep("", 0)
		}
	}()
	if err := ops.download(ctx, tag, asset, work, c.downloaded); err != nil {
		return err
	}
	c.setStep("checking", downloadShare)
	image := filepath.Join(work, asset)
	if err := verifyChecksum(image, asset); err != nil {
		return err
	}
	mount := filepath.Join(work, "mount")
	if err := os.Mkdir(mount, 0o700); err != nil {
		return fmt.Errorf("prepare disk image: %w", err)
	}
	if err := ops.attach(ctx, image, mount); err != nil {
		return fmt.Errorf("open disk image: %w", err)
	}
	defer ops.detach(mount)
	source := filepath.Join(mount, "Switcher.app")
	if err := checkApp(ctx, ops, source, latest); err != nil {
		return err
	}

	c.setStep("installing", installingShare)
	if !writable {
		if err := ops.adminSwap(ctx, source, bundle); err != nil {
			return fmt.Errorf("install update in %s: %w", parent, err)
		}
	} else if err := swapApp(ctx, ops, source, bundle, latest); err != nil {
		return err
	}
	log.Printf("update: installed Switcher %s, restarting", latest)
	c.setStep("restarting", 1)
	if err := ops.restart(bundle, exe); err != nil {
		return fmt.Errorf("Switcher %s is installed but did not restart; quit and open Switcher: %w", latest, err)
	}
	installed = true
	return nil
}

// checkApp accepts only a complete app of exactly the expected release.
func checkApp(ctx context.Context, ops appOps, app, latest string) error {
	if info, err := os.Stat(app); err != nil || !info.IsDir() {
		return errors.New("the disk image contains no Switcher.app")
	}
	version, err := ops.version(ctx, app)
	if err != nil {
		return fmt.Errorf("read the new app's version: %w", err)
	}
	if version != latest {
		return fmt.Errorf("the new app is version %q, not release %s", version, latest)
	}
	if err := ops.verify(ctx, app); err != nil {
		return fmt.Errorf("the new app's signature does not check out: %w", err)
	}
	smokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := ops.smoke(smokeCtx, filepath.Join(app, "Contents", "MacOS", "SwitcherServer"))
	if err != nil {
		return fmt.Errorf("the new app's server failed to start: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	if got := strings.TrimSpace(string(out)); got != "switcher "+latest {
		return fmt.Errorf("the new app's server reports %q, not release %s", got, latest)
	}
	return nil
}

func swapApp(ctx context.Context, ops appOps, source, bundle, latest string) error {
	parent := filepath.Dir(bundle)
	stage, err := ops.tempDir(parent, ".switcher-update-")
	if err != nil {
		return fmt.Errorf("prepare install: %w", err)
	}
	defer ops.removeAll(stage)
	staged := filepath.Join(stage, filepath.Base(bundle))
	if err := ops.copyApp(ctx, source, staged); err != nil {
		return fmt.Errorf("copy the new app: %w", err)
	}
	// The copy is what runs from now on; check it as well as the image.
	if err := checkApp(ctx, ops, staged, latest); err != nil {
		return err
	}
	// The backup stays in the same folder, so moving the running app aside
	// never changes its parent directory.
	backup := filepath.Join(parent, "."+filepath.Base(bundle)+".previous")
	if err := ops.removeAll(backup); err != nil {
		return fmt.Errorf("clear old backup: %w", err)
	}
	if err := ops.rename(bundle, backup); err != nil {
		return fmt.Errorf("move the current app aside: %w", err)
	}
	if err := ops.rename(staged, bundle); err != nil {
		if undo := ops.rename(backup, bundle); undo != nil {
			return errors.Join(fmt.Errorf("put the new app in place: %w", err),
				fmt.Errorf("restore failed; the previous app is at %s: %w", backup, undo))
		}
		return fmt.Errorf("put the new app in place: %w", err)
	}
	// The running processes keep their open files; only the folder goes.
	if err := ops.removeAll(backup); err != nil {
		log.Printf("update: could not remove %s: %v", backup, err)
	}
	return nil
}

func systemAppOps() appOps {
	return appOps{
		tempDir: os.MkdirTemp,
		download: func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
			return downloadRelease(ctx, tag, asset, dir, 500<<20, progress)
		},
		attach: func(ctx context.Context, image, mountpoint string) error {
			return run(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mountpoint, image)
		},
		detach: func(mountpoint string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := run(ctx, "/usr/bin/hdiutil", "detach", "-quiet", mountpoint); err != nil {
				return run(ctx, "/usr/bin/hdiutil", "detach", "-quiet", "-force", mountpoint)
			}
			return nil
		},
		version: func(ctx context.Context, app string) (string, error) {
			out, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-",
				filepath.Join(app, "Contents", "Info.plist")).Output()
			return strings.TrimSpace(string(out)), err
		},
		verify: func(ctx context.Context, app string) error {
			return run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", app)
		},
		smoke: func(ctx context.Context, server string) ([]byte, error) {
			return exec.CommandContext(ctx, server, "version").CombinedOutput()
		},
		copyApp: func(ctx context.Context, from, to string) error {
			// ditto keeps the bundle's symlinks, modes and signature intact.
			return run(ctx, "/usr/bin/ditto", from, to)
		},
		writable:  func(dir string) bool { return syscall.Access(dir, 0x2) == nil },
		rename:    os.Rename,
		removeAll: os.RemoveAll,
		adminSwap: adminSwap,
		restart:   restartApp,
	}
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if text := strings.TrimSpace(string(out)); text != "" {
			return fmt.Errorf("%s: %s (%w)", filepath.Base(name), text, err)
		}
		return fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return nil
}

// adminSwap installs into a folder only an administrator may change, such as
// /Applications for a standard user. macOS shows its own password prompt;
// Switcher never sees the password.
func adminSwap(ctx context.Context, from, current string) error {
	staged := current + ".switcher-new"
	backup := filepath.Join(filepath.Dir(current), "."+filepath.Base(current)+".previous")
	q := shellQuote
	script := fmt.Sprintf("/bin/rm -rf %s %s && /usr/bin/ditto %s %s && /bin/mv %s %s && "+
		"{ /bin/mv %s %s || { /bin/mv %s %s; exit 1; }; } && /bin/rm -rf %s",
		q(staged), q(backup), q(from), q(staged), q(current), q(backup),
		q(staged), q(current), q(backup), q(current), q(backup))
	apple := `do shell script "` + appleScriptEscape(script) + `" with prompt "Switcher wants to install its update." with administrator privileges`
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", apple).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return errors.New("the administrator password was not entered")
		}
		return fmt.Errorf("%s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func appleScriptEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// restartApp starts the new app. When the menu bar app started this server,
// both are stopped and the new app is opened, which starts its own server.
// A server started any other way restarts itself in place.
func restartApp(bundle, exe string) error {
	menu := filepath.Join(bundle, "Contents", "MacOS", "Switcher")
	if parent := os.Getppid(); parent > 1 && processPath(parent) == menu {
		return relaunchApp(bundle, parent, os.Getpid())
	}
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	return syscall.Exec(exe, os.Args, os.Environ())
}

func processPath(pid int) string {
	out, err := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// relaunchScript stops the old menu bar app and server, then opens the app.
// It runs in its own session, so it outlives both.
const relaunchScript = `bundle=$1
sleep 1
for pid in "$2" "$3"; do
	kill -TERM "$pid" 2>/dev/null
	i=0
	while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.2; i=$((i+1)); done
	kill -KILL "$pid" 2>/dev/null
done
exec /usr/bin/open "$bundle"
`

func relaunchApp(bundle string, menuPID, serverPID int) error {
	cmd := exec.Command("/bin/sh", "-c", relaunchScript, "switcher-relaunch", bundle,
		strconv.Itoa(menuPID), strconv.Itoa(serverPID))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
