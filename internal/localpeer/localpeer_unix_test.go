//go:build linux || darwin

package localpeer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func needsKernel(t *testing.T) {
	t.Helper()
	if !Works() {
		t.Skip("this system does not say whose a connection is")
	}
}

func me() uint32 { return uint32(os.Getuid()) }

func TestThisComputerSaysWhoOwnsAConnection(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if uid, ok := ListenerUID(addr); !ok || uid != me() {
		t.Fatalf("listener owner: %d %v", uid, ok)
	}
	if _, ok := ListenerUID(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); ok {
		t.Fatal("a port nobody listens on has an owner")
	}
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if uid, ok := PeerUID(server); !ok || uid != me() {
		t.Fatalf("peer owner: %d %v", uid, ok)
	}
	// The other way round: the client asks whose the accepted end is.
	if uid, ok := PeerUID(client); !ok || uid != me() {
		t.Fatalf("server end owner: %d %v", uid, ok)
	}
	// A pair of addresses nothing is connected between has no owner, even
	// though a listener is there for one of them.
	if _, ok := socketUID(addr, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, time.Now()); ok {
		t.Fatal("a listener passed for a connection")
	}
}

// A socket of the other family takes this port's IPv4 connections too: one
// listening on every address ("[::]"), or an IPv6 socket a client connects
// from. The kernel is asked which socket it is, so both are found.
func TestSocketsOfTheOtherFamilyAreFound(t *testing.T) {
	needsKernel(t)
	dual, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Skip("no IPv6 here")
	}
	defer dual.Close()
	port := dual.Addr().(*net.TCPAddr).Port
	v4 := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	if uid, ok := ListenerUID(v4); !ok || uid != me() {
		t.Fatalf("a listener on every address is not found for 127.0.0.1: %d %v", uid, ok)
	}

	// An IPv6 socket connecting to 127.0.0.1, as Java and .NET clients do.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skip("no IPv6 sockets here")
	}
	defer unix.Close(fd)
	to := &unix.SockaddrInet6{Port: ln.Addr().(*net.TCPAddr).Port}
	copy(to.Addr[:], net.IPv4(127, 0, 0, 1).To16())
	if err := unix.Connect(fd, to); err != nil {
		t.Skipf("no IPv4 over IPv6 sockets here: %v", err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if uid, ok := PeerUID(server); !ok || uid != me() {
		t.Fatalf("a client on an IPv6 socket is not this user's: %d %v", uid, ok)
	}
}

func TestIPv6LoopbackConnectionsHaveAnOwner(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	defer ln.Close()
	if uid, ok := ListenerUID(ln.Addr().(*net.TCPAddr)); !ok || uid != me() {
		t.Fatalf("listener owner: %d %v", uid, ok)
	}
	client, err := net.Dial("tcp6", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if uid, ok := PeerUID(server); !ok || uid != me() {
		t.Fatalf("peer owner: %d %v", uid, ok)
	}
}

// A sender that sends its request and leaves before the server looks no
// longer owns its end; such a connection has no known owner.
func TestGoneSenderIsNotThisUser(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(client, "POST /api/quit HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	client.Close()
	time.Sleep(100 * time.Millisecond)
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// Not this user's, and not root's either, though the kernel reports a
	// socket nobody holds any more under uid 0: it has no owner at all.
	if uid, ok := PeerUID(server); ok {
		t.Fatalf("a connection whose sender left still has an owner (uid %d)", uid)
	}
}

// The server takes this user's connections, and drops another user's and
// one whose owner is unknown, without ever handing them to the HTTP server.
func TestSameUserDropsOtherUsers(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SameUser(ln).(*sameUser); !ok {
		t.Fatal("the listener was not guarded")
	}
	var verdict atomic.Int32 // 0: as the kernel says, 1: another user, 2: unknown
	guarded := guard(ln, me(), false, func(conn net.Conn) (uint32, bool) {
		uid, ok := PeerUID(conn)
		switch verdict.Load() {
		case 1:
			return uid + 1, ok
		case 2:
			return 0, false
		}
		return uid, ok
	})
	accepted := make(chan net.Conn, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := guarded.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	defer func() { guarded.Close(); <-done }()
	dial := func() net.Conn {
		conn, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	mine := dial()
	defer mine.Close()
	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("this user's own connection was dropped")
	}
	for name, v := range map[string]int32{"another user's": 1, "one of unknown owner": 2} {
		verdict.Store(v)
		conn := dial()
		select {
		case got := <-accepted:
			got.Close()
			t.Fatalf("%s connection reached the server", name)
		case <-time.After(300 * time.Millisecond):
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s connection was left open", name)
		}
		conn.Close()
	}
}

// Root's connections are refused like any other account's: a program that
// runs as root and passes connections on for others would let them in. They
// are taken only where the user asks for that.
func TestRootIsServedOnlyOnRequest(t *testing.T) {
	for _, allowRoot := range []bool{false, true} {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		guarded := guard(ln, me()+7, allowRoot, func(net.Conn) (uint32, bool) { return 0, true })
		client, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		taken := make(chan error, 1)
		go func() {
			conn, err := guarded.Accept()
			if err == nil {
				conn.Close()
			}
			taken <- err
		}()
		select {
		case err := <-taken:
			if !allowRoot || err != nil {
				t.Fatalf("allowRoot=%v: root's connection was taken (%v)", allowRoot, err)
			}
		case <-time.After(300 * time.Millisecond):
			if allowRoot {
				t.Fatal("root's connection was dropped though root is allowed")
			}
		}
		client.Close()
		guarded.Close()
	}
	t.Setenv(AllowOthersEnv, "root")
	if everyone, root := allowed(); everyone || !root || !Enforced() && Works() {
		t.Fatalf("%s=root: everyone=%v root=%v", AllowOthersEnv, everyone, root)
	}
}

// Closing does not wait for anyone to take what was checked: with every
// check done and nobody calling Accept, as when a server shuts down, it
// still returns, and the connections are closed.
func TestCloseDoesNotWaitForAccept(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guarded := guard(ln, me(), false, PeerUID)
	// One Accept starts the taking; nobody asks again.
	first, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if conn, err := guarded.Accept(); err != nil {
		t.Fatal(err)
	} else {
		conn.Close()
	}
	var clients []net.Conn
	for i := 0; i < checksAtOnce+8; i++ {
		conn, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
	}
	time.Sleep(100 * time.Millisecond) // all checked, all waiting to be taken
	closed := make(chan struct{})
	go func() { guarded.Close(); guarded.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waits for someone to call Accept")
	}
	if _, err := guarded.Accept(); err == nil {
		t.Fatal("Accept after Close returned a connection")
	}
	for _, conn := range clients {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("a connection waiting to be taken was left open")
		}
		conn.Close()
	}
	// Closing a listener nobody ever accepted from is fine too.
	other, _ := net.Listen("tcp4", "127.0.0.1:0")
	idle := guard(other, me(), false, PeerUID)
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := idle.Accept(); err == nil {
		t.Fatal("Accept on a closed listener returned a connection")
	}
}

func TestSameUserCanBeTurnedOff(t *testing.T) {
	t.Setenv(AllowOthersEnv, "1")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if SameUser(ln) != ln || Enforced() {
		t.Fatal("the check stayed on")
	}
}

// Clients send nothing to a local port another account holds: the check is
// made on the connection itself, before a byte goes over it.
func TestTransportRefusesAnotherUsersServer(t *testing.T) {
	needsKernel(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "switcher")
	}))
	defer srv.Close()
	client := &http.Client{Transport: Transport(nil), Timeout: 5 * time.Second}
	get := func() (string, error) {
		resp, err := client.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body), nil
	}
	if body, err := get(); err != nil || body != "switcher" {
		t.Fatalf("this user's own server: %q %v", body, err)
	}
	// "localhost" and the dual-stack spelling reach it too.
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if resp, err := client.Get("http://localhost:" + port + "/"); err != nil {
		t.Fatalf("localhost: %v", err)
	} else {
		resp.Body.Close()
	}
	client.CloseIdleConnections()

	real := lookup
	defer func() { lookup = real }()
	before := hits.Load()
	// The port is another user's.
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
		found, ok := real(local, remote, since)
		if ok && found.listening {
			found.uid = me() + 1
		}
		return found, ok
	}
	_, err := get()
	var other *OtherUserError
	if !errors.As(err, &other) || other.UID != me()+1 || other.Unknown {
		t.Fatalf("another user's port was used: %v", err)
	}
	// The port looks like this user's, but another user's program took the
	// connection.
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
		found, ok := real(local, remote, since)
		if ok && !found.listening {
			found.uid = me() + 2
		}
		return found, ok
	}
	// The server end is live once the server has taken the connection.
	for i := 0; i < 50; i++ {
		if _, err = get(); err != nil {
			break
		}
		client.CloseIdleConnections()
	}
	if !errors.As(err, &other) || other.UID != me()+2 {
		t.Fatalf("a connection another user's program took was used: %v", err)
	}
	// A listener the system does not show: on Linux, whose kernel shows
	// every listener, nothing is sent to it; on a Mac it never keeps this
	// user from their own Switcher.
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) { return socket{}, false }
	_, err = get()
	if serveUnseen && err != nil {
		t.Fatalf("a port the system does not show was refused on a Mac: %v", err)
	}
	if !serveUnseen && (!errors.As(err, &other) || !other.Unknown) {
		t.Fatalf("a port the system does not show was used on Linux: %v", err)
	}
	// A port held by root is refused too: on macOS any user can have
	// launchd, which runs as root, hold one. Root is let in on request.
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
		found, ok := real(local, remote, since)
		found.uid = 0
		return found, ok
	}
	if _, err := get(); !errors.As(err, &other) || other.UID != 0 || !strings.Contains(err.Error(), "root") {
		t.Fatalf("root's server was used: %v", err)
	}
	t.Setenv(AllowOthersEnv, "root")
	client.CloseIdleConnections()
	if _, err := get(); err != nil {
		t.Fatalf("root's server was refused though root is let in: %v", err)
	}
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) { return socket{}, false }
	t.Setenv(AllowOthersEnv, "1")
	if _, err := get(); err != nil {
		t.Fatalf("the check stayed on: %v", err)
	}
	if got := hits.Load() - before; got < 2 {
		t.Fatalf("allowed requests did not reach the server: %d", got)
	}
}

// Requests to other computers are not this check's: they go as before.
func TestTransportLeavesOtherComputersAlone(t *testing.T) {
	var asked []string
	base := baseFunc(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.Host)
		return nil, errors.New("not sent in this test")
	})
	transport := Transport(base)
	for _, target := range []string{"http://192.0.2.1:8787/", "https://example.com/", "http://switcher-host/"} {
		r, _ := http.NewRequest(http.MethodGet, target, nil)
		_, _ = transport.RoundTrip(r)
	}
	if len(asked) != 3 {
		t.Fatalf("requests elsewhere did not use the base transport: %v", asked)
	}
}

// The kernel finds a socket by its pair of addresses, not by which
// connection it is. A sender that leaves a request behind and resets its
// connection frees its port; the next program to connect from that port,
// this user's own say, must not lend the dead connection its owner.
func TestADeadConnectionDoesNotTakeTheNextOwnerOfItsPort(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A free port to connect from, twice.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	from := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: probe.Addr().(*net.TCPAddr).Port}
	probe.Close()
	dial := func() net.Conn {
		dialer := net.Dialer{LocalAddr: from, Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1) })
		}}
		conn, err := dialer.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	first := dial()
	fmt.Fprint(first, "POST /api/quit HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	time.Sleep(20 * time.Millisecond)
	_ = first.(*net.TCPConn).SetLinger(0) // reset, not a tidy close
	first.Close()
	time.Sleep(20 * time.Millisecond)
	dead, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	if _, ok := PeerUID(dead); ok {
		t.Fatal("a reset connection has an owner")
	}
	// This user's next connection comes from the very same port.
	next := dial()
	defer next.Close()
	if uid, ok := PeerUID(dead); ok {
		t.Fatalf("the dead connection took the owner (uid %d) of the next connection from its port", uid)
	}
	// What it left behind can still be read: that is why it must be refused.
	_ = dead.SetReadDeadline(time.Now().Add(time.Second))
	left := make([]byte, 64)
	if n, _ := dead.Read(left); n == 0 {
		t.Log("nothing was left to read on the dead connection")
	}
	// The live connection itself is this user's.
	live, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if uid, ok := PeerUID(live); !ok || uid != me() {
		t.Fatalf("the live connection: %d %v", uid, ok)
	}
	// So is one whose sender is done sending but still there to be answered.
	_ = next.(*net.TCPConn).CloseWrite()
	time.Sleep(20 * time.Millisecond)
	if uid, ok := PeerUID(live); !ok || uid != me() {
		t.Fatalf("a connection whose sender finished sending: %d %v", uid, ok)
	}
}

type baseFunc func(*http.Request) (*http.Response, error)

func (f baseFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// setSupport puts the self-test's finding for IPv4 in place for a test.
func setSupport(t *testing.T, state support) {
	t.Helper()
	w := &works[0]
	w.Lock()
	was, wasChecked := w.state, w.checked
	w.state, w.checked, w.doubted = state, time.Now(), false
	w.Unlock()
	t.Cleanup(func() {
		for i := 0; i < 400; i++ { // a background test under way finishes first
			w.Lock()
			testing := w.testing
			w.Unlock()
			if !testing {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		w.Lock()
		w.state, w.checked, w.doubted = was, wasChecked, false
		w.Unlock()
	})
}

// A self-test that could not be made, or that the system did not answer
// this time, leaves the check on and is made again soon: another account
// can bring that about, by taking every free port or keeping the socket list
// busy, and must not turn the check off by it. Only a system that takes no
// questions, or answers wrongly about this process's own sockets, turns it
// off.
func TestASelfTestThatCannotBeMadeLeavesTheCheckOn(t *testing.T) {
	why := whyNoAnswer
	defer func() { whyNoAnswer = why }()
	for err, want := range map[error]support{
		errTransient:   unanswered,
		nil:            unanswered,
		errUnsupported: unsupported,
		errNotFound:    unsupported,
	} {
		whyNoAnswer = func(*net.TCPAddr, *net.TCPAddr) error { return err }
		if got := failed(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}); got != want {
			t.Errorf("a lookup failing with %v: %v, want %v", err, got, want)
		}
	}

	var tests atomic.Int32
	result := unanswered
	var resultMu sync.Mutex
	useSelfTest(t, func(bool) support {
		tests.Add(1)
		resultMu.Lock()
		defer resultMu.Unlock()
		return result
	})
	settled := func() {
		t.Helper()
		for i := 0; i < 200; i++ {
			works[0].Lock()
			testing := works[0].testing
			works[0].Unlock()
			if !testing {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("a background self-test never finished")
	}
	age := func(d time.Duration) {
		works[0].Lock()
		works[0].checked = time.Now().Add(-d)
		works[0].Unlock()
	}
	setSupport(t, unanswered)
	if !worksFor(false) || tests.Load() != 0 {
		t.Fatalf("an unanswered self-test turned the check off, or was made again at once (%d)", tests.Load())
	}
	// Made again soon, in the background: the answer stands meanwhile.
	age(selfTestSoon)
	if !worksFor(false) {
		t.Fatal("the check went off while a test ran")
	}
	settled()
	if tests.Load() != 1 {
		t.Fatalf("an unanswered self-test was not made again soon (%d)", tests.Load())
	}
	resultMu.Lock()
	result = unsupported
	resultMu.Unlock()
	age(selfTestSoon)
	worksFor(false)
	settled()
	if worksFor(false) {
		t.Fatal("a system without answers kept the check on")
	}
	// Once seen working, it stays so, unless a connection was served unseen.
	resultMu.Lock()
	result = unanswered
	resultMu.Unlock()
	setSupport(t, working)
	age(time.Hour)
	before := tests.Load()
	worksFor(false)
	settled()
	if tests.Load() != before {
		t.Fatal("a working check was tested again")
	}
}

// The first self-test is waited for, also by a caller that comes while it
// runs; no one waits for a later one.
func TestOnlyTheFirstSelfTestIsWaitedFor(t *testing.T) {
	release := make(chan struct{})
	useSelfTest(t, func(bool) support { <-release; return working })
	setSupport(t, untested)
	answers := make(chan support, 2)
	for i := 0; i < 2; i++ {
		go func() { answers <- supportFor(false) }()
	}
	select {
	case got := <-answers:
		t.Fatalf("answered %v before the first test finished", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if got := <-answers; got != working {
			t.Fatalf("the first test: %v", got)
		}
	}
	// A later test does not hold anyone up.
	block := make(chan struct{})
	defer close(block)
	useSelfTest(t, func(bool) support { <-block; return unsupported })
	works[0].Lock()
	works[0].state, works[0].checked, works[0].doubted = working, time.Now().Add(-2*selfTestAgain), true
	works[0].Unlock()
	done := make(chan support, 1)
	go func() { done <- supportFor(false) }()
	select {
	case got := <-done:
		if got != working {
			t.Fatalf("while a later test runs: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a later self-test held a caller up")
	}
}

// A connection whose owner the system does not show is served while it is
// open, by the server and the command line alike, whatever the self-test
// found: the check never turns this user's own programs away. It makes the
// self-test again instead. (One whose sender is gone is refused: see
// TestGoneSenderIsNotThisUser.)
func TestOpenConnectionsWhoseOwnerIsUnseenAreServed(t *testing.T) {
	real, wasServe := lookup, serveUnseen
	defer func() { lookup, serveUnseen = real, wasServe }()
	lookup = func(*net.TCPAddr, *net.TCPAddr, time.Time) (socket, bool) { return socket{}, false }
	serve := func(mac bool) (bool, error) {
		serveUnseen = mac
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		guarded := SameUser(ln)
		defer guarded.Close()
		taken := make(chan net.Conn, 1)
		go func() {
			if conn, err := guarded.Accept(); err == nil {
				taken <- conn
			}
		}()
		conn, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		checked := ownConnection(conn)
		select {
		case c := <-taken:
			c.Close()
			return true, checked
		case <-time.After(time.Second):
			return false, checked
		}
	}
	for _, state := range []support{working, unanswered} {
		setSupport(t, state)
		if served, err := serve(true); !served || err != nil {
			t.Fatalf("on a Mac, self-test %v: served=%v, the command line's check: %v", state, served, err)
		}
		if served, err := serve(false); served || err == nil {
			t.Fatalf("on Linux, self-test %v: served=%v, the command line's check: %v", state, served, err)
		}
	}
	// Where the self-test found the system's answers mean nothing, nothing
	// is refused for them, on Linux too.
	setSupport(t, unsupported)
	if served, err := serve(false); !served || err != nil {
		t.Fatalf("on Linux, system unsupported: served=%v, the command line's check: %v", served, err)
	}
	// On a Mac, after one is served unseen, the self-test is made again (at
	// most once a minute).
	setSupport(t, working)
	works[0].Lock()
	works[0].checked = time.Now().Add(-2 * selfTestAgain)
	works[0].Unlock()
	tested := make(chan bool, 4)
	useSelfTest(t, func(bool) support { tested <- true; return working })
	if served, _ := serve(true); !served {
		t.Fatal("not served")
	}
	serve(true)
	select {
	case <-tested:
	case <-time.After(time.Second):
		t.Fatal("the self-test was not made again after an unseen connection")
	}
}

// Only the values that say so let every account in.
func TestOnlyAYesLetsEveryoneIn(t *testing.T) {
	for value, want := range map[string][2]bool{
		"": {false, false}, "0": {false, false}, "false": {false, false}, "no": {false, false}, "off": {false, false}, "whatever": {false, false},
		"1": {true, true}, "true": {true, true}, "yes": {true, true}, "on": {true, true},
		"root": {false, true},
	} {
		t.Setenv(AllowOthersEnv, value)
		if everyone, root := allowed(); everyone != want[0] || root != want[1] {
			t.Errorf("%s=%q: everyone=%v root=%v", AllowOthersEnv, value, everyone, root)
		}
	}
}

// Another account may connect to the self-test's port before the test's own
// connection arrives: the test asks only about its own, and a stranger's
// cannot turn the check off. (On a computer short of free ports the test
// may not be made at all, which leaves the check on: it is tried again.)
func TestASelfTestAsksOnlyAboutItsOwnConnection(t *testing.T) {
	needsKernel(t)
	real := lookup
	defer func() { lookup = real }()
	for attempt := 0; attempt < 5; attempt++ {
		var stranger net.Conn
		var strangerPort int
		lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
			// While the test asks whose its listener is, a stranger connects.
			if remote.Port == 0 && stranger == nil {
				if conn, err := net.Dial("tcp4", local.String()); err == nil {
					stranger, strangerPort = conn, conn.LocalAddr().(*net.TCPAddr).Port
					time.Sleep(50 * time.Millisecond)
				}
			}
			found, ok := real(local, remote, since)
			if ok && !found.listening && local.Port == strangerPort {
				found.uid = me() + 1
			}
			return found, ok
		}
		got := selfTest(false)
		if stranger != nil {
			stranger.Close()
		}
		if got == unsupported {
			t.Fatal("a stranger's connection turned the check off")
		}
		if got == working && stranger != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Skip("the self-test could not be made here with a stranger connected; it never turned the check off")
}

// A sender that has gone is refused even while its data keeps this end of
// the connection open: it queued a request and a few megabytes behind it,
// so its closing waits behind data this end has not read, and closed before
// Switcher looked. Its socket is there, let go of: gone, not unseen.
func TestASenderThatQueuedItsRequestAndLeftIsRefused(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guarded := SameUser(ln)
	defer guarded.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	request := append([]byte("POST /api/accounts/x/activate HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 4194304\r\n\r\n"), make([]byte, 4<<20)...)
	_ = client.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = client.Write(request) // fills what the connection holds, then stops
	client.Close()
	time.Sleep(50 * time.Millisecond)
	taken := make(chan net.Conn, 1)
	go func() {
		if conn, err := guarded.Accept(); err == nil {
			taken <- conn
		}
	}()
	select {
	case conn := <-taken:
		conn.Close()
		t.Fatal("a sender that had gone was served")
	case <-time.After(500 * time.Millisecond):
	}
}

// What the system shows as another account's, or as let go of, is refused;
// on a system the self-test found unsupported, nothing is refused for what
// it says.
func TestDefiniteAnswersCountOnlyWhereTheSystemIsTrusted(t *testing.T) {
	for _, c := range []struct {
		name    string
		kind    peerKind
		uid     uint32
		trusted bool
		served  bool
	}{
		{"this user's", peerFound, 1000, true, true},
		{"another account's", peerFound, 1001, true, false},
		{"another account's, system unsupported", peerFound, 1001, false, true},
		{"gone", peerGone, 1000, true, false},
		{"gone, system unsupported", peerGone, 1000, false, true},
	} {
		l := &sameUser{uid: 1000, peer: func(net.Conn, time.Time) (uint32, peerKind) { return c.uid, c.kind },
			trusted: func() bool { return c.trusted }, otherEndOpen: func(net.Conn) bool { return true }}
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		l.Listener = ln
		if got := l.admit(nil, time.Now()); got != c.served {
			t.Errorf("%s: served=%v", c.name, got)
		}
		ln.Close()
	}
}

// useSelfTest has f stand in for the self-test, swapped under the families'
// locks as the product reads it; it is put back once no test runs.
func useSelfTest(t *testing.T, f func(bool) support) {
	t.Helper()
	swap := func(to func(bool) support) func(bool) support {
		works[0].Lock()
		works[1].Lock()
		was := runSelfTest
		runSelfTest = to
		works[1].Unlock()
		works[0].Unlock()
		return was
	}
	was := swap(f)
	t.Cleanup(func() {
		for i := 0; i < 400; i++ {
			works[0].Lock()
			busy := works[0].testing || works[1].testing
			works[0].Unlock()
			if !busy {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		swap(was)
	})
}

// On a Mac an unseen connection is served only from this computer's own
// address and only if, asked again a moment later, it is still unseen and
// open: not one whose address a NAT rule rewrote, not a sender that reset.
func TestUnseenIsServedOnlyFromLoopbackAndAfterAskingAgain(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	asks := 0
	l := &sameUser{Listener: ln, uid: me(), serveUnseen: true, trusted: func() bool { return true }}
	l.peer = func(net.Conn, time.Time) (uint32, peerKind) { asks++; return 0, peerUnseen }
	open := true
	l.otherEndOpen = func(net.Conn) bool { return open }
	if !l.admit(server, time.Now()) || asks != 2 {
		t.Fatalf("an open, unseen connection from loopback: asked %d times", asks)
	}
	// Closed by the time it is asked again: a sender that reset.
	asks = 0
	l.otherEndOpen = func(net.Conn) bool { asks++; return asks < 2 }
	l.peer = func(net.Conn, time.Time) (uint32, peerKind) { return 0, peerUnseen }
	if l.admit(server, time.Now()) {
		t.Fatal("a connection closed when asked again was served")
	}
	// Shown as another account's when asked again.
	l.otherEndOpen = func(net.Conn) bool { return true }
	n := 0
	l.peer = func(net.Conn, time.Time) (uint32, peerKind) {
		n++
		if n == 1 {
			return 0, peerUnseen
		}
		return me() + 1, peerFound
	}
	if l.admit(server, time.Now()) {
		t.Fatal("a connection shown as another account's when asked again was served")
	}
	// Not from this computer's own address: a NAT rule rewrote it.
	nat := &fakeAddrConn{Conn: server, remote: &net.TCPAddr{IP: net.IPv4(192, 168, 1, 7), Port: 5000}}
	l.peer = func(net.Conn, time.Time) (uint32, peerKind) { return 0, peerUnseen }
	if l.admit(nat, time.Now()) {
		t.Fatal("an unseen connection from another address was served")
	}
}

type fakeAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *fakeAddrConn) RemoteAddr() net.Addr { return c.remote }

// On a Mac an unseen connection whose client's address nobody holds (a
// client gone without this end being told) is refused, as is one whose
// address another account holds as far as the system shows.
func TestAnUnseenGhostIsRefused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	l := &sameUser{Listener: ln, uid: me(), serveUnseen: true, trusted: func() bool { return true },
		peer:         func(net.Conn, time.Time) (uint32, peerKind) { return 0, peerUnseen },
		otherEndOpen: func(net.Conn) bool { return true }}
	l.held = func(net.Conn) bool { return false }
	if l.admit(server, time.Now()) {
		t.Fatal("an unseen connection whose address nobody holds was served")
	}
	l.held = func(net.Conn) bool { return true }
	if !l.admit(server, time.Now()) {
		t.Fatal("an unseen connection whose address its client holds was refused")
	}
}

// A live client holds the address it connected from; once it is torn down,
// nobody does.
func TestTheClientsAddressIsHeldWhileItLives(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	from := client.LocalAddr().(*net.TCPAddr)
	if !portHeld(from) {
		t.Fatal("a live client's address reads as free")
	}
	_ = client.(*net.TCPConn).SetLinger(0)
	client.Close()
	time.Sleep(20 * time.Millisecond)
	if portHeld(from) {
		t.Fatal("a reset client's address reads as held")
	}
}
