package localpeer

import (
	"net"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// checksAtOnce bounds how many connections are being looked up at a time:
// each lookup here may read the whole socket list.
const checksAtOnce = 8

// readPCBs reads the list of TCP sockets.
func readPCBs() ([]pcb, bool) {
	var raw []byte
	var err error
	// The list can grow between asking for its size and reading it.
	for range 6 {
		if raw, err = unix.SysctlRaw("net.inet.tcp.pcblist_n"); err != unix.ENOMEM {
			break
		}
	}
	if err != nil {
		return nil, false
	}
	return parsePCBs(raw)
}

// A read of the list answers every lookup that asks for one no older than
// when the read began: lookups made at the same moment share one read, so a
// burst of connections costs a few reads, not one each.
type pcbSnapshot struct {
	began time.Time
	done  chan struct{}
	pcbs  []pcb
	ok    bool
}

var pcbShared struct {
	sync.Mutex
	latest *pcbSnapshot
}

func pcbsSince(since time.Time) ([]pcb, bool) {
	pcbShared.Lock()
	s := pcbShared.latest
	if s == nil || s.began.Before(since) {
		s = &pcbSnapshot{began: time.Now(), done: make(chan struct{})}
		pcbShared.latest = s
		pcbShared.Unlock()
		s.pcbs, s.ok = readPCBs()
		close(s.done)
		return s.pcbs, s.ok
	}
	pcbShared.Unlock()
	<-s.done
	return s.pcbs, s.ok
}

// kernelLookup asks the system's list of TCP sockets, read no earlier than
// since; see pcblist.go.
func kernelLookup(local, remote *net.TCPAddr, since time.Time) (socket, bool) {
	pcbs, ok := pcbsSince(since)
	if !ok {
		return socket{}, false
	}
	return matchPCB(pcbs, local, remote)
}

// stillConnected reports whether this end of conn has not been reset or
// closed: connected, or connected with the other end done sending.
func stillConnected(conn net.Conn) bool {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	control, err := raw.SyscallConn()
	if err != nil {
		return false
	}
	open := false
	if err := control.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
		open = err == nil && (info.State == tcpsEstablished || info.State == tcpsCloseWait)
	}); err != nil {
		return false
	}
	return open
}
