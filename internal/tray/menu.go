package tray

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Item is one menu entry. The menu is rebuilt from State, so this is a plain
// description: the Linux front end draws it, tests read it.
type Item struct {
	// Action names what a click does; empty for a label.
	Action    string
	Title     string
	Tooltip   string
	Checkable bool
	Checked   bool
	Disabled  bool
	Separator bool
	Children  []Item
}

var providerNames = map[string]string{"claude": "Claude", "codex": "Codex", "grok": "Grok", "opencode": "OpenCode",
	"antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"}

var planNames = map[string]string{"claude_max_5x": "Max 5x", "claude_max_20x": "Max 20x", "claude_max": "Max", "claude_pro": "Pro",
	"max": "Max", "pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "go": "Go", "free": "Free",
	"team": "Team", "business": "Business", "enterprise": "Enterprise", "edu": "Edu", "copilot_pro": "Pro",
	"copilot_pro_plus": "Pro+", "copilot_business": "Business", "copilot_enterprise": "Enterprise", "copilot_free": "Free"}

// awakePresets are the keep-awake choices, in minutes; 0 means until off.
var awakePresets = []struct {
	title   string
	minutes int
}{{"15 minutes", 15}, {"30 minutes", 30}, {"1 hour", 60}, {"2 hours", 120}, {"4 hours", 240}, {"8 hours", 480}, {"Until turned off", 0}}

// Status is what the tray knows besides the server's state.
type Status struct {
	Running bool   // the server answered
	Note    string // the last action's outcome, if worth showing
	Busy    string // an action in flight
}

// Build describes the whole menu for a state.
func Build(st State, status Status, now time.Time) []Item {
	var items []Item
	label := func(title string) Item { return Item{Title: title, Disabled: true} }
	sep := Item{Separator: true}
	if !status.Running {
		items = append(items, label("Switcher is not running"))
		if status.Busy != "" {
			items = append(items, label(status.Busy+"…"))
		} else if status.Note != "" {
			items = append(items, label(status.Note))
		}
		items = append(items, Item{Action: "start", Title: "Start Switcher"}, sep,
			Item{Action: "quit-tray", Title: "Close this menu"})
		return items
	}
	items = append(items, label("Switcher "+st.Version))
	if r := st.Remote; r != nil {
		switch {
		case r.Role == "client" && r.Connected:
			items = append(items, label("Using the accounts of "+r.Host))
		case r.Role == "client":
			items = append(items, label("Can't reach "+r.Host+"; requests fail until it is back"))
		case r.Role == "local":
			items = append(items, label("Paired with "+r.Host+", using this computer's accounts"))
		}
	}
	if st.HandedBack {
		items = append(items, label("Handed back: your tools use their providers directly"))
	}
	if status.Busy != "" {
		items = append(items, label(status.Busy+"…"))
	} else if status.Note != "" {
		items = append(items, label(status.Note))
	}

	providers := st.Order
	if len(providers) == 0 {
		providers = []string{"codex", "claude", "grok", "opencode", "antigravity", "gemini", "copilot"}
	}
	shown := 0
	for _, p := range providers {
		if slices.Contains(st.Hidden, p) {
			continue
		}
		var section []Item
		for _, a := range st.Accounts {
			if a.Provider == p {
				section = append(section, accountItem(a, st.HandedBack))
			}
		}
		if len(section) == 0 {
			continue
		}
		items = append(items, sep, label(name(p)))
		items = append(items, section...)
		shown++
	}
	if shown == 0 {
		items = append(items, sep, label("No accounts yet"), Item{Action: "open", Title: "Add an account…"})
	}

	items = append(items, sep)
	if a := st.Awake; a != nil && a.Supported {
		items = append(items, awakeMenu(*a, now))
	}
	if r := st.Remote; r != nil && (r.Role == "client" || r.Role == "local") {
		items = append(items, Item{Action: "use-host", Title: "Use the accounts of " + r.Host, Checkable: true, Checked: r.Role == "client",
			Tooltip: "Off: this computer uses its own accounts and stays paired"})
	}
	items = append(items, Item{Action: "hand-back", Title: "Hand back to the providers", Checkable: true, Checked: st.HandedBack,
		Tooltip: "Undo Switcher's setup so your tools talk to their providers directly, for example before removing Switcher"})
	if a := st.Autostart; a != nil && a.Supported {
		items = append(items, Item{Action: "autostart", Title: "Start at login", Checkable: true, Checked: a.Enabled})
	}

	items = append(items, sep, Item{Action: "refresh", Title: "Refresh usage", Disabled: st.HandedBack},
		Item{Action: "open", Title: "Open dashboard"})
	if st.Update != nil && st.Update.Available {
		items = append(items, Item{Action: "install-update", Title: "Install update " + strings.TrimPrefix(st.Update.Latest, "v")})
	} else {
		items = append(items, Item{Action: "check-update", Title: "Check for updates"})
	}
	items = append(items, sep, Item{Action: "quit", Title: "Quit Switcher"})
	return items
}

func name(provider string) string {
	if n := providerNames[provider]; n != "" {
		return n
	}
	return provider
}

func accountItem(a Account, handedBack bool) Item {
	parts := []string{a.Email}
	if plan := planNames[a.Plan]; plan != "" {
		parts = append(parts, plan)
	} else if a.Plan != "" {
		parts = append(parts, a.Plan)
	}
	if a.Usage != nil {
		for _, w := range a.Usage.Windows {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", w.Label, w.UsedPercent))
		}
	}
	if a.Health != nil && a.Health.Condition == "needs_relogin" {
		parts = append(parts, "needs relogin")
	}
	tooltip := "Use this account"
	if a.NativeSwitchAvailable {
		tooltip = "Use this account in Claude Code and for Claude"
	}
	return Item{Action: "activate:" + a.ID, Title: strings.Join(parts, " · "), Tooltip: tooltip,
		Checkable: true, Checked: a.Selected(), Disabled: handedBack}
}

func awakeMenu(a Awake, now time.Time) Item {
	title := "Keep awake"
	switch {
	case a.Active && a.Until > 0:
		left := time.Unix(a.Until, 0).Sub(now).Round(time.Minute)
		title = fmt.Sprintf("Keep awake: until %s (%s left)", time.Unix(a.Until, 0).Format("15:04"), compact(left))
	case a.Active:
		title = "Keep awake: until turned off"
	}
	children := []Item{{Action: "awake:off", Title: "Off", Checkable: true, Checked: !a.Active}}
	for _, p := range awakePresets {
		children = append(children, Item{Action: fmt.Sprintf("awake:%d", p.minutes), Title: p.title})
	}
	children = append(children, Item{Separator: true},
		Item{Action: "awake-display", Title: "Keep the display on", Checkable: true, Checked: !a.AllowDisplaySleep})
	return Item{Title: title, Children: children}
}

func compact(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}
