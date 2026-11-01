//go:build !linux

package tray

import "errors"

// Run is the Linux tray; a Mac has the menu bar app instead.
func Run(Options, []byte) error {
	return errors.New("the tray is for Linux; on a Mac, Switcher lives in the menu bar")
}
