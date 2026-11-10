package sysload

import (
	"context"
	"os/exec"
	"time"
)

// macOS gives no other process's times to an ordinary program without
// libproc, so ps lists them: processor time used so far, resident memory
// and the executable's path, which names the app bundle it belongs to.
// Graphics use per app is not reported this way.

// psTimeout bounds one listing.
const psTimeout = 2 * time.Second

func darwinProcs() (procSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,time=,rss=,comm=")
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return procSnapshot{}, err
	}
	return parsePS(out), nil
}
