package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func TestPasswordRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if s.VerifyPassword("hunter22") {
		t.Fatal("verify must fail without a password set")
	}
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if !s.HasPassword() {
		t.Fatal("password hash missing after set")
	}
	if !s.VerifyPassword("hunter22") {
		t.Fatal("correct password rejected")
	}
	if s.VerifyPassword("wrongpassword") {
		t.Fatal("wrong password accepted")
	}
}

func TestMenuUsageBarsDefaultOnAndPersistedOff(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if !s.MenuUsageBars() {
		t.Fatal("new installs must show menu usage bars by default")
	}
	st := s.Load()
	off := false
	st.MenuUsageBars = &off
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if New(dir).MenuUsageBars() {
		t.Fatal("disabled menu usage bars did not survive a restart")
	}
	on := true
	st.MenuUsageBars = &on
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if !New(dir).MenuUsageBars() {
		t.Fatal("enabled menu usage bars did not survive a restart")
	}
}

func TestPasswordTooShort(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("short"); err == nil {
		t.Fatal("8-character minimum not enforced")
	}
}

func TestChangePasswordRequiresCurrent(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword("wrong", "newpassword"); err == nil {
		t.Fatal("wrong current password accepted")
	}
	if err := s.ChangePassword("hunter22", "newerpass99"); err != nil {
		t.Fatal(err)
	}
	if !s.VerifyPassword("newerpass99") || s.VerifyPassword("hunter22") {
		t.Fatal("password change did not take effect")
	}
}

func TestSessionsLifecycle(t *testing.T) {
	s := New(t.TempDir())
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidateSession(token) {
		t.Fatal("fresh session rejected")
	}
	// A client-supplied value must never be adopted as a session.
	if s.ValidateSession("attacker-controlled-value") {
		t.Fatal("arbitrary string validated as a session")
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if s.ValidateSession(token) {
		t.Fatal("deleted session still valid")
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// A fresh Store instance simulates a server restart (the updater swaps
	// the process image; sessions must survive).
	reborn := New(dir)
	if !reborn.ValidateSession(token) {
		t.Fatal("session lost across restart")
	}
}

func TestDeleteAllSessions(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	tokens := make([]string, 3)
	for i := range tokens {
		var err error
		tokens[i], err = s.NewSession()
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.SessionCount() != len(tokens) {
		t.Fatalf("session count %d, want %d", s.SessionCount(), len(tokens))
	}
	if err := s.DeleteAllSessions(); err != nil {
		t.Fatal(err)
	}
	if s.SessionCount() != 0 {
		t.Fatalf("sessions remain after DeleteAllSessions: %d", s.SessionCount())
	}
	for _, token := range tokens {
		if s.ValidateSession(token) {
			t.Fatal("a session survived DeleteAllSessions")
		}
	}
	// A fresh store (server restart) must not resurrect anything either.
	if n := New(dir).SessionCount(); n != 0 {
		t.Fatalf("sessions revived from disk after DeleteAllSessions: %d", n)
	}
}

func TestDeviceTokenRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	token, err := s.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("device token length %d, want 64 hex chars", len(token))
	}
	again, err := s.EnsureDeviceToken()
	if err != nil || again != token {
		t.Fatal("device token not stable across reads")
	}
	info, err := os.Stat(filepath.Join(s.Path(), "..", "local-token"))
	_ = info
	if err != nil {
		t.Fatalf("device token file missing: %v", err)
	}
	if rotated, _ := s.RotateDeviceToken(); rotated == token {
		t.Fatal("rotation returned the same token")
	}
}

func TestRateLimiterLocksOut(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < maxFailures; i++ {
		if err := s.CheckLockout(Local); err != nil {
			t.Fatalf("locked out after %d failures", i)
		}
		s.RecordFailure(Local)
	}
	if err := s.CheckLockout(Local); err == nil {
		t.Fatal("lockout did not engage after max failures")
	}
	s.ResetFailures(Local)
	if err := s.CheckLockout(Local); err != nil {
		t.Fatalf("lockout still active after reset: %v", err)
	}
}

// Guesses from the network lock the network out and nobody else: the owner
// at this computer still gets in, and the other way round.
func TestNetworkGuessesDoNotLockOutThisComputer(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < maxFailures+3; i++ {
		s.RecordFailure(Network)
	}
	if err := s.CheckLockout(Network); err == nil {
		t.Fatal("the network was not locked out")
	}
	if _, err := s.BeginAttempt(Network); err == nil {
		t.Fatal("a locked-out network attempt was admitted")
	}
	done, err := s.BeginAttempt(Local)
	if err != nil {
		t.Fatalf("network guesses locked out this computer: %v", err)
	}
	done()
	// Signing in here does not hand the network its guesses back.
	s.ResetFailures(Local)
	if err := s.CheckLockout(Network); err == nil {
		t.Fatal("a local sign-in lifted the network's lockout")
	}
	for i := 0; i < maxFailures; i++ {
		s.RecordFailure(Local)
	}
	if err := s.CheckLockout(Local); err == nil {
		t.Fatal("this computer was not locked out by its own failures")
	}
	// A new password starts everyone afresh.
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if s.CheckLockout(Local) != nil || s.CheckLockout(Network) != nil {
		t.Fatal("a new password kept old lockouts")
	}
}

func TestDisableAuthClearsEverything(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureDeviceToken(); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableAuth(); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() || s.HasPassword() {
		t.Fatal("auth still enabled after disable")
	}
	if s.HasDeviceToken() {
		t.Fatal("device token survived disable")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); !os.IsNotExist(err) {
		t.Fatal("sessions file survived disable")
	}
}

func TestCorruptSettingsFailClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(dir)
	if !s.Enabled() || s.HasPassword() {
		t.Fatal("corrupt initial settings must require auth without inventing a password")
	}
	if _, err := s.Snapshot(); err == nil {
		t.Fatal("corrupt settings error was hidden")
	}
}

// A session renews with use, but only up to SessionMaxAge from its sign-in:
// a copied token cannot be kept alive for good.
func TestSessionEndsAtItsMaxAgeHoweverOftenItIsUsed(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidateSession(token) {
		t.Fatal("a fresh session was refused")
	}
	rewrite := func(change func(*sessionFile, string)) *Store {
		raw, err := os.ReadFile(s.sessionsPath())
		if err != nil {
			t.Fatal(err)
		}
		var file sessionFile
		if err := json.Unmarshal(raw, &file); err != nil || len(file.Sessions) != 1 {
			t.Fatalf("%v %+v", err, file)
		}
		for key := range file.Sessions {
			if file.Born[key] == 0 {
				t.Fatal("the sign-in time was not saved")
			}
			change(&file, key)
		}
		out, _ := json.Marshal(file)
		if err := os.WriteFile(s.sessionsPath(), out, 0o600); err != nil {
			t.Fatal(err)
		}
		return New(dir)
	}
	// Signed in 29 days ago and used since: its renewal stops at day 30.
	restarted := rewrite(func(f *sessionFile, key string) { f.Born[key] = time.Now().Add(-29 * 24 * time.Hour).Unix() })
	if !restarted.ValidateSession(token) {
		t.Fatal("a session within its max age was refused")
	}
	restarted.mu.Lock()
	var expiry time.Time
	for _, at := range restarted.sessions {
		expiry = at
	}
	restarted.mu.Unlock()
	if left := time.Until(expiry); left > 25*time.Hour || left < 23*time.Hour {
		t.Fatalf("renewed %s past now, want the day it has left", left)
	}
	// Past its max age it is over, though its expiry was renewed.
	s = restarted
	old := rewrite(func(f *sessionFile, key string) {
		f.Born[key] = time.Now().Add(-SessionMaxAge - time.Hour).Unix()
		f.Sessions[key] = time.Now().Add(SessionTTL).Unix()
	})
	if old.ValidateSession(token) || old.SessionCount() != 0 {
		t.Fatal("a session outlived its max age")
	}
	// One an older build saved, without a sign-in time, counts from now.
	token, err = old.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	s = old
	legacy := rewrite(func(f *sessionFile, key string) { f.Born = nil })
	if !legacy.ValidateSession(token) {
		t.Fatal("a session from an older build was refused")
	}
	// Unknown tokens leave nothing behind.
	for i := 0; i < 50; i++ {
		legacy.ValidateSession(strings.Repeat("ab", 31) + fmt.Sprintf("%02x", i))
	}
	legacy.mu.Lock()
	kept := len(legacy.sessionBorn)
	legacy.mu.Unlock()
	if kept != 1 {
		t.Fatalf("unknown tokens were remembered: %d", kept)
	}
}
