package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"switcher/internal/deviceproof"
	"switcher/internal/localpeer"
)

// client talks to the local Switcher server's API.
type client struct {
	base       string
	token      func() string
	http       *http.Client
	controlKey string
}

func newClient(base string, token func() string) *client {
	// Switcher's API never redirects. A redirect, as from a paired host's
	// answer passed on, would repeat the request, token included, elsewhere.
	// And nothing goes to another account's program that took Switcher's
	// port: the transport checks each connection to this computer.
	return &client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second, Transport: localpeer.Transport(nil),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// apiError is an error answer from the server. NeedsCode says a host
// cannot be paired without a code.
type apiError struct {
	Status    int
	Message   string
	NeedsCode bool
}

func (e *apiError) Error() string { return e.Message }

func (cl *client) call(method, path string, body, out any) error {
	return cl.callWith(method, path, body, out, nil)
}

func (cl *client) callWith(method, path string, body, out any, header http.Header) error {
	var reader io.Reader
	var sent []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		sent, reader = b, bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cl.base+path, reader)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The device token unlocks this Mac's dashboard. It is never sent, not
	// even here: each request is signed with it, so whatever answers learns
	// nothing it could use.
	if cl.token != nil && loopback(req.URL.Hostname()) {
		if token := cl.token(); token != "" {
			deviceproof.Sign(req, token, sent)
		}
	}
	resp, err := cl.http.Do(req)
	if err != nil {
		var other *localpeer.OtherUserError
		if errors.As(err, &other) {
			return other
		}
		if refused(err) {
			return errNotRunning
		}
		return fmt.Errorf("could not reach Switcher: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return answerError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected answer from Switcher: %w", err)
	}
	return nil
}

func answerError(status int, raw []byte) error {
	var body struct {
		Error        string `json:"error"`
		Details      string `json:"details"`
		AuthRequired bool   `json:"auth_required"`
		NeedsCode    bool   `json:"needs_code"`
	}
	msg := ""
	needsCode := false
	if json.Unmarshal(raw, &body) == nil {
		needsCode = body.NeedsCode
		msg = body.Error
		if body.Details != "" {
			msg += ": " + body.Details
		}
		if body.AuthRequired {
			msg = "the dashboard password is on and this Mac has no device token; turn the password off in Settings, or run the command on the Mac that runs Switcher"
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &apiError{Status: status, Message: msg, NeedsCode: needsCode}
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func refused(err error) bool {
	var op *net.OpError
	return errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &op) && op.Op == "dial")
}

// desktopHeader carries the Desktop relay control key, which the dashboard
// also reads from the server's state.
func (cl *client) desktopHeader() (http.Header, error) {
	if cl.controlKey == "" {
		var state struct {
			Key string `json:"hub_management_key"`
		}
		if err := cl.call(http.MethodGet, "/api/state", nil, &state); err != nil {
			return nil, err
		}
		cl.controlKey = state.Key
	}
	return http.Header{"X-Switcher-Desktop-Control": {cl.controlKey}}, nil
}
