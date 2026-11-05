package fileperm

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestFindPrivateGroup(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	gid, _ := strconv.Atoi(me.Gid)
	passwd := me.Username + ":x:" + me.Uid + ":" + me.Gid + "::/home/me:/bin/sh\n"
	for _, c := range []struct {
		name, group, passwd string
		want                int
	}{
		{"the user's own group", "root:x:0:\n" + me.Username + ":x:" + me.Gid + ":\n", passwd, gid},
		{"the user listed as its member", me.Username + ":x:" + me.Gid + ":" + me.Username + "\n", passwd, gid},
		{"a shared group", "staff:x:" + me.Gid + ":\n", passwd, -1},
		{"another member", me.Username + ":x:" + me.Gid + ":someone\n", passwd, -1},
		{"another user's primary group", me.Username + ":x:" + me.Gid + ":\n", passwd + "someone:x:99999:" + me.Gid + "::/:/bin/sh\n", -1},
		{"no group entry", "root:x:0:\n", passwd, -1},
	} {
		dir := t.TempDir()
		groupFile, passwdFile := filepath.Join(dir, "group"), filepath.Join(dir, "passwd")
		os.WriteFile(groupFile, []byte(c.group), 0o600)
		os.WriteFile(passwdFile, []byte(c.passwd), 0o600)
		if got := findPrivateGroup(groupFile, passwdFile); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if got := findPrivateGroup("/nonexistent/group", "/nonexistent/passwd"); got != -1 {
		t.Errorf("unreadable files: got %d", got)
	}
}

func TestOthersCanWrite(t *testing.T) {
	privateOnce.Do(func() {})
	privateGID = 1000
	defer func() { privateGID = -1; privateOnce = sync.Once{} }()
	for _, c := range []struct {
		mode uint32
		gid  uint32
		want bool
	}{
		{0o644, 20, false},
		{0o664, 1000, false}, // the user's private group
		{0o664, 20, true},    // a shared group
		{0o775, 1000, false},
		{0o646, 1000, true}, // world-writable
		{0o1777, 0, true},
	} {
		if got := OthersCanWrite(c.mode, c.gid); got != c.want {
			t.Errorf("mode %o gid %d: got %v", c.mode, c.gid, got)
		}
	}
	if OthersCanWrite(uint16(0o664), 1000) {
		t.Error("uint16 modes (macOS) must work the same")
	}
}
