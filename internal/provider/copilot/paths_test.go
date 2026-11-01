package copilot

import (
	"os"
	"path/filepath"
	"testing"
)

// The Copilot CLI keeps its tokens in Application Support on a Mac and in
// the XDG config directory on Linux.
func TestCopilotCredentialsPathPerPlatform(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got, want := copilotAppsPath("darwin", "/x"), filepath.Join(home, "Library", "Application Support", "github-copilot", "apps.json"); got != want {
		t.Errorf("darwin: %s", got)
	}
	if got, want := copilotAppsPath("linux", ""), filepath.Join(home, ".config", "github-copilot", "apps.json"); got != want {
		t.Errorf("linux default: %s", got)
	}
	if got := copilotAppsPath("linux", "/srv/config"); got != "/srv/config/github-copilot/apps.json" {
		t.Errorf("linux XDG_CONFIG_HOME: %s", got)
	}
	if got, want := copilotAppsPath("linux", "relative"), filepath.Join(home, ".config", "github-copilot", "apps.json"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored: %s", got)
	}
}
