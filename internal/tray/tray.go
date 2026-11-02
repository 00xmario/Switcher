package tray

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UI draws the menu. The Linux front end implements it with a
// StatusNotifierItem; tests record what it was given.
type UI interface {
	Render([]Item)
	Notify(title, body string)
	Quit()
}

// Options connect the tray to its process.
type Options struct {
	Client *Client
	// Start starts the server in the background.
	Start func() error
	// Open shows the dashboard in a browser.
	Open func(url string)
	// Updated restarts the tray when its binary was replaced by an update;
	// it returns only when there was nothing to restart into.
	Updated  func()
	Interval time.Duration
	Now      func() time.Time
}

// Controller keeps the menu in step with the server and runs its actions.
type Controller struct {
	opt Options
	ui  UI

	mu      sync.Mutex
	state   State
	status  Status
	drawn   []Item
	version string             // the server version first seen
	resets  map[string]float64 // window -> the reset time waiting to pass
	alerted map[string]float64
	started bool // the tray started the server once already
}

// New returns a controller drawing on ui.
func New(opt Options, ui UI) *Controller {
	if opt.Interval == 0 {
		opt.Interval = 15 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Controller{opt: opt, ui: ui, resets: map[string]float64{}, alerted: map[string]float64{}}
}

// Run polls until ctx ends. A server that is not running is started once.
func (c *Controller) Run(ctx context.Context) {
	c.Refresh(ctx)
	ticker := time.NewTicker(c.opt.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

// Refresh reads the state and redraws the menu if it changed.
func (c *Controller) Refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	st, err := c.opt.Client.State(ctx)
	c.mu.Lock()
	start := false
	if err != nil {
		c.status.Running = false
		start = errors.Is(err, ErrNotRunning) && !c.started && c.opt.Start != nil
		c.started = c.started || start
	} else {
		c.state, c.status.Running = st, true
		if c.version == "" {
			c.version = st.Version
		}
	}
	updated := err == nil && c.version != "" && st.Version != c.version
	alerts := c.dueAlertsLocked()
	c.mu.Unlock()
	if start {
		c.startServer(ctx)
		return
	}
	for _, a := range alerts {
		c.ui.Notify(a[0], a[1])
	}
	if updated && c.opt.Updated != nil {
		// The server restarted into a new release; so does the tray.
		c.opt.Updated()
		c.mu.Lock()
		c.version = st.Version
		c.mu.Unlock()
	}
	c.draw()
}

func (c *Controller) startServer(ctx context.Context) {
	c.setBusy("Starting Switcher")
	err := c.opt.Start()
	for i := 0; err == nil && i < 40; i++ {
		if _, e := c.opt.Client.State(ctx); e == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	c.finish(err, "")
	c.Refresh(ctx)
}

// dueAlertsLocked returns reset alerts whose provider-reported reset time
// has passed, while reset alerts are on, each once.
func (c *Controller) dueAlertsLocked() [][2]string {
	if !c.state.ResetNotifications || !c.status.Running {
		c.resets = map[string]float64{}
		return nil
	}
	now := float64(c.opt.Now().Unix())
	labels := map[string][2]string{}
	for _, a := range c.state.Accounts {
		if a.Usage == nil {
			continue
		}
		for _, w := range a.Usage.Windows {
			key := a.ID + "|" + w.Label
			labels[key] = [2]string{name(a.Provider) + " usage window", "Provider-reported reset time reached for " + w.Label + " (" + a.Email + ")."}
			if w.ResetsAt > now && c.alerted[key] != w.ResetsAt {
				c.resets[key] = w.ResetsAt
			}
		}
	}
	var due [][2]string
	for key, at := range c.resets {
		if now < at {
			continue
		}
		delete(c.resets, key)
		// A day late is no news: the computer slept through it.
		if now-at < 24*3600 {
			if text, ok := labels[key]; ok {
				due = append(due, text)
			}
		}
		c.alerted[key] = at
	}
	return due
}

func (c *Controller) draw() {
	c.mu.Lock()
	items := Build(c.state, c.status, c.opt.Now())
	same := reflect.DeepEqual(items, c.drawn)
	c.drawn = items
	c.mu.Unlock()
	if !same {
		c.ui.Render(items)
	}
}

func (c *Controller) setBusy(what string) {
	c.mu.Lock()
	c.status.Busy, c.status.Note = what, ""
	c.mu.Unlock()
	c.draw()
}

func (c *Controller) finish(err error, done string) {
	c.mu.Lock()
	c.status.Busy, c.status.Note = "", done
	if err != nil {
		c.status.Note = err.Error()
	}
	c.mu.Unlock()
	c.draw()
}

// Click runs a menu action. It returns once the action is done.
func (c *Controller) Click(ctx context.Context, action string) {
	c.mu.Lock()
	st, busy := c.state, c.status.Busy
	c.mu.Unlock()
	if busy != "" && action != "quit" && action != "quit-tray" && action != "open" {
		return
	}
	api := c.opt.Client
	run := func(what, done string, work func() error) {
		c.setBusy(what)
		err := work()
		c.finish(err, done)
		c.Refresh(ctx)
	}
	switch {
	case action == "open":
		if c.opt.Open != nil {
			c.opt.Open(api.Base)
		}
	case action == "start":
		c.mu.Lock()
		c.started = true
		c.mu.Unlock()
		c.startServer(ctx)
	case action == "refresh":
		run("Refreshing usage", "", func() error { return api.Post(ctx, "/api/usage/refresh", nil) })
	case strings.HasPrefix(action, "activate:"):
		id := strings.TrimPrefix(action, "activate:")
		email := id
		for _, a := range st.Accounts {
			if a.ID == id {
				email = a.Email
			}
		}
		run("Switching to "+email, "Now using "+email, func() error { return api.Post(ctx, "/api/accounts/"+id+"/activate", nil) })
	case action == "awake:off":
		run("Letting the computer sleep", "", func() error { return api.Post(ctx, "/api/awake", map[string]any{"on": false}) })
	case strings.HasPrefix(action, "awake:"):
		minutes, err := strconv.Atoi(strings.TrimPrefix(action, "awake:"))
		if err != nil {
			return
		}
		run("Keeping the computer awake", "", func() error {
			return api.Post(ctx, "/api/awake", map[string]any{"on": true, "minutes": minutes})
		})
	case action == "awake-display":
		allow := st.Awake != nil && !st.Awake.AllowDisplaySleep
		run("Saving", "", func() error { return api.Patch(ctx, "/api/awake", map[string]bool{"allow_display_sleep": allow}) })
	case action == "use-host":
		using := st.Remote != nil && st.Remote.Role == "client"
		done := "Using this computer's own accounts"
		if !using {
			done = "Using the accounts of " + st.Remote.Host
		}
		run("Saving", done, func() error { return api.Post(ctx, "/api/remote/use-host", map[string]bool{"enabled": !using}) })
	case action == "hand-back:on" || action == "hand-back:off":
		// The action names its direction, so a menu drawn before the state
		// changed elsewhere never does the opposite.
		on := action == "hand-back:on"
		what, done := "Handing your tools back", "Handed back; restart Claude Desktop when you can"
		if !on {
			what, done = "Setting Switcher up again", "Switcher is set up again"
		}
		run(what, done, func() error { return api.Post(ctx, "/api/handback", map[string]bool{"enabled": on}) })
	case action == "autostart":
		on := st.Autostart == nil || !st.Autostart.Enabled
		run("Saving", "", func() error { return api.Post(ctx, "/api/autostart", map[string]bool{"enabled": on}) })
	case action == "check-update":
		c.setBusy("Checking for updates")
		var answer struct {
			Update struct {
				Latest    string `json:"latest"`
				Available bool   `json:"update_available"`
			} `json:"update"`
		}
		err := api.do(ctx, "POST", "/api/update/check", nil, &answer)
		note := "Switcher is up to date"
		if answer.Update.Available {
			note = fmt.Sprintf("Version %s is available", strings.TrimPrefix(answer.Update.Latest, "v"))
		}
		c.finish(err, note)
		c.Refresh(ctx)
	case action == "install-update":
		run("Installing the update", "", func() error {
			// The server replaces itself and restarts, which ends this
			// request; the next poll sees the new version.
			err := api.Post(ctx, "/api/update", nil)
			if err != nil && !strings.Contains(err.Error(), "EOF") && !errors.Is(err, ErrNotRunning) && !strings.Contains(err.Error(), "connection reset") {
				return err
			}
			return nil
		})
	case action == "quit":
		_ = api.Post(ctx, "/api/quit", nil)
		c.ui.Quit()
	case action == "quit-tray":
		c.ui.Quit()
	}
}
