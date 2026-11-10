package agentskill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheSkillDescribesItself(t *testing.T) {
	text := string(Content())
	for _, want := range []string{"---\nname: switcher\n", "description: ",
		"switcher limit <account> 10", "switcher accounts --json", "`switcher limit <account> 70` (used 30% = 70% left)"} {
		if !strings.Contains(text, want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
	// Agents read the frontmatter: it must open the file and close.
	if !strings.HasPrefix(text, "---\n") || strings.Count(text[:strings.Index(text, "\n# Switcher")], "---\n") != 2 {
		t.Fatal("the frontmatter is not closed before the body")
	}
}

func TestInstallUpdateAndRemove(t *testing.T) {
	home := t.TempDir()
	if st, err := Check(home, ""); err != nil || st.Installed || st.ClaudeCode || st.Path != filepath.Join(home, ".agents", "skills", "switcher", "SKILL.md") {
		t.Fatalf("before: %+v %v", st, err)
	}
	st, err := Install(home, "")
	if err != nil || !st.Installed || !st.Current || !st.Managed || !st.ClaudeCode {
		t.Fatalf("install: %+v %v", st, err)
	}
	info, _ := os.Stat(st.Path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	// Claude Code finds it through a relative link, as the other shared
	// skills are linked.
	if target, err := os.Readlink(filepath.Join(home, ".claude", "skills", "switcher")); err != nil || target != "../../.agents/skills/switcher" {
		t.Fatalf("Claude Code link: %q %v", target, err)
	}
	// An older copy Switcher wrote is brought up to date.
	older := []byte("---\nname: switcher\n---\n# Switcher (old)\n")
	os.WriteFile(st.Path, older, 0o644)
	os.WriteFile(stampPath(home), []byte(digest(older)+"\n"), 0o644)
	if wrote, err := Refresh(home, ""); !wrote || err != nil {
		t.Fatalf("refresh: %v %v", wrote, err)
	}
	if st, _ := Check(home, ""); !st.Current || !st.Managed {
		t.Fatalf("after a refresh: %+v", st)
	}
	if wrote, _ := Refresh(home, ""); wrote {
		t.Fatal("a current skill was written again")
	}
	if st, err := Remove(home, ""); err != nil || st.Installed || st.ClaudeCode {
		t.Fatalf("remove: %+v %v", st, err)
	}
	if _, err := os.Lstat(Dir(home)); !os.IsNotExist(err) {
		t.Fatal("the empty folder stayed")
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "switcher")); !os.IsNotExist(err) {
		t.Fatal("the Claude Code link stayed")
	}
	if wrote, err := Refresh(home, ""); wrote || err != nil {
		t.Fatalf("refresh of a removed skill: %v %v", wrote, err)
	}
}

// Any edit makes the skill the user's, frontmatter kept or not: no refresh,
// no removal; only an explicit install replaces it.
func TestAnEditedSkillIsTheUsers(t *testing.T) {
	home := t.TempDir()
	if _, err := Install(home, ""); err != nil {
		t.Fatal(err)
	}
	edited := append(Content(), []byte("\nMy own note.\n")...)
	os.WriteFile(Path(home), edited, 0o644)
	if st, _ := Check(home, ""); st.Managed || st.Current {
		t.Fatalf("an edited copy counts as Switcher's: %+v", st)
	}
	if wrote, err := Refresh(home, ""); wrote || err != nil {
		t.Fatalf("refresh: %v %v", wrote, err)
	}
	if _, err := Remove(home, ""); err == nil {
		t.Fatal("removed an edited skill")
	}
	if raw, _ := os.ReadFile(Path(home)); string(raw) != string(edited) {
		t.Fatal("the edited skill changed")
	}
	if st, err := Install(home, ""); err != nil || !st.Current || !st.Managed {
		t.Fatalf("explicit install: %+v %v", st, err)
	}
}

// Something that is not a plain file in the skill's place, such as a link
// to elsewhere, is never written through, replaced or removed.
func TestALinkInTheSkillsPlaceStays(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "elsewhere.md")
	os.WriteFile(target, []byte("keep"), 0o644)
	os.MkdirAll(Dir(home), 0o755)
	if err := os.Symlink(target, Path(home)); err != nil {
		t.Skip(err)
	}
	if st, _ := Check(home, ""); !st.Blocked || st.Managed {
		t.Fatalf("status: %+v", st)
	}
	if _, err := Install(home, ""); err == nil {
		t.Fatal("installed over a link")
	}
	if _, err := Remove(home, ""); err == nil {
		t.Fatal("removed a link")
	}
	if raw, _ := os.ReadFile(target); string(raw) != "keep" {
		t.Fatal("wrote through the link")
	}
	if wrote, _ := Refresh(home, ""); wrote {
		t.Fatal("refreshed through the link")
	}
}

// A skills folder kept elsewhere (dotfiles) and linked in stays linked when
// the skill is removed; a Claude Code entry the user made stays too.
func TestTheUsersLinksStay(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles", "switcher")
	os.MkdirAll(dotfiles, 0o755)
	os.MkdirAll(filepath.Dir(Dir(home)), 0o755)
	if err := os.Symlink(dotfiles, Dir(home)); err != nil {
		t.Skip(err)
	}
	mine := filepath.Join(home, ".claude", "skills", "switcher")
	os.MkdirAll(mine, 0o755)
	os.WriteFile(filepath.Join(mine, "SKILL.md"), []byte("mine"), 0o644)
	if st, err := Install(home, ""); err != nil || !st.Current || !st.ClaudeCode {
		t.Fatalf("install: %+v %v", st, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(mine, "SKILL.md")); string(raw) != "mine" {
		t.Fatal("the user's Claude Code skill was replaced")
	}
	if _, err := Remove(home, ""); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(Dir(home)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the linked skills folder was removed")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("the user's Claude Code entry was removed")
	}
}

// CLAUDE_CONFIG_DIR moves Claude Code's skills folder; the link follows.
func TestClaudeConfigDirIsHonoured(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(t.TempDir(), "claude")
	st, err := Install(home, config)
	if err != nil || !st.ClaudeCode || st.ClaudeLink != filepath.Join(config, "skills", "switcher") {
		t.Fatalf("install: %+v %v", st, err)
	}
	if target, _ := os.Readlink(st.ClaudeLink); target != Dir(home) {
		t.Fatalf("a link outside home must be absolute: %q", target)
	}
}
