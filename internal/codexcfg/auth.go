package codexcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Codex keeps its own login in auth.json beside config.toml, unless
// cli_auth_credentials_store sends it to the system keyring (Keychain on
// macOS, Secret Service on Linux). Switcher writes that file only when Codex
// has no login at all, so a tool handed back to OpenAI can still start.

// AuthTokens is a ChatGPT login in Codex's auth.json shape.
type AuthTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// Login describes Codex's own login, without exposing it.
type Login struct {
	// Store is where Codex keeps it: "file", "keyring" or "auto".
	Store string `json:"store"`
	// Present reports a login Codex can use: tokens or an API key.
	Present bool `json:"present"`
	// AccountID is the ChatGPT account of a token login.
	AccountID string `json:"account_id,omitempty"`
	tokens    *AuthTokens
	lastFresh time.Time
}

// Tokens returns the login's tokens, if it is a ChatGPT login.
func (l Login) Tokens() (AuthTokens, time.Time, bool) {
	if l.tokens == nil {
		return AuthTokens{}, time.Time{}, false
	}
	return *l.tokens, l.lastFresh, true
}

type authFile struct {
	AuthMode     string      `json:"auth_mode,omitempty"`
	OpenAIAPIKey *string     `json:"OPENAI_API_KEY"`
	Tokens       *AuthTokens `json:"tokens,omitempty"`
	LastRefresh  string      `json:"last_refresh,omitempty"`
}

// AuthPath is Codex's auth.json for the given config.toml.
func AuthPath(configPath string) string { return filepath.Join(filepath.Dir(configPath), "auth.json") }

// AuthStore reads where Codex keeps its login. A missing or unreadable
// setting is Codex's default, the file.
func AuthStore(configPath string) string {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "file"
	}
	var cfg struct {
		Store string `toml:"cli_auth_credentials_store"`
	}
	if toml.Unmarshal(raw, &cfg) != nil {
		return "file"
	}
	switch cfg.Store {
	case "keyring", "auto":
		return cfg.Store
	}
	return "file"
}

// ReadLogin inspects Codex's own login. A login kept in the keyring is not
// read: Present is then true because Switcher cannot tell, and leaves it be.
func ReadLogin(configPath string) (Login, error) {
	login := Login{Store: AuthStore(configPath)}
	if login.Store != "file" {
		login.Present = true
		return login, nil
	}
	raw, err := readSmall(AuthPath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return login, nil
	}
	if err != nil {
		return login, err
	}
	var f authFile
	if len(bytes.TrimSpace(raw)) == 0 {
		return login, nil
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		// Something Codex wrote that Switcher does not understand is still
		// Codex's: never replace it.
		login.Present = true
		return login, nil
	}
	if f.OpenAIAPIKey != nil && *f.OpenAIAPIKey != "" {
		login.Present = true
	}
	if f.Tokens != nil && f.Tokens.RefreshToken != "" {
		login.Present = true
		tokens := *f.Tokens
		login.tokens, login.AccountID = &tokens, tokens.AccountID
		login.lastFresh, _ = time.Parse(time.RFC3339Nano, f.LastRefresh)
	}
	return login, nil
}

// WriteLogin creates auth.json with a ChatGPT login and returns the digest of
// what it wrote. It never replaces an existing file.
func WriteLogin(configPath string, tokens AuthTokens, now time.Time) (string, error) {
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return "", errors.New("the account has no complete login to hand to Codex")
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return "", errors.New("the Codex directory is not a plain directory")
	}
	raw, err := json.MarshalIndent(authFile{AuthMode: "chatgpt", Tokens: &tokens, LastRefresh: now.UTC().Format(time.RFC3339Nano)}, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(AuthPath(configPath), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(AuthPath(configPath))
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return digest(raw), nil
}

// RemoveLoginIfUnchanged deletes an auth.json Switcher wrote, but only while
// it still holds exactly what Switcher wrote.
func RemoveLoginIfUnchanged(configPath, written string) error {
	raw, err := readSmall(AuthPath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if digest(raw) != written {
		return nil
	}
	return os.Remove(AuthPath(configPath))
}

func readSmall(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err == nil && len(raw) > 1<<20 {
		return nil, errors.New("Codex auth.json is too large")
	}
	return raw, err
}
