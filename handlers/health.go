package handlers

import (
	"net/http"
)

// handleHealthz is a liveness probe (always 200 if the process is up).
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleReadyz is a readiness probe: DB reachable + storage backend present.
func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := a.DB.PingContext(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "db_unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ready", "storage": a.Store.Backend()})
}
