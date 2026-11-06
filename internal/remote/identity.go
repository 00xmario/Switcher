package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pairing without a code rests on Tailscale's node authentication: a
// connection that Tailscale itself attributes to another device of the same
// Tailscale user, in the same tailnet, may pair without a code. Every fact
// used for that decision comes from Tailscale, through the add-on's own node
// or the Tailscale app or daemon on this computer, never from anything the
// other side sends.

// tailnetSelf is this computer's node, as Tailscale reports it.
type tailnetSelf struct {
	ID     string   `json:"id"`
	User   int64    `json:"user"`
	Tagged bool     `json:"tagged"`
	Suffix string   `json:"suffix"` // the tailnet's MagicDNS suffix, such as tail1234.ts.net
	IPs    []string `json:"ips"`
}

// tailnetPeer is the node at the other end of one connection.
type tailnetPeer struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // MagicDNS name, without the trailing dot
	User   int64  `json:"user"`
	Tagged bool   `json:"tagged"`
	// Shared marks a node shared in from another tailnet (Tailscale jails
	// those too), Limited an expired or WireGuard-only one, and Routes one
	// that routes traffic for other machines: a subnet router, exit node or
	// app connector, behind which anyone may be.
	Shared  bool     `json:"shared"`
	Limited bool     `json:"limited"`
	Routes  bool     `json:"routes"`
	IPs     []string `json:"ips"`  // the node's own Tailscale addresses
	Addr    string   `json:"addr"` // the address this connection has at the node
}

// tailnetIdentity is one connection as Tailscale sees it from this node.
type tailnetIdentity struct {
	Self tailnetSelf `json:"self"`
	Peer tailnetPeer `json:"peer"`
}

// notOwnDevice says why the peer is not one of this Tailscale user's own
// devices, or returns "" when it is. Stable ids are compared, never names:
// the same user as this node, in the same tailnet, untagged, not shared in,
// reached at one of its own addresses, and not this node itself. A tagged
// node belongs to no user, so a tagged node here trusts nobody.
func notOwnDevice(id tailnetIdentity) string {
	s, p := id.Self, id.Peer
	switch {
	case s.User == 0 || s.ID == "" || s.Suffix == "":
		return "this computer is not signed in to Tailscale"
	case s.Tagged:
		return "this computer is a tagged Tailscale device, which belongs to no user"
	case p.ID == "" || p.User == 0:
		return "Tailscale does not know the other device"
	case p.ID == s.ID || hasAddress(s.IPs, p.Addr):
		return "that is this computer itself"
	case p.User != s.User:
		return "it belongs to another Tailscale user"
	case p.Tagged:
		return "it is a tagged Tailscale device, which belongs to no user"
	case p.Shared:
		return "it is shared in from another tailnet"
	case p.Limited:
		return "its Tailscale key has expired"
	case !inTailnet(p.Name, s.Suffix):
		return "it is in another tailnet"
	case !hasAddress(p.IPs, p.Addr):
		return "Tailscale does not know it at that address"
	}
	return ""
}

// hostAdmits decides whether a host lets a peer pair without a code: one of
// the user's own devices, which routes nobody else's traffic and is not this
// computer under another of its Tailscale addresses. Connections from this
// computer itself prove nothing, since any account on it can make them.
func hostAdmits(id tailnetIdentity, own []string) bool {
	return notOwnDevice(id) == "" && !id.Peer.Routes && !hasAddress(own, id.Peer.Addr)
}

func inTailnet(name, suffix string) bool {
	name, suffix = strings.ToLower(strings.TrimSuffix(name, ".")), strings.ToLower(strings.Trim(suffix, "."))
	return suffix != "" && strings.HasSuffix(name, "."+suffix) && len(name) > len(suffix)+1
}

// hasAddress reports whether addr (an address, with or without a port) is
// one of ips.
func hasAddress(ips []string, addr string) bool {
	ip, ok := parseAddr(addr)
	if !ok {
		return false
	}
	for _, s := range ips {
		if other, ok := parseAddr(s); ok && other == ip {
			return true
		}
	}
	return false
}

func parseAddr(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// onTailnetIP reports whether addr is in Tailscale's address ranges.
func onTailnetIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsValid() && (tailnet.Contains(net.IP(addr.AsSlice())) || tailnet6.Contains(net.IP(addr.AsSlice())))
}

// localTailnetIPs are this computer's own Tailscale addresses, from its
// network interfaces: the Tailscale app's or daemon's, if it runs. An
// error means they are unknown, and callers refuse.
func localTailnetIPs() ([]string, error) {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipnet.IP); ok && onTailnetIP(ip) {
					out = append(out, ip.Unmap().String())
				}
			}
		}
	}
	return out, nil
}

var (
	nodeIDShape  = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	dnsNameShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// cleanIdentity keeps only well-formed facts, so a confused or older
// Tailscale cannot slip an odd value into a decision or into the UI.
func cleanIdentity(id *tailnetIdentity) error {
	if !nodeIDShape.MatchString(id.Self.ID) || !nodeIDShape.MatchString(id.Peer.ID) {
		return errors.New("Tailscale reported a malformed node id")
	}
	id.Peer.Name = strings.ToLower(strings.TrimSuffix(id.Peer.Name, "."))
	id.Self.Suffix = strings.ToLower(strings.Trim(id.Self.Suffix, "."))
	if len(id.Peer.Name) > 253 || !dnsNameShape.MatchString(id.Peer.Name) || id.Self.Suffix != "" && !dnsNameShape.MatchString(id.Self.Suffix) {
		return errors.New("Tailscale reported a malformed name")
	}
	if id.Self.User <= 0 || id.Peer.User <= 0 {
		return errors.New("Tailscale reported no user")
	}
	return nil
}

// decodeOne decodes exactly one JSON value of at most limit bytes into v.
func decodeOne(r io.Reader, limit int64, v any) error {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errors.New("the answer is too long")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the answer")
	}
	return nil
}

// errNoTailscale says the Tailscale app or daemon is not on this computer.
var errNoTailscale = errors.New("the Tailscale app is not installed on this computer")

// tailscalePaths are where the Tailscale command line lives: Linux packages,
// the macOS command line integration, and the macOS app itself, which runs
// as the command line when given arguments.
var tailscalePaths = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}

// findTailscale prefers the fixed places over PATH, and takes a command
// line only where no other account can have put or changed it (see
// trustedBinary): its answers decide who may pair.
func findTailscale() string {
	return findTailscaleIn(tailscalePaths, func() (string, error) { return exec.LookPath("tailscale") }, trustedBinary)
}

func findTailscaleIn(fixed []string, lookPath func() (string, error), trusted func(string) bool) string {
	for _, p := range fixed {
		if trusted(p) {
			return p
		}
	}
	if p, err := lookPath(); err == nil && filepath.IsAbs(p) && trusted(p) {
		return p
	}
	return ""
}

// cappedBuffer keeps at most limit bytes and notes anything beyond.
type cappedBuffer struct {
	b     bytes.Buffer
	limit int
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.b.Len()+len(p) > c.limit {
		c.over = true
		return len(p), nil
	}
	return c.b.Write(p)
}

// runTailscale runs one read-only Tailscale command with a time limit and a
// bound on its output.
func runTailscale(ctx context.Context, limit int, args ...string) ([]byte, error) {
	bin := findTailscale()
	if bin == "" {
		return nil, errNoTailscale
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := &cappedBuffer{limit: limit}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = out, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tailscale %s: %w", args[0], err)
	}
	if out.over {
		return nil, fmt.Errorf("tailscale %s answered too much", args[0])
	}
	return out.b.Bytes(), nil
}

// systemTailscale reads identities from the Tailscale app or daemon on this
// computer through its command line, read-only (status and whois), so that
// Switcher itself needs no Tailscale code.
type systemTailscale struct {
	// run runs one Tailscale command; present reports whether Tailscale is
	// on this computer at all, whether or not a command line answers for
	// it. Tests replace both.
	run     func(ctx context.Context, limit int, args ...string) ([]byte, error)
	present func() bool
	mu      sync.Mutex
	self    tailnetSelf
	selfErr error
	selfAt  time.Time
}

func newSystemTailscale() *systemTailscale {
	return &systemTailscale{run: runTailscale, present: tailscalePresent}
}

// errTailscaleDown says the Tailscale app or daemon here reports its node
// is not connected, so the node carries nobody's traffic.
var errTailscaleDown = errors.New("Tailscale is not connected on this computer")

// downStates are the states in which Tailscale's node carries no traffic.
// Others, such as Starting, may already carry some.
var downStates = map[string]bool{"NoState": true, "Stopped": true, "NeedsLogin": true, "NeedsMachineAuth": true}

// tailscaleSockets are where Tailscale's daemon listens for its command
// line: Linux packages, the snap, and tailscaled on macOS.
var tailscaleSockets = []string{"/var/run/tailscale/tailscaled.sock", "/run/tailscale/tailscaled.sock",
	"/var/snap/tailscale/common/socket/tailscaled.sock", "/var/run/tailscaled.socket"}

// tailscalePresent reports whether Tailscale's daemon or app is on this
// computer: its socket, the macOS app, or a Tailscale address on a network
// interface. Anything it cannot rule out counts as present.
func tailscalePresent() bool {
	for _, p := range append(append([]string{}, tailscaleSockets...), "/Applications/Tailscale.app") {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	ips, err := localTailnetIPs()
	return err != nil || len(ips) > 0
}

// isAppNode reports whether peer is the node of the Tailscale app or daemon
// on this computer, which any account here can send traffic through. It
// says no only when that is certain: the node is another one, or there is
// no connected Tailscale here. When Tailscale is here but cannot be asked,
// it returns an error, and callers refuse.
func (s *systemTailscale) isAppNode(ctx context.Context, peer tailnetPeer) (bool, error) {
	if s == nil {
		return false, errNoTailscale
	}
	app, err := s.selfNode(ctx)
	switch {
	case err == nil:
		return app.ID == peer.ID || hasAddress(app.IPs, peer.Addr), nil
	case errors.Is(err, errTailscaleDown):
		return false, nil
	case errors.Is(err, errNoTailscale) && s.present != nil && !s.present():
		return false, nil
	}
	return false, err
}

type statusSelf struct {
	ID           string
	HostName     string
	DNSName      string
	UserID       int64
	Tags         []string
	TailscaleIPs []string
	Online       bool
}

type statusAnswer struct {
	BackendState   string
	Self           *statusSelf
	MagicDNSSuffix string
	CurrentTailnet *struct{ MagicDNSSuffix string }
	Peer           map[string]*statusSelf
}

func (s *systemTailscale) status(ctx context.Context, peers bool, limit int) (statusAnswer, error) {
	args := []string{"status", "--json"}
	if !peers {
		args = append(args, "--peers=false")
	}
	out, err := s.run(ctx, limit, args...)
	if err != nil {
		return statusAnswer{}, err
	}
	var st statusAnswer
	if err := decodeOne(bytes.NewReader(out), int64(limit), &st); err != nil {
		return statusAnswer{}, fmt.Errorf("read Tailscale's status: %w", err)
	}
	switch {
	case st.BackendState == "Running" && st.Self != nil:
		return st, nil
	case downStates[st.BackendState]:
		return statusAnswer{}, errTailscaleDown
	}
	return statusAnswer{}, errors.New("Tailscale on this computer is starting or in an unknown state")
}

// selfNode is this computer's node in the Tailscale app. The answer, or the
// failure, is kept for ten seconds, so callers cannot make the command line
// run more often than that.
func (s *systemTailscale) selfNode(ctx context.Context) (tailnetSelf, error) {
	s.mu.Lock()
	if time.Since(s.selfAt) < 10*time.Second {
		self, err := s.self, s.selfErr
		s.mu.Unlock()
		return self, err
	}
	s.mu.Unlock()
	self, err := s.readSelf(ctx)
	// A caller that gave up says nothing about Tailscale.
	if ctx.Err() == nil {
		s.mu.Lock()
		s.self, s.selfErr, s.selfAt = self, err, time.Now()
		s.mu.Unlock()
	}
	return self, err
}

func (s *systemTailscale) readSelf(ctx context.Context) (tailnetSelf, error) {
	st, err := s.status(ctx, false, 1<<20)
	if err != nil {
		return tailnetSelf{}, err
	}
	self := tailnetSelf{ID: st.Self.ID, User: st.Self.UserID, Tagged: len(st.Self.Tags) > 0, Suffix: st.MagicDNSSuffix}
	if st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSSuffix != "" {
		self.Suffix = st.CurrentTailnet.MagicDNSSuffix
	}
	for _, ip := range st.Self.TailscaleIPs {
		if a, ok := parseAddr(ip); ok && onTailnetIP(a) {
			self.IPs = append(self.IPs, a.String())
		}
	}
	return self, nil
}

type whoisNode struct {
	StableID        string
	Name            string
	User            int64
	Sharer          int64
	Tags            []string
	Addresses       []string
	AllowedIPs      []string
	PrimaryRoutes   []string
	IsJailed        bool
	IsWireGuardOnly bool
	Expired         bool
	Hostinfo        *struct{ RoutableIPs []string }
}

// whois asks the Tailscale app which node is behind addr.
func (s *systemTailscale) whois(ctx context.Context, addr string) (tailnetPeer, error) {
	out, err := s.run(ctx, 1<<20, "whois", "--json", addr)
	if err != nil {
		return tailnetPeer{}, err
	}
	var answer struct{ Node *whoisNode }
	if err := decodeOne(bytes.NewReader(out), 1<<20, &answer); err != nil {
		return tailnetPeer{}, fmt.Errorf("read Tailscale's answer: %w", err)
	}
	if answer.Node == nil {
		return tailnetPeer{}, errors.New("Tailscale does not know that address")
	}
	n := answer.Node
	peer := tailnetPeer{ID: n.StableID, Name: n.Name, User: n.User, Tagged: len(n.Tags) > 0,
		Shared: n.Sharer != 0 || n.IsJailed, Limited: n.Expired || n.IsWireGuardOnly, Addr: addr,
		Routes: len(n.PrimaryRoutes) > 0 || n.Hostinfo != nil && len(n.Hostinfo.RoutableIPs) > 0}
	own := map[netip.Prefix]bool{}
	for _, p := range n.Addresses {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			continue
		}
		own[prefix.Masked()] = true
		if prefix.IsSingleIP() {
			peer.IPs = append(peer.IPs, prefix.Addr().Unmap().String())
		}
	}
	// Traffic Tailscale accepts from a node may come from any of its
	// allowed addresses; any beyond its own are other machines' routes.
	for _, p := range n.AllowedIPs {
		if prefix, err := netip.ParsePrefix(p); err != nil || !own[prefix.Masked()] {
			peer.Routes = true
		}
	}
	return peer, nil
}

// identify is what the Tailscale app says about the connection between
// local (on this computer) and remote.
func (s *systemTailscale) identify(ctx context.Context, local, remote string) (tailnetIdentity, error) {
	self, err := s.selfNode(ctx)
	if err != nil {
		return tailnetIdentity{}, err
	}
	// The connection must be one Tailscale carried to this node's own
	// address, not one that merely comes from a Tailscale-looking address.
	if !hasAddress(self.IPs, local) {
		return tailnetIdentity{}, errors.New("the connection did not come in over Tailscale")
	}
	peer, err := s.whois(ctx, remote)
	if err != nil {
		return tailnetIdentity{}, err
	}
	id := tailnetIdentity{Self: self, Peer: peer}
	if err := cleanIdentity(&id); err != nil {
		return tailnetIdentity{}, err
	}
	return id, nil
}

// peers lists the Switchers the Tailscale app sees: nodes whose name starts
// with switcher-, as the add-on names them.
func (s *systemTailscale) peers(ctx context.Context) []TailnetPeer {
	st, err := s.status(ctx, true, 16<<20)
	if err != nil {
		return nil
	}
	var out []TailnetPeer
	for _, p := range st.Peer {
		if p == nil || !strings.HasPrefix(strings.ToLower(p.HostName), "switcher-") {
			continue
		}
		entry := TailnetPeer{Name: p.HostName, DNSName: strings.ToLower(strings.TrimSuffix(p.DNSName, ".")), Online: p.Online}
		for _, ip := range p.TailscaleIPs {
			if a, ok := parseAddr(ip); ok && a.Is4() {
				entry.IP = a.String()
			}
		}
		if dnsNameShape.MatchString(entry.DNSName) && len(entry.Name) <= 63 {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SystemTailnetPeers lists the Switchers the Tailscale app on this computer
// sees, for finding hosts without the add-on.
func SystemTailnetPeers(ctx context.Context) []TailnetPeer {
	return newSystemTailscale().peers(ctx)
}
