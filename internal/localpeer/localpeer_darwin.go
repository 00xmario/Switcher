package localpeer

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// readPCBs reads the list of TCP sockets.
func readPCBs() ([]pcb, bool) {
	var raw []byte
	var err error
	// The list can grow between asking for its size and reading it.
	for range 3 {
		if raw, err = unix.SysctlRaw("net.inet.tcp.pcblist_n"); err != unix.ENOMEM {
			break
		}
	}
	if err != nil {
		return nil, false
	}
	return parsePCBs(raw)
}

// kernelLookup asks the system's list of TCP sockets; see pcblist.go.
func kernelLookup(local, remote *net.TCPAddr) (socket, bool) {
	pcbs, ok := readPCBs()
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
