package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"switcher/internal/agentskill"
)

// HostSkill is the host's skill for AI agents as this computer shows it, so
// the skill can be set up on both computers from here. Without Skill, Reason
// says why: "checking" (not read yet), "unreachable", "update" (a host older
// than remoteSkillSince) or "refused", with the host's Message.
type HostSkill struct {
	Host    string             `json:"host"`
	Skill   *agentskill.Status `json:"skill,omitempty"`
	Reason  string             `json:"reason,omitempty"`
	Message string             `json:"message,omitempty"`
}

// remoteSkillSince is the first release whose skill a paired computer can
// see and set up.
const remoteSkillSince = "1.27.0"

const (
	// skillFresh is how long a reading of the host's skill answers.
	skillFresh = 20 * time.Second
	// skillRead bounds a reading, so a host that does not answer holds up
	// nobody for long; skillWrite bounds an install or removal.
	skillRead  = 2500 * time.Millisecond
	skillWrite = 10 * time.Second
)

type skillCache struct {
	mu   sync.Mutex
	view HostSkill
	at   time.Time
	busy bool // a background reading is on its way
}

// HostSkill reads the host's skill: the last reading while it is fresh,
// otherwise a new one. It never fails; a host that cannot answer gives a
// Reason.
func (c *Client) HostSkill(ctx context.Context) HostSkill {
	if view, ok := c.cachedSkill(); ok {
		return view
	}
	return c.readSkill(ctx)
}

// CachedHostSkill answers at once: the last reading, or Reason "checking"
// while the first is taken in the background.
func (c *Client) CachedHostSkill() HostSkill {
	if view, ok := c.cachedSkill(); ok {
		return view
	}
	c.skill.mu.Lock()
	view, read, busy := c.skill.view, !c.skill.at.IsZero(), c.skill.busy
	c.skill.busy = true
	c.skill.mu.Unlock()
	if !busy {
		go func() {
			c.readSkill(context.Background())
			c.skill.mu.Lock()
			c.skill.busy = false
			c.skill.mu.Unlock()
		}()
	}
	if !read {
		view = HostSkill{Reason: "checking"}
	}
	view.Host = c.Status().HostName
	return view
}

func (c *Client) cachedSkill() (HostSkill, bool) {
	c.skill.mu.Lock()
	defer c.skill.mu.Unlock()
	if c.skill.at.IsZero() || time.Since(c.skill.at) > skillFresh {
		return HostSkill{}, false
	}
	view := c.skill.view
	view.Host = c.Status().HostName
	return view, true
}

func (c *Client) keepSkill(view HostSkill) {
	c.skill.mu.Lock()
	c.skill.view, c.skill.at = view, time.Now()
	c.skill.mu.Unlock()
}

func (c *Client) readSkill(ctx context.Context) HostSkill {
	view, _ := c.skillRequest(ctx, nil)
	c.keepSkill(view)
	return view
}

// SetHostSkill installs (true) or removes (false) the host's skill. A
// change the host refuses is an error in the host's words. Replacing a skill
// edited on the host is done on the host itself.
func (c *Client) SetHostSkill(ctx context.Context, install bool) (HostSkill, error) {
	view, err := c.skillRequest(ctx, &install)
	if err == nil {
		c.keepSkill(view)
	} else {
		c.skill.mu.Lock()
		c.skill.at = time.Time{} // read again next time
		c.skill.mu.Unlock()
	}
	return view, err
}

func (c *Client) skillRequest(ctx context.Context, install *bool) (HostSkill, error) {
	view := HostSkill{Host: c.Status().HostName}
	method, body, limit := http.MethodGet, []byte(nil), skillRead
	if install != nil {
		method, limit = http.MethodPost, skillWrite
		body, _ = json.Marshal(map[string]bool{"installed": *install})
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://switcher-host/api/agent-skill", bytes.NewReader(body))
	if err != nil {
		return view, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Not routing: the skill is reached also while this computer works on
	// its own.
	resp, err := c.send(req, true)
	if err != nil {
		view.Reason = "unreachable"
		return view, fmt.Errorf("%s is not reachable right now", view.Host)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		view.Reason = "update"
		return view, fmt.Errorf("update %s to Switcher %s or later to set up its skill from here", view.Host, remoteSkillSince)
	case resp.StatusCode == http.StatusUnauthorized:
		view.Reason, view.Message = "refused", view.Host+" no longer accepts this computer; pair again"
		return view, errors.New(view.Message)
	case readErr != nil || resp.StatusCode != http.StatusOK:
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &answer)
		message := plainText(answer.Error)
		if message == "" {
			message = fmt.Sprintf("the host answered %d", resp.StatusCode)
		}
		view.Reason, view.Message = "refused", view.Host+": "+message
		return view, errors.New(view.Message)
	}
	var st agentskill.Status
	if json.Unmarshal(raw, &st) != nil {
		view.Reason = "unreachable"
		return view, errors.New("could not read " + view.Host + "'s answer")
	}
	st.Path, st.ClaudeLink = plainText(st.Path), plainText(st.ClaudeLink)
	view.Skill = &st
	return view, nil
}
