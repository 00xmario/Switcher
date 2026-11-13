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
	// A listener that cannot be seen is not known to be this user's.
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) { return socket{}, false }
	if _, err = get(); !errors.As(err, &other) || !other.Unknown {
		t.Fatalf("a port of unknown owner was used: %v", err)
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
	w.state, w.checked = state, time.Now()
	w.Unlock()
	t.Cleanup(func() {
		w.Lock()
		w.state, w.checked = was, wasChecked
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

	run := runSelfTest
	defer func() { runSelfTest = run }()
	tests := 0
	runSelfTest = func(bool) support { tests++; return unanswered }
	setSupport(t, unanswered)
	if !worksFor(false) || tests != 0 {
		t.Fatalf("an unanswered self-test turned the check off, or was made again at once (%d)", tests)
	}
	works[0].Lock()
	works[0].checked = time.Now().Add(-selfTestSoon)
	works[0].Unlock()
	if !worksFor(false) || tests != 1 {
		t.Fatalf("an unanswered self-test was not made again soon (%d)", tests)
	}
	runSelfTest = func(bool) support { tests++; return unsupported }
	works[0].Lock()
	works[0].checked = time.Now().Add(-selfTestSoon)
	works[0].Unlock()
	if worksFor(false) {
		t.Fatal("a system without answers kept the check on")
	}
	// Once seen working, it stays on.
	runSelfTest = func(bool) support { tests++; return unanswered }
	setSupport(t, working)
	works[0].Lock()
	works[0].checked = time.Now().Add(-time.Hour)
	works[0].Unlock()
	before := tests
	if !worksFor(false) || tests != before {
		t.Fatal("a working check was tested again")
	}
}

// While the self-test is unanswered, a connection whose owner cannot be
// told is refused, by the server and by the command line alike; only on a
// system known not to answer is it taken.
func TestUnknownOwnersAreRefusedUntilTheSystemIsKnownNotToAnswer(t *testing.T) {
	real := lookup
	defer func() { lookup = real }()
	lookup = func(*net.TCPAddr, *net.TCPAddr, time.Time) (socket, bool) { return socket{}, false }
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guarded := SameUser(ln)
	defer guarded.Close()
	taken := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := guarded.Accept()
			if err != nil {
				return
			}
			taken <- conn
		}
	}()
	try := func() (bool, error) {
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
		case <-time.After(300 * time.Millisecond):
			return false, checked
		}
	}
	setSupport(t, unanswered)
	if served, err := try(); served || err == nil {
		t.Fatalf("with the self-test unanswered: served=%v, the command line's check: %v", served, err)
	}
	setSupport(t, unsupported)
	if served, err := try(); !served || err != nil {
		t.Fatalf("on a system known not to answer: served=%v, the command line's check: %v", served, err)
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
