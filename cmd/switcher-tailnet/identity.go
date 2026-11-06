package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/tailscale/apitype"
)

// Pairing without a code. Only this add-on sees the tailnet side of the
// connections it carries, so Switcher asks it what Tailscale says about
// them: for a connection the add-on forwarded to the host listener, which
// device is behind it, and for a tunnel Switcher opened through it, which
// node the tunnel reached. Switcher decides; the add-on only reports.
// These names and shapes match internal/remote.

// preamble goes ahead of every connection forwarded to the host listener,
// with the connection's random id and a newline, before any byte from the
// tailnet. Switcher asks about the connection by that id, which no other
// program on this computer can know, so a loopback port reused by another
// program never inherits a device's identity.
const preamble = "SWITCHER-TAILNET "

// identityHeader carries the identity of a tunnel's far end in the answer
// to a CONNECT that asks for it with whoisHeader.
const (
	whoisHeader    = "X-Switcher-Whois"
	identityHeader = "X-Switcher-Identity"
)

type tailnetSelf struct {
	ID     string   `json:"id"`
	User   int64    `json:"user"`
	Tagged bool     `json:"tagged"`
	Suffix string   `json:"suffix"`
	IPs    []string `json:"ips"`
}

type tailnetPeer struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	User    int64    `json:"user"`
	Tagged  bool     `json:"tagged"`
	Shared  bool     `json:"shared"`
	Limited bool     `json:"limited"`
	Routes  bool     `json:"routes"`
	IPs     []string `json:"ips"`
	Addr    string   `json:"addr"`
}

type tailnetIdentity struct {
	Self tailnetSelf `json:"self"`
	Peer tailnetPeer `json:"peer"`
}

// describe is what Tailscale says about the node at addr, seen from self.
func describe(self selfInfo, who *apitype.WhoIsResponse, addr string) (tailnetIdentity, bool) {
	if who == nil || who.Node == nil || self.id.IsZero() {
		return tailnetIdentity{}, false
	}
	n := who.Node
	id := tailnetIdentity{
		Self: tailnetSelf{ID: string(self.id), User: int64(self.user), Tagged: self.tagged, Suffix: self.suffix, IPs: []string{}},
		Peer: tailnetPeer{ID: string(n.StableID), Name: strings.TrimSuffix(n.Name, "."), User: int64(n.User), Tagged: n.IsTagged(),
			Shared: !n.Sharer.IsZero() || n.IsJailed, Limited: n.Expired || n.IsWireGuardOnly, IPs: []string{}, Addr: addr},
	}
	for _, ip := range self.ips {
		id.Self.IPs = append(id.Self.IPs, ip.Unmap().String())
	}
	own := map[netip.Prefix]bool{}
	for _, p := range n.Addresses {
		own[p.Masked()] = true
		if p.IsSingleIP() {
			id.Peer.IPs = append(id.Peer.IPs, p.Addr().Unmap().String())
		}
	}
	// A node that routes for other machines, or may send from addresses
	// beyond its own, can carry anyone's traffic.
	id.Peer.Routes = len(n.PrimaryRoutes) > 0 || n.Hostinfo.Valid() && n.Hostinfo.RoutableIPs().Len() > 0
	for _, p := range n.AllowedIPs {
		if !own[p.Masked()] {
			id.Peer.Routes = true
		}
	}
	return id, true
}

// forwarded remembers the connections passed to the host listener while
// they are open, by the id each one's preamble carried.
type forwarded struct {
	mu    sync.Mutex
	conns map[string]forwardedConn
}

type forwardedConn struct {
	local  netip.AddrPort // the loopback address the add-on dialed from
	remote string         // the tailnet address of the device behind it
}

func newForwarded() *forwarded { return &forwarded{conns: map[string]forwardedConn{}} }

func (f *forwarded) add(local netip.AddrPort, remote string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	f.mu.Lock()
	f.conns[id] = forwardedConn{local: local, remote: remote}
	f.mu.Unlock()
	return id
}

func (f *forwarded) remove(id string) {
	f.mu.Lock()
	delete(f.conns, id)
	f.mu.Unlock()
}

// lookup returns the tailnet address behind the open connection id, which
// must have come from the loopback address from as well.
func (f *forwarded) lookup(id, from string) (string, bool) {
	at, err := netip.ParseAddrPort(from)
	if err != nil || id == "" {
		return "", false
	}
	f.mu.Lock()
	c, ok := f.conns[id]
	f.mu.Unlock()
	if !ok || c.local.Port() != at.Port() || c.local.Addr().Unmap() != at.Addr().Unmap() {
		return "", false
	}
	return c.remote, true
}

func (f *forwarded) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// forwardOne passes one tailnet connection to the host listener, its
// preamble first. The connection's entry goes before the connection
// closes, so a later connection on the same loopback port never finds it.
func forwardOne(conn net.Conn, dial func() (net.Conn, error), reg *forwarded) {
	defer conn.Close()
	local, err := dial()
	if err != nil {
		return
	}
	defer local.Close()
	from, err := netip.ParseAddrPort(local.LocalAddr().String())
	if err != nil {
		return
	}
	id := reg.add(from, conn.RemoteAddr().String())
	defer reg.remove(id)
	if _, err := io.WriteString(local, preamble+id+"\n"); err != nil {
		return
	}
	splice(conn, local)
}

type whoisFunc func(ctx context.Context, addr string) (*apitype.WhoIsResponse, error)

// whoisHandler answers which tailnet device is behind a connection the
// add-on forwarded, as this node's Tailscale sees it, and only while that
// connection is open.
func whoisHandler(reg *forwarded, whois whoisFunc, self func(context.Context) (selfInfo, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		remote, ok := reg.lookup(r.URL.Query().Get("conn"), r.URL.Query().Get("from"))
		if !ok {
			http.Error(w, "no such connection", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		who, err := whois(ctx, remote)
		me, selfErr := self(ctx)
		id, known := describe(me, who, remote)
		if err != nil || selfErr != nil || !known {
			http.Error(w, "Tailscale does not know that device", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(id)
	}
}

// tunnelIdentity is the identity header line for a tunnel to remote, or ""
// when Tailscale cannot tell.
func tunnelIdentity(ctx context.Context, remote net.Addr, whois whoisFunc, self func(context.Context) (selfInfo, error)) string {
	at, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	who, err := whois(ctx, at.Addr().Unmap().String())
	me, selfErr := self(ctx)
	id, known := describe(me, who, at.String())
	if err != nil || selfErr != nil || !known {
		return ""
	}
	b, err := json.Marshal(id)
	if err != nil {
		return ""
	}
	return identityHeader + ": " + base64.RawURLEncoding.EncodeToString(b) + "\r\n"
}
