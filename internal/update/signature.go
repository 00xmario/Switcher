package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// A release is trusted only when its list of checksums, SHA256SUMS, carries
// a signature by one of the keys below. The keys' private halves are kept
// apart from GitHub, so neither a stolen GitHub token nor anyone else who
// can change a release there can make an install take a build: the files
// can be swapped, the signature cannot be made.
//
// The signature is OpenSSH's (ssh-keygen -Y sign -n switcher-release), so
// install.sh checks the same file with ssh-keygen -Y verify. See
// build/release-sign.sh for how a release is signed.
const (
	sumsName           = "SHA256SUMS"
	signatureName      = "SHA256SUMS.sig"
	signatureNamespace = "switcher-release"
	sumsLimit          = 64 << 10
	signatureLimit     = 4 << 10
)

// releaseKeys are the public keys a release may be signed with, as the
// second field of their .pub files: the key the releases are signed with,
// and one kept offline to sign with should that one be lost. Tests replace
// them.
var releaseKeys = productionKeys

var productionKeys = []string{
	"AAAAC3NzaC1lZDI1NTE5AAAAIO2UJMe8yL/18YTufkesQEr9AjT32/Z8JzvCgtKjvya0", // switcher-release-1
	"AAAAC3NzaC1lZDI1NTE5AAAAINkxLFXWXQDLvgFD+nK2tMMikz25kdCf3rh1W9F4Cmzp", // switcher-release-2, offline
}

var errUnsigned = errors.New("the release's checksums are not signed by Switcher's release key")

// verifySums checks that signature is a signature of sums by one of the
// release keys.
func verifySums(sums, signature []byte) error {
	blob, err := unarmor(signature)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnsigned, err)
	}
	r := &wire{b: blob}
	if string(r.take(6)) != "SSHSIG" || r.uint32() != 1 {
		return fmt.Errorf("%w: not an SSH signature", errUnsigned)
	}
	publicKey, namespace, reserved, hashName, sig := r.str(), r.str(), r.str(), r.str(), r.str()
	if r.bad || len(r.b) != 0 {
		return fmt.Errorf("%w: malformed signature", errUnsigned)
	}
	if string(namespace) != signatureNamespace {
		return fmt.Errorf("%w: signed for %q", errUnsigned, namespace)
	}
	key, err := releaseKey(publicKey)
	if err != nil {
		return err
	}
	k := &wire{b: sig}
	if string(k.str()) != "ssh-ed25519" {
		return fmt.Errorf("%w: not an Ed25519 signature", errUnsigned)
	}
	raw := k.str()
	if k.bad || len(k.b) != 0 || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: malformed signature", errUnsigned)
	}
	var digest []byte
	switch string(hashName) {
	case "sha512":
		sum := sha512.Sum512(sums)
		digest = sum[:]
	case "sha256":
		sum := sha256.Sum256(sums)
		digest = sum[:]
	default:
		return fmt.Errorf("%w: hash %q", errUnsigned, hashName)
	}
	var signed bytes.Buffer
	signed.WriteString("SSHSIG")
	for _, field := range [][]byte{namespace, reserved, hashName, digest} {
		writeString(&signed, field)
	}
	if !ed25519.Verify(key, signed.Bytes(), raw) {
		return errUnsigned
	}
	return nil
}

// releaseKey is the Ed25519 key a signature names, if it is a release key.
func releaseKey(blob []byte) (ed25519.PublicKey, error) {
	for _, encoded := range releaseKeys {
		known, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !bytes.Equal(known, blob) {
			continue
		}
		r := &wire{b: blob}
		if string(r.str()) != "ssh-ed25519" {
			break
		}
		key := r.str()
		if r.bad || len(r.b) != 0 || len(key) != ed25519.PublicKeySize {
			break
		}
		return ed25519.PublicKey(key), nil
	}
	return nil, fmt.Errorf("%w: signed by another key", errUnsigned)
}

// unarmor is the signature inside its BEGIN and END lines.
func unarmor(armored []byte) ([]byte, error) {
	text := strings.TrimSpace(strings.ReplaceAll(string(armored), "\r\n", "\n"))
	const begin, end = "-----BEGIN SSH SIGNATURE-----", "-----END SSH SIGNATURE-----"
	if !strings.HasPrefix(text, begin) || !strings.HasSuffix(text, end) {
		return nil, errors.New("not an armored SSH signature")
	}
	body := strings.Join(strings.Fields(text[len(begin):len(text)-len(end)]), "")
	return base64.StdEncoding.DecodeString(body)
}

// signedDigest is the SHA-256 digest that a signed list gives for asset: its
// one line naming exactly that file, in the form shasum writes.
func signedDigest(sums []byte, asset string) ([]byte, error) {
	var found []byte
	for _, line := range strings.Split(strings.TrimRight(string(sums), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("invalid SHA-256 digest for %s", asset)
		}
		if found != nil {
			return nil, fmt.Errorf("the release's checksums name %s more than once", asset)
		}
		found = digest
	}
	if found == nil {
		return nil, fmt.Errorf("the release's checksums do not name %s", asset)
	}
	return found, nil
}

// wire reads SSH's wire format: big-endian lengths before each string.
type wire struct {
	b   []byte
	bad bool
}

func (w *wire) take(n int) []byte {
	if w.bad || n < 0 || n > len(w.b) {
		w.bad = true
		return nil
	}
	out := w.b[:n]
	w.b = w.b[n:]
	return out
}

func (w *wire) uint32() uint32 {
	b := w.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (w *wire) str() []byte {
	n := w.uint32()
	if n > 1<<16 {
		w.bad = true
		return nil
	}
	return w.take(int(n))
}

func writeString(buf *bytes.Buffer, field []byte) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(field)))
	buf.Write(field)
}
