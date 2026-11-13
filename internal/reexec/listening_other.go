//go:build !darwin

package reexec

import "golang.org/x/sys/unix"

// listening reports whether fd is a listening socket.
func listening(fd int) bool {
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	return err == nil && v != 0
}
