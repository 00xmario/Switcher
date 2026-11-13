// Package localpeer tells which user is at the other end of a TCP
// connection on this computer.
//
// Switcher's port on 127.0.0.1 is reachable by every account on the
// computer, and it serves the accounts without a login of its own, as the
// CLIs need. On Linux the kernel says who owns each socket, so the server
// takes connections only from the user it runs as, and the command line and
// tray make sure the port they talk to is that user's Switcher. Other
// systems do not say, and there this package answers "unknown".
package localpeer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// AllowOthersEnv turns the check off, for a setup where another account's
// programs are meant to use this Switcher, such as a container that shares
// the computer's network under its own user. Set to "root" it lets only
// root's programs in beside this user's.
const AllowOthersEnv = "SWITCHER_ALLOW_LOCAL_USERS"

// allowed is what AllowOthersEnv asks for: everyone, or root as well.
func allowed() (everyone, root bool) {
	switch value := os.Getenv(AllowOthersEnv); value {
	case "":
		return false, false
	case "root":
		return false, true
	default:
		return true, true
	}
}

// PeerUID is the user that owns the other end of conn, a connection between
// two addresses of this computer. ok is false when it cannot be told: off
// Linux, or when the other end is gone.
func PeerUID(conn net.Conn) (uid uint32, ok bool) {
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	if local == nil || remote == nil {
		return 0, false
	}
	return peerUID(conn, local, remote)
}

// serverEndUID is the user the other end of a connection this process made
// is shown under, taken by its program yet or not.
func serverEndUID(conn net.Conn) (uint32, bool) {
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	if local == nil || remote == nil {
		return 0, false
	}
	return connectedUID(remote, local)
}

// ListenerUID is the user that listens on addr on this computer.
func ListenerUID(addr *net.TCPAddr) (uid uint32, ok bool) {
	if addr == nil {
		return 0, false
	}
	return listenerUID(addr)
}

// works remembers, per address family, whether this computer says who owns
// a connection.
var works [2]struct {
	once sync.Once
	ok   bool
}

// Works reports whether this computer says who owns a connection: a
// connection this process makes to itself must come back as its own user.
// Only then is "someone else" or "unknown" a reason to refuse.
func Works() bool { return worksFor(false) }

// worksFor is Works for IPv4 or for IPv6 loopback connections. Each is
// tried by itself: a listener of one family is guarded only by what was
// seen to work for that family.
func worksFor(v6 bool) bool {
	i := 0
	if v6 {
		i = 1
	}
	works[i].once.Do(func() { works[i].ok = selfTest(v6) })
	return works[i].ok
}

func selfTest(v6 bool) bool {
	network, address := "tcp4", "127.0.0.1:0"
	if v6 {
		network, address = "tcp6", "[::1]:0"
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return false
	}
	defer ln.Close()
	if uid, ok := ListenerUID(ln.Addr().(*net.TCPAddr)); !ok || uid != uint32(os.Getuid()) {
		return false
	}
	client, err := net.Dial(network, ln.Addr().String())
	if err != nil {
		return false
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		return false
	}
	defer server.Close()
	uid, ok := PeerUID(server)
	return ok && uid == uint32(os.Getuid())
}

// SameUser wraps a listener on this computer so it takes connections only
// from the user this process runs as. Root's are refused too: root could
// read this user's files anyway, but a program that runs as root and
// passes connections on for others, as some SSH and VPN services do, would
// let those others in as root. A connection whose owner cannot be told is
// dropped as well: one whose sender has already gone, which could otherwise
// carry a request in unseen. Where the computer does not say who owns a
// connection, or AllowOthersEnv lifts the check, ln is returned as it is.
func SameUser(ln net.Listener) net.Listener {
	addr, _ := ln.Addr().(*net.TCPAddr)
	everyone, root := allowed()
	if everyone || addr == nil || !worksFor(addr.IP.To4() == nil) {
		return ln
	}
	return guard(ln, uint32(os.Getuid()), root, PeerUID)
}

func guard(ln net.Listener, uid uint32, root bool, owner func(net.Conn) (uint32, bool)) *sameUser {
	return &sameUser{Listener: ln, uid: uid, root: root, owner: owner,
		taken: make(chan net.Conn), quit: make(chan struct{}), stopped: make(chan struct{})}
}

// Enforced reports whether SameUser refuses other users here.
func Enforced() bool {
	everyone, _ := allowed()
	return !everyone && Works()
}

type sameUser struct {
	net.Listener
	uid   uint32
	root  bool // root's connections are taken as well
	owner func(net.Conn) (uint32, bool)

	start   sync.Once
	closing sync.Once
	taken   chan net.Conn  // connections found to be this user's
	quit    chan struct{}  // closed to stop everything
	stopped chan struct{}  // closed when run has ended
	err     error          // why run ended, read after stopped
	checks  sync.WaitGroup // lookups in flight
}

// checksAtOnce bounds how many connections are being looked up at a time.
const checksAtOnce = 32

// Accept returns the next connection that is this user's. Connections are
// taken off the port and looked up side by side, a bounded number at a
// time: one slow answer from the kernel then holds up one connection, not
// everyone waiting behind it.
func (l *sameUser) Accept() (net.Conn, error) {
	l.start.Do(func() { go l.run() })
	select {
	case conn := <-l.taken:
		return conn, nil
	case <-l.stopped:
		return nil, l.err
	case <-l.quit:
		return nil, net.ErrClosed
	}
}

// Close closes the listener and ends the lookups in flight; what they were
// about to hand over is closed. It does not wait for anyone to call Accept.
func (l *sameUser) Close() error {
	var err error
	l.closing.Do(func() {
		close(l.quit)
		err = l.Listener.Close()
	})
	// run ends on the closed listener, or never started.
	l.start.Do(func() { l.err = net.ErrClosed; close(l.stopped) })
	<-l.stopped
	l.checks.Wait()
	return err
}

func (l *sameUser) run() {
	defer close(l.stopped)
	slots := make(chan struct{}, checksAtOnce)
	var wait time.Duration
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			// Out of file descriptors and the like pass; a closed listener
			// does not.
			if temporary, ok := err.(interface{ Temporary() bool }); ok && temporary.Temporary() && !errors.Is(err, net.ErrClosed) {
				wait = min(max(2*wait, 5*time.Millisecond), time.Second)
				select {
				case <-time.After(wait):
					continue
				case <-l.quit:
					err = net.ErrClosed
				}
			}
			l.err = err
			return
		}
		wait = 0
		select {
		case slots <- struct{}{}:
		case <-l.quit:
			conn.Close()
			l.err = net.ErrClosed
			return
		}
		l.checks.Add(1)
		go func() {
			defer l.checks.Done()
			defer func() { <-slots }()
			if uid, ok := l.owner(conn); !ok || (uid != l.uid && !(l.root && uid == 0)) {
				conn.Close()
				return
			}
			select {
			case l.taken <- conn:
			case <-l.quit:
				conn.Close()
			}
		}()
	}
}

// Transport is an HTTP transport for this user's own programs, the command
// line and the tray, that talk to Switcher on this computer. A connection
// to this computer is checked before a byte goes over it: the port, and the
// program that took the connection, must be this user's or root's. Another
// account's program that took Switcher's port would otherwise be handed the
// device token and whatever the request holds. Connections elsewhere go as
// with base (the default transport when nil).
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	local := &http.Transport{
		// Straight to this computer: no proxy stands in between.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := ownConnection(conn); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		},
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	return &checkedTransport{local: local, base: base}
}

type checkedTransport struct {
	local *http.Transport
	base  http.RoundTripper
}

func (t *checkedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if loopbackHost(r.Context(), r.URL.Hostname()) {
		return t.local.RoundTrip(r)
	}
	return t.base.RoundTrip(r)
}

func (t *checkedTransport) CloseIdleConnections() {
	t.local.CloseIdleConnections()
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// loopbackHost reports whether host is this computer: "localhost", a
// loopback address, or a name that leads to one (some systems give the
// computer's own name the address 127.0.1.1).
func loopbackHost(ctx context.Context, host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if addr.IP.IsLoopback() {
			return true
		}
	}
	return false
}

// ownConnection checks a connection this process made to this computer.
func ownConnection(conn net.Conn) error {
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	if everyone, _ := allowed(); everyone || remote == nil || !remote.IP.IsLoopback() || !worksFor(remote.IP.To4() == nil) {
		return nil
	}
	mine := func(uid uint32) bool { return uid == uint32(os.Getuid()) || uid == 0 }
	// Whose the port is. It must be told: a listener that cannot be seen is
	// not known to be this user's.
	listener, ok := ListenerUID(remote)
	if !ok || !mine(listener) {
		return &OtherUserError{Port: remote.Port, UID: listener, Unknown: !ok}
	}
	// And whose this very connection's other end is. Before the server has
	// taken it, the kernel shows it under the listener's owner.
	if taker, ok := serverEndUID(conn); ok && !mine(taker) {
		return &OtherUserError{Port: remote.Port, UID: taker}
	}
	return nil
}

// OtherUserError says another account on this computer holds a port, or
// that it cannot be told who does.
type OtherUserError struct {
	Port    int
	UID     uint32
	Unknown bool
}

func (e *OtherUserError) Error() string {
	if e.Unknown {
		return "it cannot be told whose program holds port " + strconv.Itoa(e.Port) + " on this computer, so nothing was sent to it (" +
			AllowOthersEnv + "=1 sends anyway)"
	}
	return "port " + strconv.Itoa(e.Port) + " on this computer is held by another user (uid " + strconv.FormatUint(uint64(e.UID), 10) +
		"), not by your Switcher; nothing was sent to it"
}
