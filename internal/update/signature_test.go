package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests sign their releases with a key of their own.
var testKey, otherKey ed25519.PrivateKey

func init() {
	_, testKey, _ = ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ = ed25519.GenerateKey(rand.Reader)
	releaseKeys = []string{keyBlob(testKey)}
}

func keyBlob(key ed25519.PrivateKey) string {
	var b bytes.Buffer
	writeString(&b, []byte("ssh-ed25519"))
	writeString(&b, key.Public().(ed25519.PublicKey))
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// signSums signs sums as ssh-keygen -Y sign does.
func signSums(key ed25519.PrivateKey, sums []byte, namespace string) []byte {
	digest := sha512.Sum512(sums)
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	for _, f := range [][]byte{[]byte(namespace), nil, []byte("sha512"), digest[:]} {
		writeString(&signed, f)
	}
	publicKey, _ := base64.StdEncoding.DecodeString(keyBlob(key))
	var sig bytes.Buffer
	writeString(&sig, []byte("ssh-ed25519"))
	writeString(&sig, ed25519.Sign(key, signed.Bytes()))
	var blob bytes.Buffer
	blob.WriteString("SSHSIG")
	blob.Write([]byte{0, 0, 0, 1})
	for _, f := range [][]byte{publicKey, []byte(namespace), nil, []byte("sha512"), sig.Bytes()} {
		writeString(&blob, f)
	}
	encoded := base64.StdEncoding.EncodeToString(blob.Bytes())
	var lines []string
	for len(encoded) > 70 {
		lines, encoded = append(lines, encoded[:70]), encoded[70:]
	}
	lines = append(lines, encoded)
	return []byte("-----BEGIN SSH SIGNATURE-----\n" + strings.Join(lines, "\n") + "\n-----END SSH SIGNATURE-----\n")
}

// writeSignedSums puts a signed list of checksums in dir, as a release
// carries it.
func writeSignedSums(t *testing.T, dir, sums string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, sumsName), []byte(sums), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, signatureName), signSums(testKey, []byte(sums), signatureNamespace), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSignedChecksumsAreTheOnlyOnesTrusted(t *testing.T) {
	sums := []byte(strings.Repeat("a", 64) + "  switcher-server_1.31.0_linux_amd64\n" + strings.Repeat("b", 64) + "  Switcher_1.31.0.dmg\n")
	good := signSums(testKey, sums, signatureNamespace)
	if err := verifySums(sums, good); err != nil {
		t.Fatalf("a good signature: %v", err)
	}
	tampered := bytes.Replace(sums, []byte("aaaa"), []byte("cccc"), 1)
	bad := map[string][2][]byte{
		"another key":       {sums, signSums(otherKey, sums, signatureNamespace)},
		"another namespace": {sums, signSums(testKey, sums, "file")},
		"changed after":     {tampered, good},
		"no signature":      {sums, nil},
		"garbage":           {sums, []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n")},
		"not armored":       {sums, []byte("hello")},
		"a truncated blob":  {sums, append(good[:60:60], []byte("\n-----END SSH SIGNATURE-----\n")...)},
	}
	for name, c := range bad {
		if err := verifySums(c[0], c[1]); err == nil {
			t.Errorf("%s was trusted", name)
		}
	}
	// The digest of one asset, named exactly once.
	if d, err := signedDigest(sums, "Switcher_1.31.0.dmg"); err != nil || d[0] != 0xbb {
		t.Fatalf("digest: %x %v", d, err)
	}
	for _, asset := range []string{"Switcher_1.31.0", "switcher-server_1.31.0_linux", "other"} {
		if _, err := signedDigest(sums, asset); err == nil {
			t.Errorf("%s was found in the list", asset)
		}
	}
	twice := append(append([]byte(nil), sums...), sums...)
	if _, err := signedDigest(twice, "Switcher_1.31.0.dmg"); err == nil {
		t.Error("an asset named twice was taken")
	}
}

// A signature that ssh-keygen made is read the same, and one this test
// makes is taken by ssh-keygen: install.sh checks releases with it.
func TestSignaturesAgreeWithSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen here")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "release")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	fields := strings.Fields(string(pub))
	was := releaseKeys
	releaseKeys = []string{fields[1]}
	defer func() { releaseKeys = was }()
	sums := filepath.Join(dir, sumsName)
	if err := os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  switcher-server_1.31.0_linux_amd64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(keygen, "-Y", "sign", "-f", key, "-n", signatureNamespace, sums).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	content, _ := os.ReadFile(sums)
	signature, _ := os.ReadFile(sums + ".sig")
	if err := verifySums(content, signature); err != nil {
		t.Fatalf("ssh-keygen's signature: %v", err)
	}
	if err := verifySums(content, signSums(testKey, content, signatureNamespace)); err == nil {
		t.Fatal("a signature by a key not in the list was trusted")
	}
	// ssh-keygen takes the test's own signature, as install.sh will.
	releaseKeys = was
	allowed := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte("switcher-release namespaces=\"switcher-release\" ssh-ed25519 "+keyBlob(testKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sums+".sig", signSums(testKey, content, signatureNamespace), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(keygen, "-Y", "verify", "-f", allowed, "-I", "switcher-release", "-n", signatureNamespace, "-s", sums+".sig")
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen refused the signature: %v: %s", err, out)
	}
}

// The keys built into a release are Ed25519 keys, and two of them.
func TestProductionKeysAreEd25519(t *testing.T) {
	if len(productionKeys) != 2 {
		t.Fatalf("%d release keys", len(productionKeys))
	}
	for _, encoded := range productionKeys {
		blob, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		r := &wire{b: blob}
		if string(r.str()) != "ssh-ed25519" || len(r.str()) != ed25519.PublicKeySize || r.bad || len(r.b) != 0 {
			t.Fatalf("not an Ed25519 key: %s", encoded)
		}
	}
}
