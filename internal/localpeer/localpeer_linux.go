package localpeer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
)

// The kernel's table of TCP sockets, one per line:
//
//	sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
//	 0: 0100007F:2253 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 ...
//
// An address is the four bytes as the kernel holds them, printed as one
// number in this machine's byte order; the port is in hexadecimal.
var tcpTable = "/proc/net/tcp"

// tcp6Table lists the IPv6 sockets the same way, an address being its four
// 32-bit words, each printed in this machine's byte order.
var tcp6Table = "/proc/net/tcp6"

const stateListen = "0A"

// tableAddr is an address as its table prints it, with that table.
func tableAddr(a *net.TCPAddr) (text, table string, ok bool) {
	if a.Port < 0 || a.Port > 65535 {
		return "", "", false
	}
	if ip := a.IP.To4(); ip != nil {
		return fmt.Sprintf("%08X:%04X", binary.NativeEndian.Uint32(ip), a.Port), tcpTable, true
	}
	ip := a.IP.To16()
	if ip == nil {
		return "", "", false
	}
	for i := 0; i < 16; i += 4 {
		text += fmt.Sprintf("%08X", binary.NativeEndian.Uint32(ip[i:i+4]))
	}
	return fmt.Sprintf("%s:%04X", text, a.Port), tcp6Table, true
}

// socketUID is the owner of the socket bound to local and connected to remote.
func socketUID(local, remote *net.TCPAddr) (uint32, bool) {
	from, table, ok1 := tableAddr(local)
	to, table2, ok2 := tableAddr(remote)
	if !ok1 || !ok2 || table != table2 {
		return 0, false
	}
	return find(table, func(fields [][]byte) bool {
		return string(fields[1]) == from && string(fields[2]) == to && string(fields[3]) != stateListen
	})
}

// listenerUID is the owner of the socket listening on addr, or on every
// address at its port.
func listenerUID(addr *net.TCPAddr) (uint32, bool) {
	exact, table, ok := tableAddr(addr)
	if !ok {
		return 0, false
	}
	any := fmt.Sprintf("%08X:%04X", 0, addr.Port)
	if table == tcp6Table {
		any = fmt.Sprintf("%032X:%04X", 0, addr.Port)
	}
	return find(table, func(fields [][]byte) bool {
		return string(fields[3]) == stateListen && (string(fields[1]) == exact || string(fields[1]) == any)
	})
}

// find answers the uid of the one socket that matches. Two matches are no
// answer: the caller cannot tell which is meant.
func find(table string, match func(fields [][]byte) bool) (uint32, bool) {
	raw, err := os.ReadFile(table)
	if err != nil {
		return 0, false
	}
	var uid uint32
	found := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := bytes.Fields(scanner.Bytes())
		if len(fields) < 8 || !match(fields) {
			continue
		}
		n, err := strconv.ParseUint(string(fields[7]), 10, 32)
		if err != nil {
			return 0, false
		}
		if found > 0 && uint32(n) != uid {
			return 0, false
		}
		uid, found = uint32(n), found+1
	}
	return uid, found > 0
}
