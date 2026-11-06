# Switcher on Linux

Switcher runs on Linux (x86-64 and ARM64) with the same server, accounts,
routing, dashboard, phone page, sharing and command line as on a Mac. The
macOS menu bar app is replaced by a tray icon and the web dashboard.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/00xmario/switcher/main/install.sh | sh
```

On Linux the script downloads `switcher-server_<version>_linux_<arch>` from
the latest release, checks its SHA-256, installs it as `~/.local/bin/switcher`,
turns on **Start at login**, starts Switcher and, in a desktop session, opens
the tray. The dashboard is at <http://127.0.0.1:8787> (`switcher open`).
Over SSH, forward it with `ssh -L 8787:127.0.0.1:8787` first.

## What runs where

| | Mac | Linux |
|---|---|---|
| Main UI | Menu bar app + web dashboard | Web dashboard + tray (`switcher tray`) |
| Start at login | LaunchAgent, from the menu | systemd user service `switcher.service`, plus an XDG autostart entry for the tray |
| Keep awake | `caffeinate`, `pmset` for a closed lid (one-time sudoers rule) | `systemd-inhibit`: sleep, idle and, for a closed lid, lid-switch inhibitor locks; no setup |
| Battery guard | `pmset -g batt` | `/sys/class/power_supply` |
| Claude Code login | Keychain (`Claude Code-credentials`) | `~/.claude/.credentials.json` (0600), as Claude Code keeps it on Linux |
| Codex | `~/.codex/config.toml` | the same; Codex's own login stays in `auth.json` or, with `cli_auth_credentials_store`, the Secret Service keyring |
| Copilot import | `~/Library/Application Support/github-copilot/apps.json` | `$XDG_CONFIG_HOME/github-copilot/apps.json` |
| Claude Desktop routing | yes | the same `~/.claude/settings.json` mechanism; Anthropic ships no Linux Desktop, so this applies to community builds |
| Desktop restart and session sync | yes | no: restart Desktop yourself; sync needs macOS |
| Finding other Switchers | Bonjour (`dns-sd`) | Avahi (`avahi-browse`, `avahi-publish-service` from `avahi-utils`); without them, pair by address |
| Away from home (Tailscale add-on) | yes | yes, `switcher-tailnet_<version>_linux_<arch>` |
| Updates | Replaces Switcher.app | Replaces `~/.local/bin/switcher` and restarts in place; the tray restarts itself |
| Reset alerts | macOS notifications from the menu bar app | desktop notifications from the tray |

Switcher's own accounts live under `~/.switcher/` with `0600` permissions on
both systems, the same trust model as the CLIs. On Linux Switcher needs no
keyring of its own: Claude Code and Codex keep their logins in private files
by default, and Switcher writes Claude Code's login where Claude Code reads it.

## The tray

`switcher tray` puts Switcher's icon in the desktop's tray, through the
StatusNotifierItem protocol. KDE Plasma, Cinnamon, XFCE, LXQt and Ubuntu's
GNOME show it; on another GNOME, turn on the AppIndicator extension. The
tray starts the server when it is not running; **Quit Switcher** stops both.

**Open Switcher…**, the menu's first item, opens the Switcher panel: a small
window with the Mac menu's look, the same account cards as the phone page.
It shows each provider with its logo, every account with its plan, banked
resets and what each usage window has left, and switches accounts, refreshes
usage and spends banked resets. It opens as an app window in the first of
Google Chrome, Chromium or Microsoft Edge it finds, otherwise in the default
browser. Clicking the tray icon opens it too where the desktop activates
tray icons on a click (KDE); there the menu is on the right button. GNOME's
AppIndicator opens the menu on a click and the panel on a double click. The
panel is the page `/panel` of the dashboard, only for browsers on this
computer and behind the dashboard's password if one is set.

The menu itself follows the Mac menu as closely as a tray menu can: each
provider's name with its logo, then its accounts, the one in use checked
(for Claude, Claude Code's login). Under each account, dim lines show what
each usage window has left, with a short bar and the time to its reset, for
example `Weekly: ▰▰▱▱▱ 31% left · ↻ 2d 8h`, and notes such as **Out of usage
· back in 1h 0m** or **Sign in again**. The bars follow **Usage bars** in the
dashboard's Settings → General → Tray. Clicking an account switches to it
(Claude also switches Claude Code's login). The menu also holds keep awake,
**Use the accounts of &lt;host&gt;**, **Update &lt;host&gt;**, **Hand back
to the providers on this computer**, **Start at login**, the dashboard,
updates and Quit.

The tray and the panel show changes within a second or two, including an
account switched from another computer: they wait for the server to report
a change rather than reading on a timer. The menu changes in place, so it
stays open while it updates. With a Switcher from before this, they read
the state every few seconds instead.

## Start at login

**Start at login** (dashboard: Settings → General; `switcher autostart on`;
the tray) writes `~/.config/systemd/user/switcher.service` and enables it, so
the server runs whenever you are logged in, including over SSH, and restarts
if it fails. The tray starts with the desktop session from
`~/.config/autostart/switcher-tray.desktop`. Turning it off disables and
removes both and leaves the running Switcher alone. To start Switcher right
after boot, before anyone logs in, also run `loginctl enable-linger`.

## Firewall

Sharing listens on port 8788. If `ufw` or `firewalld` is on, allow it from
your home network, for example `sudo ufw allow from 192.168.0.0/16 to any port 8788 proto tcp`.
Switcher itself refuses connections from outside private, link-local and
Tailscale addresses either way.

## What only a real Linux desktop can confirm

These were built and tested in Linux containers, with mocked services where
a desktop is needed. Check them once on the real machine:

- The tray icon and menu in your desktop (StatusNotifierItem host): the
  provider logos, the bars (they need a font with ▰ and ▱, such as DejaVu
  Sans), the indented usage lines, and that the menu stays open while it
  updates.
- Clicking the tray icon, and the panel's app window and its size.
- Keep awake: that `systemd-inhibit --list` shows Switcher's lock and the
  computer stays awake; polkit may refuse inhibitor locks to a session that
  is not active (for example SSH only).
- The systemd user service at your next login.
- Desktop notifications for reset alerts.
- Avahi discovery between this computer and your Macs.
