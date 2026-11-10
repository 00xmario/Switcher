// Package agentskill installs Switcher's skill for AI agents: a SKILL.md in
// ~/.agents/skills/switcher that tells coding agents what Switcher is and how
// to drive it with the switcher command, so a request such as "keep 10% on
// this account and switch automatically" becomes the right command. Claude
// Code reads its own skills folder, so ~/.claude/skills/switcher links to
// that copy, as the other shared skills there do.
//
// The skill is part of this binary. Switcher records the hash of the copy it
// wrote beside it; a copy that still matches is Switcher's, brought up to
// date when Switcher updates and removed on request. A copy the user edited
// no longer matches and is left alone: it is replaced only when asked, and
// anything but a plain file never is.
package agentskill

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

//go:embed SKILL.md
var content []byte

// Content is the skill as this release writes it.
func Content() []byte { return bytes.Clone(content) }

// Dir is where the skill lives under a home directory.
func Dir(home string) string { return filepath.Join(home, ".agents", "skills", "switcher") }

// Path is the skill file under a home directory.
func Path(home string) string { return filepath.Join(Dir(home), "SKILL.md") }

// stampPath holds the hash of the copy Switcher wrote.
func stampPath(home string) string { return filepath.Join(Dir(home), ".switcher-sha256") }

// ClaudeLink is the link Claude Code finds the skill through: in
// $CLAUDE_CONFIG_DIR/skills when that is set, otherwise ~/.claude/skills.
func ClaudeLink(home, claudeConfigDir string) string {
	if claudeConfigDir != "" {
		return filepath.Join(claudeConfigDir, "skills", "switcher")
	}
	return filepath.Join(home, ".claude", "skills", "switcher")
}

// Status says whether the skill is installed and in what state.
type Status struct {
	Installed bool   `json:"installed"`
	Current   bool   `json:"current"` // the installed copy is this release's
	Managed   bool   `json:"managed"` // Switcher wrote it unchanged; it updates and removes it
	Blocked   bool   `json:"blocked"` // something other than a file holds its place
	Path      string `json:"path"`
	// ClaudeCode says Claude Code finds the skill: its skills folder links
	// to it (or holds it some other way the user set up).
	ClaudeCode bool   `json:"claude_code"`
	ClaudeLink string `json:"claude_link"`
}

// maxSkill bounds how much of an installed file is read.
const maxSkill = 1 << 20

var errBlocked = errors.New("something other than the skill file is in its place; Switcher leaves it")

// read returns a regular file's bytes; exists says something is there.
func read(path string) (raw []byte, exists bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, errBlocked
	}
	if info.Size() > maxSkill {
		return nil, true, nil
	}
	raw, err = os.ReadFile(path)
	return raw, true, err
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Check reports the skill's state.
func Check(home, claudeConfigDir string) (Status, error) {
	st := Status{Path: Path(home), ClaudeLink: ClaudeLink(home, claudeConfigDir)}
	raw, exists, err := read(st.Path)
	st.Installed = exists
	switch {
	case errors.Is(err, errBlocked):
		st.Blocked = true
	case err != nil:
		return st, err
	case exists:
		stamp, _, _ := read(stampPath(home))
		st.Current = bytes.Equal(raw, content)
		// This release's own bytes are Switcher's even without the record,
		// as after a crash between writing the skill and its record.
		st.Managed = raw != nil && (st.Current || strings.TrimSpace(string(stamp)) == digest(raw))
	}
	if info, err := os.Stat(filepath.Join(st.ClaudeLink, "SKILL.md")); err == nil && info.Mode().IsRegular() {
		st.ClaudeCode = true
	}
	return st, nil
}

// writeAtomic puts data at path through a temporary file in its folder, so
// a link in path's place is replaced rather than written through.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".switcher-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrNotOurs refuses to replace a skill file Switcher did not write, or one
// that was edited since, unless the caller asks to replace it.
var ErrNotOurs = errors.New("a skill file that Switcher did not write, or that was edited, is in its place")

// Install writes this release's skill, replacing a copy Switcher wrote, and
// links it for Claude Code. A file Switcher did not write, or one edited
// since, is replaced only with replace; anything but a plain file never is.
func Install(home, claudeConfigDir string, replace bool) (Status, error) {
	if home == "" {
		return Status{}, errors.New("no home directory")
	}
	st, err := Check(home, claudeConfigDir)
	switch {
	case err != nil:
		return st, err
	case st.Blocked:
		return st, errBlocked
	case st.Installed && !st.Managed && !replace:
		return st, ErrNotOurs
	}
	if err := os.MkdirAll(Dir(home), 0o755); err != nil {
		return Status{Path: Path(home)}, err
	}
	if err := writeAtomic(Path(home), content); err != nil {
		return Status{Path: Path(home)}, err
	}
	if err := writeAtomic(stampPath(home), []byte(digest(content)+"\n")); err != nil {
		return Status{Path: Path(home)}, err
	}
	linkClaude(home, claudeConfigDir)
	return Check(home, claudeConfigDir)
}

// linkClaude links Claude Code's skills folder to the skill, relative where
// both are under home, the way the other shared skills are linked. A name
// already taken there is left alone.
func linkClaude(home, claudeConfigDir string) {
	link := ClaudeLink(home, claudeConfigDir)
	if _, err := os.Lstat(link); err == nil {
		// Taken. Only a link of Switcher's own that leads nowhere, as after
		// its folder moved, is made again.
		if _, err := os.Stat(link); err == nil || !ownsClaudeLink(home, link) {
			return
		}
		if err := os.Remove(link); err != nil {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return
	}
	_ = os.Symlink(linkTarget(home, filepath.Dir(link)), link)
}

// linkTarget is how a link in dir leads to the skill's folder: relative,
// like the other shared skills, when both sit under home without links in
// between (the system resolves a relative link from where the link really
// is), otherwise absolute.
func linkTarget(home, dir string) string {
	target := Dir(home)
	realDir, err1 := filepath.EvalSymlinks(dir)
	realParent, err2 := filepath.EvalSymlinks(filepath.Dir(target))
	realHome, err3 := filepath.EvalSymlinks(home)
	if err1 != nil || err2 != nil || err3 != nil {
		return target
	}
	inside := func(p string) bool { return strings.HasPrefix(p, realHome+string(filepath.Separator)) }
	// The relative path must come out the same whether read from the paths
	// as written or as resolved; links in between make them differ.
	written, err4 := filepath.Rel(dir, target)
	resolved, err5 := filepath.Rel(realDir, filepath.Join(realParent, filepath.Base(target)))
	if err4 != nil || err5 != nil || written != resolved || !inside(realDir) || !inside(realParent) {
		return target
	}
	return written
}

// ownsClaudeLink reports whether Claude Code's entry is the link Switcher
// makes: a symlink that leads to the skill's folder.
func ownsClaudeLink(home, link string) bool {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	raw, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if raw == linkTarget(home, filepath.Dir(link)) || raw == Dir(home) {
		return true
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(link), raw)
	}
	return filepath.Clean(raw) == filepath.Clean(Dir(home))
}

// Remove deletes the skill when Switcher wrote it unchanged, its record, the
// Claude Code link it made, and its folder when that is then empty and a
// real folder. A copy the user edited stays.
func Remove(home, claudeConfigDir string) (Status, error) {
	st, err := Check(home, claudeConfigDir)
	if err != nil {
		return st, err
	}
	switch {
	case st.Blocked:
		return st, errBlocked
	case st.Installed && !st.Managed:
		return st, errors.New("the installed skill was changed by hand, so Switcher leaves it; delete " + st.Path + " yourself")
	}
	if ownsClaudeLink(home, st.ClaudeLink) {
		_ = os.Remove(st.ClaudeLink)
	}
	if st.Installed {
		if err := os.Remove(st.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return st, err
		}
		_ = os.Remove(stampPath(home))
		if info, err := os.Lstat(Dir(home)); err == nil && info.IsDir() {
			_ = os.Remove(Dir(home)) // only when empty
		}
	}
	return Check(home, claudeConfigDir)
}

// Refresh brings an installed skill that Switcher wrote up to this release,
// as when Switcher updates, and links it for Claude Code if that link is
// missing. It does nothing when the skill is not installed or was edited. It
// reports whether it wrote the skill.
func Refresh(home, claudeConfigDir string) (bool, error) {
	st, err := Check(home, claudeConfigDir)
	if err != nil || !st.Installed || !st.Managed {
		return false, err
	}
	if st.Current {
		return false, nil
	}
	_, err = Install(home, claudeConfigDir, false)
	return err == nil, err
}
