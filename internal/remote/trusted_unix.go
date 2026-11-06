//go:build unix

package remote

import (
	"os"
	"path/filepath"
	"syscall"
)

// trustedBinary accepts a program, after following symbolic links, that is a
// regular file owned by root or this user and that neither the file nor its
// folder lets a group or anyone else change: no other account on this
// computer can have planted or replaced it.
func trustedBinary(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	file, err := os.Stat(resolved)
	if err != nil || !file.Mode().IsRegular() {
		return false
	}
	folder, err := os.Stat(filepath.Dir(resolved))
	if err != nil {
		return false
	}
	return ownedHere(file) && ownedHere(folder)
}

// ownedHere reports whether root or this user owns fi and only the owner
// may write it.
func ownedHere(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == 0 || int(st.Uid) == os.Getuid()) && fi.Mode().Perm()&0o022 == 0
}
