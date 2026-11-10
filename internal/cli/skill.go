package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
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
remove act on both computers when the host answers; --on picks:

  --on here                 only this computer
  --on host                 only the host (needs Switcher running here)
  --on both                 both computers

--replace replaces only this computer's copy; a copy edited on the host is
replaced on the host itself.

The skill is ~/.agents/skills/switcher/SKILL.md, read by agents that use
~/.agents/skills; Claude Code finds it through a link in ~/.claude/skills (or
$CLAUDE_CONFIG_DIR/skills). Switcher keeps it up to date when it updates,
unless you edit it. On this computer it needs no running Switcher.
`

// hostSkill is the host's skill as the running Switcher reports it.
type hostSkill struct {
	Host    string             `json:"host"`
	Skill   *agentskill.Status `json:"skill,omitempty"`
	Reason  string             `json:"reason,omitempty"`
	Message string             `json:"message,omitempty"`
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
// Only the Switcher on this computer is asked: "here" is this computer, so
// its host must be this computer's host.
func (c *ctx) hostSkillOf() *hostSkill {
	var v struct {
		Host *hostSkill `json:"host"`
	}
	if c.cl == nil || !c.localSwitcher() || c.cl.call("GET", "/api/agent-skill", nil, &v) != nil {
		return nil
	}
	if v.Host != nil {
		v.Host.Host = plainName(v.Host.Host)
	}
	return v.Host
}

// localSwitcher says the command talks to the Switcher on this computer.
func (c *ctx) localSwitcher() bool {
	u, err := url.Parse(c.url)
	return err == nil && loopback(u.Hostname())
}

// plainName keeps a name printable in a terminal.
func plainName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return -1
		}
		return r
	}, s)
}

// setHostSkill installs or removes the host's skill through the running
// Switcher. A copy edited on the host is replaced only there.
func (c *ctx) setHostSkill(install bool) (*hostSkill, error) {
	var v struct {
		Host *hostSkill `json:"host"`
	}
	err := c.cl.call("POST", "/api/agent-skill", map[string]any{"installed": install, "where": "host"}, &v)
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
	var host *hostSkill
	if action == "" || *on != "here" {
		host = c.hostSkillOf()
	}
	target := *on
	if target == "" {
		// Both by default, when the host can take it from here.
		target = "here"
		if host != nil && host.Skill != nil && action != "" {
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
			case host.Message != "":
				human = append(human, plainName(host.Message))
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
			if host, err = c.setHostSkill(true); err != nil {
				return partly(here, "Installed the Switcher skill on this computer", err)
			}
			human = append(human, "Installed the Switcher skill on "+hostName+".")
		} else if *on == "" && host != nil {
			human = append(human, skippedHost(host))
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
		if !onHost && *on == "" && host != nil {
			human = append(human, skippedHost(host))
		}
		if onHost {
			if host, err = c.setHostSkill(false); err != nil {
				return partly(here, "Removed the Switcher skill on this computer", err)
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

// skippedHost says why the host was left out of a default install.
func skippedHost(h *hostSkill) string {
	name := plainName(h.Host)
	switch {
	case h.Reason == "update":
		return "Not on " + name + ": update it to Switcher 1.27.0 or later, then run switcher skill install --on host."
	case h.Message != "":
		return "Not on " + name + ": " + plainName(h.Message)
	default:
		return "Not on " + name + ": it is not reachable right now; run switcher skill install --on host later."
	}
}

// partly reports a host step that failed after this computer's step was
// done, so neither goes unsaid.
func partly(here bool, done string, err error) error {
	if !here {
		return err
	}
	return fmt.Errorf("%s, but not on the host: %w", done, err)
}

func runLoad(c *ctx, args []string) error {
	fs := c.flags("load")
	apps := fs.Bool("apps", false, "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *apps {
		return runLoadApps(c)
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

// runLoadApps prints what is using the computer, by app: the leaders by
// processor and by memory, and by graphics where the computer reports it.
func runLoadApps(c *ctx) error {
	var v sysload.AppsView
	if err := c.cl.call("GET", "/api/system/apps", nil, &v); err != nil {
		return err
	}
	name := "This computer"
	if v.Remote {
		name = plainName(v.Machine) + " (the host whose accounts this computer uses)"
	}
	if !v.Available {
		human := name + ": no readings"
		if v.Reason == "update" {
			human = name + ": update it to Switcher 1.28.0 or later to see what is using it"
		} else if v.Reason == "unreachable" {
			human = name + ": not reachable right now"
		}
		c.result(v, human)
		return nil
	}
	lines := []string{fmt.Sprintf("%s: CPU %d%% · Memory %d%% · %d processes", name, v.CPU, v.Memory, v.Processes)}
	if v.AppsUnavailable {
		c.result(v, lines[0]+"\nCould not list the apps this time; try again in a moment.")
		return nil
	}
	top := func(title string, less func(a, b sysload.App) bool, show func(a sysload.App) string) {
		apps := append([]sysload.App(nil), v.Apps...)
		sort.SliceStable(apps, func(i, j int) bool { return less(apps[i], apps[j]) })
		lines = append(lines, "", title)
		for i, a := range apps {
			if i == 8 {
				break
			}
			lines = append(lines, fmt.Sprintf("  %-28s %s", plainName(a.Name), show(a)))
		}
	}
	top("By processor:", func(a, b sysload.App) bool { return a.CPU > b.CPU }, func(a sysload.App) string {
		unit := "processes"
		if a.Processes == 1 {
			unit = "process"
		}
		return fmt.Sprintf("%5.1f%%  (%d %s)", a.CPU, a.Processes, unit)
	})
	top("By memory:", func(a, b sysload.App) bool { return a.Memory > b.Memory }, func(a sysload.App) string {
		return fmt.Sprintf("%6.1f GB", float64(a.Memory)/(1<<30))
	})
	if v.GPUApps {
		gpu := func(a sysload.App) float64 {
			if a.GPU == nil {
				return 0
			}
			return *a.GPU
		}
		top("By graphics:", func(a, b sysload.App) bool { return gpu(a) > gpu(b) || gpu(a) == gpu(b) && a.GPUMemory > b.GPUMemory }, func(a sysload.App) string {
			return fmt.Sprintf("%5.1f%%  %6.0f MB graphics memory", gpu(a), float64(a.GPUMemory)/(1<<20))
		})
	}
	c.result(v, strings.Join(lines, "\n"))
	return nil
}
