package server

import (
	"os"
	"path/filepath"
	"testing"
)

// The LAN listener's key is this user's alone, also when it replaces one an
// older build or another program left readable.
func TestTLSKeyIsPrivate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tls")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lan := lanAddress()
	if lan == "" {
		lan = "127.0.0.1"
	}
	cert, err := EnsureTLSCert(root, lan, 9123)
	if err != nil {
		t.Fatal(err)
	}
	if !certMatchesIP(cert, "127.0.0.1") {
		t.Fatal("the certificate does not cover this computer")
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "key.pem"): 0o600, filepath.Join(dir, "cert.pem"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s is %o, want %o", path, got, want)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
	// The saved pair loads again unchanged.
	again, err := EnsureTLSCert(root, lan, 9123)
	if err != nil || string(again.Certificate[0]) != string(cert.Certificate[0]) {
		t.Fatalf("the saved certificate was not reused: %v", err)
	}
}
