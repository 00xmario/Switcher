//go:build unix

package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A command line that exits saying no daemon runs is told apart from one
// that times out, is killed or says anything else.
func TestOnlyACommandThatSaysNoDaemonRunsMeansDown(t *testing.T) {
	tailscaleTimeout = 500 * time.Millisecond
	t.Cleanup(func() { tailscaleTimeout = 5 * time.Second })
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, body string
		down       bool
	}{
		{"no daemon", `echo "failed to connect to local tailscaled; it doesn't appear to be running (sudo systemctl start tailscaled ?)" >&2; exit 1`, true},
		{"a stale socket", `echo "failed to connect to local tailscaled (no tailscaled process found): dial unix /run/tailscale/tailscaled.sock: connect: connection refused" >&2; exit 1`, true},
		{"a daemon that does not answer", `echo "failed to connect to local tailscaled (which appears to be running as tailscaled, pid 9). Got error: connection refused" >&2; exit 1`, false},
		{"the message on stdout only", `echo "failed to connect to local tailscaled; it doesn't appear to be running"; exit 1`, false},
		{"a timeout", `sleep 5`, false},
		{"killed", `echo "failed to connect to local tailscaled; it doesn't appear to be running" >&2; kill -9 $$`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runProgram(context.Background(), script(filepath.Base(t.Name()), tc.body), 1<<10, "status", "--json")
			if err == nil || daemonDown(err) != tc.down {
				t.Fatalf("error %v, down %v", err, daemonDown(err))
			}
		})
	}
	if out, err := runProgram(context.Background(), script("ok", `echo '{"BackendState":"Running"}'`), 1<<10, "status"); err != nil || string(out) != "{\"BackendState\":\"Running\"}\n" {
		t.Fatalf("a working command: %q %v", out, err)
	}
}

// That Tailscale is down is kept for two seconds, as it may be started any
// moment; other answers for ten.
func TestTailscaleDownIsRememberedOnlyBriefly(t *testing.T) {
	runs := 0
	s := &systemTailscale{run: func(context.Context, int, ...string) ([]byte, error) {
		runs++
		return nil, exited("failed to connect to local tailscaled; it doesn't appear to be running")
	}}
	ctx := context.Background()
	if _, err := s.selfNode(ctx); !errors.Is(err, errTailscaleDown) {
		t.Fatalf("not down: %v", err)
	}
	s.selfNode(ctx)
	if runs != 1 {
		t.Fatalf("ran %d times within two seconds", runs)
	}
	s.mu.Lock()
	s.selfAt = s.selfAt.Add(-3 * time.Second)
	s.mu.Unlock()
	s.selfNode(ctx)
	if runs != 2 {
		t.Fatalf("down was kept past two seconds: %d runs", runs)
	}
}
