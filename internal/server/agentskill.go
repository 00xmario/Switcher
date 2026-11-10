package server

import (
	"encoding/json"
	"net/http"

	"switcher/internal/agentskill"
)

// handleAgentSkill reports whether Switcher's skill for AI agents is
// installed in this computer's ~/.agents/skills, and installs or removes it
// (POST {"installed": true|false}). The skill is this computer's, so only a
// local request may change it, also while this Switcher uses a host.
func (a *API) handleAgentSkill(w http.ResponseWriter, r *http.Request) {
	if a.Home == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "no home directory to keep the skill in"})
		return
	}
	if r.Method == http.MethodGet {
		st, err := agentskill.Check(a.Home)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	if !loopbackOnly(w, r) {
		return
	}
	var body struct {
		Installed *bool `json:"installed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Installed == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"installed": true} or {"installed": false}`})
		return
	}
	var st agentskill.Status
	var err error
	if *body.Installed {
		st, err = agentskill.Install(a.Home)
	} else {
		st, err = agentskill.Remove(a.Home)
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
