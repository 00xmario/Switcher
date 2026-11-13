// Package autostart starts Switcher when the user logs in to Linux.
//
// The server runs as a systemd user service, so it also runs for a login
// over SSH and restarts if it fails. The tray starts with the desktop
// session through an XDG autostart entry; on a computer without a systemd
// user manager the tray starts the server itself. On a Mac the menu bar app
// owns its own login item, and this package reports itself unsupported.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Unit is the systemd user service's name.
const Unit = "switcher.service"

// AlreadyRunningExit is the exit status of a server that found another one
// on its port. The service does not restart for it.
const AlreadyRunningExit = 75

// Status is what the UIs show.
type Status struct {
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
	// Service reports the systemd user service; Tray the autostart entry.
	Service bool   `json:"service"`
	Tray    bool   `json:"tray"`
	Message string `json:"message,omitempty"`
}

// Manager writes and removes the login items.
type Manager struct {
	goos       string
	configHome string // $XDG_CONFIG_HOME
	exe        string
	// systemctl runs systemctl --user; nil when there is no user manager.
	systemctl func(args ...string) error
}

// New manages login items for the binary at exe.
func New(exe string) *Manager {
	m := &Manager{goos: runtime.GOOS, exe: exe, configHome: configHome()}
	if path, err := exec.LookPath("systemctl"); err == nil {
		m.systemctl = func(args ...string) error {
			out, err := exec.Command(path, append([]string{"--user"}, args...)...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return m
}

// NewAt manages login items under configHome as goos would, running
// systemctl through the given function (nil: no user manager). Tests use it.
func NewAt(goos, configHome, exe string, systemctl func(args ...string) error) *Manager {
	return &Manager{goos: goos, configHome: configHome, exe: exe, systemctl: systemctl}
}

func configHome() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

func (m *Manager) unitPath() string { return filepath.Join(m.configHome, "systemd", "user", Unit) }
func (m *Manager) desktopPath() string {
	return filepath.Join(m.configHome, "autostart", "switcher-tray.desktop")
}

// Status reads the login items from disk; it runs nothing.
func (m *Manager) Status() Status {
	if m.goos != "linux" {
		return Status{Message: "On a Mac, Start at login is in the Switcher menu."}
	}
	if m.configHome == "" {
		return Status{Message: "No home directory to keep login items in."}
	}
	s := Status{Supported: true}
	_, err := os.Stat(m.unitPath())
	s.Service = err == nil
	_, err = os.Stat(m.desktopPath())
	s.Tray = err == nil
	s.Enabled = s.Service || s.Tray
	if s.Enabled && !s.Service {
		s.Message = "Switcher starts with your desktop session; this computer has no systemd user manager."
	}
	return s
}

// Set turns starting at login on or off. Turning it off leaves the running
// Switcher alone: it may be serving this very request.
func (m *Manager) Set(on bool) (Status, error) {
	if st := m.Status(); !st.Supported {
		return st, errors.New(st.Message)
	}
	if !on {
		var errs []error
		if m.systemctl != nil {
			if _, err := os.Stat(m.unitPath()); err == nil {
				if err := m.systemctl("disable", Unit); err != nil {
					errs = append(errs, err)
				}
			}
		}
		for _, path := range []string{m.unitPath(), m.desktopPath()} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		if m.systemctl != nil {
			_ = m.systemctl("daemon-reload")
		}
		return m.Status(), errors.Join(errs...)
	}
	if !filepath.IsAbs(m.exe) {
		return m.Status(), errors.New("cannot tell where the switcher binary is")
	}
	if err := writeFile(m.desktopPath(), desktopEntry(m.exe)); err != nil {
		return m.Status(), err
	}
	if m.systemctl == nil {
		return m.Status(), nil
	}
	if err := writeFile(m.unitPath(), unitFile(m.exe)); err != nil {
		return m.Status(), err
	}
	err := m.systemctl("daemon-reload")
	if err == nil {
		err = m.systemctl("enable", Unit)
	}
	if err != nil {
		// No user manager to enable it with (a container, or no D-Bus
		// session): the tray entry starts the server instead.
		os.Remove(m.unitPath())
		return m.Status(), nil
	}
	return m.Status(), nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// unitFile runs the server. Proxy variables a login shell may carry would
// send Switcher's own requests through its Desktop relay.
func unitFile(exe string) string {
	return `# Written by Switcher's Start at login; turn that off in Switcher instead of editing.
[Unit]
Description=Switcher: all your AI subscriptions in one place
Documentation=https://github.com/00xmario/switcher
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + systemdQuote(exe) + ` serve
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=` + fmt.Sprint(AlreadyRunningExit) + `
UnsetEnvironment=HTTPS_PROXY HTTP_PROXY ALL_PROXY NO_PROXY NODE_EXTRA_CA_CERTS https_proxy http_proxy all_proxy no_proxy

[Install]
WantedBy=default.target
`
}

// desktopEntry starts the tray with the desktop session.
func desktopEntry(exe string) string {
	return `[Desktop Entry]
Type=Application
Name=Switcher
Comment=Usage and switching for your AI accounts
Exec=` + desktopQuote(exe) + ` tray
Terminal=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
`
}

// systemdQuote quotes a path for ExecStart: specifiers and variables are
// escaped, so a path is only ever a path.
func systemdQuote(path string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(path) + `"`
}

// desktopQuote quotes a path for a desktop entry's Exec key.
func desktopQuote(path string) string {
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`, `%`, `%%`)
	return `"` + r.Replace(path) + `"`
}
