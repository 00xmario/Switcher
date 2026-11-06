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
| Pairing without a code (your own Tailscale devices) | the add-on, or the Tailscale app's command line | the add-on, or the system `tailscale` command (`tailscaled`) |
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
menu shows every account with its usage and switches accounts (Claude also
switches Claude Code's login), and holds keep awake, **Use the accounts of
&lt;host&gt;**, **Update &lt;host&gt;**, **Hand back to the providers on this
computer**, **Start at login**, updates and Quit. Everything else is in the
dashboard, which the tray opens. The tray starts the server when it is not
running; **Quit Switcher** stops both.

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

## Tailscale without the add-on

With `tailscaled` running (the Tailscale package, kernel networking as it
installs by default), Switcher pairs your own devices without a code also
without the add-on. It runs `tailscale status --json` and `tailscale whois
--json`, read-only, from `PATH`, `/usr/bin/tailscale` or
`/usr/local/bin/tailscale`; both work for any user, so no `sudo` or operator
setting is needed. A connection counts only when it reached this computer's
own Tailscale address, and never one from this computer itself, so another
account on a shared Linux machine cannot pair with the Switcher on it without
a code. Tailscale vouches for devices, not for the accounts on them: whatever
can send traffic as one of your devices, such as another account or a
container on it, can pair from it with your hosts. If others use one of your
Tailscale devices, turn this off on your hosts with `switcher share tailnet
off`. With `tailscaled --tun=userspace-networking`, connections arrive from
loopback and carry no Tailscale identity, so pairing needs a code; the add-on
works either way.
Allow port 8788 on `tailscale0` if a firewall is on, for example
`sudo ufw allow in on tailscale0 to any port 8788 proto tcp`.

## What only a real Linux desktop can confirm

These were built and tested in Linux containers, with mocked services where
a desktop is needed. Check them once on the real machine:

- The tray icon and menu in your desktop (StatusNotifierItem host).
- Keep awake: that `systemd-inhibit --list` shows Switcher's lock and the
  computer stays awake; polkit may refuse inhibitor locks to a session that
  is not active (for example SSH only).
- The systemd user service at your next login.
- Desktop notifications for reset alerts.
- Avahi discovery between this computer and your Macs.
