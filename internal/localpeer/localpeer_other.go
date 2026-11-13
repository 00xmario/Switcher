//go:build !linux && !darwin

package localpeer

import (
	"net"
	"time"
)

func peerOf(conn net.Conn, local, remote *net.TCPAddr, since time.Time) (uint32, peerKind) {
	return 0, peerUnseen
}

// lookupsAvailable says this system can be asked whose a socket is: not
// here, so nothing is checked.
const lookupsAvailable = false

const checksAtOnce = 32

func listenerUID(addr *net.TCPAddr) (uint32, bool) { return 0, false }

func connectedUID(local, remote *net.TCPAddr) (uint32, bool) { return 0, false }

func queryFailure(local, remote *net.TCPAddr) error { return errUnsupported }

func otherEndOpen(conn net.Conn) bool { return false }

func portHeld(addr *net.TCPAddr) bool { return true }

func portHolders(addr *net.TCPAddr, since time.Time) []uint32 { return nil }
