package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const shareHelp = `One Mac (the host) keeps the accounts; your other Macs use them through it.

  switcher share on        share this Mac's Switcher on your network
  switcher share code      show a pairing code, valid for ten minutes
  switcher share devices   list paired Macs
  switcher share revoke <id|name> --yes
  switcher share tailnet on|off
                           let your own Tailscale devices connect without a
                           code (on by default), or make them use one
  switcher share off

On the other Mac: switcher connect <host address>. Over Tailscale, your own
devices need no code; anything else needs --code <code>.
`

const connectHelp = `Pairs this computer with a Switcher that shares its accounts.

Over Tailscale, your own devices connect without a code: both computers'
Tailscale must say they belong to the same Tailscale user, in the same
tailnet, untagged and not shared in, and the host must allow it (switcher
share tailnet). Give the host's Tailscale name or address, as switcher
discover lists it. Anything else needs a code from the host: --code CODE,
shown there with switcher share code.

Already using another host? Add --yes to switch: this computer pairs with the
new host first and only then stops using the old one, so it keeps the old
host if the new one cannot be paired.
`

func runShare(c *ctx, args []string) error {
	fs := c.flags("share")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 0, 2)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) > 0 {
		action = pos[0]
	}
	switch action {
	case "status", "devices":
		if len(pos) > 1 {
			return usagef("unexpected argument %q", pos[1])
		}
	case "tailnet":
		if len(pos) > 1 {
			if pos[1] != "on" && pos[1] != "off" {
				return usagef("use on or off, not %q", pos[1])
			}
			if err := c.cl.call(http.MethodPost, "/api/remote/host/tailnet", map[string]bool{"enabled": pos[1] == "on"}, nil); err != nil {
				return err
			}
		}
	case "on", "off":
		if err := c.cl.call(http.MethodPost, "/api/remote/host", map[string]bool{"enabled": action == "on"}, nil); err != nil {
			return err
		}
	case "code":
		var code struct {
			Code      string `json:"code"`
			ExpiresAt string `json:"expires_at"`
		}
		if err := c.cl.call(http.MethodPost, "/api/remote/host/pairing", nil, &code); err != nil {
			return err
		}
		r, err := c.remote()
		if err != nil {
			return err
		}
		address := ""
		if r.Host != nil && len(r.Host.LAN) > 0 {
			address = r.Host.LAN[0]
		}
		c.result(map[string]any{"code": code.Code, "expires_at": code.ExpiresAt, "host": r.Host},
			fmt.Sprintf("Pairing code: %s (valid for ten minutes)\nOn the other %s: switcher connect %s --code %s", code.Code, machine(), orDefault(address, "<this "+machine()+"'s address>"), code.Code))
		return nil
	case "revoke":
		if len(pos) < 2 {
			return usagef("missing device; see: switcher share devices")
		}
		r, err := c.remote()
		if err != nil {
			return err
		}
		if r.Host == nil {
			return errors.New("sharing is unavailable on this Mac")
		}
		id := ""
		for _, d := range r.Host.Devices {
			if d.ID == pos[1] || strings.EqualFold(d.Name, pos[1]) {
				if id != "" {
					return fmt.Errorf("more than one Mac is named %q; use its id", pos[1])
				}
				id = d.ID
			}
		}
		if id == "" {
			return fmt.Errorf("no paired Mac matches %q", pos[1])
		}
		if !*yes {
			return usagef("that Mac has to pair again to use this one; add --yes to confirm")
		}
		if err := c.cl.call(http.MethodDelete, "/api/remote/host/devices/"+url.PathEscape(id), nil, nil); err != nil {
			return err
		}
	default:
		return usagef("unknown action %q; use on, off, code, devices, revoke or tailnet", action)
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	if c.json {
		c.emit(map[string]any{"host": r.Host, "client": r.Client})
		return nil
	}
	lines := []string{"Sharing: " + sharingText(r)}
	if r.Host != nil && r.Host.Enabled {
		addresses := append(append([]string{}, r.Host.LAN...), r.Host.Tailscale...)
		if len(addresses) > 0 {
			lines = append(lines, "Reachable at "+strings.Join(addresses, ", "))
		}
		if r.Host.TailnetPairing {
			lines = append(lines, "Your own Tailscale devices connect without a code (switcher share tailnet off to require one)")
		} else {
			lines = append(lines, "Every Mac needs a code, also your own Tailscale devices (switcher share tailnet on to change that)")
		}
		for _, d := range r.Host.Devices {
			via := ""
			if d.Tailscale != "" {
				via = ", paired via Tailscale as " + d.Tailscale
			}
			lines = append(lines, fmt.Sprintf("  %s (%s%s)", d.Name, d.ID, via))
		}
		if action == "on" {
			lines = append(lines, "Pair a Mac with: switcher share code, or from your own Tailscale devices: switcher connect <this Mac's Tailscale name>")
		}
	} else if action == "tailnet" && r.Host != nil {
		state := "need a code"
		if r.Host.TailnetPairing {
			state = "connect without a code"
		}
		lines = append(lines, "Your own Tailscale devices "+state+" once sharing is on")
	}
	fmt.Fprintln(c.out, strings.Join(lines, "\n"))
	return nil
}

func runConnect(c *ctx, args []string) error {
	fs := c.flags("connect")
	code := fs.String("code", "", "")
	port := fs.Int("port", 8788, "")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	if r.Host != nil && r.Host.Enabled {
		return fmt.Errorf("this %s shares its own Switcher, and a %s that shares cannot use another one at the same time; turn sharing off first: switcher share off", machine(), machine())
	}
	if r.Client.Connected && !*yes {
		return usagef("this %s uses %s; add --yes to switch to %s (it pairs with %s first, and keeps %s if that fails)", machine(), r.Client.HostName, pos[0], pos[0], r.Client.HostName)
	}
	var answer struct {
		Client struct {
			HostName string `json:"host_name"`
			Address  string `json:"address"`
		} `json:"client"`
	}
	body := map[string]any{"address": pos[0], "port": *port, "code": *code, "switch": r.Client.Connected}
	if err := c.cl.call(http.MethodPost, "/api/remote/connect", body, &answer); err != nil {
		var refused *apiError
		if errors.As(err, &refused) && refused.NeedsCode {
			return fmt.Errorf("%s\nShow a code on the other %s with: switcher share code, then run: switcher connect %s --code CODE", err, machine(), pos[0])
		}
		return err
	}
	previous := ""
	if r.Client.Connected && r.Client.HostName != answer.Client.HostName {
		previous = fmt.Sprintf(" It no longer uses %s.", r.Client.HostName)
	}
	c.result(answer, fmt.Sprintf("Connected to %s. This %s now uses its accounts.%s", answer.Client.HostName, machine(), previous))
	return nil
}

func runDisconnect(c *ctx, args []string) error {
	fs := c.flags("disconnect")
	yes := fs.Bool("yes", false, "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	if !*yes {
		return usagef("this %s forgets the other Switcher and needs a new pairing code to use it again; add --yes to confirm", machine())
	}
	if err := c.cl.call(http.MethodPost, "/api/remote/disconnect", nil, nil); err != nil {
		return err
	}
	c.result(map[string]bool{"connected": false}, "Disconnected. This "+machine()+" uses its own accounts again.")
	return nil
}

const useHostHelp = `A Switcher paired with a host uses the host's accounts. If the host stops
working, turn that off: this Switcher then uses its own accounts, refreshes
them and routes its tools itself, and stays paired.

  switcher use-host        show whether the host is in use
  switcher use-host off    work without the host, keeping the pairing
  switcher use-host on     use the host again
`

func runUseHost(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("use-host"), args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		if pos[0] != "on" && pos[0] != "off" {
			return usagef("use on or off, not %q", pos[0])
		}
		r, err := c.remote()
		if err != nil {
			return err
		}
		if !r.Client.Connected {
			return errors.New("this Switcher is not paired with another one; pair with: switcher connect <address>")
		}
		if err := c.cl.call(http.MethodPost, "/api/remote/use-host", map[string]bool{"enabled": pos[0] == "on"}, nil); err != nil {
			return err
		}
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	answer := map[string]any{"paired": r.Client.Connected, "use_host": r.Client.Connected && !r.Client.LocalOnly, "host": r.Client.HostName}
	human := "This Switcher is not paired with another one."
	switch {
	case r.Client.Connected && r.Client.LocalOnly:
		human = fmt.Sprintf("Not using %s: this Switcher uses its own accounts and stays paired.", r.Client.HostName)
	case r.Client.Connected:
		human = fmt.Sprintf("Using the accounts of %s.", r.Client.HostName)
	}
	c.result(answer, human)
	return nil
}

const updateHostHelp = `Shows which Switcher the paired host runs and whether an update is out.
With --yes the host installs the newest release and restarts, so it is
updated without going to it. The host's tools, and every computer that uses
it, pause for a moment; nothing changes on this computer. It also works
while this Switcher works without its host.`

func runUpdateHost(c *ctx, args []string) error {
	fs := c.flags("update-host")
	yes := fs.Bool("yes", false, "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	type hostUpdate struct {
		Host      string `json:"host"`
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"update_available"`
		Remote    bool   `json:"remote_update"`
	}
	var answer struct {
		HostUpdate hostUpdate `json:"host_update"`
	}
	if err := c.cl.call(http.MethodPost, "/api/remote/host-update", map[string]string{"action": "check"}, &answer); err != nil {
		return err
	}
	u := answer.HostUpdate
	latest := strings.TrimPrefix(u.Latest, "v")
	runs := "an older Switcher"
	if u.Current != "" {
		runs = "Switcher " + u.Current
	}
	if !u.Remote {
		c.result(u, fmt.Sprintf("%s runs %s, which takes updates only on itself. Update it on %s once; after that, update it from here.", u.Host, runs, u.Host))
		return nil
	}
	if !u.Available {
		c.result(u, fmt.Sprintf("%s runs Switcher %s, the newest release.", u.Host, u.Current))
		return nil
	}
	if !*yes {
		c.result(u, fmt.Sprintf("%s runs Switcher %s; %s is out. Update it with: switcher update-host --yes", u.Host, u.Current, latest))
		return nil
	}
	err := c.cl.call(http.MethodPost, "/api/remote/host-update", map[string]string{"action": "install"}, nil)
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		// The host downloads before it answers; the update goes on.
		err = nil
	}
	if err != nil {
		return err
	}
	c.result(map[string]any{"host": u.Host, "installing": latest}, fmt.Sprintf("%s is installing Switcher %s and restarts when done. Check with: switcher update-host", u.Host, latest))
	return nil
}

func runDiscover(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("discover"), args, 0, 0); err != nil {
		return err
	}
	var answer struct {
		Hosts []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
			Port    int    `json:"port"`
		} `json:"hosts"`
	}
	if err := c.cl.call(http.MethodGet, "/api/remote/discover", nil, &answer); err != nil {
		return err
	}
	if c.json {
		c.emit(answer)
		return nil
	}
	if len(answer.Hosts) == 0 {
		fmt.Fprintln(c.out, "No sharing Switcher found. Turn on sharing on the other "+machine()+" (switcher share on), or connect by its address.")
		return nil
	}
	var rows [][]string
	for _, h := range answer.Hosts {
		rows = append(rows, []string{h.Name, h.Address})
	}
	fmt.Fprintln(c.out, table(rows)+"\n\nConnect with: switcher connect <address>. Your own devices on Tailscale need no code; others need --code <code from the other "+machine()+">.")
	return nil
}

func runAway(c *ctx, args []string) error {
	fs := c.flags("away")
	noWait := fs.Bool("no-wait", false, "")
	yes := fs.Bool("yes", false, "")
	timeout := fs.Duration("timeout", 15*time.Minute, "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) == 1 {
		action = pos[0]
	}
	switch action {
	case "status":
	case "on":
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet", map[string]bool{"enabled": true}, nil); err != nil {
			return err
		}
	case "off":
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet", map[string]bool{"enabled": false}, nil); err != nil {
			return err
		}
	case "remove":
		if !*yes {
			return usagef("this signs this Mac out of Tailscale and deletes the add-on; add --yes to confirm")
		}
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet/remove", nil, nil); err != nil {
			return err
		}
	default:
		return usagef("unknown action %q; use on, off or remove", action)
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	if r.Tailnet == nil {
		return errors.New("the Tailscale add-on is unavailable on this Switcher")
	}
	if action == "on" && !*noWait {
		// Download, then sign-in: show the link once and wait for it.
		shown := ""
		err = wait(*timeout, time.Second, func() (bool, error) {
			if r, err = c.remote(); err != nil {
				return false, err
			}
			t := r.Tailnet
			switch {
			case t == nil || !t.Enabled:
				return false, errors.New("the add-on was turned off")
			case t.Error != "" && !t.Downloading && !t.Running:
				return false, errors.New(t.Error)
			case t.State == "Running":
				return true, nil
			case t.State == "NeedsLogin" && t.AuthURL != "" && t.AuthURL != shown:
				shown = t.AuthURL
				if c.json {
					c.emit(map[string]any{"event": "sign_in", "auth_url": t.AuthURL})
				}
				c.printf("Sign in with Tailscale (the same account on all your Macs):\n\n  %s\n\nWaiting…\n", t.AuthURL)
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	out := map[string]any{"tailnet": r.Tailnet}
	if action == "on" && !*noWait {
		out["event"] = "done"
	}
	c.result(out, "Away from home: "+awayText(r))
	return nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
