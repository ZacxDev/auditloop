package handlers

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ZacxDev/auditloop/internal/auth"

	"github.com/gorilla/mux"
)

// handleCreateTarget adds a target owned by the user. verified_domains defaults
// to the base URL's host (the registration is the trust signal for now; a real
// DNS-TXT verification is a documented later seam).
func (a *App) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	if name == "" || baseURL == "" {
		http.Error(w, "name and base_url are required", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		http.Error(w, "base_url must be a valid http(s) URL", http.StatusBadRequest)
		return
	}
	domains := []string{strings.ToLower(u.Hostname())}
	if _, err := a.DB.CreateTarget(uid, name, baseURL, domains); err != nil {
		http.Error(w, "failed to create target", http.StatusInternalServerError)
		return
	}
	// List mutation → full refresh (avoids empty-state lingering).
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusCreated)
}

// handleCreateRun enqueues a run for a target and redirects to the run view.
func (a *App) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	targetID := mux.Vars(r)["id"]
	if _, err := a.DB.GetTarget(uid, targetID); err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := a.DB.CreateRun(uid, targetID)
	if err != nil {
		http.Error(w, "failed to enqueue run", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/runs/"+run.ID)
	w.WriteHeader(http.StatusOK)
}
