//go:build !linux

package localpeer

import "net"

func socketUID(local, remote *net.TCPAddr) (uint32, bool) { return 0, false }

func listenerUID(addr *net.TCPAddr) (uint32, bool) { return 0, false }
