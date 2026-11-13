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
	pcbs, err := readPCBsFor()
	return pcbs, err == nil
}

// readPCBsFor is readPCBs, saying why there is no list: one that kept
// growing while it was read, or a system without it or with another layout.
func readPCBsFor() ([]pcb, error) {
	var raw []byte
	var err error
	// The list can grow between asking for its size and reading it (past
	// the room the system leaves): tried again, a moment later each time,
	// for a few hundred milliseconds.
	for try := range 20 {
		if raw, err = unix.SysctlRaw("net.inet.tcp.pcblist_n"); err != unix.ENOMEM {
			break
		}
		time.Sleep(time.Duration(min(2*try, 25)) * time.Millisecond)
	}
	switch {
	case err == unix.ENOMEM:
		return nil, errTransient
	case err != nil:
		return nil, errUnsupported
	}
	pcbs, ok := parsePCBs(raw)
	if !ok {
		return nil, errUnsupported
	}
	return pcbs, nil
}

// portHolders are the accounts of the sockets the list shows bound to addr,
// in any state, read no earlier than since.
func portHolders(addr *net.TCPAddr, since time.Time) []uint32 {
	pcbs, ok := pcbsSince(since)
	if !ok {
		return nil
	}
	want := addr.IP.To16()
	var uids []uint32
	for _, p := range pcbs {
		if p.haveSocket && p.localPort == uint16(addr.Port) && is(want, p.localAddr, p.local4) {
			uids = append(uids, p.uid)
		}
	}
	return uids
}

// queryFailure says why a lookup fails: see selfTest.
func queryFailure(local, remote *net.TCPAddr) error {
	pcbs, err := readPCBsFor()
	if err != nil {
		return err
	}
	if _, ok := matchPCB(pcbs, local, remote); !ok {
		return errNotFound
	}
	return nil
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
	state, ok := tcpState(conn)
	return ok && (state == tcpsEstablished || state == tcpsCloseWait)
}

// otherEndOpen reports whether the other end of conn has not closed it.
func otherEndOpen(conn net.Conn) bool {
	state, ok := tcpState(conn)
	return ok && state == tcpsEstablished
}

func tcpState(conn net.Conn) (int32, bool) {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	control, err := raw.SyscallConn()
	if err != nil {
		return 0, false
	}
	var state int32
	known := false
	if err := control.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
		state, known = int32(info.State), err == nil
	}); err != nil {
		return 0, false
	}
	return state, known
}
