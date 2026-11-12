package tray

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Item is one menu entry. The menu is rebuilt from State, so this is a plain
// description: the Linux front end draws it, tests read it.
type Item struct {
	// Action names what a click does; empty for a label.
	Action  string
	Title   string
	Tooltip string
	// Icon names the picture beside the title: a provider's logo, or
	// "switcher" for Switcher's own.
	Icon      string
	Checkable bool
	Checked   bool
	Disabled  bool
	Separator bool
	Children  []Item
}

// detail indents the lines under an account (its usage and notes), so they
// read as part of the account above, like the lines of a Mac menu card.
const detail = "    "

var providerNames = map[string]string{"claude": "Claude", "codex": "Codex", "grok": "Grok", "opencode": "OpenCode",
	"antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"}

var planNames = map[string]string{"claude_max_5x": "Max", "claude_max_20x": "Ultra", "claude_max": "Max", "claude_pro": "Pro",
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
	// The header: the panel, which has the Mac menu's look, then the
	// version and what changes how the accounts are used.
	items = append(items, Item{Action: "panel", Title: "Open Switcher…", Icon: "switcher"},
		label("Switcher "+st.Version))
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

	bars := st.MenuUsageBars == nil || *st.MenuUsageBars
	shown := 0
	for _, p := range providerOrder(st) {
		var section []Item
		for _, a := range st.Accounts {
			if a.Provider == p {
				section = append(section, accountItems(a, st.HandedBack, bars, now)...)
			}
		}
		if len(section) == 0 {
			continue
		}
		items = append(items, sep, Item{Title: name(p), Icon: p, Disabled: true})
		items = append(items, section...)
		shown++
	}
	if shown == 0 {
		items = append(items, sep, label("No accounts yet"), Item{Action: "open", Title: "Add an account…"})
	}

	items = append(items, sep)
	if limits, ok := limitsMenu(st); ok {
		items = append(items, limits)
	}
	if a := st.Awake; a != nil && a.Supported {
		items = append(items, awakeMenu(*a, now))
	}
	if r := st.Remote; r != nil && (r.Role == "client" || r.Role == "local") {
		// Handed back, the host waits until Switcher is set up again.
		tip := "Off: this computer uses its own accounts and stays paired"
		if st.HandedBack {
			tip = "Your tools are handed back to their providers; set Switcher up again to use " + r.Host
		}
		items = append(items, Item{Action: "use-host", Title: "Use the accounts of " + r.Host, Checkable: true,
			Checked: r.Role == "client" && !st.HandedBack, Disabled: st.HandedBack, Tooltip: tip})
	}
	if r := st.Remote; r != nil && r.Role == "client" && r.HostUpdate != nil && r.HostUpdate.Remote && r.HostUpdate.Latest != "" {
		u, release := r.HostUpdate, strings.TrimPrefix(r.HostUpdate.Latest, "v")
		switch {
		case u.Install != nil:
			items = append(items, label("Updating "+r.Host+" to "+release+"…"))
		case u.Available:
			// Updating the host restarts it for every computer that uses
			// it, so it takes a second click.
			items = append(items, Item{Title: "Update " + r.Host + " to " + release, Tooltip: r.Host + " runs Switcher " + u.Current,
				Children: []Item{label(r.Host + " restarts; nothing changes on this computer"), {Action: "host-update", Title: "Update now"}}})
		}
	}
	// Either way the hand-back changes how this computer's tools are routed,
	// so it takes a second click, as the Mac menu and the dashboard ask
	// first. Only this computer's Switcher changes, never a paired host's.
	only := "Only this computer's Switcher changes"
	if r := st.Remote; r != nil && r.Host != "" && (r.Role == "client" || r.Role == "local") {
		only += "; " + r.Host + "'s stays as it is"
	}
	title := "Hand back to the providers on this computer"
	if st.HandedBack {
		items = append(items, Item{Title: "✓ " + title, Tooltip: "Set Switcher up again on this computer",
			Children: []Item{label(only), label("Your tools go through Switcher again"), {Action: "hand-back:off", Title: "Set up again now"}}})
	} else {
		items = append(items, Item{Title: title,
			Tooltip:  "Undo Switcher's setup so this computer's tools talk to their providers directly, for example before removing Switcher",
			Children: []Item{label(only), label("Your tools will use their providers directly"), {Action: "hand-back:on", Title: "Hand back now"}}})
	}
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

// providerOrder is the dashboard's provider order without hidden providers;
// providers missing from the order come last, as on the phone page.
func providerOrder(st State) []string {
	order := st.Order
	if len(order) == 0 {
		order = []string{"codex", "claude", "grok", "opencode", "antigravity", "gemini", "copilot"}
	}
	order = slices.Clone(order)
	for _, a := range st.Accounts {
		if !slices.Contains(order, a.Provider) {
			order = append(order, a.Provider)
		}
	}
	return slices.DeleteFunc(order, func(p string) bool { return slices.Contains(st.Hidden, p) })
}

// accountItems are one account's lines: its email, plan and banked resets,
// which switch to it, then dim lines with its notes and usage, as the Mac
// menu and the phone page show an account card.
func accountItems(a Account, handedBack, bars bool, now time.Time) []Item {
	title := a.Email
	if title == "" {
		title = a.ID
	}
	if plan := planNames[a.Plan]; plan != "" {
		title += " · " + plan
	} else if a.Plan != "" {
		title += " · " + a.Plan
	}
	if a.ResetCredits != nil && a.ResetCredits.Count > 0 {
		title += fmt.Sprintf(" · ⚡ %d banked", a.ResetCredits.Count)
	}
	tooltip := "Use this account"
	if a.NativeSwitchAvailable {
		tooltip = "Use this account in Claude Code and for Claude"
	}
	items := []Item{{Action: "activate:" + a.ID, Title: title, Tooltip: tooltip,
		Checkable: true, Checked: a.Selected(), Disabled: handedBack}}
	note := func(text string) { items = append(items, Item{Title: detail + text, Disabled: true}) }

	unix := float64(now.Unix())
	if a.ExhaustedUntil > unix {
		note("Out of usage · back in " + countdown(a.ExhaustedUntil-unix))
	}
	var windows []Window
	if a.Usage != nil {
		windows = a.Usage.Windows
	}
	// The Mac menu's badges: cached usage stays on screen when checks fail,
	// so say when it is old or the login no longer refreshes.
	if a.Health != nil {
		switch a.Health.Condition {
		case "needs_relogin":
			note("Sign in again")
		case "usage_stale", "usage_unavailable":
			if len(windows) > 0 {
				note("Usage stale")
			}
		}
	}
	switch {
	case a.UsageLimit > 0 && a.AtLimit:
		note(fmt.Sprintf("At its %d%% limit · Switcher uses another account", a.UsageLimit))
	case a.UsageLimit > 0:
		note(fmt.Sprintf("Keeps %d%% · switches away at %d%% left", a.UsageLimit, a.UsageLimit))
	}
	for _, w := range windows {
		note(usageLine(w, bars, unix))
	}
	if len(windows) == 0 {
		note("No usage reported yet")
	}
	return items
}

// limitChoices are the usage limits the tray offers, as in the dashboard.
var limitChoices = []int{0, 10, 20, 30, 50}

// limitsMenu sets each account's usage limit: the share of its weekly or
// monthly usage it keeps for what Switcher cannot route. Only accounts that
// report such a window, or already keep a share, are listed.
func limitsMenu(st State) (Item, bool) {
	var children []Item
	for _, a := range st.Accounts {
		counted := a.UsageLimit > 0
		if a.Usage != nil {
			for _, w := range a.Usage.Windows {
				counted = counted || w.Label == "Weekly" || w.Label == "Monthly"
			}
		}
		if !counted {
			continue
		}
		title := name(a.Provider) + " · " + a.Email
		if a.UsageLimit > 0 {
			title += fmt.Sprintf(" · keeps %d%%", a.UsageLimit)
		}
		choices := limitChoices
		if !slices.Contains(choices, a.UsageLimit) {
			choices = append(slices.Clone(choices), a.UsageLimit)
			slices.Sort(choices)
		}
		var options []Item
		for _, v := range choices {
			label := "Off"
			if v > 0 {
				label = fmt.Sprintf("Switch away at %d%% left", v)
			}
			options = append(options, Item{Action: fmt.Sprintf("limit:%s:%d", a.ID, v), Title: label, Checkable: true,
				Checked: a.UsageLimit == v, Disabled: st.HandedBack})
		}
		children = append(children, Item{Title: title, Children: options})
	}
	if len(children) == 0 {
		return Item{}, false
	}
	return Item{Title: "Usage limits", Tooltip: "Keep a share of an account's weekly usage for the chat apps; Switcher moves to another account at that much left",
		Children: children}, true
}

// usageLine is one usage window as what is left, the way the Mac menu, the
// phone page and the dashboard show it: "Weekly: ▰▰▱▱▱ 31% left · ↻ 2d 8h".
// Model windows such as Fable or Opus carry the model's name as their label.
func usageLine(w Window, bars bool, now float64) string {
	left := percentLeft(w)
	line := w.Label + ": "
	if bars {
		line += bar(left) + " "
	}
	line += fmt.Sprintf("%d%% left", left)
	if w.ResetsAt > now {
		line += " · ↻ " + countdown(w.ResetsAt-now)
	}
	return line
}

// percentLeft turns the used share around and rounds it, as the Mac menu
// and the phone page do.
func percentLeft(w Window) int {
	used := min(100, max(0, w.UsedPercent))
	return int(math.Round(100 - used))
}

// bar draws what is left in five steps. It is full only while nothing is
// used and empty only once nothing is left, so it never reads as either
// while the percentage beside it says otherwise.
func bar(left int) string {
	filled := int(math.Round(float64(left) / 20))
	switch {
	case left > 0 && filled == 0:
		filled = 1
	case left < 100 && filled == 5:
		filled = 4
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", 5-filled)
}

// countdown is the time until a reset as the Mac menu and the phone page
// write it: 38m, 6h 12m or 2d 3h.
func countdown(seconds float64) string {
	if !(seconds > 0) {
		return ""
	}
	m := int(seconds / 60)
	if m < 60 {
		return fmt.Sprintf("%dm", max(m, 1))
	}
	h := m / 60
	if h < 24 {
		return fmt.Sprintf("%dh %dm", h, m%60)
	}
	return fmt.Sprintf("%dd %dh", h/24, h%24)
}

// sameShape reports whether b differs from a only in what its items say and
// whether they are checked or enabled. Such a menu changes in place: rebuilt,
// it would close or flicker on GNOME while open, and its countdowns change
// every minute.
func sameShape(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Separator != b[i].Separator || a[i].Checkable != b[i].Checkable ||
			(a[i].Icon == "") != (b[i].Icon == "") || !sameShape(a[i].Children, b[i].Children) {
			return false
		}
	}
	return true
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
