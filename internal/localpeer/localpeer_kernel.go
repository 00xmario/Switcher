//go:build linux || darwin

package localpeer

import (
	"net"
	"time"
)

// lookup asks for the socket whose own address is local and whose other
// end is remote. A zero remote port asks for the listener at local. The
// answer is read no earlier than since: one read can then answer several
// lookups made at once (see the macOS socket list), never with what was
// there before since.
var lookup = kernelLookup

// socketUID is the owner of the socket bound to local and connected to
// remote. A listener in its place is no answer: the connection is gone. Nor
// is a socket its program has let go of (it would pass for root's), or one
// that is only now connecting from that address or has finished there.
func socketUID(local, remote *net.TCPAddr, since time.Time) (uint32, bool) {
	found, ok := lookup(local, remote, since)
	if !ok || found.listening || !found.live || !found.connected {
		return 0, false
	}
	return found.uid, true
}

// connectedUID is the user a connected socket is shown under, held by a
// program right now or not.
func connectedUID(local, remote *net.TCPAddr) (uint32, bool) {
	found, ok := lookup(local, remote, time.Now())
	if !ok || found.listening || !found.connected {
		return 0, false
	}
	return found.uid, true
}

// peerUID is the owner of the other end of conn. The kernel finds a socket
// by its pair of addresses, not by which connection it is: once this end's
// connection is over, the same pair can be another program's, and what it
// sent before it left can still be read here. So this end is looked at
// after the question: only while it is still connected was the socket found
// the other end of this very connection.
//
// since is no later than when the connection was accepted: the socket list
// read for it must be from after then.
func peerUID(conn net.Conn, local, remote *net.TCPAddr, since time.Time) (uint32, bool) {
	uid, ok := socketUID(remote, local, since)
	if !ok || !stillConnected(conn) {
		return 0, false
	}
	return uid, true
}

// listenerUID is the owner of the socket a connection to addr would reach.
func listenerUID(addr *net.TCPAddr) (uint32, bool) {
	// From this computer's own address of the same kind, with no port.
	from := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
	if addr.IP.To4() == nil {
		from = &net.TCPAddr{IP: net.IPv6loopback}
	}
	found, ok := lookup(addr, from, time.Now())
	if !ok || !found.listening || !found.live {
		return 0, false
	}
	return found.uid, true
}
