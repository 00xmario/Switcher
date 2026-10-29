package provider_test

import (
	"context"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/provider/antigravity"
	"switcher/internal/provider/claude"
	"switcher/internal/provider/codex"
	"switcher/internal/provider/gemini"
)

// Codex and Claude redirect without the state, so their callbacks rely on
// PKCE; Google's flows have no PKCE and must keep proving the state.
func TestBrowserLoginsDeclarePKCE(t *testing.T) {
	for _, tc := range []struct {
		provider provider.Provider
		pkce     bool
	}{
		{codex.New(), true},
		{claude.New(), true},
		{gemini.New(), false},
		{antigravity.New(), false},
	} {
		info, err := tc.provider.LoginStart(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tc.provider.ID(), err)
		}
		if info.Kind != "browser" || info.PKCE != tc.pkce {
			t.Fatalf("%s: kind %q PKCE %v, want browser PKCE %v", tc.provider.ID(), info.Kind, info.PKCE, tc.pkce)
		}
	}
}
