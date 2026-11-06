package tray

import (
	"errors"
	"os/exec"
)

// appBrowsers show the panel as an app window, without tabs or an address
// bar and about the size of the Mac's menu. They are tried in this order.
var appBrowsers = []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge"}

// PanelCommand is the command that shows the panel at url: the first app
// browser lookPath finds, otherwise the desktop's default browser, which
// opens it as an ordinary page.
func PanelCommand(url string, lookPath func(string) (string, error)) []string {
	for _, name := range appBrowsers {
		if path, err := lookPath(name); err == nil {
			return []string{path, "--app=" + url, "--window-size=420,820"}
		}
	}
	return []string{"xdg-open", url}
}

// OpenPanel shows the panel at url. It returns once the browser started,
// without waiting for its window.
func OpenPanel(url string) error {
	argv := PanelCommand(url, exec.LookPath)
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return errors.New("no browser found to open Switcher in")
	}
	go cmd.Wait()
	return nil
}
