package antigravity

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/store"
)

// memoryKeychain is a keychain held in memory; down makes it unreachable.
type memoryKeychain struct {
	value []byte
	down  bool
}

func (k *memoryKeychain) name() string { return "test keychain" }
func (k *memoryKeychain) read(context.Context) ([]byte, bool, error) {
	if k.down {
		return nil, false, errNoKeychain
	}
	return k.value, k.value != nil, nil
}
func (k *memoryKeychain) write(_ context.Context, raw []byte) error {
	if k.down {
		return errNoKeychain
	}
	k.value = append([]byte(nil), raw...)
	return nil
}

// grantScopes answers tokeninfo with these scopes, and keeps backups in a
// temporary folder.
func grantScopes(t *testing.T, scopes string) (backup string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.FormValue("access_token") == "" {
			t.Errorf("tokeninfo asked as %s %s", r.Method, r.URL)
		}
		fmt.Fprintf(w, `{"scope":%q}`, scopes)
	}))
	t.Cleanup(srv.Close)
	savedURL, savedBackup := tokenInfoURL, backupPath
	backup = filepath.Join(t.TempDir(), "previous.json")
	tokenInfoURL, backupPath = srv.URL, func() string { return backup }
	t.Cleanup(func() { tokenInfoURL, backupPath = savedURL, savedBackup })
	return backup
}

func useStores(t *testing.T, keychain credentialStore) (agyFile, appFile string) {
	t.Helper()
	grantScopes(t, "openid "+agyScope)
	dir := t.TempDir()
	agyFile, appFile = filepath.Join(dir, "antigravity-cli", "antigravity-oauth-token"), filepath.Join(dir, "jetski-standalone-oauth-token")
	saved := appStores
	appStores = func() (credentialStore, []credentialStore) {
		return keychain, []credentialStore{tokenFile{path: agyFile}, tokenFile{path: appFile}}
	}
	t.Cleanup(func() { appStores = saved })
	return agyFile, appFile
}

func appAccount(email string) store.Account {
	return store.Account{ID: "antigravity-1", Provider: "antigravity", Email: email,
		Token: store.Token{AccessToken: "at-" + email, RefreshToken: "rt-" + email, ExpiresAt: time.Now().Add(50 * time.Minute).Unix()}}
}

// The document is the one agy writes: the token, "consumer" and the email.
func TestAppTokenIsWhatAgyReads(t *testing.T) {
	raw, err := appTokenOf(appAccount("me@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	token, _ := doc["token"].(map[string]any)
	if doc["auth_method"] != "consumer" || doc["id_token_email"] != "me@example.com" || token["access_token"] != "at-me@example.com" ||
		token["refresh_token"] != "rt-me@example.com" || token["token_type"] != "Bearer" {
		t.Fatalf("document %s", raw)
	}
	if _, err := time.Parse(time.RFC3339Nano, token["expiry"].(string)); err != nil {
		t.Fatalf("expiry %v: %v", token["expiry"], err)
	}
	if _, err := appTokenOf(store.Account{Email: "x@example.com", Token: store.Token{AccessToken: "at"}}); err == nil {
		t.Fatal("an account without a refresh token was handed out")
	}
}

// With a keychain, the sign-in goes there, and to agy's file only when one
// is already there; whom the apps use reads back.
func TestUseInAppsWritesTheKeychain(t *testing.T) {
	keychain := &memoryKeychain{}
	agyFile, appFile := useStores(t, keychain)
	p := New()
	if who, err := p.AppLogin(context.Background()); err != nil || who != "" {
		t.Fatalf("before: %q %v", who, err)
	}
	if err := p.UseInApps(context.Background(), appAccount("me@example.com")); err != nil {
		t.Fatal(err)
	}
	if who, _ := p.AppLogin(context.Background()); who != "me@example.com" {
		t.Fatalf("apps use %q", who)
	}
	for _, f := range []string{agyFile, appFile} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was created beside a keychain: %v", f, err)
		}
	}
	// A file agy already keeps is kept in step, privately.
	if err := os.MkdirAll(filepath.Dir(agyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.UseInApps(context.Background(), appAccount("work@example.com")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(agyFile)
	info, _ := os.Stat(agyFile)
	if err != nil || emailOf(raw) != "work@example.com" || info.Mode().Perm() != 0o600 {
		t.Fatalf("agy's file: %s %v %v", raw, err, info.Mode())
	}
	if who, _ := p.AppLogin(context.Background()); who != "work@example.com" {
		t.Fatalf("apps use %q", who)
	}
}

// Without a keychain agy reads its file: the sign-in goes there.
func TestUseInAppsWithoutAKeychainWritesAgysFile(t *testing.T) {
	agyFile, appFile := useStores(t, &memoryKeychain{down: true})
	p := New()
	if err := p.UseInApps(context.Background(), appAccount("me@example.com")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(agyFile)
	if err != nil || emailOf(raw) != "me@example.com" {
		t.Fatalf("agy's file: %s %v", raw, err)
	}
	if _, err := os.Stat(appFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the app's file was created: %v", err)
	}
	if who, _ := p.AppLogin(context.Background()); who != "me@example.com" {
		t.Fatalf("apps use %q", who)
	}
}

// The macOS Keychain gets go-keyring's base64 value through standard input,
// never the arguments, and reads it back; a missing item is no sign-in.
func TestMacKeychain(t *testing.T) {
	var stored string
	var calls [][]string
	k := macKeychain{run: func(_ context.Context, args []string, input []byte) ([]byte, error) {
		calls = append(calls, args)
		switch args[0] {
		case "-i":
			fields := strings.Fields(string(input))
			if len(fields) != 8 || fields[0] != "add-generic-password" || fields[1] != "-U" || fields[3] != keyringAccount || fields[5] != keyringService || fields[6] != "-X" {
				t.Fatalf("security -i line %q", input)
			}
			value, err := hex.DecodeString(fields[7])
			if err != nil {
				t.Fatal(err)
			}
			stored = string(value)
			return nil, nil
		case "find-generic-password":
			if stored == "" {
				return nil, exitCode(44)
			}
			return []byte(stored + "\n"), nil
		}
		t.Fatalf("unexpected security %v", args)
		return nil, nil
	}}
	if _, ok, err := k.read(context.Background()); ok || err != nil {
		t.Fatalf("missing item: %v %v", ok, err)
	}
	raw, _ := appTokenOf(appAccount("me@example.com"))
	if err := k.write(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, goKeyringPrefix) {
		t.Fatalf("stored %q", stored)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, goKeyringPrefix)); string(decoded) != string(raw) {
		t.Fatal("stored value does not decode to the document")
	}
	got, ok, err := k.read(context.Background())
	if err != nil || !ok || emailOf(got) != "me@example.com" {
		t.Fatalf("read back: %s %v %v", got, ok, err)
	}
	for _, args := range calls {
		if strings.Contains(strings.Join(args, " "), "rt-me@example.com") || strings.Contains(strings.Join(args, " "), hex.EncodeToString([]byte(stored))) {
			t.Fatalf("the secret went into the arguments: %v", args)
		}
	}
}

type exitCode int

func (e exitCode) Error() string { return "exit" }
func (e exitCode) ExitCode() int { return int(e) }

// An account without agy's access is refused before anything is written.
func TestUseInAppsRefusesAnAccountWithoutAgysAccess(t *testing.T) {
	keychain := &memoryKeychain{}
	useStores(t, keychain)
	grantScopes(t, "openid https://www.googleapis.com/auth/cloud-platform")
	if err := New().UseInApps(context.Background(), appAccount("me@example.com")); !errors.Is(err, errNeedsAgyScope) {
		t.Fatalf("err = %v", err)
	}
	if keychain.value != nil {
		t.Fatal("wrote a sign-in agy cannot use")
	}
}

// The sign-in replaced, another account's, is kept; a write that fails
// halfway puts the keychain back.
func TestUseInAppsKeepsAndRestoresThePreviousSignIn(t *testing.T) {
	keychain := &memoryKeychain{}
	agyFile, _ := useStores(t, keychain)
	backup := grantScopes(t, agyScope)
	p := New()
	if err := p.UseInApps(context.Background(), appAccount("mine@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("kept a backup with nothing replaced")
	}
	if err := p.UseInApps(context.Background(), appAccount("work@example.com")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backup)
	info, _ := os.Stat(backup)
	if err != nil || emailOf(raw) != "mine@example.com" || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup %s %v", raw, err)
	}
	// agy's file is there but cannot be written: the keychain goes back.
	if err := os.MkdirAll(agyFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.UseInApps(context.Background(), appAccount("third@example.com")); err == nil {
		t.Fatal("a failed file write reported success")
	}
	if who := emailOf(keychain.value); who != "work@example.com" {
		t.Fatalf("keychain left on %q", who)
	}
}
