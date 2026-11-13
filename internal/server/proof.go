package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"switcher/internal/deviceproof"
)

// A device-token proof lets a program on this computer show it holds the
// device token without sending it; see internal/deviceproof. The menu bar
// app, the command line and the tray sign their requests so.
const (
	proofHeader = deviceproof.Header
	proofWindow = deviceproof.Window
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
// proof, as this computer's own programs send it, or as a bearer token, as
// releases before 1.31 and scripts do.
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
	if header == "" || r.ContentLength > proofBodyLimit {
		return false
	}
	// The body is part of what is signed: read it, and put it back for the
	// handler.
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, proofBodyLimit+1))
		r.Body.Close()
		if err != nil || len(body) > proofBodyLimit {
			r.Body = io.NopCloser(bytes.NewReader(nil))
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	nonce, at, ok := deviceproof.Check(header, token, r.Method, deviceproof.Target(r.URL.Path, r.URL.RawQuery), body, now)
	if !ok {
		return false
	}
	// Each proof counts once.
	seenProofs.Lock()
	defer seenProofs.Unlock()
	for seen, until := range seenProofs.nonces {
		if now.After(until) {
			delete(seenProofs.nonces, seen)
		}
	}
	if _, used := seenProofs.nonces[nonce]; used || len(seenProofs.nonces) >= proofNonces {
		return false
	}
	seenProofs.nonces[nonce] = at.Add(proofWindow)
	return true
}
