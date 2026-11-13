package localpeer

import (
	"encoding/binary"
	"net"
	"testing"
)

// fakePCB is one socket for a made-up list, laid out as macOS lays it out.
type fakePCB struct {
	local, foreign string // "addr:port"
	v6             bool
	uid            uint32
	noFD           bool
	state          int32
}

func record(kind uint32, size int, fill func([]byte)) []byte {
	b := make([]byte, (size+7)&^7)
	binary.NativeEndian.PutUint32(b, uint32(size))
	binary.NativeEndian.PutUint32(b[4:], kind)
	if fill != nil {
		fill(b)
	}
	return b
}

func fakeList(pcbs ...fakePCB) []byte {
	header := func() []byte {
		b := make([]byte, listHeaderSize)
		binary.NativeEndian.PutUint32(b, listHeaderSize)
		binary.NativeEndian.PutUint32(b[4:], uint32(len(pcbs)))
		return b
	}
	out := header()
	for _, p := range pcbs {
		out = append(out, record(xsoInpcb, 104, func(b []byte) {
			lhost, lport, _ := net.SplitHostPort(p.local)
			fhost, fport, _ := net.SplitHostPort(p.foreign)
			binary.BigEndian.PutUint16(b[inpcbLocalPort:], uint16(atoi(lport)))
			binary.BigEndian.PutUint16(b[inpcbForeignPort:], uint16(atoi(fport)))
			if p.v6 {
				b[inpcbVflag] = inpIPv6
				copy(b[inpcbLocalAddr:], net.ParseIP(lhost).To16())
				copy(b[inpcbForeignAddr:], net.ParseIP(fhost).To16())
			} else {
				b[inpcbVflag] = inpIPv4
				copy(b[inpcbLocalAddr+12:], net.ParseIP(lhost).To4())
				copy(b[inpcbForeignAddr+12:], net.ParseIP(fhost).To4())
			}
		})...)
		out = append(out, record(xsoSocket, 104, func(b []byte) {
			binary.NativeEndian.PutUint32(b[socketUIDAt:], p.uid)
			if p.noFD {
				binary.NativeEndian.PutUint16(b[socketStateAt:], ssNoFDRef)
			}
		})...)
		out = append(out, record(xsoRcvBuf, 32, nil)...)
		out = append(out, record(xsoSndBuf, 32, nil)...)
		out = append(out, record(xsoStats, 136, nil)...)
		out = append(out, record(xsoTcpcb, 204, func(b []byte) {
			binary.NativeEndian.PutUint32(b[tcpStateAt:], uint32(p.state))
		})...)
	}
	return append(out, header()...)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func tcp(hostport string) *net.TCPAddr {
	addr, err := net.ResolveTCPAddr("tcp", hostport)
	if err != nil {
		panic(err)
	}
	return addr
}

// The list is read as macOS writes it, and a lookup finds what the Linux
// kernel's own lookup would.
func TestSocketListLookups(t *testing.T) {
	const me, other = 501, 502
	list := fakeList(
		fakePCB{local: "127.0.0.1:8787", foreign: "0.0.0.0:0", uid: me, state: tcpsListen},
		fakePCB{local: "127.0.0.1:50000", foreign: "127.0.0.1:8787", uid: me, state: tcpsEstablished},
		fakePCB{local: "127.0.0.1:8787", foreign: "127.0.0.1:50000", uid: me, state: tcpsEstablished},
		fakePCB{local: "127.0.0.1:50001", foreign: "127.0.0.1:8787", uid: other, state: tcpsEstablished},
		fakePCB{local: "127.0.0.1:50002", foreign: "127.0.0.1:8787", uid: 0, noFD: true, state: 6}, // let go of, winding down
		fakePCB{local: "127.0.0.1:50003", foreign: "127.0.0.1:8787", uid: me, state: 2},            // still connecting
		fakePCB{local: "[::ffff:127.0.0.1]:50004", foreign: "[::ffff:127.0.0.1]:8787", v6: true, uid: me, state: tcpsCloseWait},
		fakePCB{local: "[::]:9000", foreign: "[::]:0", v6: true, uid: other, state: tcpsListen},
		fakePCB{local: "[::1]:9001", foreign: "[::]:0", v6: true, uid: me, state: tcpsListen},
	)
	pcbs, ok := parsePCBs(list)
	if !ok || len(pcbs) != 9 {
		t.Fatalf("parsed %d sockets, ok=%v", len(pcbs), ok)
	}
	for _, c := range []struct {
		name               string
		local, remote      string
		uid                uint32
		ok, live, conn, ls bool
	}{
		{"this user's client", "127.0.0.1:50000", "127.0.0.1:8787", me, true, true, true, false},
		{"another user's client", "127.0.0.1:50001", "127.0.0.1:8787", other, true, true, true, false},
		{"a socket let go of", "127.0.0.1:50002", "127.0.0.1:8787", 0, true, false, true, false},
		{"a socket still connecting", "127.0.0.1:50003", "127.0.0.1:8787", me, true, true, false, false},
		{"an IPv6 socket on 127.0.0.1", "127.0.0.1:50004", "127.0.0.1:8787", me, true, true, true, false},
		{"nobody", "127.0.0.1:50009", "127.0.0.1:8787", 0, false, false, false, false},
		{"the listener", "127.0.0.1:8787", "127.0.0.1:0", me, true, true, false, true},
		{"a listener on every address", "127.0.0.1:9000", "127.0.0.1:0", other, true, true, false, true},
		{"an IPv6 listener", "[::1]:9001", "[::1]:0", me, true, true, false, true},
		{"no listener", "127.0.0.1:9999", "127.0.0.1:0", 0, false, false, false, false},
	} {
		got, ok := matchPCB(pcbs, tcp(c.local), tcp(c.remote))
		if ok != c.ok || (ok && (got.uid != c.uid || got.live != c.live || got.connected != c.conn || got.listening != c.ls)) {
			t.Errorf("%s: %+v ok=%v", c.name, got, ok)
		}
	}

	// Two listeners of different users on one port are no answer; a
	// specific address wins over every address.
	pcbs, _ = parsePCBs(fakeList(
		fakePCB{local: "0.0.0.0:7000", foreign: "0.0.0.0:0", uid: other, state: tcpsListen},
		fakePCB{local: "127.0.0.1:7000", foreign: "0.0.0.0:0", uid: me, state: tcpsListen},
		fakePCB{local: "0.0.0.0:7001", foreign: "0.0.0.0:0", uid: other, state: tcpsListen},
		fakePCB{local: "[::]:7001", foreign: "[::]:0", v6: true, uid: me, state: tcpsListen},
	))
	if got, ok := matchPCB(pcbs, tcp("127.0.0.1:7000"), tcp("127.0.0.1:0")); !ok || got.uid != me {
		t.Errorf("specific listener: %+v %v", got, ok)
	}
	if _, ok := matchPCB(pcbs, tcp("127.0.0.1:7001"), tcp("127.0.0.1:0")); ok {
		t.Error("two users' listeners on one port gave an answer")
	}
	// A socket winding down on a pair of addresses beside the one in use.
	pcbs, _ = parsePCBs(fakeList(
		fakePCB{local: "127.0.0.1:50010", foreign: "127.0.0.1:8787", uid: other, noFD: true, state: 9},
		fakePCB{local: "127.0.0.1:50010", foreign: "127.0.0.1:8787", uid: me, state: tcpsEstablished},
	))
	if got, ok := matchPCB(pcbs, tcp("127.0.0.1:50010"), tcp("127.0.0.1:8787")); !ok || got.uid != me || !got.live {
		t.Errorf("the socket in use: %+v %v", got, ok)
	}
}

// A list that is not exactly as expected gives no answer at all.
func TestBrokenSocketListsAreUnread(t *testing.T) {
	good := fakeList(fakePCB{local: "127.0.0.1:1", foreign: "127.0.0.1:2", uid: 1, state: tcpsEstablished})
	if _, ok := parsePCBs(good); !ok {
		t.Fatal("a good list was refused")
	}
	broken := map[string][]byte{
		"empty":          nil,
		"short header":   good[:10],
		"cut short":      good[:len(good)-listHeaderSize],
		"cut mid-record": good[:listHeaderSize+40],
	}
	wrongHeader := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(wrongHeader, 32)
	broken["wrong header size"] = wrongHeader
	unknown := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(unknown[listHeaderSize+4:], 0x400)
	broken["unknown record"] = unknown
	tooLong := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(tooLong[listHeaderSize:], 1<<20)
	broken["record longer than the list"] = tooLong
	tiny := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(tiny[listHeaderSize:], 4)
	broken["record shorter than its header"] = tiny
	small := append([]byte(nil), good...)
	binary.NativeEndian.PutUint32(small[listHeaderSize:], 40)
	broken["address record too small"] = small
	orphan := append(append([]byte(nil), good[:listHeaderSize]...), record(xsoSocket, 104, nil)...)
	orphan = append(orphan, good[:listHeaderSize]...)
	broken["socket record without an address record"] = orphan
	for name, list := range broken {
		if _, ok := parsePCBs(list); ok {
			t.Errorf("%s: read as a list", name)
		}
	}
}
