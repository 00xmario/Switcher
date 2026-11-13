//go:build !linux

package localpeer

import "net"

func peerUID(conn net.Conn, local, remote *net.TCPAddr) (uint32, bool) { return 0, false }

func listenerUID(addr *net.TCPAddr) (uint32, bool) { return 0, false }

func connectedUID(local, remote *net.TCPAddr) (uint32, bool) { return 0, false }
