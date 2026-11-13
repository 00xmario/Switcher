package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An account file is this user's alone from its first byte, also when a
// file with a wider mode, or a link, sits where an older build kept its
// temporary copy; nothing temporary is left behind or listed as an account.
func TestAccountWritesArePrivateAndLeaveNothingBehind(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	accounts := filepath.Join(dir, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	// What an older build, or someone else, may have left.
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(accounts, "a.json.tmp")
	if err := os.Symlink(outside, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Account{ID: "a", Provider: "claude", Email: "you@example.com", Token: Token{AccessToken: "secret"}}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "untouched" {
		t.Fatalf("the write went through a link: %q", raw)
	}
	info, err := os.Stat(filepath.Join(accounts, "a.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("account file: %v %v", info, err)
	}
	// Replacing a file that had a wider mode makes it private.
	if err := os.Chmod(filepath.Join(accounts, "a.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Account{ID: "a", Provider: "claude", Email: "you@example.com", Token: Token{AccessToken: "next"}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(accounts, "a.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("replaced file is %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(accounts)
	for _, e := range entries {
		if e.Name() != "a.json" && e.Name() != "a.json.tmp" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Token.AccessToken != "next" {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, a := range list {
		if strings.Contains(a.ID, "tmp") {
			t.Fatal("a temporary file was listed as an account")
		}
	}
}
