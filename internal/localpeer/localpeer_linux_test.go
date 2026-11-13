package localpeer

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestThisComputerSaysWhoOwnsAConnection(t *testing.T) {
	if !Works() {
		t.Skip("this system does not list its TCP sockets")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if uid, ok := ListenerUID(ln.Addr().(*net.TCPAddr)); !ok || uid != uint32(os.Getuid()) {
		t.Fatalf("listener owner: %d %v", uid, ok)
	}
	if _, ok := ListenerUID(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); ok {
		t.Fatal("a port nobody listens on has an owner")
	}
}

// The server takes this user's connections, and drops another user's and
// one whose owner is unknown, without ever handing them to the HTTP server.
func TestSameUserDropsOtherUsers(t *testing.T) {
	if !Works() {
		t.Skip("this system does not list its TCP sockets")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var verdict atomic.Int32 // 0: as the system says, 1: another user, 2: unknown
	guarded := &sameUser{Listener: ln, uid: uint32(os.Getuid()), owner: func(conn net.Conn) (uint32, bool) {
		uid, ok := PeerUID(conn)
		switch verdict.Load() {
		case 1:
			return uid + 1, ok
		case 2:
			return 0, false
		}
		return uid, ok
	}}
	defer guarded.Close()
	if _, ok := SameUser(ln).(*sameUser); !ok {
		t.Fatal("the listener was not guarded")
	}
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := guarded.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
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
		// The server hung up on it.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s connection was left open", name)
		}
		conn.Close()
	}
}

// A sender that sends its request and leaves before the server looks no
// longer owns its end; such a connection has no known owner.
func TestGoneSenderHasNoKnownOwner(t *testing.T) {
	if !Works() {
		t.Skip("this system does not list its TCP sockets")
	}
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
	if uid, ok := PeerUID(server); ok && uid == uint32(os.Getuid()) {
		t.Fatalf("a connection whose sender left still counts as this user's (uid %d)", uid)
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

func TestTableParsing(t *testing.T) {
	real := tcpTable
	defer func() { tcpTable = real }()
	tcpTable = filepath.Join(t.TempDir(), "tcp")
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8787}
	peer := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
	l, _, _ := tableAddr(local)
	p, _, _ := tableAddr(peer)
	write := func(lines ...string) {
		content := "  sl  local_address rem_address   st ... uid\n"
		for _, line := range lines {
			content += line + "\n"
		}
		if err := os.WriteFile(tcpTable, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	row := func(from, to, state string, uid int) string {
		return fmt.Sprintf("   0: %s %s %s 00000000:00000000 00:00000000 00000000 %5d        0 7 1", from, to, state, uid)
	}
	write(row(l, "00000000:0000", "0A", 1000), row(p, l, "01", 1001), row(l, p, "01", 1000))
	if uid, ok := socketUID(peer, local); !ok || uid != 1001 {
		t.Fatalf("peer: %d %v", uid, ok)
	}
	if uid, ok := listenerUID(local); !ok || uid != 1000 {
		t.Fatalf("listener: %d %v", uid, ok)
	}
	// Listening on every address counts for the port.
	write(row("00000000:2253", "00000000:0000", "0A", 1002))
	if uid, ok := listenerUID(local); !ok || uid != 1002 {
		t.Fatalf("wildcard listener: %d %v", uid, ok)
	}
	// Two owners, a broken row or an IPv6 address are no answer.
	write(row(l, "00000000:0000", "0A", 1000), row("00000000:2253", "00000000:0000", "0A", 1002))
	if _, ok := listenerUID(local); ok {
		t.Fatal("two owners gave an answer")
	}
	write("   0: " + p + " " + l + " 01 x y z notanumber")
	if _, ok := socketUID(peer, local); ok {
		t.Fatal("a broken row gave an answer")
	}
	if _, ok := socketUID(&net.TCPAddr{IP: net.ParseIP("::1"), Port: 1}, local); ok {
		t.Fatal("addresses of two families gave an answer")
	}
}

// Clients send nothing to a local port another account holds.
func TestOwnServerRefusesAnotherUsersPort(t *testing.T) {
	if !Works() {
		t.Skip("this system does not list its TCP sockets")
	}
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
	for _, target := range [][2]string{{"192.0.2.1", port}, {"example.com", "443"}, {"127.0.0.1", "1"}, {"::1", port}, {"127.0.0.1", "nonsense"}} {
		if err := OwnServer(target[0], target[1]); err != nil {
			t.Fatalf("%v: %v", target, err)
		}
	}
	real := tcpTable
	defer func() { tcpTable = real }()
	tcpTable = filepath.Join(t.TempDir(), "tcp")
	addr := ln.Addr().(*net.TCPAddr)
	exact, _, _ := tableAddr(addr)
	row := func(uid int) {
		line := fmt.Sprintf("header\n   0: %s 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5d        0 7 1\n", exact, uid)
		if err := os.WriteFile(tcpTable, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	row(os.Getuid() + 1)
	err = OwnServer("127.0.0.1", port)
	var other *OtherUserError
	if !errors.As(err, &other) || other.UID != uint32(os.Getuid()+1) {
		t.Fatalf("another user's port was accepted: %v", err)
	}
	row(0)
	if err := OwnServer("127.0.0.1", port); err != nil {
		t.Fatalf("root's port was refused: %v", err)
	}
	row(os.Getuid() + 1)
	t.Setenv(AllowOthersEnv, "1")
	if err := OwnServer("127.0.0.1", port); err != nil {
		t.Fatalf("the check stayed on: %v", err)
	}
}

// The IPv6 loopback is told the same way, where this computer has one.
func TestIPv6LoopbackConnectionsHaveAnOwner(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	defer ln.Close()
	if uid, ok := ListenerUID(ln.Addr().(*net.TCPAddr)); !ok || uid != uint32(os.Getuid()) {
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
	if uid, ok := PeerUID(server); !ok || uid != uint32(os.Getuid()) {
		t.Fatalf("peer owner: %d %v", uid, ok)
	}
	if text, table, ok := tableAddr(&net.TCPAddr{IP: net.ParseIP("::1"), Port: 0x2253}); !ok || table != tcp6Table || len(text) != 37 {
		t.Fatalf("address: %q %s %v", text, table, ok)
	}
}
