package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"switcher/internal/handback"
)

func (a *API) registerHandbackRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/handback", a.handleHandbackStatus)
	mux.HandleFunc("POST /api/handback", a.handleHandback)
}

func (a *API) handedBack() bool { return a.Handback != nil && a.Handback.HandedBack() }

func (a *API) handleHandbackStatus(w http.ResponseWriter, r *http.Request) {
	if a.Handback == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "handing back is unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, a.Handback.Status())
}

// handleHandback hands every tool back to its provider (enabled true) or
// sets Switcher up again (false). It rewrites tool settings on this machine,
// so only a local request may ask.
func (a *API) handleHandback(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	if a.Handback == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "handing back is unavailable"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// The steps edit files and may wait on Desktop's settings lock; a
	// closed browser tab must not leave them halfway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	var status handback.Status
	var err error
	if *body.Enabled {
		status, err = a.Handback.HandBack(ctx)
	} else {
		status, err = a.Handback.SetUp(ctx)
	}
	if a.Tailnet != nil {
		go a.Tailnet.SyncForwarding()
	}
	switch {
	case errors.Is(err, handback.ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "handback": status})
	case err != nil && (*body.Enabled || status.HandedBack):
		// A hand-back that failed, or a set-up that kept Switcher handed
		// back because a login could not be taken back yet.
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "handback": status})
	default:
		// Switcher uses its accounts again; a tool that could not be set up
		// is named in the steps.
		answer := map[string]any{"handback": status}
		if err != nil {
			answer["warning"] = err.Error()
		}
		writeJSON(w, http.StatusOK, answer)
	}
}
