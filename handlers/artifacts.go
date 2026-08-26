package handlers

import (
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/storage"

	"github.com/gorilla/mux"
)

// handleArtifact streams a stored artifact to an authenticated user, from
// EITHER backend, over the app's own (public, valid-TLS) origin. We deliberately
// do NOT redirect to a presigned S3 URL: in a typical self-hosted deployment the
// MinIO endpoint is cluster-internal — not resolvable or TLS-trusted by a browser
// outside that network — so a presign redirect yields
// ERR_CERT_AUTHORITY_INVALID / unreachable images. Proxying keeps every artifact
// on the app's own origin, behind the app's auth, and never exposes MinIO
// publicly. Objects are small (screenshots/JSON), so the per-request stream is
// cheap.
//
// Per-object ownership: an artifact key is {target_slug}/{run_id}/… (run/diff
// screenshots + report.json) OR {target_id}/login-tests/{id}.png (P4 login-test
// end-state screenshots, not tied to a run). We authorize BEFORE streaming, scoped
// to the Supabase-session user, so a logged-in user can't fetch another user's
// artifact by its key. Run artifacts resolve run_id → owner-scoped GetRun (mirrors
// the read API's handleAuditArtifact); login-test screenshots resolve the
// GLOBALLY-UNIQUE target_id → owner-scoped GetTarget. Both bind authorization to the
// actual owning row (a run/target the requester owns), NOT a collidable name-slug. A
// malformed/short key or a foreign/unknown run/target → 404 (no existence leak, no
// traversal — the ".." guard already ran).
func (a *App) handleArtifact(w http.ResponseWriter, r *http.Request) {
	key := mux.Vars(r)["key"]
	if key == "" || strings.Contains(key, "..") {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	if !a.ownsArtifact(auth.UserID(r.Context()), key) {
		http.NotFound(w, r)
		return
	}
	a.streamArtifact(w, r, key)
}

// ownsArtifact reports whether userID may read the artifact at key. Run artifacts
// ({slug}/{run_id}/…) are authorized by owner-scoped GetRun on the run_id;
// login-test screenshots ({target_id}/login-tests/{id}.png) are authorized by
// owner-scoped GetTarget on the (globally-unique) target_id — so authorization binds
// to the exact owning target, never a collidable name-slug. Any other/short shape →
// false.
func (a *App) ownsArtifact(userID, key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	// Target-scoped artifacts ({target_id}/login-tests/… and {target_id}/
	// walkthroughs/…) authorize on the globally-unique target_id, not a run.
	if parts[1] == "login-tests" || parts[1] == "walkthroughs" {
		_, err := a.DB.GetTarget(userID, parts[0])
		return err == nil
	}
	_, err := a.DB.GetRun(userID, parts[1])
	return err == nil
}

// artifactRunID extracts the run_id from an artifact key. Keys are
// {target_slug}/{run_id}/…, so the run_id is the 2nd path segment. Returns
// (runID, true) only when the key has at least that shape (a non-empty 2nd
// segment). Used by the read-API route's per-object ownership check
// (handleAuditArtifact).
func artifactRunID(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// streamArtifact writes the stored object at key to w over the app's own origin,
// with the defense-in-depth headers (nosniff + inline + content-type). It 404s
// when the object is missing. Callers MUST have already authorized access to the
// key (browser session for handleArtifact; per-key ownership for the read API).
func (a *App) streamArtifact(w http.ResponseWriter, r *http.Request, key string) {
	// Defense-in-depth on every artifact response (both backends): the bytes may
	// be externally pushed (P5 plugin), so never let the browser sniff a PNG/JSON
	// into HTML, and mark it inline (render, don't download).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	rc, err := a.Store.Get(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentTypeFor(key))
	w.Header().Set("Cache-Control", "private, max-age=900")
	_, _ = io.Copy(w, rc)
}

// artifactURL builds the app-origin URL for a stored artifact key. Served by
// handleArtifact (streams from either backend over auditloop's own public TLS),
// so the browser never touches the internal MinIO host. Keys are server-derived
// slug/uuid paths (safe chars + '/'), and the route is `{key:.*}`, so no escaping
// is needed.
func artifactURL(key string) string {
	if key == "" {
		return ""
	}
	return "/artifacts/" + key
}

func contentTypeFor(key string) string {
	ext := path.Ext(key)
	if ext == ".json" {
		return "application/json"
	}
	// Screenshots + captured favicons share the raster ext→MIME table with the
	// storage layer — reuse the single source of truth so the two can't drift.
	// (FaviconContentType strips the leading dot, lowercases, and falls back to
	// application/octet-stream for any unknown ext — identical to the prior table.)
	return storage.FaviconContentType(ext)
}
