package localpeer

import (
	"encoding/binary"
	"net"
)

// macOS lists every TCP socket, with the user it belongs to, in one sysctl,
// net.inet.tcp.pcblist_n: what netstat shows, readable by every user. It is
// a run of records, each starting with its length and kind; one socket is an
// address record (xinpcb_n) followed by its socket record (xsocket_n), its
// buffers and statistics, and its TCP record (xtcpcb_n). Only a few fields
// of three of them are read, at the offsets of the structures in xnu's
// in_pcb.h, socketvar.h and tcp_var.h, which are packed to 4 bytes
// (#pragma pack(4)): a 64-bit field may sit at any multiple of 4. Should
// they ever move, the self test finds that its own connection does not come
// back as its own, and the check turns itself off instead of refusing
// anyone.
const (
	xsoSocket = 0x001
	xsoRcvBuf = 0x002
	xsoSndBuf = 0x004
	xsoStats  = 0x008
	xsoInpcb  = 0x010
	xsoTcpcb  = 0x020

	listHeaderSize = 24 // struct xinpgen

	// struct xinpcb_n
	inpcbForeignPort = 16 // network byte order
	inpcbLocalPort   = 18
	inpcbVflag       = 44
	inpcbForeignAddr = 48 // 16 bytes: an IPv6 address, or an IPv4 one in the last 4
	inpcbLocalAddr   = 64
	inpcbMinSize     = 80
	inpIPv4          = 0x1
	inpIPv6          = 0x2

	// struct xsocket_n
	socketStateAt = 26 // short so_state
	socketUIDAt   = 64 // uid_t so_uid
	socketMinSize = 68
	ssNoFDRef     = 0x0001 // no file descriptor holds the socket: its program let go of it

	// struct xtcpcb_n
	tcpStateAt = 36 // int t_state
	tcpMinSize = 40

	tcpsListen      = 1
	tcpsEstablished = 4
	tcpsCloseWait   = 5
)

// The TCP states in which a socket is one end of a connection that was made.
// The TCP states a socket is in once its own program has closed the
// connection.
var bsdClosing = map[int32]bool{
	6:  true, // FIN_WAIT_1
	7:  true, // CLOSING
	8:  true, // LAST_ACK
	9:  true, // FIN_WAIT_2
	10: true, // TIME_WAIT
}

var bsdConnected = map[int32]bool{
	tcpsEstablished: true,
	tcpsCloseWait:   true,
	6:               true, // FIN_WAIT_1
	7:               true, // CLOSING
	8:               true, // LAST_ACK
	9:               true, // FIN_WAIT_2
}

// pcb is one TCP socket of the list.
type pcb struct {
	localPort, foreignPort uint16
	localAddr, foreignAddr net.IP
	// local4, foreign4: the same read as IPv4 addresses, for an IPv6 socket
	// that also carries the IPv4 mark: either reading may be the one used.
	local4, foreign4    net.IP
	uid                 uint32
	noFDRef             bool
	state               int32
	haveSocket, haveTCP bool
}

// parsePCBs reads the records of the list. Anything it does not expect
// makes the whole list unreadable: no answer is better than a wrong one.
func parsePCBs(raw []byte) ([]pcb, bool) {
	order := binary.NativeEndian
	if len(raw) < listHeaderSize || order.Uint32(raw) != listHeaderSize {
		return nil, false
	}
	var out []pcb
	at := listHeaderSize
	for at < len(raw) {
		// The list ends with a copy of its header.
		if len(raw)-at == listHeaderSize && order.Uint32(raw[at:]) == listHeaderSize {
			return out, true
		}
		if len(raw)-at < 8 {
			return nil, false
		}
		size, kind := int(order.Uint32(raw[at:])), order.Uint32(raw[at+4:])
		if size < 8 || size > len(raw)-at {
			return nil, false
		}
		record := raw[at : at+size]
		var last *pcb
		if len(out) > 0 {
			last = &out[len(out)-1]
		}
		switch kind {
		case xsoInpcb:
			if size < inpcbMinSize {
				return nil, false
			}
			vflag := record[inpcbVflag]
			out = append(out, pcb{
				foreignPort: binary.BigEndian.Uint16(record[inpcbForeignPort:]),
				localPort:   binary.BigEndian.Uint16(record[inpcbLocalPort:]),
				foreignAddr: pcbAddr(record[inpcbForeignAddr:inpcbForeignAddr+16], vflag),
				localAddr:   pcbAddr(record[inpcbLocalAddr:inpcbLocalAddr+16], vflag),
				foreign4:    pcbAddr4(record[inpcbForeignAddr:inpcbForeignAddr+16], vflag),
				local4:      pcbAddr4(record[inpcbLocalAddr:inpcbLocalAddr+16], vflag),
			})
		case xsoSocket:
			if size < socketMinSize || last == nil || last.haveSocket {
				return nil, false
			}
			last.uid = order.Uint32(record[socketUIDAt:])
			last.noFDRef = order.Uint16(record[socketStateAt:])&ssNoFDRef != 0
			last.haveSocket = true
		case xsoTcpcb:
			if size < tcpMinSize || last == nil || last.haveTCP {
				return nil, false
			}
			last.state = int32(order.Uint32(record[tcpStateAt:]))
			last.haveTCP = true
		case xsoRcvBuf, xsoSndBuf, xsoStats:
		default:
			return nil, false
		}
		at += (size + 7) &^ 7
	}
	return nil, false // no closing header: cut short
}

// pcbAddr is the address a record holds, as 16 bytes: an IPv6 socket's
// as it is, an IPv4 socket's from its last four bytes.
func pcbAddr(b []byte, vflag byte) net.IP {
	if vflag&inpIPv6 != 0 {
		return net.IP(append([]byte(nil), b...))
	}
	if vflag&inpIPv4 != 0 {
		return net.IPv4(b[12], b[13], b[14], b[15]).To16()
	}
	return nil
}

// pcbAddr4 is, for an IPv6 socket that also carries the IPv4 mark (one
// that connected to an IPv4 address may), the address read as IPv4.
func pcbAddr4(b []byte, vflag byte) net.IP {
	if vflag&inpIPv6 != 0 && vflag&inpIPv4 != 0 {
		return net.IPv4(b[12], b[13], b[14], b[15]).To16()
	}
	return nil
}

// is reports whether a socket's address, as either reading, is ip.
func is(ip, addr, addr4 net.IP) bool {
	return ip.Equal(addr) || addr4 != nil && ip.Equal(addr4)
}

// matchPCB finds in the list, as the Linux kernel's own lookup does, the
// socket whose own address is local and whose other end is remote; with a
// zero remote port, the socket listening at local.
func matchPCB(pcbs []pcb, local, remote *net.TCPAddr) (socket, bool) {
	want := local.IP.To16()
	if remote.Port == 0 {
		return listenerAt(pcbs, want, uint16(local.Port))
	}
	other := remote.IP.To16()
	var found []pcb
	for _, p := range pcbs {
		if p.haveSocket && p.haveTCP && p.state != tcpsListen && p.localPort == uint16(local.Port) && p.foreignPort == uint16(remote.Port) &&
			is(want, p.localAddr, p.local4) && is(other, p.foreignAddr, p.foreign4) {
			found = append(found, p)
		}
	}
	// One pair of addresses is one connection; a socket winding down on it
	// beside a new one is passed over for the one in use. Only sockets
	// closing is a connection whose sender has gone.
	if len(found) > 1 {
		var live, closing []pcb
		for _, p := range found {
			switch {
			case !p.noFDRef && bsdConnected[p.state]:
				live = append(live, p)
			case bsdClosing[p.state]:
				closing = append(closing, p)
			}
		}
		switch {
		case len(live) > 0:
			found = live
		case len(closing) > 0:
			found = closing[:1]
		}
	}
	if len(found) != 1 {
		return socket{}, false
	}
	p := found[0]
	return socket{uid: p.uid, live: !p.noFDRef, connected: bsdConnected[p.state], closing: bsdClosing[p.state]}, true
}

// listenerAt is the socket that takes connections to addr:port: one
// listening on that very address, or else on every address.
func listenerAt(pcbs []pcb, addr net.IP, port uint16) (socket, bool) {
	var exact, any []pcb
	for _, p := range pcbs {
		if !p.haveSocket || !p.haveTCP || p.state != tcpsListen || p.localPort != port {
			continue
		}
		switch {
		case is(addr, p.localAddr, p.local4):
			exact = append(exact, p)
		case p.localAddr != nil && p.localAddr.IsUnspecified():
			any = append(any, p)
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = any
	}
	if len(candidates) == 0 {
		return socket{}, false
	}
	// Two listeners of different users on one port are no answer.
	for _, p := range candidates[1:] {
		if p.uid != candidates[0].uid {
			return socket{}, false
		}
	}
	p := candidates[0]
	return socket{uid: p.uid, listening: true, live: !p.noFDRef}, true
}
