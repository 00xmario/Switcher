package antigravity

import (
	"context"
	"os"
	"testing"
)

// TestSecretServiceLive runs against a real Secret Service. It is opt-in
// (SWITCHER_LIVE_SECRET_SERVICE=1) and meant for a throwaway session bus,
// such as dbus-run-session with its own gnome-keyring-daemon: it writes
// agy's item.
func TestSecretServiceLive(t *testing.T) {
	if os.Getenv("SWITCHER_LIVE_SECRET_SERVICE") != "1" {
		t.Skip("set SWITCHER_LIVE_SECRET_SERVICE=1 on a throwaway session bus")
	}
	s := secretService{}
	ctx := context.Background()
	for _, email := range []string{"me@example.com", "work@example.com"} {
		raw, _ := appTokenOf(appAccount(email))
		if err := s.write(ctx, raw); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.read(ctx)
		if err != nil || !ok || emailOf(got) != email {
			t.Fatalf("read back: %s %v %v", got, ok, err)
		}
	}
}
