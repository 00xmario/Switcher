package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"switcher/internal/agentskill"
	"switcher/internal/sysload"
)

const skillHelp = `Teaches your AI agents what Switcher is and how to use it, so they
understand requests such as "keep 10% on this account and switch
automatically".

  switcher skill            show whether the skill is installed
  switcher skill install    install it, or bring it up to date
  switcher skill install --replace
                            also replace a skill file Switcher did not write
                            or that was edited
  switcher skill remove     remove it

On a computer that uses another computer's accounts (a host), install and
remove act on both computers; --on picks one:

  --on here                 only this computer
  --on host                 only the host (needs Switcher running here)
  --on both                 both computers

The skill is ~/.agents/skills/switcher/SKILL.md, read by agents that use
~/.agents/skills; Claude Code finds it through a link in ~/.claude/skills (or
$CLAUDE_CONFIG_DIR/skills). Switcher keeps it up to date when it updates,
unless you edit it. On this computer it needs no running Switcher.
`

// hostSkill is the host's skill as the running Switcher reports it.
type hostSkill struct {
	Host   string             `json:"host"`
	Skill  *agentskill.Status `json:"skill,omitempty"`
	Reason string             `json:"reason,omitempty"`
}

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

// skillState says in a sentence what state a skill is in.
func skillState(st agentskill.Status, where, fix string) string {
	switch {
	case st.Blocked:
		return "Something other than the skill file is at " + st.Path + where + "; Switcher leaves it."
	case !st.Installed:
		return "The Switcher skill for agents is not installed" + where + ". Install it with: " + fix
	case st.Current:
		return "The Switcher skill is installed at " + st.Path + where + "."
	case st.Managed:
		return "An older Switcher skill is installed at " + st.Path + where + "; update it with: " + fix
	default:
		return "A skill Switcher did not write, or one you changed, is at " + st.Path + where + "; " + fix + " --replace replaces it."
	}
}

// hostSkillOf asks the running Switcher for the host's skill; nil when this
// computer uses no host or Switcher is not running.
func (c *ctx) hostSkillOf() *hostSkill {
	var v struct {
		Host *hostSkill `json:"host"`
	}
	if c.cl == nil || c.cl.call("GET", "/api/agent-skill", nil, &v) != nil {
		return nil
	}
	return v.Host
}

// setHostSkill installs or removes the host's skill through the running
// Switcher.
func (c *ctx) setHostSkill(install, replace bool) (*hostSkill, error) {
	var v struct {
		Host *hostSkill `json:"host"`
	}
	err := c.cl.call("POST", "/api/agent-skill", map[string]any{"installed": install, "replace": replace, "where": "host"}, &v)
	return v.Host, err
}

func runSkill(c *ctx, args []string) error {
	fs := c.flags("skill")
	replace := fs.Bool("replace", false, "")
	on := fs.String("on", "", "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	switch *on {
	case "", "here", "host", "both":
	default:
		return usagef("--on is here, host or both")
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
	host := c.hostSkillOf()
	target := *on
	if target == "" {
		target = "here"
		if host != nil && action != "" {
			target = "both"
		}
	}
	if target != "here" && host == nil {
		return errors.New("this computer uses no host, or Switcher is not running here (switcher start)")
	}
	here, onHost := target != "host", target != "here"
	hostName := ""
	if host != nil {
		hostName = host.Host
	}
	var st agentskill.Status
	var human []string
	switch action {
	case "":
		st, err = agentskill.Check(home, claude)
		if err != nil {
			return err
		}
		where := ""
		if host != nil {
			where = " on this computer"
		}
		line := skillState(st, where, "switcher skill install")
		if st.Installed && !st.Blocked && !st.ClaudeCode {
			command := "switcher skill install"
			if !st.Managed {
				command += " --replace"
			}
			line += " Claude Code does not see it yet: " + command + " links it in " + st.ClaudeLink + "."
		}
		human = append(human, line)
		if host != nil {
			switch {
			case host.Skill != nil:
				human = append(human, skillState(*host.Skill, " on "+hostName, "switcher skill install --on host"))
			case host.Reason == "update":
				human = append(human, "Update "+hostName+" to set up its skill from here.")
			default:
				human = append(human, hostName+" is not reachable right now.")
			}
		}
	case "install":
		if here {
			st, err = agentskill.Install(home, claude, *replace)
			if errors.Is(err, agentskill.ErrNotOurs) {
				return fmt.Errorf("%s holds a skill Switcher did not write or that was edited; switcher skill install --replace replaces it", st.Path)
			}
			if err != nil {
				return err
			}
			line := "Installed the Switcher skill at " + st.Path + "."
			if !st.ClaudeCode {
				line += " " + st.ClaudeLink + " is taken, so Claude Code does not see it there."
			}
			human = append(human, line)
		}
		if onHost {
			if host, err = c.setHostSkill(true, *replace); err != nil {
				return err
			}
			human = append(human, "Installed the Switcher skill on "+hostName+".")
		}
		human = append(human, "Your agents pick it up in their next session.")
	case "remove", "uninstall":
		if here {
			before, _ := agentskill.Check(home, claude)
			if st, err = agentskill.Remove(home, claude); err != nil {
				return err
			}
			if before.Installed {
				human = append(human, "Removed the Switcher skill.")
			} else {
				human = append(human, "The Switcher skill was not installed.")
			}
		}
		if onHost {
			if host, err = c.setHostSkill(false, false); err != nil {
				return err
			}
			human = append(human, "Removed the Switcher skill on "+hostName+".")
		}
	default:
		return usagef("unknown action %q; use install or remove", action)
	}
	if !here {
		st, _ = agentskill.Check(home, claude)
	}
	out := map[string]any{"skill": st}
	if host != nil {
		out["host"] = host
	}
	c.result(out, strings.Join(human, " "))
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
