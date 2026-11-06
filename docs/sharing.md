# Share between Macs

One Mac (the host) keeps your accounts. Your other Macs (clients) use them
through the host's Switcher. Accounts, OAuth tokens and refreshes stay on the
host, and every request to Claude, Codex and the other providers leaves from
the host. To Anthropic and OpenAI it looks like one machine using the account.

Keeping tokens on one machine is also what keeps the accounts healthy: OAuth
refresh tokens rotate, so two Switchers refreshing the same account would sign
each other out.

## Set up

1. On the host: **Settings → Share between Macs → Share this Switcher**.
2. On the client: the same card lists the Switchers found on your network and
   in your tailnet. Click **Connect**.
   - Over Tailscale, your own Macs connect without a code: if both Macs are
     signed in to the same Tailscale account, the client is ready right away.
   - Otherwise the host asks for a code: click **Pair a Mac** on the host and
     enter the code it shows, such as `K7QF-M2XP`, valid for ten minutes.
   Away from home, enter the host's Tailscale name or address instead.

macOS may ask once whether Switcher may accept incoming connections on the
host; allow it.

### Without a code, over Tailscale

A Mac signed in to the same Tailscale account as the host, in the same
tailnet, pairs without a code: install Switcher, turn on **Away from home**
(or use the Tailscale app), pick the host and connect. No provider logins are
needed on it. A host found on your network that is in your tailnet too is
paired over Tailscale; afterwards the client uses whichever of its addresses
answers first, as always. The host decides with Tailscale's word on who is
calling, never with anything the other Mac sends:

- Both sides check. The host asks Tailscale which device is behind the
  connection; the client asks its own Tailscale which device it reached
  before it sends anything, and pins the certificate presented on that same
  connection. The client also refuses its own computer, and a device that
  answers under another Tailscale name than the one it asked for.
- Only the same Tailscale user's own devices count, compared by Tailscale's
  stable user and device ids: not another user of a shared tailnet, not a
  tagged device (it belongs to no user), not a device shared in from another
  tailnet, not an expired one, and not one that routes traffic for other
  machines (a subnet router, exit node or app connector), since anyone behind
  it would look like it. A connection from the host itself does not count
  either: any account on that computer can make one.
- Everything else uses the code, as before. A refusal looks exactly like a
  wrong code, and attempts are limited per device and per address.
- Tailscale vouches for devices, not for the accounts and programs on them.
  Anything that can send traffic as one of your devices (another account on
  it, a container or virtual machine behind it) can pair from it, and
  anything listening on port 8788 of one of your devices can pose as a host
  to your other devices. Devices added with an untagged auth key are yours
  too: a CI runner, or a Docker container started with `TS_AUTHKEY`, can
  pair without a code. Give such devices a tag (for example `tag:ci`), which
  takes them out. If other people use one of your Tailscale devices, turn
  this off on your hosts and pair with codes.

On the host, **Let my own Tailscale devices connect without a code** (under
**Share this Switcher**; `switcher share tailnet on|off`) decides. It is on
for a Mac that starts sharing with this version. A Mac that shared before
keeps it off after the update, since its owner never chose it, and offers it
once in that card (**Turn on** or **Not now**; `switcher share` says so too).
Off, every Mac needs a code. A Mac paired this way is listed as **Paired via
Tailscale** and is removed like any other. If the same Switcher on the same
device pairs again, its new pairing replaces the old one.

## Switching hosts

With more than one Mac that shares, a client lists the others under **Other
Switchers**. **Use this one** asks once, then pairs with the new host first
(without a code over Tailscale, or with the code it asks for) and only then
stops using the old one, which also drops the client's pairing. If the new
host cannot be reached or refuses, the client keeps using the old host. On
the command line: `switcher connect <other host> --yes`.

A Mac that shares cannot use another Switcher at the same time: it would pass
accounts in a circle. Turn off sharing on it first.

## Away from home

At home the Macs find each other on your network. To use the host from
anywhere, turn on **Away from home** in the same card on both Macs:

1. Switcher downloads its Tailscale add-on (about 19 MB) for your version from
   the GitHub release and checks its checksum. The app itself contains no
   Tailscale code, so nothing is downloaded unless you turn this on.
2. Click **Sign in with Tailscale** and sign in with a free Tailscale account,
   the same one on every Mac.
3. The Macs now reach each other over Tailscale wherever they are. Switchers
   in your tailnet also appear in the list of hosts to connect to, and your
   own Macs connect to them without a code.

The add-on joins your tailnet as its own device (`switcher-<mac name>`); it
installs no VPN and needs no admin password, and only Switcher's traffic uses
it. It accepts tailnet connections only while this Mac shares its Switcher,
and, with phone access on, serves the phone page to your own devices (see
[phone](phone.md)).
Tailscale's coordination service sees which devices are connected, not the
traffic, which is end-to-end encrypted. Switcher checks the add-on against the
checksum built into the app before every start and restarts it if it stops.
**Remove add-on and sign out** deletes it again. If you already run the Tailscale app, Switcher also works with that
without the add-on: it finds hosts whose add-on is in your tailnet, and asks
the Tailscale app, read-only (`tailscale status` and `tailscale whois`), who
is at the other end when pairing without a code.

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
- Pairing without a code rests on Tailscale's device authentication instead
  (see above). The host learns who is calling only from Tailscale: for a
  connection through its add-on, from the add-on, which marks each
  connection it forwards with a random id the host asks about; for one
  through the Tailscale app, from `tailscale whois`, and only for a
  connection that reached the host's own Tailscale address. A connection from
  the local network has no Tailscale identity, whatever it claims. The client
  checks the host the same way and pins the certificate of that very
  connection. Ten attempts per device and per address in ten minutes.
- Only paired Macs are served, with their own revocable token, and only for
  account views and actions, provider proxies, the hub, Claude inference,
  updating the host and dropping their own pairing, which a client does when
  it disconnects or switches to another host. An update installs only the newest official release,
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
Switcher closes its stdin.

Pairing without a code: each connection the add-on forwards starts with
`SWITCHER-TAILNET <random id>` before TLS, and the add-on keeps the id with
the tailnet address behind it while the connection is open (it forgets it
before the connection closes). The host's `POST /remote/pair/tailnet` asks the
add-on's `GET /whois?conn=<id>&from=<loopback address>`, which answers only
for a live id from that same port, with what its node's Tailscale says
(`internal/remote/tailpair.go`, `cmd/switcher-tailnet/identity.go`). A loopback
address alone proves nothing: any program on the Mac can connect from
loopback, and a closed connection's port can be reused at once. A CONNECT with
`X-Switcher-Whois: 1` gets the reached node's identity back in its answer, so
the client checks the very tunnel it then pairs over. Without the add-on,
`internal/remote/identity.go` runs the Tailscale command line, read-only, with
a five-second limit and bounded output. The decision itself
(`notOwnDevice`, `hostAdmits`) is one function on both sides. The client's server forwards
`/api/accounts/…`, `/api/providers/…`, `/api/usage/refresh`, provider paths and
`/v0/management/…`, merges `/api/state` with its local fields, and gives the
Desktop relay a remote inference hook. The host serves `/remote/anthropic/…`
through `desktoprelay.Manager.ServeAccount` and adds its own management key to
hub requests.
