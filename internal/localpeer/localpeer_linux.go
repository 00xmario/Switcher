package localpeer

import (
	"encoding/binary"
	"net"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kernel answers one question at a time over a netlink socket
// (NETLINK_SOCK_DIAG, as `ss` uses): which socket would a TCP packet from
// one address to another be handed to, and whose is it. That is the socket
// connected between the two addresses or, when there is none, the one
// listening for it, whatever kind of socket it is: an IPv6 socket that
// takes IPv4 connections is found like any other. One question costs a few
// system calls, however many sockets the computer has.
const (
	sockDiagByFamily = 20 // SOCK_DIAG_BY_FAMILY
	tcpListen        = 10 // TCP_LISTEN
	diagRequestSize  = 56 // struct inet_diag_req_v2
	diagAnswerSize   = 72 // struct inet_diag_msg
)

// The TCP states in which a socket is one end of a connection that was made.
var connectedStates = map[uint8]bool{
	1:  true, // ESTABLISHED
	4:  true, // FIN_WAIT1
	5:  true, // FIN_WAIT2
	8:  true, // CLOSE_WAIT
	9:  true, // LAST_ACK
	11: true, // CLOSING
}

// loopbackIndex is the loopback interface's number. A lookup names it: a
// socket bound to that interface (which any program may do) takes this
// computer's local connections like any other, but a lookup that names no
// interface does not find it.
var loopbackIndex = sync.OnceValue(func() uint32 {
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, i := range interfaces {
			if i.Flags&net.FlagLoopback != 0 {
				return uint32(i.Index)
			}
		}
	}
	return 1
})

func kernelLookup(local, remote *net.TCPAddr) (socket, bool) {
	family, src, ok1 := diagAddr(local.IP)
	family2, dst, ok2 := diagAddr(remote.IP)
	if !ok1 || !ok2 || family != family2 || local.Port < 0 || local.Port > 65535 || remote.Port < 0 || remote.Port > 65535 {
		return socket{}, false
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return socket{}, false
	}
	defer unix.Close(fd)
	// A kernel that does not answer must not hold a connection up.
	wait := unix.Timeval{Sec: 2}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &wait)
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &wait)

	request := make([]byte, unix.NLMSG_HDRLEN+diagRequestSize)
	binary.NativeEndian.PutUint32(request[0:], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], sockDiagByFamily)
	binary.NativeEndian.PutUint16(request[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(request[8:], 1) // sequence number
	body := request[unix.NLMSG_HDRLEN:]
	body[0] = family
	body[1] = unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(body[4:], ^uint32(0)) // sockets in any state
	binary.BigEndian.PutUint16(body[8:], uint16(local.Port))
	binary.BigEndian.PutUint16(body[10:], uint16(remote.Port))
	copy(body[12:28], src[:])
	copy(body[28:44], dst[:])
	// On the loopback interface, where local connections travel, and no
	// cookie (all ones).
	if local.IP.IsLoopback() && remote.IP.IsLoopback() {
		binary.NativeEndian.PutUint32(body[44:], loopbackIndex())
	}
	for i := 48; i < 56; i++ {
		body[i] = 0xff
	}
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return socket{}, false
	}
	answer := make([]byte, 4096)
	n, from, err := unix.Recvfrom(fd, answer, 0)
	if err != nil || n < unix.NLMSG_HDRLEN+diagAnswerSize {
		return socket{}, false
	}
	// Only the kernel's word counts: any program may send to a netlink socket.
	if kernel, ok := from.(*unix.SockaddrNetlink); !ok || kernel.Pid != 0 {
		return socket{}, false
	}
	// An error answer (no such socket) has another type.
	if binary.NativeEndian.Uint16(answer[4:]) != sockDiagByFamily {
		return socket{}, false
	}
	msg := answer[unix.NLMSG_HDRLEN:n]
	// The socket found must be the one asked for. The kernel falls back to
	// a listener when no connection matches; the caller is told which.
	if binary.BigEndian.Uint16(msg[4:]) != uint16(local.Port) {
		return socket{}, false
	}
	found := socket{uid: binary.NativeEndian.Uint32(msg[64:]), listening: msg[1] == tcpListen,
		live: binary.NativeEndian.Uint32(msg[68:]) != 0, connected: connectedStates[msg[1]]}
	if !found.listening && binary.BigEndian.Uint16(msg[6:]) != uint16(remote.Port) {
		return socket{}, false
	}
	return found, true
}

// diagAddr is an address as the request carries it: its bytes in network
// order, an IPv4 address in the first four.
func diagAddr(ip net.IP) (family uint8, addr [16]byte, ok bool) {
	if v4 := ip.To4(); v4 != nil {
		copy(addr[:], v4)
		return unix.AF_INET, addr, true
	}
	if v6 := ip.To16(); v6 != nil {
		copy(addr[:], v6)
		return unix.AF_INET6, addr, true
	}
	return 0, addr, false
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
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		open = err == nil && (info.State == 1 || info.State == 8) // ESTABLISHED, CLOSE_WAIT
	}); err != nil {
		return false
	}
	return open
}
