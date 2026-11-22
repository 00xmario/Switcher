package antigravity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/godbus/dbus/v5"
)

// runSecurity runs Apple's security tool with input on standard input.
func runSecurity(ctx context.Context, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

// secretService is the Linux keyring (GNOME Keyring, KWallet) over D-Bus,
// the way go-keyring, and so agy, uses it: the "login" collection (else the
// default one), an item with attributes service and username, a plain text
// secret. Switcher never answers a keyring prompt: a locked keyring is an
// error the user sees, not a dialog it waits on.
type secretService struct{}

const (
	secretsName       = "org.freedesktop.secrets"
	secretsPath       = dbus.ObjectPath("/org/freedesktop/secrets")
	loginCollection   = dbus.ObjectPath("/org/freedesktop/secrets/collection/login")
	defaultCollection = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
	secretCallTimeout = 10 * time.Second
)

// secret is the Secret Service's (oa{say}s) secret struct.
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func (secretService) name() string { return "keyring" }

// open connects and opens a plain session: the secret travels over the
// user's own session bus, as go-keyring sends it.
func (secretService) open(ctx context.Context) (*dbus.Conn, dbus.ObjectPath, dbus.BusObject, error) {
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, "", nil, errNoKeychain
	}
	service := conn.Object(secretsName, secretsPath)
	var output dbus.Variant
	var session dbus.ObjectPath
	if err := service.CallWithContext(ctx, "org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &session); err != nil {
		conn.Close()
		return nil, "", nil, errNoKeychain
	}
	collection := conn.Object(secretsName, loginCollection)
	if _, err := collection.GetProperty("org.freedesktop.Secret.Collection.Label"); err != nil {
		collection = conn.Object(secretsName, defaultCollection)
	}
	return conn, session, collection, nil
}

// locked reports whether the collection is locked.
func locked(collection dbus.BusObject) bool {
	v, err := collection.GetProperty("org.freedesktop.Secret.Collection.Locked")
	if err != nil {
		return false
	}
	isLocked, _ := v.Value().(bool)
	return isLocked
}

func (s secretService) read(ctx context.Context) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, secretCallTimeout)
	defer cancel()
	conn, session, collection, err := s.open(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	if locked(collection) {
		return nil, false, errors.New("the keyring is locked; unlock it (sign in to the desktop) and try again")
	}
	var items []dbus.ObjectPath
	attributes := map[string]string{"service": keyringService, "username": keyringAccount}
	if err := collection.CallWithContext(ctx, "org.freedesktop.Secret.Collection.SearchItems", 0, attributes).Store(&items); err != nil {
		return nil, false, fmt.Errorf("the keyring did not answer: %w", err)
	}
	if len(items) == 0 {
		return nil, false, nil
	}
	var got secret
	if err := conn.Object(secretsName, items[0]).CallWithContext(ctx, "org.freedesktop.Secret.Item.GetSecret", 0, session).Store(&got); err != nil {
		return nil, false, fmt.Errorf("the keyring did not give the sign-in: %w", err)
	}
	return got.Value, true, nil
}

func (s secretService) write(ctx context.Context, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, secretCallTimeout)
	defer cancel()
	conn, session, collection, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if locked(collection) {
		return errors.New("the keyring is locked; unlock it (sign in to the desktop) and try again")
	}
	properties := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label":      dbus.MakeVariant(fmt.Sprintf("Password for '%s' on '%s'", keyringAccount, keyringService)),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(map[string]string{"service": keyringService, "username": keyringAccount}),
	}
	value := secret{Session: session, Parameters: []byte{}, Value: raw, ContentType: "text/plain; charset=utf8"}
	var item, prompt dbus.ObjectPath
	if err := collection.CallWithContext(ctx, "org.freedesktop.Secret.Collection.CreateItem", 0, properties, value, true).Store(&item, &prompt); err != nil {
		return fmt.Errorf("could not write the Antigravity sign-in to the keyring: %w", err)
	}
	if prompt != "/" {
		return errors.New("the keyring asked for confirmation; unlock it and try again")
	}
	return nil
}
