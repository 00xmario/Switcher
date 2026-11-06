//go:build unix

package remote

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The Tailscale command line decides who may pair, so only one that no
// other account can have planted or changed is run.
func TestOnlyATailscaleNobodyElseCanChangeIsRun(t *testing.T) {
	// The folder's mode follows the umask, which may let the group write.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", 0o755)
	if !trustedBinary(good) {
		t.Fatal("this user's own program, writable only by them, was refused")
	}
	os.Chmod(dir, 0o770)
	if trustedBinary(good) {
		t.Fatal("trusted a program in a folder its group can change")
	}
	os.Chmod(dir, 0o700)
	for name, mode := range map[string]os.FileMode{"group-writable": 0o775, "world-writable": 0o757} {
		if trustedBinary(write(name, mode)) {
			t.Errorf("a %s program was trusted", name)
		}
	}
	link := filepath.Join(dir, "link")
	os.Symlink(good, link)
	if !trustedBinary(link) {
		t.Fatal("a link to a trusted program was refused")
	}
	badLink := filepath.Join(dir, "bad-link")
	os.Symlink(write("bad", 0o777), badLink)
	if trustedBinary(badLink) || trustedBinary(filepath.Join(dir, "missing")) || trustedBinary(dir) {
		t.Fatal("trusted a link to a changeable program, a missing one or a folder")
	}
	open := filepath.Join(dir, "open")
	os.Mkdir(open, 0o700)
	inOpen := filepath.Join(open, "tailscale")
	os.WriteFile(inOpen, nil, 0o755)
	os.Chmod(open, 0o777)
	defer os.Chmod(open, 0o700)
	if trustedBinary(inOpen) {
		t.Fatal("trusted a program in a folder anyone can change")
	}

	// The fixed places come first; PATH counts only for a trusted program.
	trusted := func(p string) bool { return p == "/fixed/b" || p == "/path/tailscale" }
	onPath := func() (string, error) { return "/path/tailscale", nil }
	if got := findTailscaleIn([]string{"/fixed/a", "/fixed/b"}, onPath, trusted); got != "/fixed/b" {
		t.Fatalf("found %q before the trusted fixed place", got)
	}
	if got := findTailscaleIn([]string{"/fixed/a"}, onPath, trusted); got != "/path/tailscale" {
		t.Fatalf("found %q", got)
	}
	untrustedPath := func() (string, error) { return "/home/other/bin/tailscale", nil }
	if got := findTailscaleIn([]string{"/fixed/a"}, untrustedPath, trusted); got != "" {
		t.Fatalf("ran %q from PATH", got)
	}
	relative := func() (string, error) { return "tailscale", nil }
	if got := findTailscaleIn(nil, relative, func(string) bool { return true }); got != "" {
		t.Fatalf("ran a relative %q", got)
	}
	if got := findTailscaleIn(nil, func() (string, error) { return "", errors.New("none") }, trusted); got != "" {
		t.Fatalf("found %q", got)
	}
}
