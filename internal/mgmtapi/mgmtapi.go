// Package mgmtapi implements the subset of CLIProxyAPI's Management API
// that T3 Code uses for its "Add a CLIProxyAPI hub" feature. Switcher
// speaks the same three routes so it can be added as a hub directly:
//
//	GET  /v0/management/auth-files   list pooled accounts
//	POST /v0/management/api-call     run a control-plane request with one
//	                                 account's credentials ($TOKEN$ swap)
//	POST /v0/management/reset-quota  clear an account's local exhaustion
//
// Every route requires the management key via Authorization: Bearer <key>.
package mgmtapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"crypto/subtle"
	"fmt"
	"switcher/internal/login"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

// API wires the management compatibility surface.
type API struct {
	Store         *store.Store
	Proxy         *proxy.Manager
	Logins        *login.Manager
	ManagementKey string
}

// Register mounts the management routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v0/management/auth-files", a.authGuard(a.handleAuthFiles))
	mux.HandleFunc("POST /v0/management/api-call", a.authGuard(a.handleAPICall))
	mux.HandleFunc("POST /v0/management/reset-quota", a.authGuard(a.handleResetQuota))
}

// authGuard enforces the management key. CLIProxyAPI accepts both a bearer
// token and its X-Management-Key header; support both shapes.
func (a *API) authGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-Management-Key")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.ManagementKey)) != 1 {
			http.Error(w, "management key required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// validAuthIndex accepts only plain account ids: path traversal in the
// store file names is impossible through the hub surface.
func validAuthIndex(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// handleAuthFiles lists every stored account in CLIProxyAPI's shape. T3
// filters for codex and claude itself, so all providers are listed.
func (a *API) handleAuthFiles(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list accounts"})
		return
	}
	files := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		file := map[string]any{
			"id":         acc.ID,
			"auth_index": acc.ID,
			"name":       acc.ID + ".json",
			"provider":   acc.Provider,
			"email":      acc.Email,
			"disabled":   false,
			"status":     "ready",
			"source":     "file",
		}
		if acc.Provider == "codex" {
			// T3 reads the plan and the account id from the id_token object.
			file["id_token"] = map[string]any{
				"chatgpt_account_id": acc.Token.AccountID,
				"chatgpt_plan_type":  acc.Plan,
			}
		}
		files = append(files, file)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// controlPlaneProviders says whose accounts each allowed host takes: an
// account's token goes to its own provider and to no other company.
var controlPlaneProviders = map[string]string{
	"chatgpt.com":       "codex",
	"api.anthropic.com": "claude",
}

// apiCallRequest mirrors CLIProxyAPI's management api-call body. Header
// values may contain the $TOKEN$ placeholder, which CPA swaps for the
// account's credentials before dispatching. Data is CPA's string holding
// the request body, which is how T3 Code sends it; a JSON value in its
// place is taken as the body itself.
type apiCallRequest struct {
	AuthIndex string            `json:"auth_index"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header"`
	Data      json.RawMessage   `json:"data"`
}

// controlPlaneEndpoints are the only requests a hub-driven api-call may
// make, with the method each takes: the usage and reset-credit calls T3
// Code sends (apps/server/src/usage/cliproxyApi.ts). The management API is a
// credential-bearing primitive, and paired Macs reach it with this Mac's
// key, so nothing else an account's token could reach, such as its ChatGPT
// conversations, is open to it.
var controlPlaneEndpoints = map[string]string{
	"chatgpt.com/backend-api/wham/usage":                            http.MethodGet,
	"chatgpt.com/backend-api/wham/rate-limit-reset-credits":         http.MethodGet,
	"chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume": http.MethodPost,
	"api.anthropic.com/api/oauth/usage":                             http.MethodGet,
}

// controlPlaneMethod returns the method an allowed endpoint takes. Hosts
// must be ASCII: strings.ToLower folds "İ" to "i", while the transport
// would dial the punycode of the original, another domain.
func controlPlaneMethod(u *url.URL) (string, bool) {
	if u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || (u.Port() != "" && u.Port() != "443") {
		return "", false
	}
	host := u.Hostname()
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf {
			return "", false
		}
	}
	method, ok := controlPlaneEndpoints[strings.ToLower(host)+u.EscapedPath()]
	return method, ok
}

// apiCallBody returns the request body that data carries, if any.
func apiCallBody(data json.RawMessage) ([]byte, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if data[0] != '"' {
		return data, nil
	}
	var body string
	if err := json.Unmarshal(data, &body); err != nil || body == "" {
		return nil, err
	}
	return []byte(body), nil
}

func sameControlPlaneOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Hostname(), b.Hostname()) && a.Scheme == b.Scheme
}

func (a *API) handleAPICall(w http.ResponseWriter, r *http.Request) {
	var req apiCallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if req.AuthIndex == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "auth_index and url are required"})
		return
	}
	parsed, err := parseURL(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid url"})
		return
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	if want, ok := controlPlaneMethod(parsed); !ok || method != want {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url and method must name an allowed control-plane request"})
		return
	}
	body, err := apiCallBody(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid data"})
		return
	}

	if !validAuthIndex(req.AuthIndex) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid auth_index"})
		return
	}
	if a.Proxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account manager unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Checked before the account's credentials are touched: a Claude token
	// is never sent to OpenAI, nor a ChatGPT token to Anthropic.
	stored, err := a.Store.Get(req.AuthIndex)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown auth_index"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account credentials unavailable"})
		}
		return
	}
	if want := controlPlaneProviders[strings.ToLower(parsed.Hostname())]; want == "" || stored.Provider != want {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this account does not belong to that provider"})
		return
	}
	account, err := a.Proxy.PrepareAccount(ctx, req.AuthIndex)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown auth_index"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account credentials unavailable"})
		}
		return
	}
	if account.Provider != stored.Provider {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this account does not belong to that provider"})
		return
	}

	// Redirects re-validate the allowlist, and headers that carried the
	// account token are stripped on any cross-host hop so the token can
	// never leak to a host the allowlist did not bless.
	tokenHeaderNames := []string{}
	for key, value := range req.Header {
		if strings.Contains(value, "$TOKEN$") {
			tokenHeaderNames = append(tokenHeaderNames, key)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many redirects")
		}
		if want, ok := controlPlaneMethod(req.URL); !ok || req.Method != want {
			return fmt.Errorf("redirect target is not allowed")
		}
		for _, previous := range via {
			if !sameControlPlaneOrigin(req.URL, previous.URL) {
				for _, name := range tokenHeaderNames {
					req.Header.Del(name)
				}
				break
			}
		}
		return nil
	}}
	call := func(account store.Account) (*http.Response, error) {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bodyReader)
		if err != nil {
			return nil, err
		}
		for key, value := range req.Header {
			httpReq.Header.Set(key, strings.ReplaceAll(value, "$TOKEN$", account.Token.AccessToken))
		}
		return client.Do(httpReq)
	}
	resp, err := call(account)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed"})
		return
	}
	// A redirected request no longer represents authentication at the original
	// endpoint. Never refresh the original account based on a cross-origin 401.
	tokenWasSent := false
	if resp.Request != nil && sameControlPlaneOrigin(resp.Request.URL, parsed) {
		for _, name := range tokenHeaderNames {
			if strings.Contains(resp.Request.Header.Get(name), account.Token.AccessToken) {
				tokenWasSent = true
			}
		}
	}
	if resp.StatusCode == http.StatusUnauthorized && tokenWasSent {
		_ = resp.Body.Close()
		account, err = a.Proxy.RefreshAccountAfter401(ctx, account.ID, account.Token.AccessToken)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account credentials could not be refreshed"})
			return
		}
		resp, err = call(account)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed"})
			return
		}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(raw) > 4<<20 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not read upstream response"})
		return
	}
	log.Printf("mgmt: api-call %s %s -> %d", method, parsed.Host, resp.StatusCode)
	writeJSON(w, http.StatusOK, map[string]any{
		"status_code": resp.StatusCode,
		"body":        string(raw),
	})
}

func (a *API) handleResetQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || !validAuthIndex(req.AuthIndex) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid auth_index"})
		return
	}
	account, err := a.Store.Get(req.AuthIndex)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown auth_index"})
		return
	}
	if err := a.Proxy.ClearExhausted(account.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "quota state could not be saved"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"auth_index": req.AuthIndex,
	})
}
