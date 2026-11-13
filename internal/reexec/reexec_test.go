package reexec

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The next image serves on the very socket this one listened on: a
// connection made in between is answered, and nobody else can take the port.
func TestListenerSurvivesARestartInPlace(t *testing.T) {
	if os.Getenv("REEXEC_TEST_CHILD") == "1" {
		child()
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	Keep(ln)
	// A connection waiting from before the restart.
	early, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	// The test binary re-runs itself as the "next image": a child process
	// started the way Exec hands the socket over, since Exec itself would
	// replace the test.
	kept.Lock()
	fd := kept.fd
	kept.Unlock()
	if flags, _ := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("the kept socket is open to every program this process starts")
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestListenerSurvivesARestartInPlace")
	cmd.ExtraFiles = []*os.File{os.NewFile(uintptr(mustDup(t, fd)), "kept")}
	cmd.Env = append(without(os.Environ()), "REEXEC_TEST_CHILD=1", envName+"=3,"+addr)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// This image is gone: its own listener closes. The port stays taken.
	ln.Close()
	if squatter, err := net.Listen("tcp", addr); err == nil {
		squatter.Close()
		t.Fatal("the port was free during the restart")
	}
	buf := make([]byte, 256)
	n, _ := out.Read(buf)
	if got := string(buf[:n]); !strings.HasPrefix(got, "serving "+addr) {
		t.Fatalf("the next image did not take the socket over: %q", got)
	}
	reply := make([]byte, 16)
	n, err = early.Read(reply)
	if err != nil || string(reply[:n]) != "hello" {
		t.Fatalf("the connection made before the restart was not answered: %q %v", reply[:n], err)
	}
	_ = cmd.Wait()
}

func mustDup(t *testing.T, fd int) int {
	t.Helper()
	copyFD, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	return copyFD
}

// child is the next image: it takes the socket over and answers one
// connection.
func child() {
	value := os.Getenv(envName)
	_, addr, _ := strings.Cut(value, ",")
	ln := Inherited(addr)
	if ln == nil {
		fmt.Println("nothing inherited")
		os.Exit(1)
	}
	if os.Getenv(envName) != "" {
		fmt.Println("the hand-over is still in the environment")
		os.Exit(1)
	}
	fmt.Println("serving " + ln.Addr().String())
	conn, err := ln.Accept()
	if err != nil {
		os.Exit(1)
	}
	conn.Write([]byte("hello"))
	conn.Close()
	os.Exit(0)
}

// What is handed over must be this port's listening socket; anything else
// is closed and not used.
func TestInheritedTakesOnlyTheRightSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	Keep(ln)
	kept.Lock()
	fd := kept.fd
	kept.Unlock()
	for name, value := range map[string]string{
		"another address":  fmt.Sprintf("%d,127.0.0.1:1", mustDup(t, fd)),
		"not a number":     "x," + addr,
		"standard input":   "0," + addr,
		"a closed number":  "987654," + addr,
		"no address":       fmt.Sprint(mustDup(t, fd)),
		"an ordinary file": "",
	} {
		if name == "an ordinary file" {
			file, err := os.Open(os.Args[0])
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			value = fmt.Sprintf("%d,%s", mustDup(t, int(file.Fd())), addr)
		}
		t.Setenv(envName, value)
		if got := Inherited(addr); got != nil {
			got.Close()
			t.Errorf("%s was taken as the listener", name)
		}
		if os.Getenv(envName) != "" {
			t.Errorf("%s: the hand-over stayed in the environment", name)
		}
	}
	t.Setenv(envName, fmt.Sprintf("%d,%s", mustDup(t, fd), addr))
	got := Inherited(addr)
	if got == nil || got.Addr().String() != addr {
		t.Fatal("the right socket was not taken")
	}
	got.Close()
	if Inherited(addr) != nil {
		t.Fatal("a hand-over was read twice")
	}
	if entries := without([]string{"A=1", envName + "=3,x", "B=2"}); len(entries) != 2 {
		t.Fatalf("environment: %v", entries)
	}
}
