package server

import (
	"encoding/json"
	"net/http"

	"switcher/internal/autostart"
)

func (a *API) registerAutostartRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/autostart", a.handleAutostartGet)
	mux.HandleFunc("POST /api/autostart", a.handleAutostartSet)
}

func (a *API) autostartStatus() autostart.Status {
	if a.Autostart == nil {
		return autostart.Status{}
	}
	return a.Autostart.Status()
}

func (a *API) handleAutostartGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.autostartStatus())
}

// handleAutostartSet writes this computer's login items, so only a local
// request may change them.
func (a *API) handleAutostartSet(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if st := a.autostartStatus(); !st.Supported {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": orText(st.Message, "starting at login is unavailable here")})
		return
	}
	st, err := a.Autostart.Set(*body.Enabled)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "autostart": st})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
