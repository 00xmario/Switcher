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
// request may change it, or the paired computer of a host, which can install
// or remove the host's own copy from there (not replace one edited by hand);
// a paired computer's request never reaches further.
//
// GET ?wait=0 answers at once with the host's last known skill, so a page
// never waits on a host that is slow to answer.
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
			var host remote.HostSkill
			if r.URL.Query().Get("wait") == "0" {
				host = a.RemoteClient.CachedHostSkill()
			} else {
				host = a.RemoteClient.HostSkill(r.Context())
			}
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
	if fromPaired {
		a.pairedSkill(w, body.Where, *body.Installed, body.Replace)
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
		if err != nil {
			if host {
				err = errors.New("this computer: " + err.Error())
			}
			errs = append(errs, err)
		}
	} else {
		view.Status, _ = agentskill.Check(a.Home, a.ClaudeConfigDir)
	}
	if paired {
		var hs remote.HostSkill
		if host {
			// The host may be slow to answer; the change is still made there.
			hs, err = a.RemoteClient.SetHostSkill(context.WithoutCancel(r.Context()), *body.Installed)
			if err != nil {
				errs = append(errs, err)
				if hs.Skill == nil && hs.Reason == "refused" {
					hs = a.RemoteClient.HostSkill(r.Context())
				}
			}
		} else {
			hs = a.RemoteClient.CachedHostSkill()
		}
		view.Host = &hs
	}
	if err := errors.Join(errs...); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "skill": view})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// pairedSkill installs or removes this computer's skill for the computer
// paired with it. A copy edited by hand here is replaced only here.
func (a *API) pairedSkill(w http.ResponseWriter, where string, install, replace bool) {
	if where != "" && where != "here" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a paired computer sets up only this computer's skill"})
		return
	}
	if replace {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a skill edited by hand is replaced only on its own computer"})
		return
	}
	var st agentskill.Status
	var err error
	if install {
		st, err = agentskill.Install(a.Home, a.ClaudeConfigDir, false)
	} else {
		st, err = agentskill.Remove(a.Home, a.ClaudeConfigDir)
	}
	switch {
	case errors.Is(err, agentskill.ErrNotOurs):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "its skill was changed by hand; replace it there with: switcher skill install --replace"})
	case err != nil:
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, st)
	}
}
