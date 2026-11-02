# Share between Macs

One Mac (the host) keeps your accounts. Your other Macs (clients) use them
through the host's Switcher. Accounts, OAuth tokens and refreshes stay on the
host, and every request to Claude, Codex and the other providers leaves from
the host. To Anthropic and OpenAI it looks like one machine using the account.

Keeping tokens on one machine is also what keeps the accounts healthy: OAuth
refresh tokens rotate, so two Switchers refreshing the same account would sign
each other out.

## Set up

1. On the host: **Settings → Share between Macs → Share this Switcher**, then
   **Pair a Mac**. A code such as `K7QF-M2XP` is valid for ten minutes.
2. On the client: the same card lists Switchers found on your network. Click
   **Connect**, enter the code and connect. Away from home, enter the host's
   Tailscale name or address instead.

macOS may ask once whether Switcher may accept incoming connections on the
host; allow it.

## Away from home

At home the Macs find each other on your network. To use the host from
anywhere, turn on **Away from home** in the same card on both Macs:

1. Switcher downloads its Tailscale add-on (about 19 MB) for your version from
   the GitHub release and checks its checksum. The app itself contains no
   Tailscale code, so nothing is downloaded unless you turn this on.
2. Click **Sign in with Tailscale** and sign in with a free Tailscale account,
   the same one on every Mac.
3. The Macs now reach each other over Tailscale wherever they are. Switchers
   in your tailnet also appear in the list of hosts to connect to.

The add-on joins your tailnet as its own device (`switcher-<mac name>`); it
installs no VPN and needs no admin password, and only Switcher's traffic uses
it. It accepts tailnet connections only while this Mac shares its Switcher,
and, with phone access on, serves the phone page to your own devices (see
[phone](phone.md)).
Tailscale's coordination service sees which devices are connected, not the
traffic, which is end-to-end encrypted. Switcher checks the add-on against the
checksum built into the app before every start and restarts it if it stops.
**Remove add-on and sign out** deletes it again. If you already run the Tailscale app, Switcher also works with that
without the add-on.

## What follows the host

- Account cards, usage and the menu bar show the host's accounts. Switching an
  account on the client switches it on the host, exactly as switching on the
  host would: the account every Mac uses, and the host's own Claude Code
  login. If the host's Claude Code cannot switch (for example while its
  Keychain is locked), the account still switches and the client says why
  Claude Code did not follow. Accounts are added and signed in again on the
  host.
- Codex CLI and every provider proxy on `127.0.0.1:8787`, and the T3 Code hub.
  Their configuration does not change.
- Claude Code and Claude Desktop, once **Connect Claude Desktop** is done on the
  client: their Claude requests go to the host, which uses its selected Claude
  account (or the account picked for a conversation). Claude Code on the client
  still needs to be logged in to some account to start.

The client's own settings, CLI setup and usage history stay local. While
connected, the client does not refresh or poll its own accounts. **Disconnect**
returns the client to its own accounts.

## When the host stops working

If the host is down or unreachable, the client does not have to unpair. Turn
off **Use the accounts of &lt;host&gt;** (Settings → Share between Macs, the
**Work without it** button on the Accounts tab, the menu bar or tray, or
`switcher use-host off`). The client then does everything itself: its own
accounts and usage checks, and Codex, Claude Code and Claude Desktop routed
with its own accounts. Nothing is sent to the host. The pairing stays, also
across restarts; turn the switch back on (or `switcher use-host on`) to use
the host again. While paired, sharing onward stays off either way.

## Updating the host

A client shows which Switcher its host runs. When an update is out, **Update
&lt;host&gt;** (Settings → Share between Macs, the menu bar or tray, or
`switcher update-host --yes`) has the host install the newest release and
restart, without going to it. The host's tools, and every Mac that uses it,
pause for a moment; nothing changes on the client. It works while the client
works without its host too. A host whose Switcher.app sits in a folder only
an administrator can change refuses, since nobody is at it to enter the
password; update that one on the host. A host before Switcher 1.21.0 does
not take updates from its clients yet: update it on the host once.

## Network and security

- The host listens on port 8788 with its own certificate. The client pins that
  certificate during pairing, so it reaches the host by its Bonjour name, LAN
  address or Tailscale address without reconfiguration. The client refreshes
  the host's addresses every few minutes, so a host that joins Tailscale
  later, or gets a new LAN address, needs no new pairing.
- The host only accepts connections from itself, private and link-local
  addresses, and Tailscale. Connections from the internet are dropped before
  TLS, even if a router forwards the port or IPv6 reaches the Mac directly.
- Pairing proves the code in both directions, bound to the host certificate
  the client sees: a device in the middle cannot pair with its own
  certificate, and a fake host cannot learn the code or get the client to
  trust it. Five wrong codes invalidate a code, and a network address that
  keeps guessing is refused for ten minutes.
- Only paired Macs are served, with their own revocable token, and only for
  account views and actions, provider proxies, the hub, Claude inference and
  updating the host. An update installs only the newest official release,
  checked against its published SHA-256 (and its signature on a Mac), and a
  host never waits on a password prompt for it. Tokens are never sent to
  clients.
- A Mac cannot share and use another Switcher at the same time.
- Over the internet, use Away from home (or the Tailscale app); opening port
  8788 on your router would not work and is not needed.

## Implementation

`internal/remote` holds the host listener, pairing, device tokens, Bonjour
(via macOS `dns-sd`), the client's forwarding and the add-on manager. The
add-on is `cmd/switcher-tailnet`, a separate Go module built on `tsnet`: it
forwards its tailnet port 8788 to the local host listener and offers Switcher
a token-protected loopback CONNECT proxy and status endpoint. It exits when
Switcher closes its stdin. The client's server forwards
`/api/accounts/…`, `/api/providers/…`, `/api/usage/refresh`, provider paths and
`/v0/management/…`, merges `/api/state` with its local fields, and gives the
Desktop relay a remote inference hook. The host serves `/remote/anthropic/…`
through `desktoprelay.Manager.ServeAccount` and adds its own management key to
hub requests.
