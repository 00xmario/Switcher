package reexec

import "golang.org/x/sys/unix"

// listening reports whether fd is a listening TCP socket. macOS does not
// answer SO_ACCEPTCONN on every release; the socket's TCP state does.
func listening(fd int) bool {
	if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err == nil && v != 0 {
		return true
	}
	info, err := unix.GetsockoptTCPConnectionInfo(fd, unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	return err == nil && info.State == 1 // TCPS_LISTEN
}
