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
// agy and app) is taken as known: /api/state is polled every few seconds and
// reading the keychain each time would be slow.
const appLoginTTL = 15 * time.Second

// appLogins caches, per provider, whom its apps are signed in with.
type appLogins struct {
	mu   sync.Mutex
	seen map[string]appLoginSeen
}

type appLoginSeen struct {
	email string
	at    time.Time
}

func (c *appLogins) email(providerID string, apps provider.AppLoginProvider) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.seen[providerID]; ok && time.Since(s.at) < appLoginTTL {
		return s.email
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	email, err := apps.AppLogin(ctx)
	if err != nil {
		email = ""
	}
	if c.seen == nil {
		c.seen = map[string]appLoginSeen{}
	}
	c.seen[providerID] = appLoginSeen{email: email, at: time.Now()}
	return email
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
			"native": map[string]any{"changed": false, "message": "Antigravity on this computer keeps its own sign-in: Switcher signs Antigravity in only on the computer that holds the account."}})
		return
	}
	if _, approved := phone.ApprovedPhone(r.Context()); !approved && !loopbackOnly(w, r) {
		return
	}
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
		"native": map[string]any{"changed": true, "message": "agy and the Antigravity app now use " + fresh.Email + ". Restart the Antigravity app to pick it up; agy notices by itself."}})
}
