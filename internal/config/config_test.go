package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexConfigPathRespectsCodexHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if got, want := CodexConfigPath(), filepath.Join(root, "config.toml"); got != want {
		t.Fatalf("Codex config path = %q, want %q", got, want)
	}
}

// The menu bar app may create the data directory with default permissions
// before the server starts; Dir makes it this user's alone again.
func TestDirIsPrivateEvenWhenCreatedOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	open := filepath.Join(home, ".switcher")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(); got != open {
		t.Fatalf("Dir() = %q, want %q", got, open)
	}
	info, err := os.Stat(open)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("data directory mode %o, want 700", perm)
	}
}
