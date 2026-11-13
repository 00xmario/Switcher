package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A device-token proof lets a program on this computer show it holds the
// device token without sending it: the menu bar app signs each request
// with the token instead (X-Switcher-Proof). Whatever answers on the port
// learns nothing it could use, also when it is another account's program
// that took the port while Switcher was not running. The proof covers the
// method, the path, the body, the time and a one-time nonce:
//
//	X-Switcher-Proof: v1.<unix seconds>.<nonce: 32 hex>.<hex HMAC-SHA256>
//	HMAC key: the device token; message:
//	switcher-proof-v1|<method>|<path>[?query]|<hex SHA-256 of body>|<seconds>|<nonce>
const (
	proofHeader = "X-Switcher-Proof"
	proofWindow = 2 * time.Minute
	// proofBodyLimit bounds the body a proof-signed request may carry: the
	// menu bar app sends small JSON bodies.
	proofBodyLimit = 1 << 20
	// proofNonces bounds the nonces remembered within the window.
	proofNonces = 20_000
)

var seenProofs = struct {
	sync.Mutex
	nonces map[string]time.Time
}{nonces: map[string]time.Time{}}

// deviceAuthorized reports whether a request shows the device token: as a
// bearer token (the command line and the tray, which check first that the
// port is their user's), or as a proof.
func deviceAuthorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != "" && got != r.Header.Get("Authorization") && subtleEqual(got, token) {
		return true
	}
	return validProof(r, token, time.Now())
}

func validProof(r *http.Request, token string, now time.Time) bool {
	header := r.Header.Get(proofHeader)
	if header == "" {
		return false
	}
	parts := strings.Split(header, ".")
	if len(parts) != 4 || parts[0] != "v1" || !hexOf(parts[2], 32) || !hexOf(parts[3], 64) {
		return false
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(seconds, 0)
	if at.Before(now.Add(-proofWindow)) || at.After(now.Add(proofWindow)) {
		return false
	}
	// The body is part of what is signed: read it, and put it back for the
	// handler.
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, err = io.ReadAll(io.LimitReader(r.Body, proofBodyLimit+1))
		r.Body.Close()
		if err != nil || len(body) > proofBodyLimit {
			r.Body = io.NopCloser(bytes.NewReader(nil))
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if !hmac.Equal([]byte(parts[3]), []byte(proofMAC(token, r.Method, proofTarget(r), body, parts[1], parts[2]))) {
		return false
	}
	// Each proof counts once.
	seenProofs.Lock()
	defer seenProofs.Unlock()
	for nonce, until := range seenProofs.nonces {
		if now.After(until) {
			delete(seenProofs.nonces, nonce)
		}
	}
	if _, used := seenProofs.nonces[parts[2]]; used || len(seenProofs.nonces) >= proofNonces {
		return false
	}
	seenProofs.nonces[parts[2]] = at.Add(proofWindow)
	return true
}

// proofTarget is the path and query a proof names.
func proofTarget(r *http.Request) string {
	target := r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return target
}

func proofMAC(token, method, target string, body []byte, seconds, nonce string) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("switcher-proof-v1|" + method + "|" + target + "|" + hex.EncodeToString(sum[:]) + "|" + seconds + "|" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func hexOf(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
