package agentskill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheSkillDescribesItself(t *testing.T) {
	text := string(Content())
	for _, want := range []string{"---\nname: switcher\n", "description: ", "managed-by: switcher",
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
	if st, err := Check(home); err != nil || st.Installed || st.Path != filepath.Join(home, ".agents", "skills", "switcher", "SKILL.md") {
		t.Fatalf("before: %+v %v", st, err)
	}
	st, err := Install(home)
	if err != nil || !st.Installed || !st.Current || !st.Managed {
		t.Fatalf("install: %+v %v", st, err)
	}
	info, _ := os.Stat(st.Path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(Dir(home))
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	// An older copy Switcher wrote is brought up to date.
	old := bytes.Replace(content, []byte("# Switcher"), []byte("# Switcher (old)"), 1)
	os.WriteFile(st.Path, old, 0o644)
	if wrote, err := Refresh(home); !wrote || err != nil {
		t.Fatalf("refresh: %v %v", wrote, err)
	}
	if st, _ := Check(home); !st.Current {
		t.Fatal("not current after a refresh")
	}
	if wrote, _ := Refresh(home); wrote {
		t.Fatal("a current skill was written again")
	}
	if st, err := Remove(home); err != nil || st.Installed {
		t.Fatalf("remove: %+v %v", st, err)
	}
	if _, err := os.Stat(Dir(home)); !os.IsNotExist(err) {
		t.Fatal("the empty folder stayed")
	}
	if wrote, err := Refresh(home); wrote || err != nil {
		t.Fatalf("refresh of a removed skill: %v %v", wrote, err)
	}
}

// A skill the user edited and took the mark out of is theirs: no refresh,
// no removal, but an explicit install replaces it.
func TestAnEditedSkillIsTheUsers(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(Dir(home), 0o755)
	os.WriteFile(Path(home), []byte("---\nname: switcher\n---\nmine\n"), 0o644)
	if wrote, err := Refresh(home); wrote || err != nil {
		t.Fatalf("refresh: %v %v", wrote, err)
	}
	if _, err := Remove(home); err == nil {
		t.Fatal("removed an edited skill")
	}
	if raw, _ := os.ReadFile(Path(home)); string(raw) != "---\nname: switcher\n---\nmine\n" {
		t.Fatal("the edited skill changed")
	}
	if st, err := Install(home); err != nil || !st.Current {
		t.Fatalf("explicit install: %+v %v", st, err)
	}
}

// Something that is not a plain file in the skill's place, such as a link
// to elsewhere, is never written through or replaced.
func TestALinkInTheSkillsPlaceStays(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "elsewhere.md")
	os.WriteFile(target, []byte("keep"), 0o644)
	os.MkdirAll(Dir(home), 0o755)
	if err := os.Symlink(target, Path(home)); err != nil {
		t.Skip(err)
	}
	if _, err := Install(home); err == nil {
		t.Fatal("installed over a link")
	}
	if _, err := Remove(home); err == nil {
		// Check reports it installed but not managed: removal refuses.
		t.Fatal("removed a link")
	}
	if raw, _ := os.ReadFile(target); string(raw) != "keep" {
		t.Fatal("wrote through the link")
	}
	if wrote, _ := Refresh(home); wrote {
		t.Fatal("refreshed through the link")
	}
}
