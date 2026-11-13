// Package deviceproof lets a program on this computer show Switcher it
// holds the device token without sending it: each request is signed with
// the token instead. Whatever answers on the port learns nothing it could
// use, also when it is another account's program that took the port while
// Switcher was not running. The proof covers the method, the path and
// query, the body, the time and a one-time nonce:
//
//	X-Switcher-Proof: v1.<unix seconds>.<nonce: 32 hex>.<hex HMAC-SHA256>
//	HMAC key: the device token; message:
//	switcher-proof-v1|<method>|<path>[?query]|<hex SHA-256 of body>|<seconds>|<nonce>
//
// The menu bar app makes the same proof (build/macos/menubar.swift).
package deviceproof

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header carries the proof.
const Header = "X-Switcher-Proof"

// MAC is the proof's signature over a request.
func MAC(token, method, target string, body []byte, seconds, nonce string) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("switcher-proof-v1|" + method + "|" + target + "|" + hex.EncodeToString(sum[:]) + "|" + seconds + "|" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// Target is the path and query a proof names.
func Target(path, rawQuery string) string {
	if rawQuery != "" {
		return path + "?" + rawQuery
	}
	return path
}

// Sign proves req, whose body is body, with token, and leaves the token
// itself out of it.
func Sign(req *http.Request, token string, body []byte) {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	seconds := strconv.FormatInt(time.Now().Unix(), 10)
	n := hex.EncodeToString(nonce)
	req.Header.Del("Authorization")
	req.Header.Set(Header, "v1."+seconds+"."+n+"."+MAC(token, req.Method, Target(req.URL.Path, req.URL.RawQuery), body, seconds, n))
}

// Window is how far a proof's time may be from now.
const Window = 2 * time.Minute

// Check reports whether header is a proof of a request with this method,
// target and body made with token within Window of now, and gives its
// nonce: the caller makes sure each nonce counts once.
func Check(header, token, method, target string, body []byte, now time.Time) (nonce string, at time.Time, ok bool) {
	parts := strings.Split(header, ".")
	if token == "" || len(parts) != 4 || parts[0] != "v1" || !hexOf(parts[2], 32) || !hexOf(parts[3], 64) {
		return "", time.Time{}, false
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	at = time.Unix(seconds, 0)
	if at.Before(now.Add(-Window)) || at.After(now.Add(Window)) {
		return "", time.Time{}, false
	}
	if !hmac.Equal([]byte(parts[3]), []byte(MAC(token, method, target, body, parts[1], parts[2]))) {
		return "", time.Time{}, false
	}
	return parts[2], at, true
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
