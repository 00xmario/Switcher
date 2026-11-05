// Package fileperm decides whether a file's mode lets anyone but the current
// user change it.
//
// Linux distributions such as Ubuntu give each user a private group of their
// own and a umask of 002, so the user's files and folders are group-writable
// by default. Group write access to such a group lets nobody else in, so it
// does not make a file unsafe. On a Mac the user's group is a shared one
// (staff), where group write access stays unsafe.
package fileperm

import (
	"bufio"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

// OthersCanWrite reports whether a file with this mode and group can be
// changed by anyone but its owner: it is world-writable, or group-writable
// for a group other than the current user's private group.
func OthersCanWrite[M ~uint16 | ~uint32](mode M, gid uint32) bool {
	if mode&0o002 != 0 {
		return true
	}
	return mode&0o020 != 0 && !PrivateGroup(gid)
}

var (
	privateOnce sync.Once
	privateGID  = -1
)

// PrivateGroup reports whether gid is the current user's private group: the
// user's primary group, named after the user, with no other members and no
// other user's primary group.
func PrivateGroup(gid uint32) bool {
	privateOnce.Do(func() { privateGID = findPrivateGroup("/etc/group", "/etc/passwd") })
	return privateGID >= 0 && uint32(privateGID) == gid
}

// findPrivateGroup returns the current user's private group id, or -1.
func findPrivateGroup(groupFile, passwdFile string) int {
	me, err := user.Current()
	if err != nil {
		return -1
	}
	gid, err := strconv.Atoi(me.Gid)
	if err != nil || gid != os.Getgid() {
		return -1
	}
	private := false
	if !eachEntry(groupFile, func(f []string) bool {
		if len(f) < 4 || f[2] != me.Gid {
			return true
		}
		if f[0] != me.Username || (f[3] != "" && f[3] != me.Username) {
			return false
		}
		private = true
		return true
	}) || !private {
		return -1
	}
	if !eachEntry(passwdFile, func(f []string) bool {
		return len(f) < 4 || f[3] != me.Gid || f[2] == me.Uid
	}) {
		return -1
	}
	return gid
}

// eachEntry calls keep with the fields of every line of a colon-separated
// account file. It reports false when keep does or the file cannot be read.
func eachEntry(path string, keep func([]string) bool) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !keep(strings.Split(line, ":")) {
			return false
		}
	}
	return lines.Err() == nil
}
