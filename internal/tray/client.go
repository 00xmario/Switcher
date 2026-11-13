// Package tray is Switcher's tray icon on Linux: the equivalent of the
// macOS menu bar app's menu, drawn by the desktop's StatusNotifierItem host
// (KDE, Ubuntu's GNOME, Cinnamon, XFCE and most others). It shows every
// account's usage and switches accounts, and carries the switches the menu
// bar app has: keep awake, using a paired host, handing the tools back, and
// starting at login. Everything else lives in the web dashboard, which the
// tray opens. The tray is a client of the local server, like the CLI.
package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"switcher/internal/localpeer"
)

// State is the part of /api/state the tray shows.
type State struct {
	Version  string    `json:"version"`
	Order    []string  `json:"order"`
	Hidden   []string  `json:"hidden"`
	Accounts []Account `json:"accounts"`
	Update   *struct {
		Latest    string `json:"latest"`
		Available bool   `json:"update_available"`
	} `json:"update"`
	Awake  *Awake `json:"awake"`
	Remote *struct {
		Role      string `json:"role"` // client: using the host; local: paired, not using it
		Host      string `json:"host"`
		Connected bool   `json:"connected"`
		// HostUpdate is the host's Switcher version and update, while used.
		HostUpdate *struct {
			Current   string `json:"current"`
			Latest    string `json:"latest"`
			Available bool   `json:"update_available"`
			Remote    bool   `json:"remote_update"` // the host takes updates from here
			Install   *struct {
				Step string `json:"step"`
			} `json:"install"`
		} `json:"host_update"`
	} `json:"remote"`
	HandedBack bool `json:"handed_back"`
	Autostart  *struct {
		Supported bool `json:"supported"`
		Enabled   bool `json:"enabled"`
	} `json:"autostart"`
	ResetNotifications bool `json:"reset_notifications"`
	// MenuUsageBars is the dashboard's choice of usage bars in the menu;
	// unset means on, as in the Mac menu.
	MenuUsageBars *bool `json:"menu_usage_bars"`
}

// Account is one account card.
type Account struct {
	ID                    string `json:"id"`
	Provider              string `json:"provider"`
	Email                 string `json:"email"`
	Plan                  string `json:"plan"`
	Active                bool   `json:"active"`
	NativeActive          bool   `json:"native_active"`
	NativeSwitchAvailable bool   `json:"native_switch_available"`
	// ExhaustedUntil is when an account that ran out of usage gets it back.
	ExhaustedUntil float64 `json:"exhausted_until"`
	Usage          *struct {
		Windows []Window `json:"windows"`
	} `json:"usage"`
	Health *struct {
		Condition string `json:"condition"`
	} `json:"health"`
	ResetCredits *struct {
		Count int `json:"count"`
	} `json:"reset_credits"`
	// UsageLimit is the share of the weekly or monthly usage the account
	// keeps; AtLimit says it reached it.
	UsageLimit int  `json:"usage_limit"`
	AtLimit    bool `json:"at_limit"`
}

// Selected is the account the account card marks: Claude Code's login for
// Claude, the proxy's account otherwise.
func (a Account) Selected() bool {
	if a.NativeSwitchAvailable {
		return a.NativeActive
	}
	return a.Active
}

// Window is one usage window.
type Window struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    float64 `json:"resets_at"`
}

// Awake is the keep-awake state.
type Awake struct {
	Supported         bool  `json:"supported"`
	Active            bool  `json:"active"`
	Until             int64 `json:"until"`
	AllowDisplaySleep bool  `json:"allow_display_sleep"`
}

// Client talks to the local Switcher server.
type Client struct {
	Base  string
	Token func() string // the device token, needed while a password is set
	HTTP  *http.Client
}

// ErrNotRunning means nothing answers on the server's port.
var ErrNotRunning = errors.New("Switcher is not running")

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		if token := c.Token(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	client := c.HTTP
	if client == nil {
		// Switcher's API never redirects; see the command line's client.
		client = &http.Client{Timeout: 60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// Nothing goes to another account's program that took Switcher's port.
	if err := ownServer(req.URL); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return &refusal{status: resp.StatusCode, text: e.Error}
		}
		return &refusal{status: resp.StatusCode, text: fmt.Sprintf("Switcher answered %d", resp.StatusCode)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// refusal is an answer the server refused, with its message.
type refusal struct {
	status int
	text   string
}

func (r *refusal) Error() string { return r.text }

// State reads the dashboard state.
func (c *Client) State(ctx context.Context) (State, error) {
	var st State
	err := c.do(ctx, http.MethodGet, "/api/state", nil, &st)
	return st, err
}

// ErrNoChanges means the server cannot report changes: it predates them.
var ErrNoChanges = errors.New("this Switcher does not report changes")

// Changes waits until the state differs from the one tagged since, or until
// the server's own limit (25 seconds) passes, and returns the current
// state's tag. An empty or outdated since answers at once.
func (c *Client) Changes(ctx context.Context, since string) (tag string, changed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var answer struct {
		ETag    string `json:"etag"`
		Changed bool   `json:"changed"`
	}
	err = c.do(ctx, http.MethodGet, "/api/state/changes?since="+url.QueryEscape(since), nil, &answer)
	if r := (*refusal)(nil); errors.As(err, &r) && r.status == http.StatusNotFound {
		return "", false, ErrNoChanges
	}
	return answer.ETag, answer.Changed, err
}

// Post sends one action.
func (c *Client) Post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Patch changes settings.
func (c *Client) Patch(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPatch, path, body, nil)
}

// ownServer refuses a server on this computer that another account runs.
func ownServer(u *url.URL) error {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return localpeer.OwnServer(u.Hostname(), port)
}
