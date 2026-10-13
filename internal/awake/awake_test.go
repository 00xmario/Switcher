package awake

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSystem records every effect. Nothing holds an assertion, runs pmset
// or touches sudoers.
type fakeSystem struct {
	mu            sync.Mutex
	holders       int // assertions currently held
	holds         []bool
	sleepOff      bool
	sleepSets     []bool
	ruleReady     bool
	installs      int
	installErr    error
	setErr        error
	percent       int
	onBattery     bool
	leftoverStops []int
}

func (f *fakeSystem) hold(allowDisplaySleep bool) (func(), int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holders++
	f.holds = append(f.holds, allowDisplaySleep)
	var once sync.Once
	return func() {
		once.Do(func() { f.mu.Lock(); f.holders--; f.mu.Unlock() })
	}, 4242, nil
}

func (f *fakeSystem) sleepDisabled() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sleepOff, nil
}

func (f *fakeSystem) setSleepDisabled(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.sleepOff = on
	f.sleepSets = append(f.sleepSets, on)
	return nil
}

func (f *fakeSystem) lidRuleReady() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ruleReady }

func (f *fakeSystem) installLidRule() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	if f.installErr != nil {
		return f.installErr
	}
	f.ruleReady = true
	return nil
}

func (f *fakeSystem) battery() (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.percent, f.onBattery, nil
}

func (f *fakeSystem) stopLeftover(pid int) { f.leftoverStops = append(f.leftoverStops, pid) }

func (f *fakeSystem) snapshot() (holders int, sleepOff bool, sets []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holders, f.sleepOff, append([]bool(nil), f.sleepSets...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func newFixture(t *testing.T) (*Manager, *fakeSystem, string) {
	t.Helper()
	sys := &fakeSystem{ruleReady: true, percent: 80}
	path := filepath.Join(t.TempDir(), "awake.json")
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	m := newManager(path, sys, c.now, 10*time.Millisecond)
	t.Cleanup(m.Close)
	return m, sys, path
}

func boolPtr(v bool) *bool { return &v }

func TestStartUntilTurnedOffAndStop(t *testing.T) {
	m, sys, _ := newFixture(t)
	state, err := m.Start(0)
	if err != nil || !state.Active || state.Until != 0 {
		t.Fatalf("start: %+v %v", state, err)
	}
	if holders, _, _ := sys.snapshot(); holders != 1 {
		t.Errorf("holders = %d", holders)
	}
	if sys.holds[0] {
		t.Error("the display stays on by default")
	}
	state = m.Stop()
	if state.Active {
		t.Error("still active after stop")
	}
	if holders, _, _ := sys.snapshot(); holders != 0 {
		t.Errorf("holders after stop = %d", holders)
	}
}

func TestTimedSessionEndsByItself(t *testing.T) {
	m, sys, _ := newFixture(t)
	state, err := m.Start(30 * time.Millisecond)
	if err != nil || state.Until != time.Unix(1_800_000_000, 0).Add(30*time.Millisecond).Unix() {
		t.Fatalf("start: %+v %v", state, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.State().Active && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	state = m.State()
	if state.Active || state.Ended != "time" {
		t.Errorf("after the time: %+v", state)
	}
	if holders, _, _ := sys.snapshot(); holders != 0 {
		t.Errorf("holders = %d", holders)
	}
}

func TestReplacingASessionKeepsOneHolder(t *testing.T) {
	m, sys, _ := newFixture(t)
	if _, err := m.Start(time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	if holders, _, _ := sys.snapshot(); holders != 1 {
		t.Errorf("holders = %d", holders)
	}
	if _, err := m.Start(-time.Second); err == nil {
		t.Error("negative durations are refused")
	}
	if _, err := m.Start(8 * 24 * time.Hour); err == nil {
		t.Error("more than a week is refused")
	}
}

func TestLidClosedDisablesSleepOnlyWhileAwake(t *testing.T) {
	m, sys, _ := newFixture(t)
	if _, err := m.Update(Patch{LidClosed: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, sleepOff, _ := sys.snapshot(); sleepOff {
		t.Error("the setting alone must not disable sleep")
	}
	state, err := m.Start(0)
	if err != nil || !state.LidActive {
		t.Fatalf("start: %+v %v", state, err)
	}
	// A new duration must not flip the lid setting in between.
	if _, err := m.Start(time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, sets := sys.snapshot(); len(sets) != 1 || !sets[0] {
		t.Errorf("sleep changes while replacing: %v", sets)
	}
	m.Stop()
	_, sleepOff, sets := sys.snapshot()
	if sleepOff || len(sets) != 2 || sets[1] {
		t.Errorf("after stop: sleepOff=%v sets=%v", sleepOff, sets)
	}
}

func TestLidToggledDuringASession(t *testing.T) {
	m, sys, _ := newFixture(t)
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	state, err := m.Update(Patch{LidClosed: boolPtr(true)})
	if err != nil || !state.LidActive {
		t.Fatalf("lid on: %+v %v", state, err)
	}
	state, err = m.Update(Patch{LidClosed: boolPtr(false)})
	if _, sleepOff, _ := sys.snapshot(); err != nil || state.LidActive || sleepOff {
		t.Errorf("lid off: %+v %v sleepOff=%v", state, err, sleepOff)
	}
}

func TestSleepDisabledByAnotherAppIsLeftAlone(t *testing.T) {
	m, sys, _ := newFixture(t)
	sys.sleepOff = true // e.g. another app's closed-lid mode
	if _, err := m.Update(Patch{LidClosed: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	m.Stop()
	if _, sleepOff, sets := sys.snapshot(); !sleepOff || len(sets) != 0 {
		t.Errorf("touched a setting it did not make: sleepOff=%v sets=%v", sleepOff, sets)
	}
}

func TestFirstLidUseInstallsTheRuleOnce(t *testing.T) {
	m, sys, _ := newFixture(t)
	sys.ruleReady = false
	sys.installErr = errors.New("the administrator password was not entered")
	state, err := m.Update(Patch{LidClosed: boolPtr(true)})
	if err == nil || state.LidClosed {
		t.Fatalf("a refused password keeps the lid off: %+v %v", state, err)
	}
	sys.installErr = nil
	state, err = m.Update(Patch{LidClosed: boolPtr(true)})
	if err != nil || !state.LidClosed || !state.LidReady {
		t.Fatalf("after the password: %+v %v", state, err)
	}
	if _, err := m.Update(Patch{LidClosed: boolPtr(true)}); err != nil || sys.installs != 2 {
		t.Errorf("installs = %d, err = %v", sys.installs, err)
	}
}

func TestLowBatteryEndsKeepAwake(t *testing.T) {
	m, sys, _ := newFixture(t)
	if _, err := m.Update(Patch{LidClosed: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	sys.mu.Lock()
	sys.percent, sys.onBattery = 10, true
	sys.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for m.State().Active && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	state := m.State()
	if _, sleepOff, _ := sys.snapshot(); state.Active || state.Ended != "battery" || sleepOff {
		t.Errorf("after low battery: %+v sleepOff=%v", state, sleepOff)
	}
}

func TestBatteryGuardIgnoresChargingAndCanBeOff(t *testing.T) {
	m, sys, _ := newFixture(t)
	sys.percent, sys.onBattery = 5, false
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !m.State().Active {
		t.Fatal("charging must not stop keep awake")
	}
	zero := 0
	if _, err := m.Update(Patch{BatteryFloor: &zero}); err != nil {
		t.Fatal(err)
	}
	sys.mu.Lock()
	sys.onBattery = true
	sys.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if !m.State().Active {
		t.Error("a guard turned off must not stop keep awake")
	}
	bad := 95
	if _, err := m.Update(Patch{BatteryFloor: &bad}); err == nil {
		t.Error("battery limits above 90% are refused")
	}
}

func TestDisplayChoiceRestartsTheHolder(t *testing.T) {
	m, sys, _ := newFixture(t)
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Update(Patch{AllowDisplaySleep: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if holders, _, _ := sys.snapshot(); holders != 1 || len(sys.holds) != 2 || !sys.holds[1] {
		t.Errorf("holders=%d holds=%v", holders, sys.holds)
	}
}

func TestSettingsPersistAndALeftoverIsCleanedUp(t *testing.T) {
	m, sys, path := newFixture(t)
	if _, err := m.Update(Patch{LidClosed: boolPtr(true), AllowDisplaySleep: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	// Switcher dies here without Close: the record says what to undo.
	var rec record
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &rec); err != nil || !rec.LidApplied || rec.HolderPID != 4242 {
		t.Fatalf("record = %+v %v", rec, err)
	}
	next := &fakeSystem{ruleReady: true, sleepOff: true}
	restarted := newManager(path, next, time.Now, time.Hour)
	if len(next.leftoverStops) != 1 || next.leftoverStops[0] != 4242 || next.sleepOff {
		t.Errorf("leftover stops=%v sleepOff=%v", next.leftoverStops, next.sleepOff)
	}
	state := restarted.State()
	if !state.LidClosed || !state.AllowDisplaySleep || state.Active || state.BatteryFloor != DefaultBatteryFloor {
		t.Errorf("restored state = %+v", state)
	}
	_ = sys
}

func TestUnrestoredSleepStaysOnRecord(t *testing.T) {
	m, sys, path := newFixture(t)
	if _, err := m.Update(Patch{LidClosed: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(0); err != nil {
		t.Fatal(err)
	}
	sys.setErr = errors.New("sudo: a password is required")
	state := m.Stop()
	if state.Error == "" {
		t.Error("a failed restore must be shown")
	}
	var rec record
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &rec)
	if !rec.LidApplied {
		t.Error("the next start must retry turning sleep back on")
	}
}

func TestUnsupportedManager(t *testing.T) {
	m := newManager(filepath.Join(t.TempDir(), "awake.json"), nil, time.Now, time.Minute)
	if m.Supported() || m.State().Supported {
		t.Error("no system means unsupported")
	}
	if _, err := m.Start(0); !errors.Is(err, ErrUnsupported) {
		t.Errorf("start: %v", err)
	}
}

func TestLidRuleAllowsOnlyPmsetDisablesleep(t *testing.T) {
	name, line, err := lidRule("mario.makdis")
	if err != nil {
		t.Fatal(err)
	}
	if name != "switcher_lid_mario_makdis" || strings.Contains(name, ".") {
		t.Errorf("name = %q; sudo skips sudoers.d files with dots", name)
	}
	want := "mario.makdis ALL=(root) NOPASSWD: /usr/bin/pmset disablesleep 1, /usr/bin/pmset disablesleep 0"
	if line != want {
		t.Errorf("line = %q", line)
	}
	for _, bad := range []string{"", "root ALL", "a,b", "x'y", "-n"} {
		if _, _, err := lidRule(bad); err == nil {
			t.Errorf("accepted user %q", bad)
		}
	}
	// visudo must accept the exact rule the script installs.
	if _, err := os.Stat("/usr/sbin/visudo"); err == nil {
		file := filepath.Join(t.TempDir(), "rule")
		if err := os.WriteFile(file, []byte(line+"\n"), 0o440); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/usr/sbin/visudo", "-cf", file).CombinedOutput(); err != nil {
			t.Errorf("visudo rejects the rule: %s %v", out, err)
		}
	}
	script := lidRuleScript(name, line)
	for _, want := range []string{"visudo -cf", "-m 0440 -o root -g wheel", "/etc/sudoers.d/switcher_lid_mario_makdis"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

func TestParseBattery(t *testing.T) {
	percent, onBattery, err := parseBattery("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t9%; discharging; 0:20 remaining present: true\n")
	if err != nil || percent != 9 || !onBattery {
		t.Errorf("battery: %d %v %v", percent, onBattery, err)
	}
	percent, onBattery, err = parseBattery("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t84%; charging; present: true\n")
	if err != nil || percent != 84 || onBattery {
		t.Errorf("AC: %d %v %v", percent, onBattery, err)
	}
	if _, _, err := parseBattery("Now drawing from 'AC Power'\n"); err == nil {
		t.Error("a Mac without a battery reports an error")
	}
}
