// Package fileperm decides whether a file's mode lets anyone but the current
// user change it.
//
// Linux distributions such as Ubuntu give each user a private group of their
// own and a umask of 002, so the user's files and folders are group-writable
// by default. Group write access to such a group lets nobody else in, so it
// does not make a file unsafe. On a Mac the user's group is a shared one
// (staff), and group write access always counts as unsafe.
package fileperm

import (
	"bufio"
	"errors"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

// OthersCanWrite reports whether a file with this mode and group can be
// changed by anyone but its owner: it is world-writable, or group-writable
// for a group other than the current user's private group.
//
// With fd >= 0, an open descriptor of that file, a group-writable file must
// also carry no POSIX ACL, whose named entries the group bits only mask. With
// fd < 0 only the mode is judged: callers that check a name first must check
// the same file again through its descriptor.
func OthersCanWrite[M ~uint16 | ~uint32](fd int, mode M, gid uint32) bool {
	if mode&0o002 != 0 {
		return true
	}
	if mode&0o020 == 0 {
		return false
	}
	if !PrivateGroup(gid) {
		return true
	}
	return fd >= 0 && hasACL(fd)
}

// The files that decide who is in a group. A change to any of them makes
// PrivateGroup look again.
var accountFiles = []string{"/etc/group", "/etc/passwd", "/etc/nsswitch.conf", "/etc/sssd/sssd.conf", "/var/lib/sss/pipes/nss"}

// userdbDirs hold systemd userdb records, which can add group members that
// /etc/group does not list.
var userdbDirs = []string{"/etc/userdb", "/run/userdb", "/run/host/userdb", "/usr/local/lib/userdb", "/usr/lib/userdb"}

var private struct {
	sync.Mutex
	signature string
	gid       int
}

// PrivateGroup reports whether gid is the current user's private group: the
// user's primary group, named after the user, with no other members and no
// other user's primary group, as far as the system's account databases say.
// It answers false wherever membership can come from a source it cannot read
// (LDAP, NIS, a configured sssd, systemd userdb records), and on systems
// other than Linux.
func PrivateGroup(gid uint32) bool {
	if !privateGroupsApply {
		return false
	}
	sig := signature(accountFiles, userdbDirs)
	private.Lock()
	defer private.Unlock()
	if sig != private.signature || private.signature == "" {
		private.gid = findPrivateGroup("/etc/group", "/etc/passwd", "/etc/nsswitch.conf", "/etc/sssd/sssd.conf", "/var/lib/sss/pipes/nss", userdbDirs)
		private.signature = sig
	}
	return private.gid >= 0 && uint32(private.gid) == gid
}

// signature identifies the current content of the account files, so a
// changed group or user is noticed without restarting.
func signature(files, dirs []string) string {
	var b strings.Builder
	b.WriteString("v1")
	for _, name := range append(append([]string{}, files...), dirs...) {
		b.WriteByte('|')
		info, err := os.Stat(name)
		if err != nil {
			b.WriteString("-")
			continue
		}
		b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(info.Size(), 10))
	}
	return b.String()
}

// findPrivateGroup returns the current user's private group id, or -1.
func findPrivateGroup(groupFile, passwdFile, nsswitch, sssdConf, sssdPipe string, userdb []string) int {
	if !localAccountsOnly(nsswitch, sssdConf, sssdPipe, userdb) {
		return -1
	}
	me, err := user.Current()
	if err != nil {
		return -1
	}
	gid, err := strconv.ParseUint(me.Gid, 10, 32)
	if err != nil || gid != uint64(os.Getgid()) {
		return -1
	}
	private := false
	if !eachEntry(groupFile, 3, func(f []string, id uint64) bool {
		if id != gid {
			return true
		}
		if private || f[0] != me.Username || (len(f) > 3 && strings.TrimSpace(f[3]) != "" && strings.TrimSpace(f[3]) != me.Username) {
			return false
		}
		private = true
		return true
	}) || !private {
		return -1
	}
	if !eachEntry(passwdFile, 4, func(f []string, id uint64) bool {
		return id != gid || strings.TrimSpace(f[2]) == me.Uid
	}) {
		return -1
	}
	return int(gid)
}

// localAccountsOnly reports whether users and groups come only from files
// this package reads: files, systemd without userdb records, and sss while
// sssd is not set up.
func localAccountsOnly(nsswitch, sssdConf, sssdPipe string, userdb []string) bool {
	f, err := os.Open(nsswitch)
	if errors.Is(err, os.ErrNotExist) {
		return true // glibc's default for passwd and group is files
	}
	if err != nil {
		return false
	}
	defer f.Close()
	seen := map[string]bool{}
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line, _, _ := strings.Cut(lines.Text(), "#")
		database, sources, ok := strings.Cut(line, ":")
		database = strings.TrimSpace(database)
		if !ok || (database != "passwd" && database != "group" && database != "initgroups") {
			continue
		}
		seen[database] = true
		for _, source := range strings.Fields(sources) {
			switch {
			case strings.HasPrefix(source, "["):
				// An action such as [NOTFOUND=return] only narrows lookups.
			case source == "files":
			case source == "systemd":
				if !emptyDirs(userdb) {
					return false
				}
			case source == "sss":
				if exists(sssdConf) || exists(sssdPipe) {
					return false
				}
			default:
				return false
			}
		}
	}
	return lines.Err() == nil && seen["passwd"] && seen["group"]
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func emptyDirs(dirs []string) bool {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || len(entries) != 0 {
			return false
		}
	}
	return true
}

// eachEntry calls keep with the fields and numeric id (field idField) of
// every line of a colon-separated account file. It reports false when keep
// does, when a line is an NIS +/- entry or has no readable id, or when the
// file cannot be read.
func eachEntry(path string, idField int, keep func([]string, uint64) bool) bool {
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
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			return false
		}
		fields := strings.Split(line, ":")
		if len(fields) <= idField-1 || len(fields) < 3 {
			return false
		}
		id, err := strconv.ParseUint(strings.TrimSpace(fields[idField-1]), 10, 32)
		if err != nil {
			return false
		}
		if !keep(fields, id) {
			return false
		}
	}
	return lines.Err() == nil
}
