package server

import (
	"io/fs"
	"net/http"
)

// Panel serves the tray's panel: the phone page's account cards in a small
// window, opened from the Linux tray. Its page uses the dashboard's own API
// behind the dashboard's login, and only a browser on this computer gets
// it; the LAN listener and paired devices have the dashboard.
func Panel(static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.NotFound(w, r)
			return
		}
		page, err := fs.ReadFile(static, "panel.html")
		if err != nil {
			http.Error(w, "the panel is unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// With a password the gate already made it no-store.
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		_, _ = w.Write(page)
	})
}
