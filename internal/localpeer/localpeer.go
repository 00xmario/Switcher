// Package localpeer tells which user is at the other end of a TCP
// connection on this computer.
//
// Switcher's port on 127.0.0.1 is reachable by every account on the
// computer, and it serves the accounts without a login of its own, as the
// CLIs need. Linux's kernel and macOS's socket list say who owns each
// socket, so the server takes connections only from the user it runs as,
// and the command line and tray make sure the port they talk to is that
// user's Switcher. Other systems do not say, and there this package answers
// "unknown".
package localpeer

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AllowOthersEnv turns the check off, for a setup where another account's
// programs are meant to use this Switcher, such as a container that shares
// the computer's network under its own user: set to 1 (or true, yes, on).
// Set to "root" it lets only root's programs in beside this user's. Any
// other value, 0 or false among them, leaves the check on.
const AllowOthersEnv = "SWITCHER_ALLOW_LOCAL_USERS"

// allowed is what AllowOthersEnv asks for: everyone, or root as well.
func allowed() (everyone, root bool) {
	switch value := os.Getenv(AllowOthersEnv); value {
	case "1", "true", "yes", "on":
		return true, true
	case "root":
		return false, true
	default:
		return false, false
	}
}

// Why a lookup has no answer.
var (
	errNotFound    = errors.New("no such socket")
	errUnsupported = errors.New("this system does not say whose a socket is")
	errTransient   = errors.New("the system did not answer this time")
)

// socket is what the kernel says about one socket. live is false for one
// its program has closed and the kernel is still winding down: such a
// socket belongs to nobody any more, and the kernel reports it as root's.
// connected is true once both ends have agreed on the connection, and
// until it is over: not while it is still being made, and not afterwards.
type socket struct {
	uid       uint32
	listening bool
	live      bool
	connected bool
	// closing: in a state a socket is in once its program has closed the
	// connection (FIN_WAIT_1 and _2, CLOSING, LAST_ACK, TIME_WAIT, CLOSE).
	closing bool
}

// peerKind is what the system shows of the other end of a connection.
type peerKind int

const (
	// peerUnseen: the system did not show it, or could not be read.
	peerUnseen peerKind = iota
	// peerFound: a live, connected socket, and whose it is.
	peerFound
	// peerGone: there, but its program has let go of it or it is no longer
	// connected: its sender has gone.
	peerGone
)

// PeerUID is the user that owns the other end of conn, a connection between
// two addresses of this computer. ok is false when it cannot be told: on a
// system other than Linux and macOS, or when the other end is gone.
func PeerUID(conn net.Conn) (uid uint32, ok bool) {
	uid, kind := peerSince(conn, time.Now())
	return uid, kind == peerFound
}

// peerSince is what the system shows of the other end of conn, a connection
// accepted no earlier than since.
func peerSince(conn net.Conn, since time.Time) (uint32, peerKind) {
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	if local == nil || remote == nil {
		return 0, peerUnseen
	}
	return peerOf(conn, local, remote, since)
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

// support is what the self-test found out about this computer.
type support int

const (
	// untested: not tried yet. Definite answers count.
	untested support = iota
	// working: another process of this user connected, and the system
	// showed its connection as this user's. Definite answers count.
	working
	// unanswered: the test could not be made, or the system did not answer
	// it this time, as when the computer is out of ports or its socket list
	// kept changing while read; another account can bring that about.
	// Definite answers still count, and the test is made again soon.
	unanswered
	// unsupported: the system shows no other process's sockets, or showed
	// them wrongly. Its answers mean nothing: nothing is refused for them.
	// The test is made again now and then.
	unsupported
)

// works remembers, per address family, what the self-test found. The first
// test is made when first needed; later ones run in the background while
// the last answer stands, so no connection ever waits for one.
var works [2]struct {
	sync.Mutex
	state   support
	checked time.Time
	testing bool // a test is under way
	doubted bool // a connection was served unseen: test again
	done    chan struct{}
}

const (
	selfTestSoon  = 5 * time.Second
	selfTestAgain = time.Minute
	selfTestWait  = 2 * time.Second
)

// Works reports whether the check is on for IPv4 loopback connections:
// everywhere but on a computer known not to show whose a connection is.
func Works() bool { return worksFor(false) }

// worksFor is Works for IPv4 or for IPv6 loopback connections. Each is
// tried by itself: a listener of one family is guarded by what was found
// for that family.
func worksFor(v6 bool) bool { return supportFor(v6) != unsupported }

func family(v6 bool) int {
	if v6 {
		return 1
	}
	return 0
}

// supportFor is what the self-test found for a family. The first time, it
// waits for the test (one already under way included); afterwards it
// answers at once and has a test that is due made in the background.
func supportFor(v6 bool) support {
	w := &works[family(v6)]
	w.Lock()
	run := runSelfTest
	if w.state == untested {
		if !w.testing {
			w.testing, w.done = true, make(chan struct{})
			w.Unlock()
			finishTest(v6, run(v6))
			w.Lock()
		} else {
			done := w.done
			w.Unlock()
			<-done
			w.Lock()
		}
		state := w.state
		w.Unlock()
		return state
	}
	age := time.Since(w.checked)
	due := w.state == unanswered && age >= selfTestSoon ||
		w.state == unsupported && age >= selfTestAgain ||
		w.doubted && age >= selfTestAgain
	if due && !w.testing {
		w.testing, w.doubted, w.done = true, false, make(chan struct{})
		go func() { finishTest(v6, run(v6)) }()
	}
	state := w.state
	w.Unlock()
	return state
}

func finishTest(v6 bool, state support) {
	w := &works[family(v6)]
	w.Lock()
	w.state, w.checked, w.testing = state, time.Now(), false
	close(w.done)
	w.Unlock()
}

// doubt is told of a connection served though the system did not show
// whose it was: the self-test is made again (at most once a minute), in case
// the system no longer shows every socket.
func doubt(v6 bool) {
	w := &works[family(v6)]
	w.Lock()
	w.doubted = true
	w.Unlock()
	supportFor(v6)
}

// runSelfTest and whyNoAnswer are selfTest and queryFailure; tests stand
// in for them (runSelfTest is read under the family's lock).
var (
	runSelfTest = selfTest
	whyNoAnswer = queryFailure
)

// selfTest has another process of this user connect to it, as every real
// client is, and asks whose its listener and that connection are. A system
// that shows only this process's own sockets says nothing about the clients:
// the test finds that out, and the check stands aside.
func selfTest(v6 bool) support {
	network, address := "tcp4", "127.0.0.1:0"
	from := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
	if v6 {
		network, address = "tcp6", "[::1]:0"
		from = &net.TCPAddr{IP: net.IPv6loopback}
	}
	me := uint32(os.Getuid())
	ln, err := net.Listen(network, address)
	if err != nil {
		return unanswered
	}
	defer ln.Close()
	deadline := time.Now().Add(selfTestWait)
	if tcp, ok := ln.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(deadline)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if uid, ok := ListenerUID(addr); !ok {
		return failed(addr, from)
	} else if uid != me {
		return unsupported
	}
	server, stop, err := connectFromProbe(ln, addr, deadline)
	if err != nil {
		return unanswered
	}
	defer stop()
	defer server.Close()
	switch uid, kind := peerSince(server, time.Now()); {
	case kind == peerFound && uid == me:
		return working
	case !otherEndOpen(server):
		// The probe went away (it crashed, or something stopped it):
		// nothing was learned.
		return unanswered
	case kind == peerUnseen:
		return failed(server.RemoteAddr().(*net.TCPAddr), addr)
	default:
		// Another user's, or closed, though the probe is alive and holding
		// it: the system's answers are wrong here.
		return unsupported
	}
}

// probeReaders bounds the connections read at once while the self-test
// waits for its probe's.
const probeReaders = 32

// connectFromProbe starts this program again as the probe (see probeEnv),
// and returns this end of its connection: the one that says the probe's
// token. Any account may connect to the test's port: every connection is
// read side by side, and those that do not say the token are closed.
func connectFromProbe(ln net.Listener, addr *net.TCPAddr, deadline time.Time) (net.Conn, func(), error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	token := hex.EncodeToString(raw)
	// "version": a release without the probe prints its version and exits
	// rather than start a server.
	cmd := exec.Command(exe, "version")
	cmd.Env = append(withoutProbe(os.Environ()), probeEnv+"="+addr.String()+","+token)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	stop := func() { stdin.Close(); _ = cmd.Wait() }
	found := make(chan net.Conn, 1)
	slots := make(chan struct{}, probeReaders)
	var readers sync.WaitGroup
	accepted := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				accepted <- err
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close() // too many at once: not the probe's alone
				continue
			}
			readers.Add(1)
			go func() {
				defer readers.Done()
				defer func() { <-slots }()
				_ = conn.SetReadDeadline(deadline)
				line, _ := bufio.NewReader(conn).ReadString('\n')
				_ = conn.SetReadDeadline(time.Time{})
				if strings.TrimSpace(line) == token {
					select {
					case found <- conn:
						return
					default:
					}
				}
				conn.Close()
			}()
		}
	}()
	select {
	case conn := <-found:
		// The listener closes when the test ends; what it still accepts is
		// closed by the readers.
		return conn, stop, nil
	case err := <-accepted:
		readers.Wait()
		select {
		case conn := <-found:
			return conn, stop, nil
		default:
		}
		stop()
		return nil, nil, err
	}
}

func withoutProbe(environ []string) []string {
	out := environ[:0:0]
	for _, kv := range environ {
		if !strings.HasPrefix(kv, probeEnv+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// failed is what the system not showing one of the self-test's own sockets
// means: a system without answers, or one that does not show it though it
// is there, is unsupported; one that did not answer this time is not.
func failed(local, remote *net.TCPAddr) support {
	switch err := whyNoAnswer(local, remote); {
	case errors.Is(err, errUnsupported), errors.Is(err, errNotFound):
		return unsupported
	default:
		return unanswered
	}
}

// SameUser wraps a listener on this computer so it takes connections only
// from the user this process runs as. Root's are refused too: root could
// read this user's files anyway, but a program that runs as root and passes
// connections on for others, as some SSH and VPN services do, would let
// those others in as root (AllowOthersEnv=root lets root in).
//
// Only a definite answer turns a connection away: the system showing it as
// another account's, or as let go of by its program (a sender that has
// gone, whose request nobody waits for), on a system the self-test did not
// find unsupported. A connection whose socket the system does not show is
// served while its other end is open, and the self-test is made again: the
// check never turns this user's own programs away. One that is closed from
// the other end already is not served. Where the computer cannot be asked
// at all, or AllowOthersEnv lifts the check, ln is returned as it is.
func SameUser(ln net.Listener) net.Listener {
	addr, _ := ln.Addr().(*net.TCPAddr)
	everyone, root := allowed()
	if everyone || addr == nil || !lookupsAvailable {
		return ln
	}
	g := guard(ln, uint32(os.Getuid()), root, nil)
	v6 := addr.IP.To4() == nil
	g.peer = peerSince
	g.otherEndOpen = otherEndOpen
	g.trusted = func() bool { return worksFor(v6) }
	g.doubt = func() { doubt(v6) }
	g.serveUnseen = serveUnseen
	return g
}

// guard makes the listener wrapper; owner, when set, stands in for the
// system's answer in tests: ok means found.
func guard(ln net.Listener, uid uint32, root bool, owner func(net.Conn) (uint32, bool)) *sameUser {
	g := &sameUser{Listener: ln, uid: uid, root: root,
		taken: make(chan net.Conn), quit: make(chan struct{}), stopped: make(chan struct{})}
	if owner != nil {
		g.peer = func(conn net.Conn, _ time.Time) (uint32, peerKind) {
			if uid, ok := owner(conn); ok {
				return uid, peerFound
			}
			return 0, peerUnseen
		}
	}
	return g
}

// Enforced reports whether SameUser refuses other users here.
func Enforced() bool {
	everyone, _ := allowed()
	return !everyone && lookupsAvailable && Works()
}

type sameUser struct {
	net.Listener
	uid  uint32
	root bool // root's connections are taken as well
	// peer is what the system shows of a connection's other end, accepted
	// no earlier than the time given.
	peer func(net.Conn, time.Time) (uint32, peerKind)
	// otherEndOpen says whether the other end of a connection has not closed
	// it; trusted, whether definite answers count (the self-test did not
	// find the system unsupported); doubt is told of a connection served
	// unseen.
	otherEndOpen func(net.Conn) bool
	trusted      func() bool
	doubt        func()
	serveUnseen  bool      // see serveUnseen
	logged       time.Time // the last refusal logged
	unseenLogged time.Time // the last connection served unseen, logged
	logMu        sync.Mutex

	start   sync.Once
	closing sync.Once
	taken   chan net.Conn  // connections found to be this user's
	quit    chan struct{}  // closed to stop everything
	stopped chan struct{}  // closed when run has ended
	err     error          // why run ended, read after stopped
	checks  sync.WaitGroup // lookups in flight
}

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
		accepted := time.Now()
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
			if !l.admit(conn, accepted) {
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

// serveUnseen says whether a connection whose socket the system does not
// show may be served (see admit): on a Mac, where a system that did not
// show this user's own programs' sockets once turned every one of them
// away. Linux's kernel shows every socket it carries; one it does not show
// is not a client of this computer's own (a connection whose address a NAT
// rule rewrote, a sender that reset it), and is refused.
var serveUnseen = runtime.GOOS == "darwin"

// recheckWait is how long admit waits before asking again about a
// connection the system did not show.
var recheckWait = 20 * time.Millisecond

// admit decides whether a connection is served.
func (l *sameUser) admit(conn net.Conn, accepted time.Time) bool {
	uid, kind := l.peer(conn, accepted)
	if kind == peerUnseen && l.serveUnseen && loopbackPeer(conn) && l.otherEndOpen != nil && l.otherEndOpen(conn) {
		// Asked again a moment later: a sender that reset its connection
		// is no longer open then, one that was only being set up is shown.
		time.Sleep(recheckWait)
		uid, kind = l.peer(conn, time.Now())
		if kind == peerUnseen && l.otherEndOpen(conn) {
			// Still open, still not shown, from this computer's own address:
			// a client waiting for its answer whose socket this system does
			// not show. Served: the check must never turn this user's own
			// programs away. The self-test is made again.
			l.logUnseen(conn)
			if l.doubt != nil {
				l.doubt()
			}
			return true
		}
	}
	switch {
	case kind == peerFound && (uid == l.uid || l.root && uid == 0):
		return true
	case l.trusted != nil && !l.trusted():
		// The self-test found this system's answers mean nothing: nothing
		// is refused for them.
		return true
	}
	l.logRefusal(conn, uid, kind)
	return false
}

// loopbackPeer reports whether conn comes from a loopback address, as a
// program on this computer connecting to it directly does.
func loopbackPeer(conn net.Conn) bool {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	return ok && remote.IP.IsLoopback()
}

// logUnseen says, at most once a minute, that a connection was served
// though the system did not show whose it was.
func (l *sameUser) logUnseen(conn net.Conn) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	if time.Since(l.unseenLogged) < time.Minute {
		return
	}
	l.unseenLogged = time.Now()
	log.Printf("served a connection to %s from %s whose program this system did not show; checking again whether it shows every program's connections", l.Addr(), conn.RemoteAddr())
}

// logRefusal says, at most once a minute, that a connection was refused,
// so that someone wondering why a program gets no answer finds out.
func (l *sameUser) logRefusal(conn net.Conn, uid uint32, kind peerKind) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	if time.Since(l.logged) < time.Minute {
		return
	}
	l.logged = time.Now()
	who := "a program whose socket this system did not show"
	switch kind {
	case peerFound:
		who = "another account's program (uid " + strconv.FormatUint(uint64(uid), 10) + ")"
		if uid == 0 {
			who = "a program running as root"
		}
	case peerGone:
		who = "a program that had already closed it"
	}
	from := "?"
	if conn != nil {
		from = conn.RemoteAddr().String()
	}
	log.Printf("refused a connection to %s from %s (%s); %s=1 lets every account's programs in, %s=root root's", l.Addr(), from, who, AllowOthersEnv, AllowOthersEnv)
}

// Transport is an HTTP transport for this user's own programs, the command
// line and the tray, that talk to Switcher on this computer. A connection
// to this computer is checked before a byte goes over it: the port, and the
// program that took the connection, must be this user's (or root's, where
// the user lets root in). Another account's program that took Switcher's
// port would otherwise be handed whatever the request holds; the device
// token itself never travels (see internal/deviceproof). Connections
// elsewhere go as with base (the default transport when nil).
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
	if everyone, _ := allowed(); everyone || remote == nil || !remote.IP.IsLoopback() || !lookupsAvailable {
		return nil
	}
	// This user's, or root's only where the user lets root in: on macOS a
	// launchd socket that any user can set up is held by root.
	_, root := allowed()
	mine := func(uid uint32) bool { return uid == uint32(os.Getuid()) || root && uid == 0 }
	// An answer naming another account stops the request, on a system whose
	// answers the self-test found to mean something (made then, when first
	// needed). A listener the system does not show, asked a few times, stops
	// it on Linux, whose kernel shows every listener; on a Mac it never keeps
	// this user from their own Switcher (see serveUnseen; the device token is
	// never sent, see internal/deviceproof).
	trusted := func() bool { return worksFor(remote.IP.To4() == nil) }
	for try := 0; ; try++ {
		listener, ok := ListenerUID(remote)
		if ok && !mine(listener) && trusted() {
			return &OtherUserError{Port: remote.Port, UID: listener}
		}
		if ok {
			break
		}
		if try == 2 {
			if !serveUnseen && trusted() {
				return &OtherUserError{Port: remote.Port, Unknown: true}
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And whose this very connection's other end is. Before the server has
	// taken it, the kernel shows it under the listener's owner.
	if taker, ok := serverEndUID(conn); ok && !mine(taker) && trusted() {
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
	who := "another user (uid " + strconv.FormatUint(uint64(e.UID), 10) + ")"
	if e.UID == 0 {
		who = "root"
	}
	return "port " + strconv.Itoa(e.Port) + " on this computer is held by " + who + ", not by your Switcher; nothing was sent to it (" +
		AllowOthersEnv + "=root sends to root's)"
}
