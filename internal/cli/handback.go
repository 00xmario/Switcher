package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const handBackHelp = `Hand every tool back to its provider, for example before you remove
Switcher. Codex, Claude Code and Claude Desktop then talk to OpenAI and
Anthropic directly with their own logins, and keep working without Switcher:

  - Codex's config.toml gets its previous provider back. A Codex with no
    login of its own gets the Codex account selected in Switcher.
  - Claude Code keeps its login; one with none gets the selected Claude
    account. Claude Code refreshes it itself from then on.
  - Claude Desktop's settings are restored; restart Desktop when you can.
  - Sharing with your other computers, and using a paired host, turn off.
  - Switcher stops using its accounts: nothing is routed, refreshed or
    switched.

  switcher hand-back           show whether the tools are handed back
  switcher hand-back on --yes  hand them back
  switcher hand-back off       set Switcher up again, as it was before
`

type handbackStatus struct {
	HandedBack bool   `json:"handed_back"`
	Since      string `json:"since,omitempty"`
	Incomplete bool   `json:"incomplete,omitempty"`
	Steps      []struct {
		Tool    string `json:"tool"`
		Outcome string `json:"outcome"`
		Message string `json:"message"`
	} `json:"steps"`
}

func runHandBack(c *ctx, args []string) error {
	fs := c.flags("hand-back")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		var st handbackStatus
		if err := c.cl.call(http.MethodGet, "/api/handback", nil, &st); err != nil {
			return err
		}
		human := "Switcher manages your tools."
		if st.HandedBack {
			human = "Your tools are handed back to their providers; Switcher does not use its accounts. Set it up again with: switcher hand-back off"
			if st.Incomplete {
				human = "Handing back stopped halfway; finish it with: switcher hand-back on --yes"
			}
		}
		c.result(st, human)
		return nil
	}
	on := pos[0] == "on"
	if !on && pos[0] != "off" {
		return usagef("use on or off, not %q", pos[0])
	}
	if on && !*yes {
		return usagef("this changes Codex's and Claude Desktop's settings and stops Switcher using its accounts; add --yes to confirm")
	}
	var answer struct {
		Handback handbackStatus `json:"handback"`
		Warning  string         `json:"warning,omitempty"`
	}
	// A refused hand-back names the tool that needs attention; nothing changed.
	if err := c.cl.call(http.MethodPost, "/api/handback", map[string]bool{"enabled": on}, &answer); err != nil {
		return err
	}
	if c.json {
		c.emit(answer)
		if answer.Warning != "" {
			return errors.New(answer.Warning)
		}
		return nil
	}
	var b strings.Builder
	for _, step := range answer.Handback.Steps {
		mark := map[string]string{"done": "✓", "unchanged": "·", "skipped": "–", "failed": "✗"}[step.Outcome]
		fmt.Fprintf(&b, "%s %s: %s\n", orDefault(mark, "·"), step.Tool, step.Message)
	}
	switch {
	case answer.Warning != "":
		fmt.Fprint(c.out, b.String())
		return errors.New(answer.Warning)
	case on:
		b.WriteString("Your tools are handed back. Switcher can be removed, or set up again with: switcher hand-back off")
	default:
		b.WriteString("Switcher is set up again.")
	}
	fmt.Fprintln(c.out, strings.TrimRight(b.String(), "\n"))
	return nil
}
