// Package agentskill installs Switcher's skill for AI agents: a SKILL.md in
// ~/.agents/skills/switcher that tells coding agents what Switcher is and how
// to drive it with the switcher command, so a request such as "keep 10% on
// this account and switch automatically" becomes the right command. The
// skill is part of this binary; an installed copy that Switcher wrote is kept
// up to date when Switcher updates, and one the user edited is left alone.
package agentskill

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var content []byte

// managedMark is in the skill's frontmatter: a copy that carries it is
// Switcher's to update and remove.
var managedMark = []byte("managed-by: switcher")

// Content is the skill as this release writes it.
func Content() []byte { return bytes.Clone(content) }

// Dir is where the skill lives under a home directory.
func Dir(home string) string { return filepath.Join(home, ".agents", "skills", "switcher") }

// Path is the skill file under a home directory.
func Path(home string) string { return filepath.Join(Dir(home), "SKILL.md") }

// Status says whether the skill is installed and in what state.
type Status struct {
	Installed bool   `json:"installed"`
	Current   bool   `json:"current"` // the installed copy is this release's
	Managed   bool   `json:"managed"` // Switcher wrote it; it updates and removes it
	Path      string `json:"path"`
}

// maxSkill bounds how much of an installed file is read.
const maxSkill = 1 << 20

func read(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		// Something else under that name (a link, a directory) is the
		// user's: Switcher neither reads nor replaces it.
		return nil, true, errNotOurs
	}
	if info.Size() > maxSkill {
		return nil, true, nil
	}
	raw, err := os.ReadFile(path)
	return raw, true, err
}

var errNotOurs = errors.New("the skill's place holds something that is not a file Switcher wrote")

// Check reports the skill's state under home.
func Check(home string) (Status, error) {
	st := Status{Path: Path(home)}
	raw, exists, err := read(st.Path)
	st.Installed = exists
	if err != nil {
		if errors.Is(err, errNotOurs) {
			return st, nil
		}
		return st, err
	}
	st.Managed = bytes.Contains(raw, managedMark)
	st.Current = bytes.Equal(raw, content)
	return st, nil
}

// Install writes this release's skill under home, replacing an earlier copy
// atomically. It refuses to replace anything that is not a regular file.
func Install(home string) (Status, error) {
	if home == "" {
		return Status{}, errors.New("no home directory")
	}
	path := Path(home)
	if _, _, err := read(path); err != nil {
		return Status{Path: path, Installed: true}, err
	}
	if err := os.MkdirAll(Dir(home), 0o755); err != nil {
		return Status{Path: path}, err
	}
	tmp, err := os.CreateTemp(Dir(home), ".SKILL-*.md")
	if err != nil {
		return Status{Path: path}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return Status{Path: path}, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return Status{Path: path}, err
	}
	if err := tmp.Close(); err != nil {
		return Status{Path: path}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Status{Path: path}, err
	}
	return Check(home)
}

// Remove deletes the skill when Switcher wrote it, and its folder when that
// is then empty. A copy the user edited (without the mark) stays.
func Remove(home string) (Status, error) {
	st, err := Check(home)
	if err != nil || !st.Installed {
		return st, err
	}
	if !st.Managed {
		return st, errors.New("the installed skill was changed by hand, so Switcher leaves it; delete " + st.Path + " yourself")
	}
	if err := os.Remove(st.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	_ = os.Remove(Dir(home)) // only when empty
	return Check(home)
}

// Refresh brings an installed skill that Switcher wrote up to this release,
// as when Switcher updates. It does nothing when the skill is not installed,
// already current, or was edited by hand. It reports whether it wrote.
func Refresh(home string) (bool, error) {
	st, err := Check(home)
	if err != nil || !st.Installed || !st.Managed || st.Current {
		return false, err
	}
	_, err = Install(home)
	return err == nil, err
}
