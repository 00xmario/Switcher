//go:build !linux && !darwin

package localpeer

import (
	"net"
	"time"
)

func peerUID(conn net.Conn, local, remote *net.TCPAddr, since time.Time) (uint32, bool) {
	return 0, false
}

const checksAtOnce = 32

func listenerUID(addr *net.TCPAddr) (uint32, bool) { return 0, false }

func connectedUID(local, remote *net.TCPAddr) (uint32, bool) { return 0, false }

func queryFailure(local, remote *net.TCPAddr) error { return errUnsupported }

func otherEndOpen(conn net.Conn) bool { return false }
