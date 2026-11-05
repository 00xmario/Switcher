package fileperm

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestFindPrivateGroup(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	passwd := me.Username + ":x:" + me.Uid + ":" + me.Gid + "::/home/me:/bin/sh\n"
	own := me.Username + ":x:" + me.Gid + ":\n"
	files := "passwd: files systemd\ngroup: files [SUCCESS=merge] systemd\n"
	type setup struct {
		name, group, passwd, nsswitch string
		sssdConf, userdbRecord        bool
		private                       bool
	}
	for _, c := range []setup{
		{"the user's own group", "root:x:0:\n" + own, passwd, files, false, false, true},
		{"the user listed as its member", me.Username + ":x:" + me.Gid + ":" + me.Username + "\n", passwd, files, false, false, true},
		{"the gid written with a leading zero", me.Username + ":x:0" + me.Gid + ":\n", passwd, files, false, false, true},
		{"no nsswitch.conf", own, passwd, "", false, false, true},
		{"sss listed while sssd is not set up", own, passwd, "passwd: files systemd sss\ngroup: files systemd sss\n", false, false, true},
		{"a shared group", "staff:x:" + me.Gid + ":\n", passwd, files, false, false, false},
		{"another member", me.Username + ":x:" + me.Gid + ":someone\n", passwd, files, false, false, false},
		{"another group line with the gid", own + "other:x:" + me.Gid + ":\n", passwd, files, false, false, false},
		{"another user's primary group", own, passwd + "someone:x:99999:" + me.Gid + "::/:/bin/sh\n", files, false, false, false},
		{"no group entry", "root:x:0:\n", passwd, files, false, false, false},
		{"an unreadable gid", own + "odd:x:abc:\n", passwd, files, false, false, false},
		{"an NIS line", own + "+:::\n", passwd, files, false, false, false},
		{"LDAP groups", own, passwd, "passwd: files ldap\ngroup: files ldap\n", false, false, false},
		{"a configured sssd", own, passwd, "passwd: files sss\ngroup: files sss\n", true, false, false},
		{"systemd userdb records", own, passwd, files, false, true, false},
		{"group missing from nsswitch.conf", own, passwd, "passwd: files\n", false, false, false},
	} {
		dir := t.TempDir()
		path := func(name string) string { return filepath.Join(dir, name) }
		os.WriteFile(path("group"), []byte(c.group), 0o600)
		os.WriteFile(path("passwd"), []byte(c.passwd), 0o600)
		if c.nsswitch != "" {
			os.WriteFile(path("nsswitch.conf"), []byte(c.nsswitch), 0o600)
		}
		if c.sssdConf {
			os.WriteFile(path("sssd.conf"), []byte("[sssd]\n"), 0o600)
		}
		userdb := path("userdb")
		if c.userdbRecord {
			os.Mkdir(userdb, 0o700)
			os.WriteFile(filepath.Join(userdb, "someone.membership"), []byte("{}"), 0o600)
		}
		got := findPrivateGroup(path("group"), path("passwd"), path("nsswitch.conf"), path("sssd.conf"), path("sss-pipe"), []string{userdb})
		if (got >= 0) != c.private {
			t.Errorf("%s: got %d, want private=%v", c.name, got, c.private)
		}
	}
}

func TestSignatureNoticesAChangedFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "group")
	os.WriteFile(file, []byte("a:x:1:\n"), 0o600)
	before := signature([]string{file}, nil)
	os.WriteFile(file, []byte("a:x:1:bob\n"), 0o600)
	if signature([]string{file}, nil) == before {
		t.Fatal("a changed group file kept its signature")
	}
}

func TestOthersCanWrite(t *testing.T) {
	if !OthersCanWrite(-1, uint32(0o646), uint32(os.Getgid())) {
		t.Error("world-writable counted as safe")
	}
	if OthersCanWrite(-1, uint32(0o644), 20) || OthersCanWrite(-1, uint16(0o755), 20) {
		t.Error("owner-only write counted as unsafe")
	}
	if !OthersCanWrite(-1, uint32(0o664), 4294967294) {
		t.Error("write for a group that is not the user's private group counted as safe")
	}
	if !privateGroupsApply && !OthersCanWrite(-1, uint16(0o664), uint32(os.Getgid())) {
		t.Error("group write counted as safe outside Linux")
	}
}
