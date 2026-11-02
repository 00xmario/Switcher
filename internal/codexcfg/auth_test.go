package codexcfg

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteLoginNeverReplacesCodexsOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if login, err := ReadLogin(path); err != nil || login.Present || login.Store != "file" {
		t.Fatalf("no login yet: %+v %v", login, err)
	}
	written, err := WriteLogin(path, AuthTokens{IDToken: "i", AccessToken: "a", RefreshToken: "r", AccountID: "acct"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	login, err := ReadLogin(path)
	tokens, fresh, ok := login.Tokens()
	if err != nil || !login.Present || !ok || tokens.RefreshToken != "r" || login.AccountID != "acct" || fresh.IsZero() {
		t.Fatalf("written login: %+v %+v %v", login, tokens, err)
	}
	if _, err := WriteLogin(path, AuthTokens{AccessToken: "b", RefreshToken: "s"}, time.Now()); err == nil {
		t.Fatal("replaced an existing auth.json")
	}
	// Codex refreshed it: it is Codex's now, and stays.
	os.WriteFile(AuthPath(path), []byte(`{"tokens":{"access_token":"a2","refresh_token":"r2"}}`), 0o600)
	if err := RemoveLoginIfUnchanged(path, written); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AuthPath(path)); err != nil {
		t.Fatal("removed a login Codex had changed")
	}
}

func TestRemoveLoginIfUnchangedRemovesOnlyWhatSwitcherWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	written, err := WriteLogin(path, AuthTokens{AccessToken: "a", RefreshToken: "r"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveLoginIfUnchanged(path, written); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AuthPath(path)); !os.IsNotExist(err) {
		t.Fatal("auth.json stayed")
	}
}

func TestWriteLoginNeedsCodexsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-ran", "config.toml")
	if _, err := WriteLogin(path, AuthTokens{AccessToken: "a", RefreshToken: "r"}, time.Now()); !errors.Is(err, ErrNoCodex) {
		t.Fatalf("no Codex: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("created Codex's directory")
	}
}

// A login read can be removed while the file is still what was read.
func TestALoginReadCanBeRemovedUntilCodexRewritesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(AuthPath(path), []byte(`{"tokens":{"access_token":"a2","refresh_token":"r2","account_id":"acct"}}`), 0o600)
	login, err := ReadLogin(path)
	if err != nil || login.Digest() == "" || login.APIKey {
		t.Fatalf("read: %+v %v", login, err)
	}
	os.WriteFile(AuthPath(path), []byte(`{"tokens":{"access_token":"a3","refresh_token":"r3","account_id":"acct"}}`), 0o600)
	if err := RemoveLoginIfUnchanged(path, login.Digest()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AuthPath(path)); err != nil {
		t.Fatal("removed a login Codex rewrote after it was read")
	}
	login, _ = ReadLogin(path)
	if err := RemoveLoginIfUnchanged(path, login.Digest()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AuthPath(path)); !os.IsNotExist(err) {
		t.Fatal("the login read stayed")
	}
	os.WriteFile(AuthPath(path), []byte(`{"OPENAI_API_KEY":"sk-x"}`), 0o600)
	if login, _ := ReadLogin(path); !login.APIKey {
		t.Fatal("an API key went unnoticed")
	}
}

func TestReadLoginKeepsWhatItDoesNotUnderstand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, content := range []string{`not json`, `{"OPENAI_API_KEY":"sk-x"}`} {
		os.WriteFile(AuthPath(path), []byte(content), 0o600)
		if login, err := ReadLogin(path); err != nil || !login.Present {
			t.Fatalf("%s: %+v %v", content, login, err)
		}
	}
	os.WriteFile(path, []byte("cli_auth_credentials_store = \"auto\"\n"), 0o600)
	os.Remove(AuthPath(path))
	if login, _ := ReadLogin(path); !login.Present || login.Store != "auto" {
		t.Fatalf("keyring login: %+v", login)
	}
}
