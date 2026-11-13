package localpeer

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
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
	if err := OwnServer("127.0.0.1", fmt.Sprint(port)); err != nil {
		t.Fatalf("own dual-stack server: %v", err)
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
	if uid, ok := PeerUID(server); ok && uid == me() {
		t.Fatalf("a connection whose sender left still counts as this user's (uid %d)", uid)
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
	guarded := &sameUser{Listener: ln, uid: me(), owner: func(conn net.Conn) (uint32, bool) {
		uid, ok := PeerUID(conn)
		switch verdict.Load() {
		case 1:
			return uid + 1, ok
		case 2:
			return 0, false
		}
		return uid, ok
	}}
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
	guarded := &sameUser{Listener: ln, uid: me(), owner: func(conn net.Conn) (uint32, bool) {
		uid, ok := PeerUID(conn)
		if conn.RemoteAddr().(*net.TCPAddr).IP.Equal(net.IPv4(127, 0, 0, 2)) {
			return uid + 1, ok
		}
		return uid, ok
	}}
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

// Clients send nothing to a local port another account holds.
func TestOwnServerRefusesAnotherUsersPort(t *testing.T) {
	needsKernel(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, host := range []string{"127.0.0.1", "localhost"} {
		if err := OwnServer(host, port); err != nil {
			t.Fatalf("this user's own server at %s was refused: %v", host, err)
		}
	}
	// Elsewhere, unknown and not listening are not this check's to refuse.
	for _, target := range [][2]string{{"192.0.2.1", port}, {"example.com", "443"}, {"127.0.0.1", "1"}, {"127.0.0.1", "nonsense"}} {
		if err := OwnServer(target[0], target[1]); err != nil {
			t.Fatalf("%v: %v", target, err)
		}
	}
	real := lookup
	defer func() { lookup = real }()
	owner := me() + 1
	lookup = func(local, remote *net.TCPAddr) (socket, bool) { return socket{uid: owner, listening: true}, true }
	err = OwnServer("127.0.0.1", port)
	var other *OtherUserError
	if !errors.As(err, &other) || other.UID != me()+1 {
		t.Fatalf("another user's port was accepted: %v", err)
	}
	owner = 0
	if err := OwnServer("127.0.0.1", port); err != nil {
		t.Fatalf("root's port was refused: %v", err)
	}
	owner = me() + 1
	t.Setenv(AllowOthersEnv, "1")
	if err := OwnServer("127.0.0.1", port); err != nil {
		t.Fatalf("the check stayed on: %v", err)
	}
}
