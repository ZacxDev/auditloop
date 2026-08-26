package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/apikey"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/storage"

	"github.com/gorilla/mux"
)

// The read API lets autonomous agents/CLIs pull structured audit results with a
// per-user, read-only API key (Bearer) instead of a Supabase user JWT. Every
// read is strictly scoped to the key's owner — a key can read ONLY runs,
// targets, and artifacts whose target.user_id == key.user_id; anything else 404s
// (we don't leak existence). The key is NEVER accepted on push/mutation routes.

// apiKeyAuth authenticates a read-API request by its bearer API key: hash the
// presented key → APIKeyLookup → constant-time compare of the stored hash →
// per-key rate limit → stamp the owning user onto the request context (so the
// downstream handlers scope via auth.UserID exactly like the Supabase-authed
// ones). Any miss → 401 (credential-free). It guards ONLY the read routes; a
// read key can never reach a push/mutation route (they live on other routers).
func (a *App) apiKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := auth.BearerToken(r)
		if token == "" {
			metrics.APIReads.WithLabelValues("unauthorized").Inc()
			http.Error(w, "missing bearer api key", http.StatusUnauthorized)
			return
		}
		hash := apikey.Hash(token)
		userID, _, storedHash, found, err := a.DB.APIKeyLookup(hash)
		if err != nil {
			metrics.APIReads.WithLabelValues("error").Inc()
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found || !apikey.ConstantTimeEqual(storedHash, hash) || userID == "" {
			metrics.APIReads.WithLabelValues("unauthorized").Inc()
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}
		// Per-key rate limit (token bucket, generous for polling). Relaxed under
		// DEV_MODE only (keeps hermetic tests fast); prod behavior is unchanged.
		if !a.Cfg.DevMode && a.apiReadLimiter != nil && !a.apiReadLimiter.Allow(hash) {
			metrics.APIReads.WithLabelValues("rate_limited").Inc()
			http.Error(w, "rate limited; slow down", http.StatusTooManyRequests)
			return
		}
		// Best-effort last-used stamp (never fails the read).
		_ = a.DB.TouchAPIKeyLastUsed(hash)
		// Stamp the owner onto the context (email empty — this is a machine key).
		ctx := auth.WithClaims(r.Context(), &auth.Claims{UserID: userID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiRunListItem is the JSON shape for GET /api/audit/targets/{id}/runs.
type apiRunListItem struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Trigger    string `json:"trigger"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	PrevRunID  string `json:"prev_run_id,omitempty"`
	HasDiff    bool   `json:"has_diff"`
	Pages      int    `json:"pages"`
}

// resolveTarget resolves the {id} path param of a target-scoped read route to an
// owned target. It tries the UUID/id path first (GetTarget → WHERE id=? AND
// user_id=?), then falls back to the plugin-target NAME (GetTargetByName → owner
// -scoped, most-recent on name collision) — so consumers can reference the same
// stable spec name the push side keys on, not an opaque UUID. BOTH lookups are
// user_id-scoped, so name resolution never crosses users. Miss either way →
// nil + ErrNotFound (the caller answers the existing 404, no existence leak).
func (a *App) resolveTarget(uid, param string) (*db.Target, error) {
	if tgt, err := a.DB.GetTarget(uid, param); err == nil {
		return tgt, nil
	}
	return a.DB.GetTargetByName(uid, param)
}

// handleAuditRunList lists a target's runs (ownership-checked → 404 for a
// foreign/unknown target). {id} accepts the target UUID OR its owner-scoped name.
func (a *App) handleAuditRunList(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	tgt, err := a.resolveTarget(uid, mux.Vars(r)["id"])
	if err != nil {
		a.apiNotFound(w)
		return
	}
	id := tgt.ID
	runs, err := a.DB.ListRuns(uid, id)
	if err != nil {
		metrics.APIReads.WithLabelValues("error").Inc()
		http.Error(w, "failed to load runs", http.StatusInternalServerError)
		return
	}
	out := make([]apiRunListItem, 0, len(runs))
	for _, run := range runs {
		out = append(out, apiRunListItem{
			RunID:      run.ID,
			Status:     run.Status,
			Trigger:    run.Trigger,
			CreatedAt:  run.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			FinishedAt: rfc3339Ptr(run.FinishedAt),
			PrevRunID:  run.PrevRunID,
			HasDiff:    strings.TrimSpace(run.DiffJSON) != "",
			Pages:      pagesFromSummary(run.SummaryJSON),
		})
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.writeJSON(w, out)
}

// handleAuditLatestRun serves the report.json of a target's newest 'done' run.
// {id} accepts the target UUID OR its owner-scoped name.
func (a *App) handleAuditLatestRun(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	tgt, err := a.resolveTarget(uid, mux.Vars(r)["id"])
	if err != nil {
		a.apiNotFound(w)
		return
	}
	run, err := a.DB.LatestDoneRunForTargetOwned(uid, tgt.ID)
	if err != nil {
		metrics.APIReads.WithLabelValues("error").Inc()
		http.Error(w, "failed to load run", http.StatusInternalServerError)
		return
	}
	if run == nil {
		a.apiNotFound(w)
		return
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.streamArtifact(w, r, storage.ReportKey(a.targetSlug(tgt), run.ID))
}

// handleAuditRunReport serves a run's report.json (ownership-checked → 404).
func (a *App) handleAuditRunReport(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	runID := mux.Vars(r)["run_id"]
	run, err := a.DB.GetRun(uid, runID)
	if err != nil {
		a.apiNotFound(w)
		return
	}
	// The run is the key-owner's; resolve its target (also owner-scoped) for the
	// report.json key.
	tgt, err := a.DB.GetTarget(uid, run.TargetID)
	if err != nil {
		a.apiNotFound(w)
		return
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.streamArtifact(w, r, storage.ReportKey(a.targetSlug(tgt), run.ID))
}

// handleAuditArtifact serves raw artifact bytes with a per-object ownership
// check. The key path is `{target_slug}/{run_id}/...`; we resolve the run_id
// segment → its owning run (owner-scoped) and only serve when it belongs to the
// key's user — closing the "authed-but-not-per-object-owner" gap for this route.
func (a *App) handleAuditArtifact(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	key := mux.Vars(r)["key"]
	if key == "" || strings.Contains(key, "..") {
		metrics.APIReads.WithLabelValues("not_found").Inc()
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	// Per-object ownership: run artifacts ({slug}/{run_id}/…) resolve run_id →
	// owner-scoped GetRun; target-scoped artifacts ({target_id}/walkthroughs/… and
	// login-tests) resolve the target_id → owner-scoped GetTarget. ownsArtifact
	// encodes both; anything else → 404 (don't leak existence).
	if !a.ownsArtifact(uid, key) {
		a.apiNotFound(w)
		return
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.streamArtifact(w, r, key)
}

// apiNotFound answers 404 (JSON) without leaking whether the resource exists.
func (a *App) apiNotFound(w http.ResponseWriter) {
	metrics.APIReads.WithLabelValues("not_found").Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func (a *App) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(v)
}

func rfc3339Ptr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z07:00")
}

// pagesFromSummary extracts the pages_crawled count from a run's summary JSON
// (best effort; 0 when absent/unparseable).
func pagesFromSummary(summaryJSON string) int {
	if strings.TrimSpace(summaryJSON) == "" {
		return 0
	}
	var s struct {
		PagesCrawled int `json:"pages_crawled"`
	}
	_ = json.Unmarshal([]byte(summaryJSON), &s)
	return s.PagesCrawled
}

// --- API-key management (Supabase-authed UI routes) ---

// handleCreateAPIKey mints a read-only API key for the current user and returns
// a fragment showing the token ONCE (it is never retrievable again).
func (a *App) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	token, hash, err := apikey.Generate()
	if err != nil {
		http.Error(w, "failed to generate api key", http.StatusInternalServerError)
		return
	}
	if _, err := a.DB.CreateAPIKey(uid, name, hash, db.ScopeRead); err != nil {
		http.Error(w, "failed to store api key", http.StatusInternalServerError)
		return
	}
	render(w, pages.APIKeyReveal(&pages.APIKeyRevealVM{
		Name:    name,
		Token:   token,
		BaseURL: strings.TrimRight(a.Cfg.BaseURL, "/"),
	}))
}

// handleRevokeAPIKey revokes (deletes) a key owned by the current user. A
// foreign/unknown id → 404 (ownership-scoped).
func (a *App) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	if err := a.DB.RevokeAPIKey(uid, id); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}
