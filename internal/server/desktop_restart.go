package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"switcher/internal/desktoprelay"
)

var desktopAppOperation sync.Mutex

func (a *API) handleDesktopRestart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirmed bool `json:"confirmed"`
	}
	if !decodeDesktopRelayBody(w, r, &body) {
		return
	}
	if !body.Confirmed {
		writeDesktopRelayBadRequest(w)
		return
	}
	if !desktopAppOperation.TryLock() {
		writeDesktopRelayError(w, desktoprelay.ErrBusy)
		return
	}
	defer desktopAppOperation.Unlock()
	setup := a.desktopRelaySetup()
	if setup.Condition == "unavailable" {
		writeDesktopRelayError(w, desktoprelay.ErrUnavailable)
		return
	}
	// Manager status retains these ownership fields after a completed restore.
	// A never-configured path has no record and must not authorize app control.
	owned := setup.RestartRequired && setup.ScopeID != "" && setup.BackupPath != ""
	configured := setup.Configured && setup.Condition == "configured"
	restored := !setup.Configured && setup.Condition == "not_configured"
	if !owned || !(configured || restored) {
		writeDesktopRelayError(w, errDesktopSetupConflict)
		return
	}
	// Confirmation permits interrupting tasks. Neither GET nor setup queries
	// inspect OS processes, and restart does not sync indexes or credentials.
	restart := restartClaudeDesktop
	if a.RestartDesktopForTest != nil {
		restart = a.RestartDesktopForTest
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := restart(ctx); err != nil {
		writeDesktopRestartError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restart_requested"})
}

// desktopRestartError is a failed restart in words for the person who asked.
// The text comes from this package; the cause only goes to the log.
type desktopRestartError struct {
	message string
	cause   error
}

func (e *desktopRestartError) Error() string {
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

func (e *desktopRestartError) Unwrap() error { return e.cause }

func restartFailure(message string, cause error) error {
	return &desktopRestartError{message: message, cause: cause}
}

func writeDesktopRestartError(w http.ResponseWriter, err error) {
	message := "Claude Desktop could not be restarted. If it closed, open it yourself."
	var failure *desktopRestartError
	if errors.As(err, &failure) {
		message = failure.message
	}
	log.Printf("desktop restart: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message, "error_code": "desktop_restart_failed"})
}

type desktopRestartOperations struct {
	quit    func(context.Context) error
	running func(context.Context) (bool, error)
	open    func(context.Context) error
}

func restartClaudeDesktop(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
	case "linux":
		return newLinuxDesktop().restart(ctx)
	default:
		return restartFailure("Switcher can restart Claude Desktop only on a Mac or on Linux.", nil)
	}
	return runDesktopRestart(ctx, desktopRestartOperations{
		quit: func(ctx context.Context) error {
			return exec.CommandContext(ctx, "/usr/bin/osascript", "-e",
				`if application "Claude" is running then tell application "Claude" to quit`).Run()
		},
		running: func(ctx context.Context) (bool, error) {
			err := exec.CommandContext(ctx, "/usr/bin/pgrep", "-x", "Claude").Run()
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return false, nil
			}
			return err == nil, err
		},
		open: func(ctx context.Context) error {
			return exec.CommandContext(ctx, "/usr/bin/open", "-a", "Claude").Run()
		},
	})
}

func runDesktopRestart(ctx context.Context, ops desktopRestartOperations) error {
	quitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := ops.quit(quitCtx)
	cancel()
	if err != nil {
		return restartFailure("Claude Desktop could not be asked to quit, so it was left running.", err)
	}
	// After a successful quit request, finish waiting and reopening even if
	// the client disconnects. A failed exit check still blocks open.
	exitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = waitDesktopExit(exitCtx, ops.running)
	cancel()
	if err != nil {
		return restartFailure("Claude Desktop did not quit in time, so it was not reopened. Close it yourself and open it again.", err)
	}
	reopenCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := ops.open(reopenCtx); err != nil {
		return restartFailure("Claude Desktop closed but did not come back. Open it yourself.", err)
	}
	return nil
}

func waitDesktopExit(ctx context.Context, running func(context.Context) (bool, error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		active, err := running(ctx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
