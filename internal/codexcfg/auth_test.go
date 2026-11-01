package codexcfg

import (
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
