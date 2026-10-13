package awake

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type darwinSystem struct{}

const (
	caffeinate = "/usr/bin/caffeinate"
	pmset      = "/usr/bin/pmset"
	sudo       = "/usr/bin/sudo"
)

// hold runs caffeinate: -i keeps the Mac from idle sleep, -m keeps disks
// spinning, -d keeps the display on, and -w ends it with this process.
func (darwinSystem) hold(allowDisplaySleep bool) (func(), int, error) {
	args := []string{"-i", "-m", "-w", strconv.Itoa(os.Getpid())}
	if !allowDisplaySleep {
		args = append(args, "-d")
	}
	cmd := exec.Command(caffeinate, args...)
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	var once sync.Once
	// caffeinate ignores SIGTERM. macOS drops the assertion with the
	// process, so killing it is the clean way out.
	release := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			<-exited
		})
	}
	return release, cmd.Process.Pid, nil
}

var sleepDisabledLine = regexp.MustCompile(`(?m)^\s*SleepDisabled\s+(\d)`)

func (darwinSystem) sleepDisabled() (bool, error) {
	out, err := exec.Command(pmset, "-g").Output()
	if err != nil {
		return false, err
	}
	match := sleepDisabledLine.FindSubmatch(out)
	return match != nil && string(match[1]) == "1", nil
}

func (darwinSystem) setSleepDisabled(on bool) error {
	value := "0"
	if on {
		value = "1"
	}
	// -n: never prompt. The closed-lid rule allows exactly this command.
	out, err := exec.Command(sudo, "-n", pmset, "disablesleep", value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (darwinSystem) lidRuleReady() bool {
	for _, value := range []string{"1", "0"} {
		if exec.Command(sudo, "-n", "-l", pmset, "disablesleep", value).Run() != nil {
			return false
		}
	}
	return true
}

var safeUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// lidRule is the whole sudoers rule: this user may run pmset as root to turn
// sleep off and on, and nothing else.
func lidRule(username string) (name, line string, err error) {
	if !safeUser.MatchString(username) {
		return "", "", fmt.Errorf("user name %q cannot be used in a sudoers rule", username)
	}
	// sudo skips files in sudoers.d whose names contain a dot.
	name = "switcher_lid_" + strings.ReplaceAll(username, ".", "_")
	line = username + " ALL=(root) NOPASSWD: " + pmset + " disablesleep 1, " + pmset + " disablesleep 0"
	return name, line, nil
}

// lidRuleScript checks the rule with visudo before it goes live, so a bad
// file can never lock sudo.
func lidRuleScript(name, line string) string {
	return `tmp=$(/usr/bin/mktemp /tmp/switcher-sudoers.XXXXXX) || exit 1; ` +
		`/usr/bin/printf '%s\n' '` + line + `' > "$tmp" && /usr/sbin/visudo -cf "$tmp" >/dev/null && ` +
		`/usr/bin/install -m 0440 -o root -g wheel "$tmp" /etc/sudoers.d/` + name + `; ` +
		`status=$?; /bin/rm -f "$tmp"; exit $status`
}

// installLidRule asks macOS for an administrator password, once. Switcher
// never sees the password.
func (darwinSystem) installLidRule() error {
	current, err := user.Current()
	if err != nil {
		return err
	}
	name, line, err := lidRule(current.Username)
	if err != nil {
		return err
	}
	script := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(lidRuleScript(name, line))
	apple := `do shell script "` + script + `" with prompt "Switcher wants to keep your Mac running with the lid closed. ` +
		`This allows Switcher only to turn sleep off and back on." with administrator privileges`
	out, err := exec.Command("/usr/bin/osascript", "-e", apple).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "-128") {
			return errors.New("the administrator password was not entered")
		}
		if text := strings.TrimSpace(string(out)); text != "" {
			return fmt.Errorf("%s (%w)", text, err)
		}
		return err
	}
	return nil
}

var batteryPercent = regexp.MustCompile(`(\d{1,3})%`)

func (darwinSystem) battery() (int, bool, error) {
	out, err := exec.Command(pmset, "-g", "batt").Output()
	if err != nil {
		return 0, false, err
	}
	return parseBattery(string(out))
}

func parseBattery(out string) (int, bool, error) {
	match := batteryPercent.FindStringSubmatch(out)
	if match == nil {
		return 0, false, errors.New("no battery")
	}
	percent, _ := strconv.Atoi(match[1])
	return percent, strings.Contains(out, "'Battery Power'"), nil
}

func (darwinSystem) stopLeftover(pid int) {
	out, err := exec.Command("/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err == nil && strings.TrimSpace(string(out)) == caffeinate {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
