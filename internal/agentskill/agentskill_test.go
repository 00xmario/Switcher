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
	st, err := Install(home, "", false)
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
	if _, err := Install(home, "", false); err != nil {
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
	if _, err := Install(home, "", false); err == nil {
		t.Fatal("an install replaced an edited skill without being asked")
	}
	if st, err := Install(home, "", true); err != nil || !st.Current || !st.Managed {
		t.Fatalf("install asked to replace: %+v %v", st, err)
	}
}

// A skill file some other tool or the user put there before Switcher is
// kept by a plain install, as the install script runs it.
func TestAnUnownedSkillIsKept(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(Dir(home), 0o755)
	os.WriteFile(Path(home), []byte("---\nname: switcher\n---\nsomeone else's\n"), 0o644)
	if _, err := Install(home, "", false); err == nil {
		t.Fatal("replaced a skill Switcher did not write")
	}
	if raw, _ := os.ReadFile(Path(home)); !strings.Contains(string(raw), "someone else's") {
		t.Fatal("the skill changed")
	}
}

// This release's own bytes count as Switcher's even without the record, as
// after a crash between writing the skill and its record.
func TestThisReleasesSkillIsSwitchersWithoutItsRecord(t *testing.T) {
	home := t.TempDir()
	if _, err := Install(home, "", false); err != nil {
		t.Fatal(err)
	}
	os.Remove(stampPath(home))
	if st, _ := Check(home, ""); !st.Managed {
		t.Fatalf("status: %+v", st)
	}
	if _, err := Remove(home, ""); err != nil {
		t.Fatal(err)
	}
}

// With ~/.claude itself a link into dotfiles, a relative link would lead
// nowhere: the link is absolute and works.
func TestTheClaudeLinkWorksThroughALinkedClaudeFolder(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles", "claude")
	os.MkdirAll(dotfiles, 0o755)
	if err := os.Symlink(dotfiles, filepath.Join(home, ".claude")); err != nil {
		t.Skip(err)
	}
	st, err := Install(home, "", false)
	if err != nil || !st.ClaudeCode {
		t.Fatalf("install: %+v %v", st, err)
	}
	if target, _ := os.Readlink(st.ClaudeLink); target != Dir(home) {
		t.Fatalf("link target %q", target)
	}
	if st, err := Remove(home, ""); err != nil || st.ClaudeCode {
		t.Fatalf("remove: %+v %v", st, err)
	}
	if _, err := os.Lstat(st.ClaudeLink); !os.IsNotExist(err) {
		t.Fatal("the link stayed")
	}
	// A link that reads like Switcher's but, through the linked folder,
	// leads elsewhere is the user's: install and remove leave it.
	os.MkdirAll(filepath.Join(dotfiles, "skills"), 0o755)
	os.Symlink("../../.agents/skills/switcher", st.ClaudeLink)
	if st, _ := Install(home, "", false); st.ClaudeCode {
		t.Fatal("a link leading elsewhere counts as working")
	}
	Remove(home, "")
	if _, err := os.Lstat(st.ClaudeLink); err != nil {
		t.Fatal("removed a link that is not Switcher's")
	}
	// Switcher's own link that leads nowhere is made again.
	os.Remove(st.ClaudeLink)
	os.Symlink(Dir(home), st.ClaudeLink)
	os.RemoveAll(filepath.Join(home, ".agents"))
	if _, err := os.Stat(st.ClaudeLink); err == nil {
		t.Fatal("the link should lead nowhere now")
	}
	if st, err := Install(home, "", false); err != nil || !st.ClaudeCode {
		t.Fatalf("own link not made again: %+v %v", st, err)
	}
}

// CLAUDE_CONFIG_DIR moves Claude Code's skills folder; the link follows.
func TestClaudeConfigDirIsHonoured(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(t.TempDir(), "claude")
	st, err := Install(home, config, false)
	if err != nil || !st.ClaudeCode || st.ClaudeLink != filepath.Join(config, "skills", "switcher") {
		t.Fatalf("install: %+v %v", st, err)
	}
	if target, _ := os.Readlink(st.ClaudeLink); target != Dir(home) {
		t.Fatalf("a link outside home must be absolute: %q", target)
	}
}
