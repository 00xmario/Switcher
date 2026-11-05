package tray

import "embed"

// The provider marks the menu shows beside each provider's name: the Mac
// menu's own (build/macos/logos), drawn at the 32 pixels a menu icon needs
// on a HiDPI screen. Grok, Copilot and OpenCode, which follow the theme on
// the Mac, are grey, so one picture reads on light and dark menus.
//
//go:embed logos/*.png
var logos embed.FS

// logo returns the PNG for a provider, or nil for one without a mark.
func logo(provider string) []byte {
	data, err := logos.ReadFile("logos/" + provider + ".png")
	if err != nil {
		return nil
	}
	return data
}
