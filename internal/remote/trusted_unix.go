//go:build unix

package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// resolveTrusted follows path to the program it names, one name at a time as
// the system does, and returns where the program really is. It accepts it
// only when the program and every folder looked in on the way, from / down
// and including the folders that hold each symbolic link, are owned by root
// or this user and writable by nobody else: no other account can have
// planted or swapped any part of it. Callers run the returned path, so no
// link can change in between.
func resolveTrusted(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	cur := "/"
	if fi, err := os.Stat(cur); err != nil || !trustedEntry(cur, fi) {
		return "", false
	}
	pending := pathNames(path)
	for links := 0; len(pending) > 0; {
		name := pending[0]
		pending = pending[1:]
		if name == ".." {
			cur = filepath.Dir(cur) // already checked on the way down
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// The link lives in cur, which is trusted; its target is
			// walked like the rest.
			if links++; links > 40 {
				return "", false
			}
			target, err := os.Readlink(next)
			if err != nil || target == "" {
				return "", false
			}
			if filepath.IsAbs(target) {
				cur = "/"
			}
			pending = append(pathNames(target), pending...)
			continue
		}
		if !trustedEntry(next, fi) {
			return "", false
		}
		if len(pending) == 0 {
			if !fi.Mode().IsRegular() {
				return "", false
			}
			return next, true
		}
		if !fi.IsDir() {
			return "", false
		}
		cur = next
	}
	return "", false
}

func pathNames(p string) []string {
	var out []string
	for _, n := range strings.Split(p, "/") {
		if n != "" && n != "." {
			out = append(out, n)
		}
	}
	return out
}

// assumeTrusted lets tests treat their temporary folder's ancestors, such
// as /tmp, as trusted. It is nil in Switcher.
var assumeTrusted func(path string) bool

// trustedEntry accepts a file or folder owned by root or this user that
// nobody else may write. A sticky folder such as /tmp is not trusted. On a
// Mac, folders such as /Applications let the admin group write; its members
// can act as root anyway, as can members of root's own group.
func trustedEntry(path string, fi os.FileInfo) bool {
	if assumeTrusted != nil && assumeTrusted(path) {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 && int(st.Uid) != os.Getuid() {
		return false
	}
	perm := fi.Mode().Perm()
	if perm&0o002 != 0 {
		return false
	}
	return perm&0o020 == 0 || st.Gid == 0 || runtime.GOOS == "darwin" && st.Gid == 80
}
