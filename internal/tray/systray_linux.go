//go:build linux

package tray

import (
	"context"
	"errors"
	"log"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

// Run shows the tray icon until Quit Switcher or Close this menu is chosen.
// It needs the desktop session's D-Bus and a StatusNotifierItem host: KDE,
// Ubuntu's GNOME (AppIndicator), Cinnamon, XFCE and most others have one.
func Run(opt Options, icon []byte) error {
	conn, err := dbus.SessionBus()
	if err != nil {
		return errors.New("no desktop session to show a tray icon in; use the dashboard instead: switcher open")
	}
	var owner string
	if conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, "org.kde.StatusNotifierWatcher").Store(&owner) != nil {
		// The icon appears once a host starts; until then the dashboard works.
		log.Print("tray: this desktop shows no tray icons yet; on GNOME, turn on the AppIndicator extension")
	}
	ui := &systrayUI{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ui.ctrl = New(opt, ui)
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTitle("Switcher")
		systray.SetTooltip("Switcher")
		go ui.ctrl.Run(ctx)
	}, cancel)
	return nil
}

type systrayUI struct {
	conn *dbus.Conn
	ctrl *Controller
}

func (u *systrayUI) Render(items []Item) {
	systray.ResetMenu()
	for _, item := range items {
		u.add(nil, item)
	}
}

func (u *systrayUI) add(parent *systray.MenuItem, item Item) {
	if item.Separator {
		if parent == nil {
			systray.AddSeparator()
		} else {
			parent.AddSeparator()
		}
		return
	}
	var mi *systray.MenuItem
	switch {
	case parent == nil && item.Checkable:
		mi = systray.AddMenuItemCheckbox(item.Title, item.Tooltip, item.Checked)
	case parent == nil:
		mi = systray.AddMenuItem(item.Title, item.Tooltip)
	case item.Checkable:
		mi = parent.AddSubMenuItemCheckbox(item.Title, item.Tooltip, item.Checked)
	default:
		mi = parent.AddSubMenuItem(item.Title, item.Tooltip)
	}
	if item.Disabled {
		mi.Disable()
	}
	for _, child := range item.Children {
		u.add(mi, child)
	}
	if item.Action == "" {
		return
	}
	go func(action string) {
		// The channel closes when the menu is rebuilt.
		for range mi.ClickedCh {
			go u.ctrl.Click(context.Background(), action)
		}
	}(item.Action)
}

// Notify shows a desktop notification through the freedesktop service.
func (u *systrayUI) Notify(title, body string) {
	call := u.conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").Call(
		"org.freedesktop.Notifications.Notify", 0, "Switcher", uint32(0), "", title, body, []string{}, map[string]dbus.Variant{}, int32(-1))
	if call.Err != nil {
		log.Printf("tray: notification failed: %v", call.Err)
	}
}

func (u *systrayUI) Quit() { systray.Quit() }
