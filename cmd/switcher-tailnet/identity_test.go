package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

var studio = selfInfo{user: me, id: "nStudio", suffix: "tail1.ts.net", dnsName: "switcher-studio.tail1.ts.net",
	ips: []netip.Addr{netip.MustParseAddr("100.101.2.3")}}

func airNode() *tailcfg.Node {
	own := []netip.Prefix{netip.MustParsePrefix("100.101.2.4/32"), netip.MustParsePrefix("fd7a:115c:a1e0::4/128")}
	return &tailcfg.Node{StableID: "nAir", Name: "switcher-air.tail1.ts.net.", User: me, Addresses: own, AllowedIPs: own,
		Hostinfo: (&tailcfg.Hostinfo{OS: "macOS"}).View()}
}

func TestDescribeReportsWhatTailscaleKnows(t *testing.T) {
	id, ok := describe(studio, &apitype.WhoIsResponse{Node: airNode()}, "100.101.2.4:51234")
	if !ok || id.Self.ID != "nStudio" || id.Self.User != int64(me) || id.Self.Suffix != "tail1.ts.net" || id.Self.IPs[0] != "100.101.2.3" {
		t.Fatalf("self: %+v", id.Self)
	}
	p := id.Peer
	if p.ID != "nAir" || p.Name != "switcher-air.tail1.ts.net" || p.User != int64(me) || p.Tagged || p.Shared || p.Limited || p.Routes ||
		strings.Join(p.IPs, ",") != "100.101.2.4,fd7a:115c:a1e0::4" || p.Addr != "100.101.2.4:51234" {
		t.Fatalf("peer: %+v", p)
	}
	for _, tc := range []struct {
		name  string
		node  func(*tailcfg.Node)
		check func(tailnetPeer) bool
	}{
		{"tagged", func(n *tailcfg.Node) { n.Tags = []string{"tag:server"} }, func(p tailnetPeer) bool { return p.Tagged }},
		{"shared in", func(n *tailcfg.Node) { n.Sharer = 8 }, func(p tailnetPeer) bool { return p.Shared }},
		{"jailed", func(n *tailcfg.Node) { n.IsJailed = true }, func(p tailnetPeer) bool { return p.Shared }},
		{"expired", func(n *tailcfg.Node) { n.Expired = true }, func(p tailnetPeer) bool { return p.Limited }},
		{"WireGuard only", func(n *tailcfg.Node) { n.IsWireGuardOnly = true }, func(p tailnetPeer) bool { return p.Limited }},
		{"a subnet router", func(n *tailcfg.Node) { n.PrimaryRoutes = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")} }, func(p tailnetPeer) bool { return p.Routes }},
		{"an exit node offer", func(n *tailcfg.Node) {
			n.Hostinfo = (&tailcfg.Hostinfo{RoutableIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}).View()
		}, func(p tailnetPeer) bool { return p.Routes }},
		{"other allowed addresses", func(n *tailcfg.Node) {
			n.AllowedIPs = append(n.AllowedIPs, netip.MustParsePrefix("10.0.0.0/8"))
		}, func(p tailnetPeer) bool { return p.Routes }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := airNode()
			tc.node(n)
			id, ok := describe(studio, &apitype.WhoIsResponse{Node: n}, "100.101.2.4:51234")
			if !ok || !tc.check(id.Peer) {
				t.Fatalf("not reported: %+v", id.Peer)
			}
		})
	}
	if _, ok := describe(studio, &apitype.WhoIsResponse{}, "x"); ok {
		t.Fatal("described no node")
	}
	if _, ok := describe(selfInfo{}, &apitype.WhoIsResponse{Node: airNode()}, "x"); ok {
		t.Fatal("described from a node that is not signed in")
	}
}

// fakeTailnetConn is the tailnet side of a forwarded connection.
type fakeTailnetConn struct {
	net.Conn
	remote net.Addr
}

func (c fakeTailnetConn) RemoteAddr() net.Addr { return c.remote }

// A forwarded connection carries its preamble first, is known by its id
// and port while it is open, and is forgotten before it closes.
func TestForwardedConnectionsAreKnownOnlyWhileOpen(t *testing.T) {
	hostListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostListener.Close()
	reg := newForwarded()
	peer, tailnetSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		forwardOne(fakeTailnetConn{Conn: tailnetSide, remote: &net.TCPAddr{IP: net.ParseIP("100.101.2.4"), Port: 51234}},
			func() (net.Conn, error) { return net.Dial("tcp", hostListener.Addr().String()) }, reg)
		close(done)
	}()
	host, err := hostListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	host.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(host)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, preamble) {
		t.Fatalf("preamble %q %v", line, err)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(line, preamble), "\n")
	if len(id) != 32 {
		t.Fatalf("id %q", id)
	}
	go peer.Write([]byte("\x16tls"))
	if b := make([]byte, 4); func() bool { _, err := io.ReadFull(br, b); return err != nil || string(b) != "\x16tls" }() {
		t.Fatal("the tailnet bytes did not follow the preamble")
	}
	from := host.RemoteAddr().String()
	if remote, ok := reg.lookup(id, from); !ok || remote != "100.101.2.4:51234" {
		t.Fatalf("an open connection: %q %v", remote, ok)
	}
	other, _ := netip.ParseAddrPort(from)
	if _, ok := reg.lookup(id, netip.AddrPortFrom(other.Addr(), other.Port()+1).String()); ok {
		t.Fatal("known from another port")
	}
	if _, ok := reg.lookup(strings.Repeat("0", 32), from); ok {
		t.Fatal("an unknown id was known")
	}
	peer.Close()
	<-done
	if _, ok := reg.lookup(id, from); ok || reg.open() != 0 {
		t.Fatal("a closed connection is still known")
	}
}

func TestWhoisAnswersOnlyForOpenConnections(t *testing.T) {
	reg := newForwarded()
	from := netip.MustParseAddrPort("127.0.0.1:4242")
	id := reg.add(from, "100.101.2.4:51234")
	var asked []string
	whois := func(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
		asked = append(asked, addr)
		if addr != "100.101.2.4:51234" {
			return nil, errors.New("peer not found")
		}
		return &apitype.WhoIsResponse{Node: airNode()}, nil
	}
	self := func(context.Context) (selfInfo, error) { return studio, nil }
	handler := whoisHandler(reg, whois, self)
	get := func(conn, at string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/whois?"+url.Values{"conn": {conn}, "from": {at}}.Encode(), nil))
		return w
	}
	w := get(id, "127.0.0.1:4242")
	var got tailnetIdentity
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Peer.ID != "nAir" || got.Self.ID != "nStudio" {
		t.Fatalf("an open connection: %d %s", w.Code, w.Body)
	}
	if w := get(id, "127.0.0.1:4243"); w.Code != http.StatusNotFound {
		t.Fatalf("another port: %d", w.Code)
	}
	reg.remove(id)
	if w := get(id, "127.0.0.1:4242"); w.Code != http.StatusNotFound || len(asked) != 1 {
		t.Fatalf("a closed connection: %d, asked %v", w.Code, asked)
	}
}

func TestTunnelIdentityNamesTheNodeTheTunnelReached(t *testing.T) {
	var asked string
	whois := func(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
		asked = addr
		return &apitype.WhoIsResponse{Node: airNode()}, nil
	}
	self := func(context.Context) (selfInfo, error) { return studio, nil }
	line := tunnelIdentity(context.Background(), &net.TCPAddr{IP: net.ParseIP("100.101.2.4"), Port: 8788}, whois, self)
	value, found := strings.CutPrefix(strings.TrimSuffix(line, "\r\n"), identityHeader+": ")
	raw, err := base64.RawURLEncoding.DecodeString(value)
	var id tailnetIdentity
	if !found || err != nil || json.Unmarshal(raw, &id) != nil || id.Peer.ID != "nAir" || id.Peer.Addr != "100.101.2.4:8788" || asked != "100.101.2.4" {
		t.Fatalf("header %q, asked %q", line, asked)
	}
	unknown := func(context.Context, string) (*apitype.WhoIsResponse, error) {
		return nil, errors.New("peer not found")
	}
	if line := tunnelIdentity(context.Background(), &net.TCPAddr{IP: net.ParseIP("100.101.2.4"), Port: 8788}, unknown, self); line != "" {
		t.Fatalf("an unknown node got a header: %q", line)
	}
}
