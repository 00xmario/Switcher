package localpeer

import (
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// probeEnv makes this program the self-test's other process: a separate
// process of this user, as every real client is. Set to "<loopback
// address>,<token>", the program connects there and says the token at once,
// before anything else of it runs, then holds the connection until its
// standard input closes, and exits.
const probeEnv = "SWITCHER_PEER_PROBE"

func init() {
	value, set := os.LookupEnv(probeEnv)
	if !set {
		return
	}
	os.Unsetenv(probeEnv)
	addr, token, _ := strings.Cut(value, ",")
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() || len(token) != 32 {
		os.Exit(2)
	}
	conn, err := net.DialTimeout("tcp", addr, selfTestWait)
	if err != nil {
		os.Exit(2)
	}
	_, _ = conn.Write([]byte(token + "\n"))
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * selfTestWait):
	}
	conn.Close()
	os.Exit(0)
}
