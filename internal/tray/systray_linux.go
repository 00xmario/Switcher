//go:build linux

package tray

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"

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
	ui := &systrayUI{conn: conn, icon: icon}
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
	icon []byte // Switcher's own, for items with the "switcher" icon

	mu    sync.Mutex
	drawn []Item  // what the menu shows
	nodes []*node // its items, in the same shape
}

// node is one drawn menu item; a separator has none.
type node struct {
	item *systray.MenuItem
	// action is read by the item's click loop, so an item changed in place
	// runs its new action.
	action   atomic.Value
	children []*node
}

// Render draws the menu. A menu of the same shape changes in place and
// keeps its items: rebuilding it while it is open closes or flickers it on
// GNOME.
func (u *systrayUI) Render(items []Item) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.nodes != nil && sameShape(u.drawn, items) {
		u.update(u.nodes, u.drawn, items)
	} else {
		systray.ResetMenu()
		u.nodes = u.build(nil, items)
	}
	u.drawn = items
}

func (u *systrayUI) build(parent *systray.MenuItem, items []Item) []*node {
	nodes := make([]*node, len(items))
	for i, item := range items {
		if item.Separator {
			if parent == nil {
				systray.AddSeparator()
			} else {
				parent.AddSeparator()
			}
			continue
		}
		title := menuTitle(item.Title)
		var mi *systray.MenuItem
		switch {
		case parent == nil && item.Checkable:
			mi = systray.AddMenuItemCheckbox(title, item.Tooltip, item.Checked)
		case parent == nil:
			mi = systray.AddMenuItem(title, item.Tooltip)
		case item.Checkable:
			mi = parent.AddSubMenuItemCheckbox(title, item.Tooltip, item.Checked)
		default:
			mi = parent.AddSubMenuItem(title, item.Tooltip)
		}
		if item.Disabled {
			mi.Disable()
		}
		if picture := u.picture(item.Icon); picture != nil {
			mi.SetIcon(picture)
		}
		n := &node{item: mi}
		n.action.Store(item.Action)
		n.children = u.build(mi, item.Children)
		nodes[i] = n
		go u.listen(n)
	}
	return nodes
}

// update changes the drawn items that differ; was and now have the same
// shape as nodes.
func (u *systrayUI) update(nodes []*node, was, now []Item) {
	for i, item := range now {
		n, old := nodes[i], was[i]
		if n == nil {
			continue
		}
		n.action.Store(item.Action)
		if item.Title != old.Title {
			n.item.SetTitle(menuTitle(item.Title))
		}
		if item.Disabled != old.Disabled {
			if item.Disabled {
				n.item.Disable()
			} else {
				n.item.Enable()
			}
		}
		if item.Checked != old.Checked {
			if item.Checked {
				n.item.Check()
			} else {
				n.item.Uncheck()
			}
		}
		if item.Icon != old.Icon {
			if picture := u.picture(item.Icon); picture != nil {
				n.item.SetIcon(picture)
			}
		}
		u.update(n.children, old.Children, item.Children)
	}
}

func (u *systrayUI) listen(n *node) {
	// The channel closes when the menu is rebuilt.
	for range n.item.ClickedCh {
		if action, _ := n.action.Load().(string); action != "" {
			go u.ctrl.Click(context.Background(), action)
		}
	}
}

func (u *systrayUI) picture(name string) []byte {
	if name == "switcher" {
		return u.icon
	}
	return logo(name)
}

// menuTitle escapes a title for dbusmenu, which reads "_" as an access-key
// marker; "__" is a plain one, so first_last@example.com keeps its
// underscore.
func menuTitle(title string) string {
	return strings.ReplaceAll(title, "_", "__")
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
