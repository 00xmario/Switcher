//go:build unix

package remote

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real system program resolves through the system's own links, and the
// path returned is the program itself.
func TestSystemProgramsResolveToThemselves(t *testing.T) {
	resolved, ok := resolveTrusted("/bin/sh")
	if !ok {
		t.Fatal("/bin/sh is not trusted")
	}
	fi, err := os.Lstat(resolved)
	if err != nil || !fi.Mode().IsRegular() || !filepath.IsAbs(resolved) {
		t.Fatalf("resolved to %q, not a program", resolved)
	}
	for _, p := range []string{"bin/sh", "/", "/bin", "/no/such/program"} {
		if _, ok := resolveTrusted(p); ok {
			t.Errorf("%q was trusted", p)
		}
	}
}

// The Tailscale command line decides who may pair, so only one that no
// other account can have planted or changed, along the whole way to it, is
// run, and the path run is the resolved one.
func TestOnlyATailscaleNobodyElseCanChangeIsRun(t *testing.T) {
	// The temporary folder's ancestors, such as /tmp, count as trusted
	// here; everything below it is checked. Its own mode follows the
	// umask, which may let the group write.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	assumeTrusted = func(p string) bool { return p == "/" || p == root || strings.HasPrefix(root, p+"/") }
	t.Cleanup(func() { assumeTrusted = nil })
	dir := func(name string, mode os.FileMode) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		t.Cleanup(func() { os.Chmod(p, 0o700) })
		return p
	}
	program := func(p string, mode os.FileMode) string {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	link := func(p, target string) string {
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	trusted := func(p string) bool { _, ok := resolveTrusted(p); return ok }

	good := program(filepath.Join(root, "good"), 0o755)
	if got, ok := resolveTrusted(good); !ok || got != good {
		t.Fatalf("this user's own program: %q %v", got, ok)
	}
	if got, ok := resolveTrusted(link(filepath.Join(root, "link"), good)); !ok || got != good {
		t.Fatalf("a link resolves to %q, %v; the program itself is what runs", got, ok)
	}
	sub := dir("sub", 0o755)
	if got, ok := resolveTrusted(link(filepath.Join(sub, "up"), "../good")); !ok || got != good {
		t.Fatalf("a relative link: %q %v", got, ok)
	}
	for name, p := range map[string]string{
		"a group-writable program":                  program(filepath.Join(root, "group"), 0o775),
		"a world-writable program":                  program(filepath.Join(root, "world"), 0o757),
		"a program in a folder its group can write": program(filepath.Join(dir("group-folder", 0o770), "tailscale"), 0o755),
		"a program in a sticky shared folder":       program(filepath.Join(dir("sticky", 0o1777), "tailscale"), 0o755),
		"a link in a folder anyone can write":       link(filepath.Join(dir("open-links", 0o777), "tailscale"), good),
		"a link to a program in a folder anyone can write": link(filepath.Join(root, "to-open"),
			program(filepath.Join(dir("open-target", 0o777), "tailscale"), 0o755)),
		"a link loop":       link(filepath.Join(root, "loop"), filepath.Join(root, "loop")),
		"a folder":          sub,
		"a missing program": filepath.Join(root, "missing"),
	} {
		if trusted(p) {
			t.Errorf("%s was trusted", name)
		}
	}

	// The fixed places come first; PATH counts only for a trusted program;
	// what is returned is the resolved path.
	fake := func(p string) (string, bool) {
		switch p {
		case "/fixed/b":
			return "/real/b", true
		case "/path/tailscale":
			return "/real/path", true
		}
		return "", false
	}
	onPath := func() (string, error) { return "/path/tailscale", nil }
	if got := findTailscaleIn([]string{"/fixed/a", "/fixed/b"}, onPath, fake); got != "/real/b" {
		t.Fatalf("found %q", got)
	}
	if got := findTailscaleIn([]string{"/fixed/a"}, onPath, fake); got != "/real/path" {
		t.Fatalf("found %q", got)
	}
	untrustedPath := func() (string, error) { return "/home/other/bin/tailscale", nil }
	if got := findTailscaleIn([]string{"/fixed/a"}, untrustedPath, fake); got != "" {
		t.Fatalf("ran %q from PATH", got)
	}
	relative := func() (string, error) { return "tailscale", nil }
	if got := findTailscaleIn(nil, relative, func(p string) (string, bool) { return p, true }); got != "" {
		t.Fatalf("ran a relative %q", got)
	}
	if got := findTailscaleIn(nil, func() (string, error) { return "", errors.New("none") }, fake); got != "" {
		t.Fatalf("found %q", got)
	}
}
