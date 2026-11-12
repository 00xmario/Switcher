//go:build darwin && cgo

package claudecode

// The native Keychain writer against a real keychain. These run only where
// build/macos/keychain-test.sh has made a throwaway keychain the default one
// (a CI runner); everywhere else they skip, and never touch a login keychain.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/store"
)

func realKeychain(t *testing.T) string {
	t.Helper()
	if os.Getenv("SWITCHER_REAL_KEYCHAIN_TEST") != "1" {
		t.Skip("needs a throwaway default keychain")
	}
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	return "Switcher-test-" + hex.EncodeToString(raw)
}

func printable(prefix string, size int) []byte {
	head := `{"fixture":"` + prefix + `","pad":"`
	return []byte(head + strings.Repeat("x", size-len(head)-2) + `"}`)
}

var volatile = regexp.MustCompile(`"mdat".*`)

// Everything but the secret and the modification date.
func attributes(t *testing.T, service string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-a", keychainUser(), "-s", service).CombinedOutput()
	if err != nil {
		t.Fatalf("attributes: %v: %s", err, out)
	}
	return volatile.ReplaceAllString(string(out), "")
}

func TestRealKeychainOversizedUpdate(t *testing.T) {
	service := realKeychain(t)
	ctx := context.Background()
	k := SystemKeychain{}
	t.Cleanup(func() { _ = k.Delete(context.Background(), service) })

	large := printable("large", 9000)
	if err := k.Validate(ctx, service, large, fileValue{}); !errors.Is(err, errKeychainItemMissing) {
		t.Fatalf("preflight without an item: %v", err)
	}
	if err := k.Write(ctx, service, large); !errors.Is(err, errKeychainItemMissing) {
		t.Fatalf("oversized write without an item: %v", err)
	}
	if _, exists, err := k.Read(ctx, service); err != nil || exists {
		t.Fatalf("a failed oversized write left an item: exists=%v err=%v", exists, err)
	}

	small := printable("small", 300)
	if err := k.Write(ctx, service, small); err != nil {
		t.Fatalf("create through /usr/bin/security: %v", err)
	}
	before := attributes(t, service)

	// The preflight writes the current bytes back and changes nothing.
	if err := k.Validate(ctx, service, large, fileValue{Data: small, Exists: true}); err != nil {
		t.Fatalf("preflight with an item: %v", err)
	}
	if got, _, _ := k.Read(ctx, service); !bytes.Equal(got, small) {
		t.Fatal("the preflight changed the item")
	}
	started := time.Now()
	if err := k.Write(ctx, service, large); err != nil {
		t.Fatalf("native update of 9000 bytes: %v", err)
	}
	t.Logf("native 9000-byte update and readback took %v", time.Since(started))
	got, exists, err := k.Read(ctx, service)
	if err != nil || !exists || !bytes.Equal(got, large) {
		t.Fatalf("readback: exists=%v err=%v equal=%v", exists, err, bytes.Equal(got, large))
	}
	if after := attributes(t, service); after != before {
		t.Fatalf("item attributes changed:\n%s\n---\n%s", before, after)
	}

	for _, size := range []int{2100, 65536, 512 << 10, nativeFileLimit} {
		value := printable("sized", size)
		started := time.Now()
		if err := k.Write(ctx, service, value); err != nil {
			t.Errorf("native update of %d bytes: %v", size, err)
			continue
		}
		got, _, _ := k.Read(ctx, service)
		if !bytes.Equal(got, value) {
			t.Errorf("readback of %d bytes differs", size)
		}
		t.Logf("%d bytes took %v", size, time.Since(started))
	}
	if err := k.Write(ctx, service, printable("over", nativeFileLimit+1)); err == nil {
		t.Fatal("a value over the storage limit was accepted")
	}

	// /usr/bin/security, which Claude Code uses, still owns the item.
	if err := k.Write(ctx, service, small); err != nil {
		t.Fatalf("command-line update after a native one: %v", err)
	}
	got, _, _ = k.Read(ctx, service)
	if !bytes.Equal(got, small) {
		t.Fatal("command-line readback after a native update differs")
	}
	if after := attributes(t, service); after != before {
		t.Fatalf("item attributes changed after the round trip:\n%s\n---\n%s", before, after)
	}

	var wg sync.WaitGroup
	values := make([][]byte, 8)
	for i := range values {
		values[i] = printable("race-"+string(rune('a'+i)), 5000+i)
		wg.Add(1)
		go func(value []byte) {
			defer wg.Done()
			_ = k.Write(ctx, service, value) // losers may fail to verify
		}(values[i])
	}
	wg.Wait()
	got, exists, err = k.Read(ctx, service)
	if err != nil || !exists {
		t.Fatalf("read after concurrent updates: %v", err)
	}
	matched := false
	for _, value := range values {
		matched = matched || bytes.Equal(got, value)
	}
	if !matched {
		t.Fatal("concurrent updates left a value nobody wrote")
	}
}

// The whole switch and refresh path against the real Keychain, with a login
// too large for the command line.
func TestRealKeychainManagerSwitchAndRefresh(t *testing.T) {
	service := realKeychain(t)
	ctx := context.Background()
	k := SystemKeychain{}
	t.Cleanup(func() { _ = k.Delete(context.Background(), service) })

	h := newHarness(t, true)
	h.paths.Service = service
	h.manager.paths.Service = service
	h.manager.keys = k

	live := h.keys.values["Claude Code-credentials"]
	if err := k.Write(ctx, service, live); err != nil {
		t.Fatalf("seed through /usr/bin/security: %v", err)
	}
	fields, _ := object(live)
	fields["mcpOAuth"] = encoded(strings.Repeat("fixture-large-shared-field", 400))
	oversized := encoded(fields)
	if _, err := keychainWriteLine(service, oversized); !errors.Is(err, errKeychainLineTooLong) {
		t.Fatal("fixture is not oversized")
	}
	if err := k.Write(ctx, service, oversized); err != nil {
		t.Fatalf("seed oversized login: %v", err)
	}
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: oversized, Exists: true}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"b", "a"} {
		commits := 0
		result, err := h.manager.Switch(ctx, "claude-"+name, func() error { commits++; return nil })
		if err != nil {
			t.Fatalf("switch to %s: %v", name, err)
		}
		if !result.Changed || commits != 1 {
			t.Fatalf("switch to %s: %+v", name, result)
		}
		raw, exists, err := k.Read(ctx, service)
		if err != nil || !exists {
			t.Fatalf("read after switch to %s: %v", name, err)
		}
		oauth, _ := parseOAuth(raw)
		now, _ := object(raw)
		if oauth.AccessToken != "at-"+name || !bytes.Equal(now["mcpOAuth"], fields["mcpOAuth"]) {
			t.Fatalf("switch to %s did not reach the Keychain intact", name)
		}
		if _, err := keychainWriteLine(service, raw); !errors.Is(err, errKeychainLineTooLong) {
			t.Fatal("the switched login is no longer oversized")
		}
	}

	raw, _, _ := k.Read(ctx, service)
	credential, _ := object(raw)
	oauth, _ := object(credential["claudeAiOauth"])
	oauth["expiresAt"] = encoded(int64(1000))
	credential["claudeAiOauth"] = encoded(oauth)
	expired := encoded(credential)
	if err := k.Write(ctx, service, expired); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: expired, Exists: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := h.accounts.Get("claude-a")
	if err := h.manager.Refresh(ctx, &a, func(ctx context.Context, a *store.Account) error { return h.manager.grant(ctx, a) }); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	raw, _, _ = k.Read(ctx, service)
	refreshed, _ := parseOAuth(raw)
	now, _ := object(raw)
	if refreshed.RefreshToken != "rt-successor" || h.grants != 1 || !bytes.Equal(now["mcpOAuth"], fields["mcpOAuth"]) {
		t.Fatalf("refresh did not reach the Keychain intact: grants=%d", h.grants)
	}
}

// With the keychain locked there is nobody to answer a dialog: the native
// writer and the preflight must fail quickly instead of waiting for one.
func TestRealKeychainLockedFailsFast(t *testing.T) {
	if os.Getenv("SWITCHER_REAL_KEYCHAIN_LOCKED") == "" {
		t.Skip("needs the workflow to lock the keychain")
	}
	service := os.Getenv("SWITCHER_REAL_KEYCHAIN_LOCKED")
	done := make(chan error, 1)
	go func() {
		if err := (SystemKeychain{}).Validate(context.Background(), service, printable("locked", 9000), fileValue{Data: []byte("small"), Exists: true}); err == nil {
			done <- nil
			return
		}
		done <- updateExistingKeychainItem(context.Background(), keychainUser(), service, printable("locked", 9000))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a locked keychain accepted the update")
		}
		t.Logf("locked keychain: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the native writer waited on a locked keychain")
	}
}

// One native update of an item the workflow created, so the workflow can
// compare the item's access control list before and after.
func TestRealKeychainUpdateNamed(t *testing.T) {
	service := os.Getenv("SWITCHER_REAL_KEYCHAIN_UPDATE")
	if service == "" {
		t.Skip("needs the workflow to name an item")
	}
	value := printable("named", 9000)
	if err := (SystemKeychain{}).Write(context.Background(), service, value); err != nil {
		t.Fatal(err)
	}
}
