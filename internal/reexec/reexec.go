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

// envName carries "<process id>,<descriptor>,<address>" to the next image.
// Replacing the image keeps the process id, so only the image that follows
// in this very process takes the hand-over: a value that some other process
// inherited, or that was left in an environment, names a descriptor that is
// not this process's and is left alone.
const envName = "SWITCHER_KEPT_LISTENER"

var kept struct {
	sync.Mutex
	fd    int // a descriptor of the listening socket, closed on exec until Exec
	addr  string
	image os.FileInfo // the program file this process runs, as it was at Keep
}

func init() { kept.fd = -1 }

// runningImage is the program file of this process.
func runningImage() os.FileInfo {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

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
	if kept.image == nil {
		kept.image = runningImage()
	}
	kept.Unlock()
}

// Exec replaces this process with the program at path, as syscall.Exec
// does, and hands it the kept listener. It returns only on failure. The
// program must be one that takes the hand-over: a release of Switcher that
// has it, as an update installs.
func Exec(path string, args, environ []string) error { return replace(path, args, environ, true) }

// ExecSame is Exec for restarting the program this process already runs.
// If the file at path is no longer that program, replaced by hand with
// another release say, the socket is not handed over: a release without
// the hand-over would hold the port through the descriptor it does not know
// about and fail to listen on it. The port is then free for a moment
// instead.
func ExecSame(path string, args, environ []string) error {
	kept.Lock()
	image := kept.image
	kept.Unlock()
	now, err := os.Stat(path)
	return replace(path, args, environ, err == nil && image != nil && os.SameFile(image, now) && image.ModTime().Equal(now.ModTime()) && image.Size() == now.Size())
}

func replace(path string, args, environ []string, handOver bool) error {
	kept.Lock()
	fd, addr := kept.fd, kept.addr
	kept.Unlock()
	environ = without(environ)
	if fd < 0 || !handOver {
		return syscall.Exec(path, args, environ)
	}
	// No other program is started while the descriptor is open to one:
	// only the next image gets it.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
		return syscall.Exec(path, args, environ)
	}
	err := syscall.Exec(path, args, append(environ, envName+"="+strconv.Itoa(os.Getpid())+","+strconv.Itoa(fd)+","+addr))
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

// Inherited is the listener the previous image of this process handed over,
// if it is one listening on addr; otherwise nil. It reads the hand-over
// once: nothing this process starts sees it. A descriptor that is not a
// socket is not this function's to touch and is left as it is; a socket
// that turns out not to be the one listening on addr is closed.
func Inherited(addr string) net.Listener {
	value := os.Getenv(envName)
	if value == "" {
		return nil
	}
	_ = os.Unsetenv(envName)
	parts := strings.SplitN(value, ",", 3)
	if len(parts) != 3 || parts[0] != strconv.Itoa(os.Getpid()) {
		return nil
	}
	fd, err := strconv.Atoi(parts[1])
	if err != nil || fd < 3 {
		return nil
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil
	}
	// From here on it is the socket handed over: closed to the programs
	// this process starts, whatever comes of it.
	unix.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "kept-listener")
	if file == nil {
		return nil
	}
	defer file.Close()
	listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil || listening != 1 || parts[2] != addr {
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
