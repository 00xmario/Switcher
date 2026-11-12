package claudecode

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

type Keychain interface {
	Read(context.Context, string) ([]byte, bool, error)
	Write(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type SystemKeychain struct {
	// Only tests replace Runner; production pins Apple's system executable.
	Runner func(context.Context, []string, []byte) ([]byte, error)
	// Only tests set Update; production updates through Security.framework.
	Update func(ctx context.Context, account, service string, value []byte) error
}

// updater is the writer for records too large for the command line, if this
// build has one. A test's Runner alone stands for a build without it.
func (k SystemKeychain) updater() func(context.Context, string, string, []byte) error {
	if k.Update != nil {
		return k.Update
	}
	if nativeKeychainUpdate && k.Runner == nil {
		return updateExistingKeychainItem
	}
	return nil
}

func keychainUser() string {
	if value := os.Getenv("USER"); value != "" {
		return value
	}
	if current, err := user.Current(); err == nil {
		return current.Username
	}
	return "claude-code-user"
}

func (k SystemKeychain) run(ctx context.Context, args []string, input []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if k.Runner != nil {
		return k.Runner(ctx, args, input)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

func missingKey(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == 44
}

func (k SystemKeychain) Read(ctx context.Context, service string) ([]byte, bool, error) {
	return k.read(ctx, service, 5*time.Second)
}

func (k SystemKeychain) read(ctx context.Context, service string, timeout time.Duration) ([]byte, bool, error) {
	value, err := k.run(ctx, []string{"find-generic-password", "-a", keychainUser(), "-s", service, "-w"}, nil, timeout)
	if missingKey(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("Claude Code Keychain is locked, denied, or unavailable; no credential fallback was used")
	}
	return bytes.TrimSuffix(value, []byte{'\n'}), true, nil
}

func securityQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

var errKeychainLineTooLong = errors.New("native credentials exceed the secure Keychain stdin limit; use Claude Code login for this profile")

var errKeychainItemMissing = errors.New("no existing Claude Code Keychain item; sign in with Claude Code first")

func keychainWriteLine(service string, value []byte) (string, error) {
	if strings.ContainsAny(service+keychainUser(), "\n\r\x00") {
		return "", errors.New("invalid native Keychain item identifier")
	}
	prefix := fmt.Sprintf("add-generic-password -U -a %s -s %s -X ", securityQuote(keychainUser()), securityQuote(service))
	// Unlike upstream's long-payload fallback, never put OAuth/MCP secrets in
	// process arguments. security -i would truncate its fixed-size line
	// buffer, so a longer record goes to the native writer or is refused.
	if len(prefix)+hex.EncodedLen(len(value))+1 > 4032 {
		return "", errKeychainLineTooLong
	}
	return prefix + hex.EncodeToString(value) + "\n", nil
}

func (k SystemKeychain) writeLine(service string, value []byte) (string, error) {
	if len(value) > nativeFileLimit {
		return "", errors.New("native credentials exceed the supported size")
	}
	line, err := keychainWriteLine(service, value)
	if k.updater() != nil && errors.Is(err, errKeychainLineTooLong) {
		return "", nil // Use the native writer when the command cannot fit.
	}
	return line, err
}

// Validate says, before a refresh spends its grant, whether next can be
// written over current. A record too large for the command line needs the
// native writer, which only updates an item that exists, and only when macOS
// lets this build of Switcher do so. Writing the bytes already there finds
// that out while the grant is still unspent.
func (k SystemKeychain) Validate(ctx context.Context, service string, next []byte, current fileValue) error {
	line, err := k.writeLine(service, next)
	if err != nil || line != "" {
		return err
	}
	if !current.Exists {
		return errKeychainItemMissing
	}
	if len(current.Data) == 0 {
		return nil // Nothing to write back; the write itself will say.
	}
	return k.updater()(ctx, keychainUser(), service, current.Data)
}

func (k SystemKeychain) Write(ctx context.Context, service string, value []byte) error {
	line, err := k.writeLine(service, value)
	if err != nil {
		return err
	}
	readTimeout := 5 * time.Second
	if line == "" {
		readTimeout = 12 * time.Second
		if err := k.updater()(ctx, keychainUser(), service, value); err != nil {
			return err
		}
	} else if _, err := k.run(ctx, []string{"-i"}, []byte(line), 5*time.Second); err != nil {
		return errors.New("could not write Claude Code Keychain credentials")
	}
	got, exists, err := k.read(ctx, service, readTimeout)
	if err != nil || !exists || !bytes.Equal(got, value) {
		return errors.New("Claude Code Keychain write did not verify")
	}
	return nil
}

func (k SystemKeychain) Delete(ctx context.Context, service string) error {
	_, err := k.run(ctx, []string{"delete-generic-password", "-a", keychainUser(), "-s", service}, nil, 5*time.Second)
	if err != nil && !missingKey(err) {
		return errors.New("could not clear conflicting native Claude API key")
	}
	return nil
}
