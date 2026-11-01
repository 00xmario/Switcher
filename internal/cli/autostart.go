package cli

import (
	"path/filepath"

	"switcher/internal/autostart"
)

const autostartHelp = `On Linux, start Switcher when you log in: a systemd user service runs the
server (also for a login over SSH) and the tray starts with your desktop.
On a Mac, use Start at login in the Switcher menu. This works whether or
not Switcher is running.

  switcher autostart        show whether Switcher starts at login
  switcher autostart on
  switcher autostart off    the running Switcher keeps running

To run Switcher right after boot, before anyone logs in: loginctl enable-linger
`

func runAutostart(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("autostart"), args, 0, 1)
	if err != nil {
		return err
	}
	exe := c.opt.Executable
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	m := c.opt.Autostart
	if m == nil {
		m = autostart.New(exe)
	}
	st := m.Status()
	if len(pos) == 1 {
		if pos[0] != "on" && pos[0] != "off" {
			return usagef("use on or off, not %q", pos[0])
		}
		if st, err = m.Set(pos[0] == "on"); err != nil {
			return err
		}
	}
	human := "Switcher does not start at login."
	switch {
	case !st.Supported:
		human = st.Message
	case st.Service:
		human = "Switcher starts at login (systemd user service switcher.service, and the tray with your desktop)."
	case st.Enabled:
		human = "Switcher starts with your desktop session."
	}
	c.result(st, human)
	return nil
}
