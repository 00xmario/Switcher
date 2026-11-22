package antigravity

// The Antigravity CLI (agy) and the Antigravity app keep one Google sign-in,
// in the system keychain: a generic password with service "gemini" and
// account "antigravity" (go-keyring), its value a small JSON document. Where
// no keychain answers, agy keeps the same document in a file instead.
// Switcher writes an account there only when the user picks it ("Use in
// Antigravity"), never by itself: Google suspends Antigravity accounts it
// sees rotated automatically or used through other tools, so Switcher
// neither routes Antigravity requests nor switches it on its own.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"switcher/internal/config"
	"switcher/internal/provider"
	"switcher/internal/store"
)

const (
	keyringService = "gemini"
	keyringAccount = "antigravity"
	// goKeyringPrefix marks a value go-keyring stored base64-encoded on macOS.
	goKeyringPrefix = "go-keyring-base64:"
)

// appToken is the document agy and the app read: the OAuth token, how it was
// obtained ("consumer": a personal Google sign-in) and whose it is.
type appToken struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
		ExpiresIn    int64  `json:"expires_in,omitempty"`
	} `json:"token"`
	AuthMethod   string `json:"auth_method"`
	IDTokenEmail string `json:"id_token_email,omitempty"`
}

// appTokenOf builds the document for an account.
func appTokenOf(a store.Account) ([]byte, error) {
	if a.Token.AccessToken == "" || a.Token.RefreshToken == "" || a.Email == "" {
		return nil, errors.New("this account has no complete sign-in; sign in to it again in Switcher")
	}
	var doc appToken
	doc.Token.AccessToken = a.Token.AccessToken
	doc.Token.TokenType = "Bearer"
	doc.Token.RefreshToken = a.Token.RefreshToken
	expiry := time.Now().Add(time.Hour)
	if a.Token.ExpiresAt > 0 {
		expiry = time.Unix(a.Token.ExpiresAt, 0)
	}
	doc.Token.Expiry = expiry.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	if left := int64(time.Until(expiry).Seconds()); left > 0 {
		doc.Token.ExpiresIn = left
	}
	doc.AuthMethod = "consumer"
	doc.IDTokenEmail = a.Email
	return json.Marshal(doc)
}

// emailOf reads whose sign-in a stored document is.
func emailOf(raw []byte) string {
	var doc appToken
	if json.Unmarshal(raw, &doc) != nil || doc.Token.RefreshToken == "" {
		return ""
	}
	return doc.IDTokenEmail
}

// credentialStore is one place agy reads its sign-in from.
type credentialStore interface {
	// read gives the stored document; ok is false when there is none.
	read(ctx context.Context) (raw []byte, ok bool, err error)
	write(ctx context.Context, raw []byte) error
	name() string
}

// appStores are the stores of this computer, the keychain first. A test
// replaces it.
var appStores = func() (keychain credentialStore, files []credentialStore) {
	home, _ := os.UserHomeDir()
	gemini := filepath.Join(home, ".gemini")
	files = []credentialStore{
		tokenFile{path: filepath.Join(gemini, "antigravity-cli", "antigravity-oauth-token")},
		tokenFile{path: filepath.Join(gemini, "jetski-standalone-oauth-token")},
	}
	switch runtime.GOOS {
	case "darwin":
		return macKeychain{}, files
	case "linux":
		return secretService{}, files
	}
	return nil, files
}

// AppLogin implements provider.AppLoginProvider: the email agy and the
// Antigravity app are signed in with, or "" when they are not, or Switcher
// cannot tell.
func (p *Provider) AppLogin(ctx context.Context) (string, error) {
	keychain, files := appStores()
	if keychain != nil {
		raw, ok, err := keychain.read(ctx)
		if err == nil && ok {
			return emailOf(raw), nil
		}
		if err == nil {
			// Signed out, or never signed in, on a keychain that answers:
			// agy does not read its file then.
			return "", nil
		}
	}
	// No keychain answers: agy reads its file.
	if raw, ok, err := files[0].read(ctx); err == nil && ok {
		return emailOf(raw), nil
	}
	return "", nil
}

// UseInApps implements provider.AppLoginProvider: it signs agy and the
// Antigravity app in with the account, in the keychain, and in agy's file
// when one is already there or no keychain answers. a must carry a fresh
// access token, granted what agy needs (aicode): an account added before
// Switcher asked for it is refused, not handed over to fail in agy. The
// sign-in it replaces, when it is another account's, is kept in
// backupPath, and a write that fails halfway puts the keychain back.
func (p *Provider) UseInApps(ctx context.Context, a store.Account) error {
	raw, err := appTokenOf(a)
	if err != nil {
		return err
	}
	if err := checkAgyScope(ctx, a.Token.AccessToken); err != nil {
		return err
	}
	keychain, files := appStores()
	var previous []byte
	hadKeychain := false
	if keychain != nil {
		old, ok, err := keychain.read(ctx)
		switch {
		case err == nil:
			previous, hadKeychain = old, ok
		case !errors.Is(err, errNoKeychain):
			return err
		default:
			keychain = nil
		}
	}
	if previous == nil {
		previous, _, _ = files[0].read(ctx)
	}
	if who := emailOf(previous); who != "" && who != a.Email {
		if err := (tokenFile{path: backupPath()}).write(ctx, previous); err != nil {
			return fmt.Errorf("could not keep the sign-in being replaced: %w", err)
		}
	}
	if keychain != nil {
		if err := keychain.write(ctx, raw); err != nil {
			return err
		}
		got, ok, rerr := keychain.read(ctx)
		if rerr != nil || !ok || emailOf(got) != a.Email || !bytes.Contains(got, []byte(a.Token.RefreshToken)) {
			restore(ctx, keychain, previous, hadKeychain)
			return fmt.Errorf("the %s did not keep the new sign-in", keychain.name())
		}
	}
	for i, f := range files {
		// Something unreadable there counts as there: it is written over,
		// or the switch fails and is undone.
		_, exists, rerr := f.read(ctx)
		if !exists && rerr == nil && (keychain != nil || i > 0) {
			continue
		}
		if err := f.write(ctx, raw); err != nil {
			if keychain != nil {
				restore(ctx, keychain, previous, hadKeychain)
			}
			return err
		}
	}
	return nil
}

// restore puts the keychain's earlier sign-in back after a failed switch.
func restore(ctx context.Context, keychain credentialStore, previous []byte, had bool) {
	if had && len(previous) > 0 {
		_ = keychain.write(ctx, previous)
	}
}

// backupPath is where the sign-in a switch replaced is kept, when it was
// another account's. A test replaces it.
var backupPath = func() string { return filepath.Join(config.Dir(), "antigravity-previous-signin.json") }

// agyScope is the access agy needs from a sign-in.
const agyScope = "https://www.googleapis.com/auth/aicode"

// tokenInfoURL answers which scopes an access token carries.
var tokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"

// errNeedsAgyScope refuses an account signed in before Switcher asked for
// agy's access.
var errNeedsAgyScope = errors.New("this account was signed in to Switcher before Switcher asked for what agy needs; sign in to it again in Switcher (Relogin in its menu), then use it in Antigravity")

// checkAgyScope makes sure the access token carries agy's scope. The token
// goes in the request body, never the URL.
func checkAgyScope(ctx context.Context, accessToken string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenInfoURL, strings.NewReader(url.Values{"access_token": {accessToken}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not check the account's access: %w", err)
	}
	defer resp.Body.Close()
	var info struct {
		Scope string `json:"scope"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info) != nil {
		return fmt.Errorf("could not check the account's access: http %d", resp.StatusCode)
	}
	for _, scope := range strings.Fields(info.Scope) {
		if scope == agyScope {
			return nil
		}
	}
	return errNeedsAgyScope
}

// errNoKeychain says this computer has no keychain Switcher can reach, such
// as a Linux machine without a desktop session: agy reads its file then.
var errNoKeychain = errors.New("no keychain")

// tokenFile is agy's file fallback, kept private to the user.
type tokenFile struct{ path string }

func (f tokenFile) name() string { return f.path }

func (f tokenFile) read(context.Context) ([]byte, bool, error) {
	raw, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (f tokenFile) write(_ context.Context, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("could not write %s: %w", f.path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".switcher-*")
	if err != nil {
		return fmt.Errorf("could not write %s: %w", f.path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), f.path)
		}
		if err == nil {
			return nil
		}
	} else {
		tmp.Close()
	}
	return fmt.Errorf("could not write %s: %w", f.path, err)
}

// macKeychain is the login keychain, through Apple's security tool. The
// secret goes in on standard input (security -i), never in the arguments,
// and an item agy made keeps its access rules (-U updates it in place).
type macKeychain struct {
	// run is a test seam; production runs /usr/bin/security.
	run func(ctx context.Context, args []string, input []byte) ([]byte, error)
}

func (macKeychain) name() string { return "macOS Keychain" }

func (k macKeychain) exec(ctx context.Context, args []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if k.run != nil {
		return k.run(ctx, args, input)
	}
	return runSecurity(ctx, args, input)
}

func (k macKeychain) read(ctx context.Context) ([]byte, bool, error) {
	out, err := k.exec(ctx, []string{"find-generic-password", "-a", keyringAccount, "-s", keyringService, "-w"}, nil)
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == 44 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("the macOS Keychain is locked or refused")
	}
	value := strings.TrimSpace(string(out))
	if strings.HasPrefix(value, goKeyringPrefix) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, goKeyringPrefix))
		if err != nil {
			return nil, false, errors.New("the Antigravity sign-in in the macOS Keychain is unreadable")
		}
		return raw, true, nil
	}
	return []byte(value), true, nil
}

func (k macKeychain) write(ctx context.Context, raw []byte) error {
	value := goKeyringPrefix + base64.StdEncoding.EncodeToString(raw)
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", keyringAccount, keyringService, hex.EncodeToString([]byte(value)))
	// security -i reads a line of at most about 4 KB.
	if len(line) > 4000 {
		return errors.New("the Antigravity sign-in is too large for the macOS Keychain tool")
	}
	if _, err := k.exec(ctx, []string{"-i"}, []byte(line)); err != nil {
		return errors.New("could not write the Antigravity sign-in to the macOS Keychain")
	}
	return nil
}
