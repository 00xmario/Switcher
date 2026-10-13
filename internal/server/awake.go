package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"switcher/internal/awake"
)

func (a *API) registerAwakeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/awake", a.handleAwakeGet)
	mux.HandleFunc("POST /api/awake", a.handleAwakeSet)
	mux.HandleFunc("PATCH /api/awake", a.handleAwakeSettings)
}

// awakeState is the keep-awake part of /api/state, absent off macOS.
func (a *API) awakeState() (awake.State, bool) {
	if !a.Awake.Supported() {
		return awake.State{}, false
	}
	return a.Awake.State(), true
}

func (a *API) handleAwakeGet(w http.ResponseWriter, r *http.Request) {
	state, ok := a.awakeState()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": awake.ErrUnsupported.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleAwakeSet starts or stops keep awake. Minutes 0 means until turned
// off; Until is a Unix time and wins over Minutes.
func (a *API) handleAwakeSet(w http.ResponseWriter, r *http.Request) {
	if !a.Awake.Supported() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": awake.ErrUnsupported.Error()})
		return
	}
	var body struct {
		On      bool  `json:"on"`
		Minutes int   `json:"minutes"`
		Until   int64 `json:"until"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	var state awake.State
	var err error
	switch {
	case !body.On:
		state = a.Awake.Stop()
	case body.Until > 0:
		state, err = a.Awake.StartUntil(time.Unix(body.Until, 0))
	default:
		state, err = a.Awake.Start(time.Duration(body.Minutes) * time.Minute)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "awake": state})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// handleAwakeSettings changes keep-awake settings. Turning on the closed lid
// the first time shows macOS's administrator prompt on this Mac, so only a
// request from this Mac may do that.
func (a *API) handleAwakeSettings(w http.ResponseWriter, r *http.Request) {
	if !a.Awake.Supported() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": awake.ErrUnsupported.Error()})
		return
	}
	var patch awake.Patch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if patch.LidClosed != nil && *patch.LidClosed && !a.Awake.State().LidReady && !isLoopbackRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "set up the closed lid on the Mac itself; it asks for an administrator password once"})
		return
	}
	state, err := a.Awake.Update(patch)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, awake.ErrUnsupported) {
			status = http.StatusNotImplemented
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "awake": state})
		return
	}
	writeJSON(w, http.StatusOK, state)
}
