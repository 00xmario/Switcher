package localpeer

import (
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
