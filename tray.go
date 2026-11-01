package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"switcher/internal/config"
	"switcher/internal/settings"
	"switcher/internal/tray"
)

// runTray shows the Linux tray icon for the Switcher on port, starting that
// Switcher if it is not running. One tray runs per user.
func runTray(port int) int {
	lock, err := os.OpenFile(filepath.Join(config.Dir(), "tray.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil && unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		fmt.Fprintln(os.Stderr, "switcher tray: the Switcher tray is already running")
		return 0
	}
	exe := executablePath()
	launched, _ := os.Stat(exe)
	icon, _ := fs.ReadFile(webFS, "web/favicon.png")
	err = tray.Run(tray.Options{
		Client: &tray.Client{Base: fmt.Sprintf("http://127.0.0.1:%d", port), Token: func() string {
			token, _ := settings.New(config.Dir()).ReadDeviceToken()
			return token
		}},
		Start: func() error { return startInBackground(port) },
		Open: func(url string) {
			open := exec.Command("xdg-open", url)
			if open.Start() == nil {
				go open.Wait()
			}
		},
		Updated: func() {
			// An update replaced the binary: run the new one. The lock's
			// descriptor closes on exec, and the new tray takes it again.
			if now, err := os.Stat(exe); err == nil && launched != nil && !os.SameFile(now, launched) {
				_ = syscall.Exec(exe, os.Args, os.Environ())
			}
		},
	}, icon)
	if err != nil {
		fmt.Fprintln(os.Stderr, "switcher tray:", err)
		return 1
	}
	return 0
}
