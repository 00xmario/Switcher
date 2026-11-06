package remote

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// tailnetPreamble starts every connection the Tailscale add-on forwards to
// the host listener, followed by that connection's random id and a newline,
// before TLS begins. Only the add-on knows a live id, and the host asks the
// add-on over its token-protected control endpoint which tailnet device is
// behind it. Any program on this computer can connect from loopback, and a
// closed connection's port can be reused at once, so a loopback address
// alone never names a device.
const tailnetPreamble = "SWITCHER-TAILNET "

var preambleID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// installShape is a Switcher's install id, as the client makes it.
var installShape = preambleID

// loopbackConn is a connection from this computer. It reads the add-on's
// preamble, if one comes first, when the TLS handshake first reads, so a
// slow client never holds up accepting others.
type loopbackConn struct {
	net.Conn
	once sync.Once
	r    *bufio.Reader
	id   string
	err  error
}

func (c *loopbackConn) Read(p []byte) (int, error) {
	c.once.Do(c.readPreamble)
	if c.err != nil {
		return 0, c.err
	}
	if c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

func (c *loopbackConn) readPreamble() {
	c.r = bufio.NewReaderSize(c.Conn, 64)
	first, err := c.r.Peek(1)
	if err != nil {
		c.err = err
		return
	}
	// A TLS client starts with a handshake record, 0x16.
	if first[0] != tailnetPreamble[0] {
		return
	}
	line, err := c.r.ReadSlice('\n')
	id, found := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), tailnetPreamble)
	if err != nil || !found || !preambleID.MatchString(id) {
		c.err = errors.New("malformed connection preamble")
		return
	}
	c.id = id
}

// tailnetID is the add-on's id for this connection, or "" for any other.
func (c *loopbackConn) tailnetID() string {
	c.once.Do(c.readPreamble)
	return c.id
}

type loopbackKey struct{}

// withLoopbackConn keeps a loopback connection with its requests, so the
// pairing route can learn the add-on's id for it.
func withLoopbackConn(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if lc, ok := c.(*loopbackConn); ok {
		return context.WithValue(ctx, loopbackKey{}, lc)
	}
	return ctx
}

const (
	// tailnetTries bounds code-free pairing attempts per source address and
	// per Tailscale device within ten minutes; tailnetTriesAll bounds the
	// Tailscale app lookups they cause in all.
	tailnetTries    = 10
	tailnetTriesAll = 120
)

// allowTailnet reports whether one more code-free pairing attempt stays
// within the limit of every key, and counts it against all of them only
// then: a source already refused cannot use up the limit others share.
func (h *Host) allowTailnet(keys ...string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if h.tailnetTries == nil || len(h.tailnetTries) > 4096 {
		kept := map[string]failure{}
		for k, f := range h.tailnetTries {
			if now.Sub(f.since) <= codeTTL {
				kept[k] = f
			}
		}
		h.tailnetTries = kept
	}
	counts := make([]failure, len(keys))
	for i, k := range keys {
		f := h.tailnetTries[k]
		if now.Sub(f.since) > codeTTL {
			f = failure{since: now}
		}
		limit := tailnetTries
		if k == "all" {
			limit = tailnetTriesAll
		}
		if f.count >= limit {
			return false
		}
		f.count++
		counts[i] = f
	}
	for i, k := range keys {
		h.tailnetTries[k] = counts[i]
	}
	return true
}

// pairTailnet pairs one of the user's own Tailscale devices without a code.
// Who is calling comes only from Tailscale: from the add-on, for a
// connection it forwarded, or from the Tailscale app, for one that reached
// this computer's own Tailscale address. Nothing the caller sends counts.
// Every refusal gives the same answer as a wrong code, so a caller learns
// nothing about why.
func (h *Host) pairTailnet(w http.ResponseWriter, r *http.Request) {
	// Install is the client Switcher's own random id, which only tells two
	// Switchers behind one Tailscale device apart; it grants nothing.
	var req struct {
		Name    string `json:"name"`
		Install string `json:"install"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil {
		writeError(w, http.StatusBadRequest, "invalid pairing request")
		return
	}
	open := func() bool { return h.state.Enabled && h.state.tailnetPairing() }
	h.mu.Lock()
	allowed := open()
	h.mu.Unlock()
	if !allowed {
		writeError(w, http.StatusForbidden, pairFailed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	id, viaAddon, ok := h.identify(ctx, r)
	own, err := h.ownTailnet()
	if !ok || err != nil || !hostAdmits(id, own) {
		writeError(w, http.StatusForbidden, pairFailed)
		return
	}
	// Through the add-on, the Tailscale app on this computer is one more way
	// in for any account here, also without a network interface (userspace
	// networking): its node is this computer too. Asking it is spent only
	// on the user's own devices, so nobody else uses up what they share, and
	// whenever Tailscale is here but cannot say, the answer is no.
	if viaAddon {
		if !h.allowTailnet("all") {
			writeError(w, http.StatusForbidden, pairFailed)
			return
		}
		if app, err := h.system.isAppNode(ctx, id.Peer); err != nil || app {
			writeError(w, http.StatusForbidden, pairFailed)
			return
		}
	}
	d, token := newDevice(req.Name)
	d.TailscaleNode, d.TailscaleName = id.Peer.ID, id.Peer.Name
	if installShape.MatchString(req.Install) {
		d.TailscaleInstall = req.Install
	}
	h.mu.Lock()
	// Sharing or this setting may have been turned off during the lookup.
	if !open() {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, pairFailed)
		return
	}
	err = h.addDeviceLocked(d)
	h.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save the pairing")
		return
	}
	lan, tailscale := h.addresses()
	writeJSONResponse(w, http.StatusOK, pairResponse{DeviceID: d.ID, Token: token, HostName: h.name, Addresses: append(lan, tailscale...)})
}

// identify asks Tailscale who is behind the request's connection. A
// connection from loopback counts only with the add-on's preamble and the
// add-on's word for that id and port; one from a Tailscale address only
// when the Tailscale app carried it to this computer's own Tailscale
// address. Anything else, such as the local network, has no identity, and
// no header or body field can give it one. Every attempt counts against
// its source and its device. viaAddon says the add-on answered.
func (h *Host) identify(ctx context.Context, r *http.Request) (id tailnetIdentity, viaAddon, ok bool) {
	remote, ok := parseAddr(r.RemoteAddr)
	if !ok {
		return tailnetIdentity{}, false, false
	}
	switch {
	case remote.IsLoopback():
		lc, _ := r.Context().Value(loopbackKey{}).(*loopbackConn)
		h.mu.Lock()
		whois := h.addonWhois
		h.mu.Unlock()
		if lc == nil || whois == nil || lc.tailnetID() == "" {
			return tailnetIdentity{}, false, false
		}
		id, err := whois(ctx, lc.tailnetID(), r.RemoteAddr)
		if err != nil || cleanIdentity(&id) != nil {
			return tailnetIdentity{}, false, false
		}
		peer, ok := parseAddr(id.Peer.Addr)
		if !ok || !onTailnetIP(peer) {
			return tailnetIdentity{}, false, false
		}
		return id, true, h.allowTailnet("src:"+peer.String(), "node:"+id.Peer.ID)
	case onTailnetIP(remote):
		// Only a connection to this computer's own Tailscale address counts,
		// and that is checked before anything is spent on it: a neighbour
		// in the same address range, such as a carrier's, cannot use up the
		// limit all sources share.
		local, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		own, err := h.ownTailnet()
		if local == nil || h.system == nil || err != nil || !hasAddress(own, local.String()) {
			return tailnetIdentity{}, false, false
		}
		if !h.allowTailnet("src:"+remote.String(), "all") {
			return tailnetIdentity{}, false, false
		}
		id, err := h.system.identify(ctx, local.String(), r.RemoteAddr)
		if err != nil {
			return tailnetIdentity{}, false, false
		}
		return id, false, h.allowTailnet("node:" + id.Peer.ID)
	}
	return tailnetIdentity{}, false, false
}
