package localpeer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	if _, ok := socketUID(addr, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}); ok {
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
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
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

// Refusing costs a few system calls each, however many connections there
// are: a flood of refused ones must not keep this user's own out.
func TestRefusingAFloodDoesNotHoldUpThisUser(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Connections from 127.0.0.2 stand for another user's.
	guarded := guard(ln, me(), false, func(conn net.Conn) (uint32, bool) {
		uid, ok := PeerUID(conn)
		if conn.RemoteAddr().(*net.TCPAddr).IP.Equal(net.IPv4(127, 0, 0, 2)) {
			return uid + 1, ok
		}
		return uid, ok
	})
	accepted := make(chan net.Conn, 16)
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		for {
			conn, err := guarded.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	stop := make(chan struct{})
	// Everything this test started is over before it returns.
	defer func() {
		close(stop)
		guarded.Close()
		running.Wait()
	}()
	var refused atomic.Int64
	for i := 0; i < 16; i++ {
		running.Add(1)
		go func() {
			defer running.Done()
			dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}, Timeout: time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if conn, err := dialer.Dial("tcp4", ln.Addr().String()); err == nil {
					conn.Close()
					refused.Add(1)
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 5; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp4", ln.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-accepted:
			got.Close()
		case <-time.After(3 * time.Second):
			t.Fatalf("this user's connection was not taken within 3 s during a flood (%d refused so far)", refused.Load())
		}
		conn.Close()
		if waited := time.Since(start); waited > time.Second {
			t.Fatalf("this user's connection waited %s during a flood", waited)
		}
	}
	if refused.Load() < 500 {
		t.Logf("only %d connections were made by the flood", refused.Load())
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
	lookup = func(local, remote *net.TCPAddr) (socket, bool) {
		found, ok := real(local, remote)
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
	lookup = func(local, remote *net.TCPAddr) (socket, bool) {
		found, ok := real(local, remote)
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
	lookup = func(local, remote *net.TCPAddr) (socket, bool) { return socket{}, false }
	if _, err = get(); !errors.As(err, &other) || !other.Unknown {
		t.Fatalf("a port of unknown owner was used: %v", err)
	}
	// Root's is fine, and the check can be turned off.
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr) (socket, bool) {
		found, ok := real(local, remote)
		found.uid = 0
		return found, ok
	}
	if _, err := get(); err != nil {
		t.Fatalf("root's server was refused: %v", err)
	}
	client.CloseIdleConnections()
	lookup = func(local, remote *net.TCPAddr) (socket, bool) { return socket{}, false }
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

// A socket bound to the loopback interface takes this computer's local
// connections like any other. It must be found: an unseen listener would
// pass for nobody's, and its clients would be dropped.
func TestSocketsBoundToTheLoopbackInterfaceAreFound(t *testing.T) {
	needsKernel(t)
	bound := func(_, _ string, c syscall.RawConn) error {
		var bindErr error
		if err := c.Control(func(fd uintptr) { bindErr = unix.BindToDevice(int(fd), "lo") }); err != nil {
			return err
		}
		return bindErr
	}
	ln, err := (&net.ListenConfig{Control: bound}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("this kernel does not let a program bind to the loopback interface: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if uid, ok := ListenerUID(addr); !ok || uid != me() {
		t.Fatalf("a listener bound to the loopback interface is not found: %d %v", uid, ok)
	}
	// A plain client reaches it, and both ends are found.
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
		t.Fatalf("its client: %d %v", uid, ok)
	}
	if uid, ok := PeerUID(client); !ok || uid != me() {
		t.Fatalf("its accepted end, asked from the client: %d %v", uid, ok)
	}
	// A client bound to the interface, connecting to an ordinary listener.
	plain, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	loClient, err := (&net.Dialer{Control: bound}).Dial("tcp4", plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer loClient.Close()
	accepted, err := plain.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if uid, ok := PeerUID(accepted); !ok || uid != me() {
		t.Fatalf("a client bound to the loopback interface: %d %v", uid, ok)
	}
}

type baseFunc func(*http.Request) (*http.Response, error)

func (f baseFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
