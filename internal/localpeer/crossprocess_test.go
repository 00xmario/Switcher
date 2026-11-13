//go:build linux || darwin

package localpeer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary also stands in for another program of this user: run with
// LOCALPEER_TEST_CONNECT, it connects there, says LOCALPEER_TEST_TOKEN, and
// holds the connection until its standard input closes.
func TestMain(m *testing.M) {
	if addr := os.Getenv("LOCALPEER_TEST_CONNECT"); addr != "" {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(conn, os.Getenv("LOCALPEER_TEST_TOKEN"))
		_, _ = io.Copy(io.Discard, os.Stdin)
		conn.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// connectFromAnotherProcess has a separate process of this user connect to
// ln and returns this end of its connection; stop ends that process.
func connectFromAnotherProcess(t *testing.T, ln net.Listener) (conn net.Conn, stop func()) {
	t.Helper()
	token := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCALPEER_TEST_CONNECT="+ln.Addr().String(), "LOCALPEER_TEST_TOKEN="+token)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() { stdin.Close(); _ = cmd.Wait() }
	if tcp, ok := ln.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(10 * time.Second))
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			stop()
			t.Fatalf("the other process's connection never came: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		_ = c.SetReadDeadline(time.Time{})
		if strings.TrimSpace(line) == token {
			return c, stop
		}
		c.Close()
	}
}

// Another program of this user, a separate process as every real client is,
// is found as this user's: the check must see other processes' connections,
// not only its own.
func TestAnotherProcessOfThisUserIsFound(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, stop := connectFromAnotherProcess(t, ln)
	defer stop()
	defer conn.Close()
	uid, ok := PeerUID(conn)
	if ok && uid == me() {
		return
	}
	t.Errorf("another process of this user: uid=%d ok=%v (this test runs as uid %d)", uid, ok, os.Getuid())
	t.Log(describeLookup(conn))
}

// The same with a system program as the client: curl.
func TestCurlIsFoundAsThisUser(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	type answer struct {
		uid uint32
		ok  bool
		why string
	}
	seen := make(chan answer, 1)
	ln2, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln2.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		uid, ok := PeerUID(c)
		why := ""
		if !(ok && uid == me()) {
			why = describeLookup(c)
		}
		seen <- answer{uid, ok, why}
		c.Write([]byte("HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"))
	}()
	out, err := exec.Command(curl, "-s", "--max-time", "5", "http://"+ln2.Addr().String()+"/").CombinedOutput()
	<-done
	select {
	case a := <-seen:
		if !(a.ok && a.uid == me()) {
			t.Errorf("curl: uid=%d ok=%v (this test runs as uid %d); curl said %q %v", a.uid, a.ok, os.Getuid(), out, err)
			t.Log(a.why)
		}
	default:
		t.Fatalf("curl never connected: %q %v", out, err)
	}
}

// The self-test has another process connect, as real clients do: here it
// works, and a system that does not show another process's socket turns the
// check off rather than refuse every client.
func TestTheSelfTestUsesAnotherProcess(t *testing.T) {
	needsKernel(t)
	if got := selfTest(false); got != working {
		t.Fatalf("the self-test with another process: %v", got)
	}
	real, why := lookup, whyNoAnswer
	defer func() { lookup, whyNoAnswer = real, why }()
	lookup = func(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
		found, ok := real(local, remote, since)
		if ok && !found.listening {
			return socket{}, false // the other process's socket is not shown
		}
		return found, ok
	}
	whyNoAnswer = func(*net.TCPAddr, *net.TCPAddr) error { return errNotFound }
	if got := selfTest(false); got != unsupported {
		t.Fatalf("a system that does not show other processes' sockets: %v", got)
	}
}
