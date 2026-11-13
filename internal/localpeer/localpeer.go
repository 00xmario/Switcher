// Package localpeer tells which user is at the other end of a TCP
// connection on this computer.
//
// Switcher's port on 127.0.0.1 is reachable by every account on the
// computer, and it serves the accounts without a login of its own, as the
// CLIs need. On Linux the kernel says who owns each socket, so the server
// takes connections only from the user it runs as, and the command line and
// tray make sure the port they talk to is that user's Switcher. Other
// systems do not say, and there this package answers "unknown".
package localpeer

import (
	"net"
	"os"
	"strconv"
	"sync"
)

// AllowOthersEnv turns the check off, for a setup where another account's
// programs are meant to use this Switcher, such as a container that shares
// the computer's network under its own user.
const AllowOthersEnv = "SWITCHER_ALLOW_LOCAL_USERS"

// PeerUID is the user that owns the other end of conn, a connection between
// two addresses of this computer. ok is false when it cannot be told: off
// Linux, or when the other end is gone.
func PeerUID(conn net.Conn) (uid uint32, ok bool) {
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	if local == nil || remote == nil {
		return 0, false
	}
	return socketUID(remote, local)
}

// ListenerUID is the user that listens on addr on this computer.
func ListenerUID(addr *net.TCPAddr) (uid uint32, ok bool) {
	if addr == nil {
		return 0, false
	}
	return listenerUID(addr)
}

var works struct {
	once sync.Once
	ok   bool
}

// Works reports whether this computer says who owns a connection: a
// connection this process makes to itself must come back as its own user.
// Only then is "someone else" or "unknown" a reason to refuse.
func Works() bool {
	works.once.Do(func() { works.ok = selfTest() })
	return works.ok
}

func selfTest() bool {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return false
	}
	defer ln.Close()
	if uid, ok := ListenerUID(ln.Addr().(*net.TCPAddr)); !ok || uid != uint32(os.Getuid()) {
		return false
	}
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		return false
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		return false
	}
	defer server.Close()
	uid, ok := PeerUID(server)
	return ok && uid == uint32(os.Getuid())
}

// SameUser wraps a listener on this computer so it takes connections only
// from the user this process runs as. A connection whose owner cannot be
// told is dropped too: one whose sender has already gone, which could
// otherwise carry a request in unseen. Where the computer does not say who
// owns a connection, or AllowOthersEnv is set, ln is returned as it is.
func SameUser(ln net.Listener) net.Listener {
	if os.Getenv(AllowOthersEnv) != "" || !Works() {
		return ln
	}
	return &sameUser{Listener: ln, uid: uint32(os.Getuid()), owner: PeerUID}
}

// Enforced reports whether SameUser refuses other users here.
func Enforced() bool { return os.Getenv(AllowOthersEnv) == "" && Works() }

type sameUser struct {
	net.Listener
	uid   uint32
	owner func(net.Conn) (uint32, bool)
}

func (l *sameUser) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uid, ok := l.owner(conn); ok && uid == l.uid {
			return conn, nil
		}
		conn.Close()
	}
}

// OwnServer checks, before a client sends anything to a server on this
// computer, that the port belongs to this user (or to root): another
// account that took Switcher's port would otherwise be handed the device
// token and whatever the request holds. It answers nil for any address
// that is not this computer's, and wherever the owner cannot be told.
func OwnServer(host, port string) error {
	if os.Getenv(AllowOthersEnv) != "" {
		return nil
	}
	ip := net.ParseIP(host)
	if host == "localhost" {
		ip = net.IPv4(127, 0, 0, 1)
	}
	n, err := net.LookupPort("tcp", port)
	if ip == nil || !ip.IsLoopback() || ip.To4() == nil || err != nil || !Works() {
		return nil
	}
	uid, ok := ListenerUID(&net.TCPAddr{IP: ip, Port: n})
	if !ok || uid == uint32(os.Getuid()) || uid == 0 {
		return nil
	}
	return &OtherUserError{Port: n, UID: uid}
}

// OtherUserError says another account on this computer holds a port.
type OtherUserError struct {
	Port int
	UID  uint32
}

func (e *OtherUserError) Error() string {
	return "port " + strconv.Itoa(e.Port) + " on this computer is held by another user (uid " + strconv.FormatUint(uint64(e.UID), 10) +
		"), not by your Switcher; nothing was sent to it"
}
