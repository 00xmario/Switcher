package localpeer

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// On a Mac the check must work: the system's list of sockets must read as
// expected. If it does not, the records around this test's own connection
// are printed, so the offsets in pcblist.go can be put right.
func TestTheMacSaysWhoOwnsAConnection(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	uid, ok := PeerUID(server)
	listener, lok := ListenerUID(ln.Addr().(*net.TCPAddr))
	if ok && uid == me() && lok && listener == me() && Works() && worksFor(true) {
		return
	}
	t.Errorf("peer uid=%d ok=%v, listener uid=%d ok=%v, works v4=%v v6=%v; this test runs as uid %d pid %d",
		uid, ok, listener, lok, Works(), worksFor(true), os.Getuid(), os.Getpid())
	raw, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		t.Fatalf("sysctl: %v", err)
	}
	pcbs, parsed := parsePCBs(raw)
	t.Logf("list: %d bytes, parsed=%v (%d sockets)", len(raw), parsed, len(pcbs))
	ports := map[uint16]bool{
		uint16(ln.Addr().(*net.TCPAddr).Port):          true,
		uint16(client.LocalAddr().(*net.TCPAddr).Port): true,
	}
	t.Logf("looking for ports %v (big-endian bytes %s)", ports, portBytes(ports))
	t.Log(dumpRecords(raw, ports))
}

func portBytes(ports map[uint16]bool) string {
	var parts []string
	for p := range ports {
		b := make([]byte, 2)
		binary.BigEndian.PutUint16(b, p)
		parts = append(parts, hex.EncodeToString(b))
	}
	return strings.Join(parts, ",")
}

// dumpRecords prints the record headers of the list, and in full every run
// of records that holds one of ports anywhere in its address record.
func dumpRecords(raw []byte, ports map[uint16]bool) string {
	var out strings.Builder
	order := binary.NativeEndian
	if len(raw) < 8 {
		return "list too short"
	}
	fmt.Fprintf(&out, "header len=%d count=%d\n", order.Uint32(raw), order.Uint32(raw[4:]))
	at := int(order.Uint32(raw))
	var group [][]byte
	flush := func() {
		if len(group) == 0 {
			return
		}
		hit := false
		for p := range ports {
			b := make([]byte, 2)
			binary.BigEndian.PutUint16(b, p)
			if strings.Contains(string(group[0]), string(b)) {
				hit = true
			}
		}
		if hit {
			for _, r := range group {
				fmt.Fprintf(&out, "record len=%d kind=%#x\n%s", order.Uint32(r), order.Uint32(r[4:]), hex.Dump(r))
			}
		}
		group = nil
	}
	kinds := map[uint32]int{}
	for at+8 <= len(raw) {
		size, kind := int(order.Uint32(raw[at:])), order.Uint32(raw[at+4:])
		if size < 8 || at+size > len(raw) {
			fmt.Fprintf(&out, "stopped at %d: len=%d kind=%#x\n", at, size, kind)
			break
		}
		if kind == xsoInpcb {
			flush()
		}
		kinds[kind]++
		group = append(group, raw[at:at+size])
		at += (size + 7) &^ 7
	}
	flush()
	fmt.Fprintf(&out, "records by kind: %v\n", kinds)
	return out.String()
}

// describeLookup prints what the socket list holds about both ends of conn.
func describeLookup(conn net.Conn) string {
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	remote, _ := conn.RemoteAddr().(*net.TCPAddr)
	var out strings.Builder
	raw, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	pcbs, parsed := parsePCBs(raw)
	fmt.Fprintf(&out, "list: %d bytes, err=%v, parsed=%v (%d sockets); still connected here: %v\n", len(raw), err, parsed, len(pcbs), stillConnected(conn))
	found, ok := matchPCB(pcbs, remote, local)
	fmt.Fprintf(&out, "match for the other end: %+v ok=%v\n", found, ok)
	for _, p := range pcbs {
		if p.localPort == uint16(local.Port) || p.foreignPort == uint16(local.Port) || p.localPort == uint16(remote.Port) || p.foreignPort == uint16(remote.Port) {
			fmt.Fprintf(&out, "  pcb %s:%d -> %s:%d uid=%d state=%d socket=%v tcp=%v nofdref=%v\n", p.localAddr, p.localPort, p.foreignAddr, p.foreignPort, p.uid, p.state, p.haveSocket, p.haveTCP, p.noFDRef)
		}
	}
	out.WriteString(dumpRecords(raw, map[uint16]bool{uint16(local.Port): true, uint16(remote.Port): true}))
	return out.String()
}
