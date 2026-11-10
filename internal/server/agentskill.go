package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"switcher/internal/agentskill"
	"switcher/internal/remote"
)

// skillView is this computer's skill, and on a computer paired with a host,
// the host's too, so both can be set up from here.
type skillView struct {
	agentskill.Status
	Host *remote.HostSkill `json:"host,omitempty"`
}

// handleAgentSkill reports whether Switcher's skill for AI agents is
// installed in this computer's ~/.agents/skills, and installs or removes it:
// POST {"installed": true|false, "where": "here"|"host"|"both"}. Only a local
// request may change it, or the paired Mac of a host, which can set up the
// host's skill from there; a paired Mac's request never reaches further.
func (a *API) handleAgentSkill(w http.ResponseWriter, r *http.Request) {
	if a.Home == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "no home directory to keep the skill in"})
		return
	}
	_, fromPaired := remote.PairedDevice(r)
	paired := !fromPaired && a.RemoteClient != nil && a.RemoteClient.Paired()
	if r.Method == http.MethodGet {
		st, err := agentskill.Check(a.Home, a.ClaudeConfigDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if fromPaired {
			writeJSON(w, http.StatusOK, st)
			return
		}
		view := skillView{Status: st}
		if paired {
			host, _ := a.RemoteClient.HostSkill(r.Context(), nil, false)
			view.Host = &host
		}
		writeJSON(w, http.StatusOK, view)
		return
	}
	if !fromPaired && !loopbackOnly(w, r) {
		return
	}
	var body struct {
		Installed *bool  `json:"installed"`
		Replace   bool   `json:"replace"` // also replace a skill file Switcher did not write
		Where     string `json:"where"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Installed == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"installed": true} or {"installed": false}`})
		return
	}
	here, host := true, false
	switch body.Where {
	case "", "here":
	case "host":
		here, host = false, true
	case "both":
		host = true
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `where is "here", "host" or "both"`})
		return
	}
	if fromPaired && body.Where != "" && body.Where != "here" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a paired device sets up only this computer's skill"})
		return
	}
	if host && !paired {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this Switcher does not use another Switcher's accounts"})
		return
	}
	var errs []error
	var view skillView
	var err error
	if here {
		if *body.Installed {
			view.Status, err = agentskill.Install(a.Home, a.ClaudeConfigDir, body.Replace)
		} else {
			view.Status, err = agentskill.Remove(a.Home, a.ClaudeConfigDir)
		}
		errs = append(errs, err)
	} else {
		view.Status, _ = agentskill.Check(a.Home, a.ClaudeConfigDir)
	}
	if fromPaired {
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, view.Status)
		return
	}
	if paired {
		var hs remote.HostSkill
		if host {
			// The host may be slow to answer; the change is still made there.
			hs, err = a.RemoteClient.HostSkill(context.WithoutCancel(r.Context()), body.Installed, body.Replace)
			errs = append(errs, err)
			if err != nil && hs.Skill == nil {
				hs, _ = a.RemoteClient.HostSkill(r.Context(), nil, false)
			}
		} else {
			hs, _ = a.RemoteClient.HostSkill(r.Context(), nil, false)
		}
		view.Host = &hs
	}
	if err := errors.Join(errs...); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "skill": view})
		return
	}
	writeJSON(w, http.StatusOK, view)
}
