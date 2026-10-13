// Package awake keeps the Mac awake on demand, for a while or until it is
// turned off, so agents keep working while nobody touches the Mac.
//
// An idle-sleep assertion is held by /usr/bin/caffeinate, which watches this
// process and lets go the moment Switcher exits for any reason. Closing the
// lid still sleeps the Mac; "keep going with the lid closed" additionally
// runs `pmset disablesleep 1` while keep awake is on. That needs root, so it
// is set up once: an administrator approves a sudoers rule that allows only
// `pmset disablesleep 1` and `pmset disablesleep 0` for this user. Switcher
// turns sleep back on whenever keep awake stops, and on its next start if it
// stopped without cleaning up. A sleep setting Switcher did not change, such
// as one from another app, is left alone.
package awake

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Settings are the user's choices; they persist across restarts.
type Settings struct {
	// AllowDisplaySleep lets the screen turn off while the Mac stays awake.
	AllowDisplaySleep bool `json:"allow_display_sleep"`
	// LidClosed keeps the Mac running with the lid closed while keep awake
	// is on.
	LidClosed bool `json:"lid_closed"`
	// BatteryFloor stops keep awake when the Mac runs on battery at or
	// below this percentage. Zero turns the guard off.
	BatteryFloor int `json:"battery_floor"`
}

// DefaultBatteryFloor protects a Mac left running in a bag.
const DefaultBatteryFloor = 10

// State is what the menu and web app show.
type State struct {
	Supported bool `json:"supported"`
	Active    bool `json:"active"`
	// Until is when keep awake ends, in Unix seconds; 0 while active means
	// it runs until turned off.
	Until int64 `json:"until,omitempty"`
	// LidActive reports that sleep is fully disabled right now.
	LidActive bool `json:"lid_active"`
	// LidReady reports that the closed-lid rule lets Switcher toggle sleep
	// without a password.
	LidReady bool `json:"lid_ready"`
	Settings
	// Ended says why the last session stopped by itself: "time", "battery"
	// or "error".
	Ended string `json:"ended,omitempty"`
	Error string `json:"error,omitempty"`
}

// ErrUnsupported is returned off macOS.
var ErrUnsupported = errors.New("keep awake is available on macOS only")

// system is every operating-system effect, so tests run none of them.
type system interface {
	// hold keeps the Mac (and the display, unless allowDisplaySleep) awake
	// until release is called or this process exits.
	hold(allowDisplaySleep bool) (release func(), pid int, err error)
	sleepDisabled() (bool, error)
	setSleepDisabled(on bool) error
	lidRuleReady() bool
	installLidRule() error
	// battery reports the charge and whether the Mac runs on battery.
	battery() (percent int, onBattery bool, err error)
	// stopLeftover ends an assertion holder left by an earlier Switcher.
	stopLeftover(pid int)
}

// record is the file on disk: settings plus what must be undone after a
// crash.
type record struct {
	Settings
	HolderPID  int  `json:"holder_pid,omitempty"`
	LidApplied bool `json:"lid_applied,omitempty"`
}

type session struct {
	until     time.Time // zero: until turned off
	release   func()
	holderPID int
	lid       bool // this session disabled sleep and must turn it back on
	timer     *time.Timer
	done      chan struct{}
}

// Manager owns keep awake for this Mac.
type Manager struct {
	mu        sync.Mutex
	sys       system
	path      string
	settings  Settings
	current   *session
	ended     string
	lastError string
	now       func() time.Time
	poll      time.Duration

	lidReady     bool
	lidCheckedAt time.Time
}

// New loads the settings in dir and undoes anything a previous Switcher left
// behind.
func New(dir string) *Manager {
	return newManager(filepath.Join(dir, "awake.json"), newSystem(), time.Now, time.Minute)
}

func newManager(path string, sys system, now func() time.Time, poll time.Duration) *Manager {
	m := &Manager{sys: sys, path: path, now: now, poll: poll,
		settings: Settings{BatteryFloor: DefaultBatteryFloor}}
	if sys == nil {
		return m
	}
	rec, err := m.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("awake: %v", err)
	}
	if err == nil {
		m.settings = rec.Settings
	}
	if rec.HolderPID > 0 {
		sys.stopLeftover(rec.HolderPID)
	}
	if rec.LidApplied {
		if err := sys.setSleepDisabled(false); err != nil {
			log.Printf("awake: could not turn sleep back on after the last run: %v", err)
		} else {
			log.Print("awake: turned sleep back on after the last run")
		}
	}
	if rec.HolderPID > 0 || rec.LidApplied {
		m.saveLocked()
	}
	return m
}

// Supported reports whether keep awake works on this system.
func (m *Manager) Supported() bool { return m != nil && m.sys != nil }

// State reports the current state. It may run pmset and sudo, briefly.
func (m *Manager) State() State {
	if !m.Supported() {
		return State{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

// lidReadyLocked asks sudo at most every 30 seconds; the menu polls often.
func (m *Manager) lidReadyLocked() bool {
	if now := m.now(); now.Sub(m.lidCheckedAt) > 30*time.Second {
		m.lidReady, m.lidCheckedAt = m.sys.lidRuleReady(), now
	}
	return m.lidReady
}

func (m *Manager) stateLocked() State {
	s := State{Supported: true, Settings: m.settings, Ended: m.ended, Error: m.lastError,
		LidReady: m.lidReadyLocked()}
	if c := m.current; c != nil {
		s.Active = true
		if !c.until.IsZero() {
			s.Until = c.until.Unix()
		}
		s.LidActive = c.lid
	}
	return s
}

// Start keeps the Mac awake for d, or until Stop when d is zero. A running
// session is replaced.
func (m *Manager) Start(d time.Duration) (State, error) {
	if !m.Supported() {
		return State{}, ErrUnsupported
	}
	if d < 0 || d > 7*24*time.Hour {
		return State{}, errors.New("keep awake lasts between a minute and a week")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var until time.Time
	if d > 0 {
		until = m.now().Add(d)
	}
	// A new duration replaces the session but keeps a disabled sleep
	// setting as it is, so the lid never flips back in between.
	lid := false
	if c := m.current; c != nil {
		if c.timer != nil {
			c.timer.Stop()
		}
		close(c.done)
		c.release()
		lid, m.current = c.lid, nil
	}
	if err := m.startLocked(until, lid); err != nil {
		if lid {
			m.restoreLidLocked(&session{lid: true})
		}
		return m.stateLocked(), err
	}
	return m.stateLocked(), nil
}

// StartUntil keeps the Mac awake until the given time.
func (m *Manager) StartUntil(t time.Time) (State, error) {
	d := t.Sub(m.now())
	if d < time.Minute {
		return m.State(), errors.New("pick a time in the future")
	}
	return m.Start(d)
}

// Stop lets the Mac sleep normally again.
func (m *Manager) Stop() State {
	if !m.Supported() {
		return State{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked("")
	m.ended = ""
	return m.stateLocked()
}

// Close stops keep awake for a shutting down Switcher.
func (m *Manager) Close() {
	if !m.Supported() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked("")
}

// Patch changes some settings. Nil fields stay as they are.
type Patch struct {
	AllowDisplaySleep *bool `json:"allow_display_sleep"`
	LidClosed         *bool `json:"lid_closed"`
	BatteryFloor      *int  `json:"battery_floor"`
}

// Update applies a settings change, also to a running session. Turning on
// the closed lid the first time asks macOS for an administrator password to
// install the closed-lid rule; that call blocks until it is answered.
func (m *Manager) Update(p Patch) (State, error) {
	if !m.Supported() {
		return State{}, ErrUnsupported
	}
	if p.BatteryFloor != nil && (*p.BatteryFloor < 0 || *p.BatteryFloor > 90) {
		return m.State(), errors.New("the battery limit is between 0 and 90 percent")
	}
	// The password prompt runs without the lock, so the menu keeps polling.
	if p.LidClosed != nil && *p.LidClosed && !m.sys.lidRuleReady() {
		if err := m.sys.installLidRule(); err != nil {
			return m.State(), fmt.Errorf("set up the closed lid: %w", err)
		}
		if !m.sys.lidRuleReady() {
			return m.State(), errors.New("the closed-lid rule was installed but sudo does not accept it")
		}
		m.mu.Lock()
		m.lidReady, m.lidCheckedAt = true, m.now()
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.settings
	if p.AllowDisplaySleep != nil {
		next.AllowDisplaySleep = *p.AllowDisplaySleep
	}
	if p.LidClosed != nil {
		next.LidClosed = *p.LidClosed
	}
	if p.BatteryFloor != nil {
		next.BatteryFloor = *p.BatteryFloor
	}
	previous := m.settings
	m.settings = next
	m.saveLocked()
	c := m.current
	if c == nil {
		return m.stateLocked(), nil
	}
	if next.AllowDisplaySleep != previous.AllowDisplaySleep {
		release, pid, err := m.sys.hold(next.AllowDisplaySleep)
		if err != nil {
			return m.stateLocked(), fmt.Errorf("keep awake: %w", err)
		}
		c.release()
		c.release, c.holderPID = release, pid
		m.saveLocked()
	}
	if next.LidClosed && !c.lid {
		if err := m.applyLidLocked(c); err != nil {
			return m.stateLocked(), err
		}
	} else if !next.LidClosed && c.lid {
		m.restoreLidLocked(c)
	}
	return m.stateLocked(), nil
}

func (m *Manager) startLocked(until time.Time, lid bool) error {
	release, pid, err := m.sys.hold(m.settings.AllowDisplaySleep)
	if err != nil {
		m.lastError = err.Error()
		return fmt.Errorf("keep awake: %w", err)
	}
	c := &session{until: until, release: release, holderPID: pid, lid: lid, done: make(chan struct{})}
	m.current, m.ended, m.lastError = c, "", ""
	if !until.IsZero() {
		c.timer = time.AfterFunc(until.Sub(m.now()), func() { m.end(c, "time") })
	}
	go m.guard(c)
	if m.settings.LidClosed && !c.lid {
		if err := m.applyLidLocked(c); err != nil {
			// Keep awake still runs with the lid open; the menu shows why
			// the closed lid did not apply.
			m.lastError = err.Error()
		}
	}
	m.saveLocked()
	return nil
}

func (m *Manager) applyLidLocked(c *session) error {
	already, err := m.sys.sleepDisabled()
	if err != nil {
		return fmt.Errorf("read sleep setting: %w", err)
	}
	if already {
		// Another app or the user disabled sleep; it is theirs to undo.
		return nil
	}
	if err := m.sys.setSleepDisabled(true); err != nil {
		return fmt.Errorf("disable sleep for the closed lid: %w", err)
	}
	c.lid = true
	m.saveLocked()
	return nil
}

func (m *Manager) restoreLidLocked(c *session) {
	if !c.lid {
		return
	}
	if err := m.sys.setSleepDisabled(false); err != nil {
		m.lastError = "could not turn sleep back on: " + err.Error()
		log.Printf("awake: %s", m.lastError)
		return // still recorded, so the next start retries
	}
	c.lid = false
	m.saveLocked()
}

func (m *Manager) stopLocked(reason string) {
	c := m.current
	if c == nil {
		return
	}
	m.restoreLidLocked(c)
	if c.timer != nil {
		c.timer.Stop()
	}
	close(c.done)
	c.release()
	m.current = nil
	m.ended = reason
	// A sleep setting that could not be restored stays on record.
	if c.lid {
		m.saveLockedWith(record{Settings: m.settings, LidApplied: true})
		return
	}
	m.saveLocked()
}

// end stops session c for a reason, unless it was already replaced.
func (m *Manager) end(c *session, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == c {
		m.stopLocked(reason)
	}
}

// guard ends keep awake when the battery runs low.
func (m *Manager) guard(c *session) {
	ticker := time.NewTicker(m.poll)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		floor := m.settings.BatteryFloor
		m.mu.Unlock()
		if floor <= 0 {
			continue
		}
		percent, onBattery, err := m.sys.battery()
		if err == nil && onBattery && percent <= floor {
			log.Printf("awake: battery at %d%%, letting the Mac sleep", percent)
			m.end(c, "battery")
			return
		}
	}
}

func (m *Manager) load() (record, error) {
	rec := record{Settings: Settings{BatteryFloor: DefaultBatteryFloor}}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{Settings: Settings{BatteryFloor: DefaultBatteryFloor}}, fmt.Errorf("read %s: %w", m.path, err)
	}
	return rec, nil
}

func (m *Manager) saveLocked() {
	rec := record{Settings: m.settings}
	if c := m.current; c != nil {
		rec.HolderPID, rec.LidApplied = c.holderPID, c.lid
	}
	m.saveLockedWith(rec)
}

func (m *Manager) saveLockedWith(rec record) {
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		log.Printf("awake: %v", err)
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		log.Printf("awake: %v", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		log.Printf("awake: %v", err)
	}
}

func newSystem() system {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return darwinSystem{}
}
