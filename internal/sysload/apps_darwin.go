package sysload

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// macOS gives no other process's times to an ordinary program without
// libproc, so ps lists them: processor time used so far, resident memory,
// when each started, and the kernel's name for its program; a second
// listing gives the path each was started as, which names the app bundle
// it belongs to. Graphics use per app is not reported this way.

// psTimeout bounds one listing.
const psTimeout = 2 * time.Second

func darwinProcs() (procSnapshot, error) {
	stats, err := runPS("pid=,time=,rss=,lstart=,ucomm=")
	if err != nil {
		return procSnapshot{}, err
	}
	paths, err := runPS("pid=,comm=")
	if err != nil {
		paths = nil // names without app bundles
	}
	return parsePS(stats, paths), nil
}

func runPS(format string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	// -ww: whole paths, however wide; ps would cut them to a terminal's
	// width otherwise.
	cmd := exec.CommandContext(ctx, "/bin/ps", "-ww", "-axo", format)
	cmd.WaitDelay = 500 * time.Millisecond
	// Start times in one fixed form, and names as they are written.
	env := []string{"LC_TIME=C", "LC_CTYPE=en_US.UTF-8"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LC_") && !strings.HasPrefix(kv, "LANG=") && !strings.HasPrefix(kv, "COLUMNS=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	return cmd.Output()
}
