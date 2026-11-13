// Package reexec restarts Switcher in place without letting go of its port.
//
// Switcher restarts itself after an update and after a change to its
// listeners, by replacing its own process image. A listening socket that is
// closed for that moment leaves the port free: another account's program on
// the computer could take it and be sent, in Switcher's place, the menu bar
// app's token and the CLIs' requests. So the socket is handed to the next
// image, which serves on it: the port is never free, and connections made
// during the restart wait in its queue instead of being refused.
package reexec

import (
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// envName carries "<descriptor>,<address>" to the next image.
const envName = "SWITCHER_KEPT_LISTENER"

var kept struct {
	sync.Mutex
	fd   int // a descriptor of the listening socket, closed on exec until Exec
	addr string
}

func init() { kept.fd = -1 }

// Keep remembers ln as the listener to hand to the next image. The copy it
// holds stays closed to every other program this process starts.
func Keep(ln net.Listener) {
	type rawConn interface {
		SyscallConn() (syscall.RawConn, error)
	}
	source, ok := ln.(rawConn)
	if !ok {
		return
	}
	raw, err := source.SyscallConn()
	if err != nil {
		return
	}
	copyFD := -1
	if err := raw.Control(func(fd uintptr) {
		copyFD, _ = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 3)
	}); err != nil || copyFD < 0 {
		return
	}
	kept.Lock()
	if kept.fd >= 0 {
		unix.Close(kept.fd)
	}
	kept.fd, kept.addr = copyFD, ln.Addr().String()
	kept.Unlock()
}

// Exec replaces this process with the program at path, as syscall.Exec
// does, and hands it the kept listener. It returns only on failure.
func Exec(path string, args, environ []string) error {
	kept.Lock()
	fd, addr := kept.fd, kept.addr
	kept.Unlock()
	environ = without(environ)
	if fd < 0 {
		return syscall.Exec(path, args, environ)
	}
	// No other program is started while the descriptor is open to one:
	// only the next image gets it.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		return syscall.Exec(path, args, environ)
	}
	err := syscall.Exec(path, args, append(environ, envName+"="+strconv.Itoa(fd)+","+addr))
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	return err
}

// without is environ minus a kept listener's entry.
func without(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, envName+"=") {
			out = append(out, entry)
		}
	}
	return out
}

// Inherited is the listener the previous image handed over, if it is one
// listening on addr; otherwise nil, and whatever was handed over is closed.
// It reads the hand-over once: nothing this process starts sees it.
func Inherited(addr string) net.Listener {
	value := os.Getenv(envName)
	if value == "" {
		return nil
	}
	_ = os.Unsetenv(envName)
	number, want, _ := strings.Cut(value, ",")
	fd, err := strconv.Atoi(number)
	if err != nil || fd < 3 {
		return nil
	}
	// Closed to the programs this process starts, before anything else.
	unix.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "kept-listener")
	if file == nil {
		return nil
	}
	defer file.Close()
	listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil || listening != 1 || want != addr {
		return nil
	}
	ln, err := net.FileListener(file)
	if err != nil {
		return nil
	}
	if _, ok := ln.(*net.TCPListener); !ok || ln.Addr().String() != addr {
		ln.Close()
		return nil
	}
	return ln
}
