package autostart

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func linuxManager(t *testing.T, systemctl func(args ...string) error) *Manager {
	t.Helper()
	return &Manager{goos: "linux", configHome: t.TempDir(), exe: "/home/mario/.local/bin/switcher", systemctl: systemctl}
}

func TestStartAtLoginWritesAndEnablesTheUserService(t *testing.T) {
	var calls []string
	m := linuxManager(t, func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil })
	if st := m.Status(); !st.Supported || st.Enabled {
		t.Fatalf("before: %+v", st)
	}
	st, err := m.Set(true)
	if err != nil || !st.Enabled || !st.Service || !st.Tray {
		t.Fatalf("on: %+v %v", st, err)
	}
	unit, _ := os.ReadFile(m.unitPath())
	for _, want := range []string{`ExecStart="/home/mario/.local/bin/switcher" serve`, "Restart=on-failure", "RestartPreventExitStatus=75", "UnsetEnvironment=HTTPS_PROXY", "WantedBy=default.target"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	entry, _ := os.ReadFile(m.desktopPath())
	if !strings.Contains(string(entry), `Exec="/home/mario/.local/bin/switcher" tray`) {
		t.Errorf("desktop entry:\n%s", entry)
	}
	if strings.Join(calls, ";") != "daemon-reload;enable switcher.service" {
		t.Fatalf("systemctl calls: %v", calls)
	}

	calls = nil
	st, err = m.Set(false)
	if err != nil || st.Enabled || st.Service || st.Tray {
		t.Fatalf("off: %+v %v", st, err)
	}
	if strings.Join(calls, ";") != "disable switcher.service;daemon-reload" {
		t.Fatalf("systemctl calls: %v", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "stop") {
			t.Fatal("turning it off stopped the running Switcher")
		}
	}
}

func TestWithoutAUserManagerTheTrayStartsSwitcher(t *testing.T) {
	m := linuxManager(t, func(args ...string) error { return errors.New("Failed to connect to bus") })
	st, err := m.Set(true)
	if err != nil || !st.Enabled || st.Service || !st.Tray || st.Message == "" {
		t.Fatalf("no user manager: %+v %v", st, err)
	}
	m = linuxManager(t, nil)
	if st, err := m.Set(true); err != nil || st.Service || !st.Tray {
		t.Fatalf("no systemctl: %+v %v", st, err)
	}
}

func TestStartAtLoginOnAMacIsTheMenuBarAppsJob(t *testing.T) {
	m := &Manager{goos: "darwin", configHome: t.TempDir(), exe: "/x"}
	if st := m.Status(); st.Supported {
		t.Fatalf("darwin: %+v", st)
	}
	if _, err := m.Set(true); err == nil {
		t.Fatal("wrote Linux login items on a Mac")
	}
}

func TestOddPathsStayOnePath(t *testing.T) {
	path := `/home/mario/My "Apps"/$HOME/100%/switcher`
	if got := systemdQuote(path); got != `"/home/mario/My \"Apps\"/$$HOME/100%%/switcher"` {
		t.Errorf("systemd: %s", got)
	}
	if got := desktopQuote(path); got != `"/home/mario/My \\"Apps\\"/\\$HOME/100%%/switcher"` {
		t.Errorf("desktop: %s", got)
	}
	m := &Manager{goos: "linux", configHome: t.TempDir(), exe: "switcher"}
	if _, err := m.Set(true); err == nil {
		t.Fatal("accepted a relative binary path")
	}
}
