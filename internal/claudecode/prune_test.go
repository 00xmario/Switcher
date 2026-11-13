package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Recovery copies hold whole logins. Old ones go; recent ones, anything a
// link points to, and everything while a switch is unfinished, stay.
func TestPruneRecoveryRemovesOnlyOldCopies(t *testing.T) {
	root := t.TempDir()
	m := &Manager{dataRoot: filepath.Join(root, "claude-native")}
	backups, generations := filepath.Join(m.dataRoot, "backups"), filepath.Join(m.dataRoot, "generations")
	for _, dir := range []string{backups, generations} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-RecoveryKept - time.Hour)
	age := func(path string) {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	file := func(path string) {
		if err := os.WriteFile(path, []byte(`{"refresh":"secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"old-switch", "new-switch"} {
		if err := os.Mkdir(filepath.Join(backups, name), 0o700); err != nil {
			t.Fatal(err)
		}
		file(filepath.Join(backups, name, "snapshot.json"))
	}
	age(filepath.Join(backups, "old-switch"))
	file(filepath.Join(generations, "a-old.json"))
	age(filepath.Join(generations, "a-old.json"))
	file(filepath.Join(generations, "a-new.json"))
	// Something outside, reached through an old link: never followed.
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	file(filepath.Join(outside, "keep.json"))
	age(outside)
	if err := os.Symlink(outside, filepath.Join(backups, "link")); err != nil {
		t.Fatal(err)
	}
	exists := func(path string) bool { _, err := os.Lstat(path); return err == nil }

	// An unfinished switch keeps everything.
	file(m.pendingPath())
	if n := m.PruneRecovery(RecoveryKept); n != 0 || !exists(filepath.Join(backups, "old-switch")) {
		t.Fatalf("pruned %d copies during an unfinished switch", n)
	}
	if err := os.Remove(m.pendingPath()); err != nil {
		t.Fatal(err)
	}
	if n := m.PruneRecovery(RecoveryKept); n != 2 {
		t.Fatalf("pruned %d copies, want 2", n)
	}
	for path, want := range map[string]bool{
		filepath.Join(backups, "old-switch"):                  false,
		filepath.Join(generations, "a-old.json"):              false,
		filepath.Join(backups, "new-switch", "snapshot.json"): true,
		filepath.Join(generations, "a-new.json"):              true,
		filepath.Join(backups, "link"):                        true,
		filepath.Join(outside, "keep.json"):                   true,
	} {
		if exists(path) != want {
			t.Errorf("%s: exists=%v, want %v", path, exists(path), want)
		}
	}
	// Nothing to prune, and no folders at all, are fine.
	if n := m.PruneRecovery(RecoveryKept); n != 0 {
		t.Fatalf("second run pruned %d", n)
	}
	if n := (&Manager{dataRoot: filepath.Join(root, "none")}).PruneRecovery(RecoveryKept); n != 0 {
		t.Fatalf("empty root pruned %d", n)
	}
}
