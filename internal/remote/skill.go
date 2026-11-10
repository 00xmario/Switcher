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
	// Stale marks a reading old enough that a new one is on its way.
	Stale bool `json:"stale,omitempty"`
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
	// gen counts writes and host changes: a reading that started before one
	// is not kept, so it cannot undo what the write showed.
	gen uint64
	// reading is closed when the reading on its way is in; nil when none is.
	reading chan struct{}
}

// HostSkill reads the host's skill: the last reading while it is fresh,
// otherwise a new one, shared with any already on its way. It never fails;
// a host that cannot answer gives a Reason.
func (c *Client) HostSkill(ctx context.Context) HostSkill {
	if view, fresh, _ := c.cachedSkill(); fresh {
		return view
	}
	done := c.startSkillRead()
	select {
	case <-done:
	case <-ctx.Done():
	}
	view, _, _ := c.cachedSkill()
	return view
}

// CachedHostSkill answers at once with the last reading, marked Stale when
// it is old, or Reason "checking" before the first; a new reading is then
// taken in the background.
func (c *Client) CachedHostSkill() HostSkill {
	view, fresh, read := c.cachedSkill()
	if fresh {
		return view
	}
	c.startSkillRead()
	if !read {
		return HostSkill{Host: c.Status().HostName, Reason: "checking"}
	}
	view.Stale = true
	return view
}

// cachedSkill is the last reading, whether it is fresh, and whether there
// is one.
func (c *Client) cachedSkill() (view HostSkill, fresh, read bool) {
	c.skill.mu.Lock()
	view, at := c.skill.view, c.skill.at
	c.skill.mu.Unlock()
	view.Host = c.Status().HostName
	return view, !at.IsZero() && time.Since(at) <= skillFresh, !at.IsZero()
}

// startSkillRead starts a reading unless one is on its way, and says when
// it is in.
func (c *Client) startSkillRead() <-chan struct{} {
	c.skill.mu.Lock()
	defer c.skill.mu.Unlock()
	if c.skill.reading != nil {
		return c.skill.reading
	}
	done := make(chan struct{})
	c.skill.reading = done
	gen := c.skill.gen
	go func() {
		view, _ := c.skillRequest(context.Background(), nil)
		c.skill.mu.Lock()
		if c.skill.gen == gen {
			c.skill.view, c.skill.at = view, time.Now()
		}
		c.skill.reading = nil
		c.skill.mu.Unlock()
		close(done)
	}()
	return done
}

// forgetSkill drops the last reading, as when the host changes; a reading
// on its way is not kept.
func (c *Client) forgetSkill() {
	c.skill.mu.Lock()
	c.skill.gen++
	c.skill.view, c.skill.at = HostSkill{}, time.Time{}
	c.skill.mu.Unlock()
}

// SetHostSkill installs (true) or removes (false) the host's skill. A
// change the host refuses is an error in the host's words. Replacing a skill
// edited on the host is done on the host itself.
func (c *Client) SetHostSkill(ctx context.Context, install bool) (HostSkill, error) {
	view, err := c.skillRequest(ctx, &install)
	c.skill.mu.Lock()
	c.skill.gen++ // a reading started before this is out of date
	if err == nil {
		c.skill.view, c.skill.at = view, time.Now()
	} else {
		c.skill.at = time.Time{} // read again next time
	}
	c.skill.mu.Unlock()
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
