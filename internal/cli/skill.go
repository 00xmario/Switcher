package cli

import (
	"errors"
	"fmt"
	"os"

	"switcher/internal/agentskill"
	"switcher/internal/sysload"
)

const skillHelp = `Teaches your AI agents what Switcher is and how to use it, so they
understand requests such as "keep 10% on this account and switch
automatically".

  switcher skill            show whether the skill is installed
  switcher skill install    install it, or bring it up to date
  switcher skill remove     remove it

The skill is ~/.agents/skills/switcher/SKILL.md, read by agents that use
~/.agents/skills; Claude Code finds it through a link in ~/.claude/skills (or
$CLAUDE_CONFIG_DIR/skills). Switcher keeps it up to date when it updates,
unless you edit it. It needs no running Switcher.
`

// home is the user's home directory, from HOME when it is set.
func (c *ctx) home() (string, error) {
	if home := c.opt.Getenv("HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("no home directory to install the skill in")
	}
	return home, nil
}

func runSkill(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("skill"), args, 0, 1)
	if err != nil {
		return err
	}
	home, err := c.home()
	if err != nil {
		return err
	}
	action := ""
	if len(pos) == 1 {
		action = pos[0]
	}
	claude := c.opt.Getenv("CLAUDE_CONFIG_DIR")
	var st agentskill.Status
	var human string
	switch action {
	case "":
		st, err = agentskill.Check(home, claude)
		switch {
		case err != nil:
		case st.Blocked:
			human = "Something other than the skill file is at " + st.Path + "; Switcher leaves it."
		case !st.Installed:
			human = "The Switcher skill for agents is not installed. Install it with: switcher skill install"
		case st.Current:
			human = "The Switcher skill is installed at " + st.Path + "."
		case st.Managed:
			human = "An older Switcher skill is installed at " + st.Path + "; update it with: switcher skill install"
		default:
			human = "A Switcher skill you changed is at " + st.Path + "; switcher skill install replaces it."
		}
		if err == nil && st.Installed && !st.ClaudeCode {
			human += " Claude Code does not see it yet: switcher skill install links it in " + st.ClaudeLink + "."
		}
	case "install":
		st, err = agentskill.Install(home, claude)
		human = "Installed the Switcher skill at " + st.Path + ". Your agents pick it up in their next session."
		if err == nil && !st.ClaudeCode {
			human += " " + st.ClaudeLink + " is taken, so Claude Code does not see it there."
		}
	case "remove", "uninstall":
		var before agentskill.Status
		before, _ = agentskill.Check(home, claude)
		st, err = agentskill.Remove(home, claude)
		human = "Removed the Switcher skill."
		if !before.Installed {
			human = "The Switcher skill was not installed."
		}
	default:
		return usagef("unknown action %q; use install or remove", action)
	}
	if err != nil {
		return err
	}
	c.result(map[string]any{"skill": st}, human)
	return nil
}

func runLoad(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("load"), args, 0, 0); err != nil {
		return err
	}
	var v sysload.View
	if err := c.cl.call("GET", "/api/system", nil, &v); err != nil {
		return err
	}
	name := "This computer"
	if v.Remote {
		name = v.Machine + " (the host whose accounts this computer uses)"
	}
	human := name + ": "
	switch {
	case v.Available:
		human += fmt.Sprintf("CPU %d%%", v.CPU)
		if v.Cores > 0 {
			human += fmt.Sprintf(" of %d cores", v.Cores)
		}
		human += fmt.Sprintf(" · Memory %d%%", v.Memory)
		if v.MemTotal > 0 {
			human += fmt.Sprintf(" (%.1f of %.1f GB)", float64(v.MemUsed)/(1<<30), float64(v.MemTotal)/(1<<30))
		}
		if v.GPU != nil {
			human += fmt.Sprintf(" · GPU %d%%", *v.GPU)
		} else {
			human += " · GPU no reading"
		}
	case v.Reason == "unreachable":
		human += "not reachable right now"
	case v.Reason == "update":
		human += "update it to see its load"
	default:
		human += "no load readings here"
	}
	c.result(v, human)
	return nil
}
