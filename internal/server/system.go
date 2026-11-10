package server

import (
	"net/http"
	"os"
	"runtime"
	"strings"

	"switcher/internal/sysload"
)

// handleSystem answers how busy this computer is, for the menu's load card:
// processor, memory and graphics shares. While this Switcher uses a paired
// host's accounts it answers the host's load instead, since that is where
// the work runs; working without the host, it answers its own.
func (a *API) handleSystem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.RemoteClient != nil && a.RemoteClient.Connected() {
		writeJSON(w, http.StatusOK, a.RemoteClient.SystemLoad(r.Context()))
		return
	}
	view := sysload.View{Machine: machineName(), Platform: runtime.GOOS}
	if !a.Load.Supported() {
		view.Reason = "unsupported"
		writeJSON(w, http.StatusOK, view)
		return
	}
	sample, err := a.Load.Read(r.Context())
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		view.Reason = "unsupported"
		writeJSON(w, http.StatusOK, view)
		return
	}
	writeJSON(w, http.StatusOK, sysload.ViewOf(sample, view.Machine, view.Platform))
}

// machineName is this computer's short host name, without a .local or other
// domain.
func machineName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	name, _, _ = strings.Cut(name, ".")
	return name
}
