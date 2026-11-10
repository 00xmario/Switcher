package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"switcher/internal/agentskill"
)

// HostSkill is the host's skill for AI agents as this Mac shows it, so the
// skill can be installed on both computers from here. Reason says why there
// is no Skill: "unreachable", or "update" for a host older than
// remoteSkillSince.
type HostSkill struct {
	Host   string             `json:"host"`
	Skill  *agentskill.Status `json:"skill,omitempty"`
	Reason string             `json:"reason,omitempty"`
}

// remoteSkillSince is the first release whose skill a paired Mac can see
// and install.
const remoteSkillSince = "1.27.0"

// HostSkill reads the host's skill, or installs (true) or removes (false)
// it. Reading never fails: a host that cannot answer gives a Reason. A
// change the host refuses is an error in the host's words.
func (c *Client) HostSkill(ctx context.Context, install *bool, replace bool) (HostSkill, error) {
	view := HostSkill{Host: c.Status().HostName}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	method, body := http.MethodGet, []byte(nil)
	if install != nil {
		method = http.MethodPost
		body, _ = json.Marshal(map[string]bool{"installed": *install, "replace": replace})
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://switcher-host/api/agent-skill", bytes.NewReader(body))
	if err != nil {
		return view, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Not routing: the skill is reached also while this Mac works on its own.
	resp, err := c.send(req, true)
	if err != nil {
		view.Reason = "unreachable"
		if install != nil {
			return view, fmt.Errorf("%s is unreachable: %w", view.Host, err)
		}
		return view, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		view.Reason = "update"
		if install != nil {
			return view, fmt.Errorf("update %s to Switcher %s or later to set up its skill from here", view.Host, remoteSkillSince)
		}
		return view, nil
	case err != nil || resp.StatusCode != http.StatusOK:
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &answer)
		message := plainText(answer.Error)
		if message == "" {
			message = fmt.Sprintf("the host answered %d", resp.StatusCode)
		}
		if install == nil {
			view.Reason = "unreachable"
			return view, nil
		}
		return view, errors.New(view.Host + ": " + message)
	}
	var st agentskill.Status
	if json.Unmarshal(raw, &st) != nil {
		view.Reason = "unreachable"
		if install != nil {
			return view, errors.New("could not read " + view.Host + "'s answer")
		}
		return view, nil
	}
	st.Path, st.ClaudeLink = plainText(st.Path), plainText(st.ClaudeLink)
	view.Skill = &st
	return view, nil
}
