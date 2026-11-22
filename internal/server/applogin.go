package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"switcher/internal/phone"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

// appLoginTTL is how long the sign-in of a provider's own apps (Antigravity's
// agy and app) is taken as known: /api/state is polled every few seconds,
// and reading the keychain each time would be slow and could ask the user.
// A switch made here updates it at once; one made in agy shows within this.
const appLoginTTL = 2 * time.Minute

// appLogins caches, per provider, whom its apps are signed in with. A read
// that fails is cached too, so a locked keychain is not asked again and
// again. The keychain is read outside the lock: a slow one never holds up
// /api/state for other providers.
type appLogins struct {
	mu      sync.Mutex
	seen    map[string]appLoginSeen
	reading map[string]bool
	// switching serializes Use in Antigravity: two quick picks cannot leave
	// the keychain on one account and agy's file on another.
	switching sync.Mutex
}

type appLoginSeen struct {
	email string
	at    time.Time
}

func (c *appLogins) email(providerID string, apps provider.AppLoginProvider) string {
	c.mu.Lock()
	s, known := c.seen[providerID]
	if known && time.Since(s.at) < appLoginTTL || c.reading[providerID] {
		c.mu.Unlock()
		return s.email
	}
	if c.reading == nil {
		c.reading = map[string]bool{}
	}
	c.reading[providerID] = true
	c.mu.Unlock()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	email, err := apps.AppLogin(ctx)
	cancel()
	if err != nil {
		email = ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reading, providerID)
	if c.seen == nil {
		c.seen = map[string]appLoginSeen{}
	}
	// A switch made while this read ran is newer than what it read.
	if cur, ok := c.seen[providerID]; !ok || cur.at.Before(started) {
		c.seen[providerID] = appLoginSeen{email: email, at: time.Now()}
	}
	return c.seen[providerID].email
}

func (c *appLogins) set(providerID, email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]appLoginSeen{}
	}
	c.seen[providerID] = appLoginSeen{email: email, at: time.Now()}
}

// activateInApps is "Use in Antigravity": the user's own pick, so it signs
// the provider's apps on this computer in with the account, with a fresh
// token. Never automatic. A paired Mac uses this Mac's accounts but has its
// own apps, which this Mac cannot sign in: it gets the selection only.
func (a *API) activateInApps(w http.ResponseWriter, r *http.Request, account store.Account, apps provider.AppLoginProvider, paired bool) {
	if a.handedBack() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": proxy.ErrHandedBack.Error()})
		return
	}
	if paired {
		if err := a.Proxy.Activate(account.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not activate account"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "active": account.ID,
			"apps": map[string]any{"changed": false, "message": "Antigravity on this computer keeps its own sign-in: Switcher signs Antigravity in only on the computer that holds the account."}})
		return
	}
	if _, approved := phone.ApprovedPhone(r.Context()); !approved && !loopbackOnly(w, r) {
		return
	}
	a.appLogins.switching.Lock()
	defer a.appLogins.switching.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	fresh, err := a.Proxy.PrepareAccount(ctx, account.ID)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": "Sign in to this account again in Switcher first: " + err.Error()})
		return
	}
	if err := apps.UseInApps(ctx, fresh); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "Antigravity was not signed in", "details": err.Error()})
		return
	}
	a.appLogins.set(account.Provider, fresh.Email)
	if err := a.Proxy.Activate(account.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not activate account"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "active": account.ID,
		"apps": map[string]any{"changed": true, "message": "agy and the Antigravity app now use " + fresh.Email + ". Quit and reopen them if they are running: until then they may keep, and save back, the previous sign-in."}})
}
