package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxRequestBytes bounds a request the relay reads into memory, packed or
// unpacked. Anthropic's own limit is half of it.
const maxRequestBytes = 64 << 20

// unpacking and unpackingPaired bound how many packed requests are unpacked
// at once: this computer's own, and paired Macs', each with turns of their
// own, so a paired Mac cannot keep this computer's requests waiting.
var (
	unpacking       = make(chan struct{}, 2)
	unpackingPaired = make(chan struct{}, 2)
)

type identityView struct {
	SessionID string
	ParentID  string
	AgentID   string
	Model     string
	Thread    string // "create", "continue", or "" without a message thread
	// AccountUUID is the Anthropic account the caller's own login says it
	// is, from metadata.user_id.
	AccountUUID string
}

// identity reads the routing fields Claude Code sends. It never rejects a
// request: anything it cannot read simply leaves the caller's own credential
// in place.
func identity(r *http.Request, body []byte) identityView { return identityIn(unpacking, r, body) }

// identityIn is identity with the turns a packed body waits for.
func identityIn(turns chan struct{}, r *http.Request, body []byte) identityView {
	var v identityView
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		body = unpack(turns, r, body)
	}
	return identityOf(v, r, body)
}

// unpack is a packed body unpacked, or nil for one that is too large or
// was given up on. It is unpacked only up to the size a request may have: a
// small packed body can hold gigabytes, and whoever may send a request, a
// paired Mac included, could otherwise use up this computer's memory. Few
// are unpacked at a time, too: many small ones at once add up to the same.
func unpack(turns chan struct{}, r *http.Request, body []byte) []byte {
	// A request given up on does not wait for a turn, nor take one.
	select {
	case turns <- struct{}{}:
	case <-r.Context().Done():
		return nil
	}
	// The turn is for the unpacking alone.
	defer func() { <-turns }()
	z, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer z.Close()
	unpacked, _ := io.ReadAll(io.LimitReader(z, maxRequestBytes+1))
	if len(unpacked) > maxRequestBytes {
		return nil
	}
	return unpacked
}

func identityOf(v identityView, r *http.Request, body []byte) identityView {
	var top struct {
		Thread *struct {
			Type string `json:"type"`
		} `json:"thread"`
		Model    string `json:"model"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	json.Unmarshal(body, &top)
	v.Model = top.Model
	if top.Thread != nil {
		v.Thread = "create"
		if top.Thread.Type == "continue" {
			v.Thread = "continue"
		}
	}
	var user struct {
		SessionID       string `json:"session_id"`
		ParentSessionID string `json:"parent_session_id"`
		AgentID         string `json:"agent_id"`
		AccountUUID     string `json:"account_uuid"`
	}
	json.Unmarshal([]byte(top.Metadata.UserID), &user)
	v.SessionID = strings.ToLower(r.Header.Get("X-Claude-Code-Session-Id"))
	if v.SessionID == "" {
		v.SessionID = strings.ToLower(user.SessionID)
	}
	if !validUUID(v.SessionID) {
		v.SessionID = ""
	}
	if parent := strings.ToLower(user.ParentSessionID); validUUID(parent) {
		v.ParentID = parent
	}
	v.AgentID = user.AgentID
	if validUUID(strings.ToLower(user.AccountUUID)) {
		v.AccountUUID = strings.ToLower(user.AccountUUID)
	}
	return v
}
