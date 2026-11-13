//go:build linux || darwin

package localpeer

import (
	"net"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lookup asks for the socket whose own address is local and whose other
// end is remote. A zero remote port asks for the listener at local. The
// answer is read no earlier than since: one read can then answer several
// lookups made at once (see the macOS socket list), never with what was
// there before since.
var lookup = kernelLookup

// lookupsAvailable says this system can be asked whose a socket is.
const lookupsAvailable = true

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

// socketPeer is what the system shows of the socket bound to local and
// connected to remote: a live, connected one (also one whose program is done
// sending but holds it still) and whose it is; one that is closing with no
// program holding it any more, or a listener in its place (no such
// connection any more): gone; anything else, nothing shown at all included:
// unseen.
func socketPeer(local, remote *net.TCPAddr, since time.Time) (uint32, peerKind) {
	found, ok := lookup(local, remote, since)
	switch {
	case !ok:
		return 0, peerUnseen
	case found.listening:
		return 0, peerGone
	case found.live && found.connected:
		return found.uid, peerFound
	case found.closing && !found.live:
		return found.uid, peerGone
	case found.connected && !found.closing && fdlessIsFound:
		// macOS shows a connected socket with no descriptor (an MPTCP
		// subflow) under the account that made it.
		return found.uid, peerFound
	}
	return 0, peerUnseen
}

// fdlessIsFound: see socketPeer.
const fdlessIsFound = runtime.GOOS == "darwin"

// portHeld reports whether some socket on this computer holds addr, the
// address a client connected from: this process cannot take it then. A
// client whose socket the system does not show still holds its address; a
// connection whose client is gone without this end being told (its socket
// torn down, its reset never delivered) holds none. Any error but success
// counts as held, so nothing is refused for a test that could not be made.
func portHeld(addr *net.TCPAddr) bool {
	family, sa := unix.AF_INET, unix.Sockaddr(nil)
	if v4 := addr.IP.To4(); v4 != nil {
		s := &unix.SockaddrInet4{Port: addr.Port}
		copy(s.Addr[:], v4)
		sa = s
	} else {
		s := &unix.SockaddrInet6{Port: addr.Port}
		copy(s.Addr[:], addr.IP.To16())
		family, sa = unix.AF_INET6, s
	}
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(family, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return true
	}
	defer unix.Close(fd)
	return unix.Bind(fd, sa) != nil
}

// peerOf is what the system shows of the other end of conn. The kernel finds
// a socket by its pair of addresses, not by which connection it is: once
// this end's connection is over, the same pair can be another program's, and
// what it sent before it left can still be read here. So this end is looked
// at after the question: only while it is still connected was the socket
// found the other end of this very connection.
//
// since is no later than when the connection was accepted: the socket list
// read for it must be from after then.
func peerOf(conn net.Conn, local, remote *net.TCPAddr, since time.Time) (uint32, peerKind) {
	uid, kind := socketPeer(remote, local, since)
	if kind == peerFound && !stillConnected(conn) {
		return uid, peerGone
	}
	return uid, kind
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
