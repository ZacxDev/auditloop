package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
	"github.com/ZacxDev/auditloop/internal/worker"

	"github.com/gorilla/mux"
)

// Push ingestion caps. The total multipart body is bounded first (413 on
// over-limit), then parsed with an in-memory cap (larger parts spill to a temp
// file), and each individual file is capped independently.
const (
	maxPushBodyBytes   = 64 << 20 // 64 MiB total multipart body
	maxPushMemoryBytes = 16 << 20 // in-memory ParseMultipartForm budget
	maxPushFileBytes   = 16 << 20 // per-file cap
)

// handlePluginPush ingests a completed run pushed by an external harness (P5).
// It authenticates via a bearer push token (NOT a URL path param → no token in
// access logs), validates the multipart payload FULLY before storing anything,
// then creates a done run + pages + findings, uploads the artifacts, and runs the
// SAME P2 regression diff a crawled run gets. It is a PUBLIC route (token-authed,
// not behind Supabase auth). Errors are credential-free.
func (a *App) handlePluginPush(w http.ResponseWriter, r *http.Request) {
	// Bearer push token.
	token := auth.BearerToken(r)
	if token == "" {
		metrics.PluginPushes.WithLabelValues("unauthorized").Inc()
		http.Error(w, "missing bearer push token", http.StatusUnauthorized)
		return
	}
	hash := plugin.HashToken(token)
	tgt, storedHash, err := a.DB.PluginTokenLookup(hash)
	if err != nil || !plugin.ConstantTimeEqual(storedHash, hash) {
		metrics.PluginPushes.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid push token", http.StatusUnauthorized)
		return
	}

	// Rate-limit per target (mirrors the P4 login-test limiter). Relaxed under
	// DEV_MODE only (keeps hermetic tests/e2e green); prod behavior is unchanged.
	if !a.Cfg.DevMode && a.pluginPushLimiter != nil && !a.pluginPushLimiter.Allow(tgt.ID) {
		metrics.PluginPushes.WithLabelValues("rate_limited").Inc()
		http.Error(w, "pushes are rate-limited for this target; retry shortly", http.StatusTooManyRequests)
		return
	}

	// Bound the total body, then parse multipart with an in-memory budget.
	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	if err := r.ParseMultipartForm(maxPushMemoryBytes); err != nil {
		metrics.PluginPushes.WithLabelValues("too_large").Inc()
		http.Error(w, "request too large or not valid multipart/form-data", http.StatusRequestEntityTooLarge)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	// metadata part (a plain form field carrying the push-schema JSON).
	meta := r.FormValue("metadata")
	if strings.TrimSpace(meta) == "" {
		a.rejectPush(w, "missing metadata part")
		return
	}
	payload, err := plugin.Parse(strings.NewReader(meta))
	if err != nil {
		a.rejectPush(w, err.Error())
		return
	}

	// Read all file parts into memory (bounded by the body cap) so we can VALIDATE
	// EVERYTHING before creating a run or storing a single byte.
	provided := map[string]bool{}
	fileBytes := map[string][]byte{}
	for name, headers := range r.MultipartForm.File {
		if len(headers) == 0 {
			continue
		}
		fh := headers[0]
		f, err := fh.Open()
		if err != nil {
			a.rejectPush(w, "could not read uploaded file "+name)
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxPushFileBytes+1))
		_ = f.Close()
		if err != nil || int64(len(data)) > maxPushFileBytes {
			a.rejectPush(w, "uploaded file "+name+" exceeds the per-file size cap")
			return
		}
		provided[name] = true
		fileBytes[name] = data
	}

	// Structural validation: refs↔parts integrity (no missing/orphan), viewports,
	// finding types, page caps.
	if err := payload.Validate(provided); err != nil {
		a.rejectPush(w, err.Error())
		return
	}

	// Sniff every screenshot — must be a real PNG/JPEG (don't trust declared type).
	for _, pg := range payload.Pages {
		if ct := http.DetectContentType(fileBytes[pg.Screenshot]); ct != "image/png" && ct != "image/jpeg" {
			a.rejectPush(w, "screenshot "+pg.Screenshot+" is not a PNG or JPEG image")
			return
		}
	}

	// Validate + normalise every referenced DOM/a11y digest BEFORE anything is
	// persisted. This content is UNTRUSTED and, unlike every other pushed artifact, it
	// feeds a gate that DROPS persona findings — so a malformed digest must FAIL THE
	// PUSH LOUDLY (400, nothing stored) rather than be accepted-then-ignored, which
	// would be indistinguishable from a working one. What we store is the re-serialised
	// canonical form, never the producer's raw bytes. Pushes with no digest skip this
	// entirely and behave exactly as before.
	digests := map[string][]byte{}
	for i, pg := range payload.Pages {
		if pg.A11yDigest == "" {
			continue
		}
		norm, err := plugin.NormalizeA11yDigest(fileBytes[pg.A11yDigest])
		if err != nil {
			a.rejectPush(w, fmt.Sprintf("page %d: %v", i, err))
			return
		}
		digests[pg.A11yDigest] = norm
	}

	// --- Validation complete; create the run and persist artifacts. ---
	run, err := a.DB.CreatePushedRun(tgt.UserID, tgt.ID, payload.Label, payload.Environment)
	if err != nil {
		metrics.PluginPushes.WithLabelValues("error").Inc()
		http.Error(w, "failed to create run", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	targetSlug := a.targetSlug(tgt)

	rep := &report.Report{
		Schema:      report.SchemaVersion,
		Tool:        "auditloop",
		ToolVersion: "plugin-push",
		RunID:       run.ID,
		TargetID:    tgt.ID,
		TargetName:  tgt.Name,
		BaseURL:     tgt.BaseURL,
		AuthMode:    db.AuthPlugin,
		Label:       payload.Label,
		Environment: payload.Environment,
		StartedAt:   startedOr(run.StartedAt),
		Status:      db.RunDone,
		Versions:    report.ToolVersions{Auditloop: "plugin-push"},
	}
	for _, pg := range payload.Pages {
		if err := a.persistPushedPage(ctx, run.ID, targetSlug, pg, fileBytes, digests, rep, payload.Environment); err != nil {
			log.Printf("plugin push: persist page %s: %v", pg.URL, err)
		}
		metrics.PluginPagesIngested.Inc()
	}
	rep.Summary.PagesCrawled = countPushURLs(payload.Pages)
	rep.FinishedAt = time.Now().UTC()

	// P2 diff vs the previous push for this target (reuses the crawl worker's
	// diffing verbatim).
	var diffJSON string
	if d := worker.Diff(ctx, a.DB, a.Store, run, targetSlug); d != nil {
		rep.Diff = d
		if b, err := json.Marshal(d); err == nil {
			diffJSON = string(b)
		}
	}

	if b, err := rep.Marshal(); err == nil {
		_ = a.Store.Put(ctx, storage.ReportKey(targetSlug, run.ID), "application/json", bytes.NewReader(b), int64(len(b)))
	}

	summary, _ := json.Marshal(rep.Summary)
	if err := a.DB.FinishRun(run.ID, db.RunDone, string(summary), ""); err != nil {
		metrics.PluginPushes.WithLabelValues("error").Inc()
		http.Error(w, "failed to finalize run", http.StatusInternalServerError)
		return
	}
	if diffJSON != "" {
		_ = a.DB.SetRunDiff(run.ID, diffJSON)
	}

	metrics.PluginPushes.WithLabelValues("ok").Inc()
	metrics.RunsTotal.WithLabelValues(db.RunDone).Inc()
	log.Printf("plugin push: target %s run %s ingested (%d pages)", tgt.ID, run.ID, rep.Summary.PagesCrawled)

	runURL := strings.TrimRight(a.Cfg.BaseURL, "/") + "/runs/" + run.ID
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(plugin.PushResult{RunID: run.ID, URL: runURL})
}

// persistPushedPage uploads a pushed page's artifacts and inserts the page +
// findings, rolling the page into the report.
// digests carries the already-VALIDATED + normalised a11y-digest bytes, keyed by the
// multipart filename (empty for a push that sent none).
func (a *App) persistPushedPage(ctx context.Context, runID, targetSlug string, pg plugin.PushPage, files map[string][]byte, digests map[string][]byte, rep *report.Report, environment string) error {
	pageSlug := storage.PageSlug(pg.URL)

	shotKey := storage.ScreenshotKey(targetSlug, runID, pageSlug, pg.Viewport)
	shotData := files[pg.Screenshot]
	if err := a.Store.Put(ctx, shotKey, http.DetectContentType(shotData), bytes.NewReader(shotData), int64(len(shotData))); err != nil {
		return err
	}
	keys := plugin.PageKeys{Screenshot: shotKey}
	if pg.Axe != "" {
		axeKey := storage.AxeKey(targetSlug, runID, pageSlug)
		if err := a.Store.Put(ctx, axeKey, "application/json", bytes.NewReader(files[pg.Axe]), int64(len(files[pg.Axe]))); err == nil {
			keys.Axe = axeKey
		}
	}
	if pg.Network != "" {
		netKey := storage.NetworkKey(targetSlug, runID, pageSlug)
		if err := a.Store.Put(ctx, netKey, "application/json", bytes.NewReader(files[pg.Network]), int64(len(files[pg.Network]))); err == nil {
			keys.Network = netKey
		}
	}

	// DOM/a11y digest: store the CANONICAL (validated + re-serialised) bytes under the
	// same key scheme the crawl path uses, so the persona evaluator's existing
	// pages.a11y_digest_key read path picks it up with zero eval-side changes. A store
	// failure leaves the key unset → the run degrades to screenshot-only (the gate
	// no-ops), never a half-wired page.
	if norm, ok := digests[pg.A11yDigest]; ok && pg.A11yDigest != "" {
		a11yKey := storage.A11yDigestKey(targetSlug, runID, pageSlug)
		if err := a.Store.Put(ctx, a11yKey, "application/json", bytes.NewReader(norm), int64(len(norm))); err == nil {
			keys.A11yDigest = a11yKey
		} else {
			log.Printf("plugin push: store a11y digest for %s: %v", pg.URL, err)
		}
	}

	mapped := plugin.MapPage(runID, pg, keys, environment)
	pageID, err := a.DB.InsertPage(mapped.Page)
	if err != nil {
		return err
	}
	for _, f := range mapped.Findings {
		f.PageID = pageID
		_, _ = a.DB.InsertFinding(f)
	}

	rep.Pages = append(rep.Pages, mapped.Report)
	rep.Summary.A11yViolations += pg.AxeViolations
	rep.Summary.ConsoleFirst += pg.ConsoleFirstParty
	rep.Summary.ConsoleThird += pg.ConsoleThirdParty
	rep.Summary.NetworkFirst += pg.NetworkFirstParty
	rep.Summary.NetworkThird += pg.NetworkThirdParty
	return nil
}

func (a *App) rejectPush(w http.ResponseWriter, msg string) {
	metrics.PluginPushes.WithLabelValues("rejected").Inc()
	http.Error(w, msg, http.StatusBadRequest)
}

// --- Plugin-target management (Supabase-authed UI routes) ---

// handleCreatePluginTarget creates a push-only plugin target and mints its first
// push token, returning a fragment that shows the token ONCE.
func (a *App) handleCreatePluginTarget(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	tgt, err := a.DB.CreatePluginTarget(uid, name, label)
	if err != nil {
		http.Error(w, "failed to create plugin target", http.StatusInternalServerError)
		return
	}
	token, hash, err := plugin.GenerateToken()
	if err != nil {
		http.Error(w, "failed to generate push token", http.StatusInternalServerError)
		return
	}
	if err := a.DB.SetPluginToken(tgt.ID, hash); err != nil {
		http.Error(w, "failed to store push token", http.StatusInternalServerError)
		return
	}
	render(w, pages.PluginTokenReveal(a.pluginVM(tgt, token)))
}

// handleRotatePluginToken regenerates a plugin target's push token (invalidating
// the old one) and shows the new token ONCE.
func (a *App) handleRotatePluginToken(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	tgt, err := a.DB.GetTarget(uid, id)
	if err != nil || tgt.AuthMode != db.AuthPlugin {
		http.NotFound(w, r)
		return
	}
	token, hash, err := plugin.GenerateToken()
	if err != nil {
		http.Error(w, "failed to generate push token", http.StatusInternalServerError)
		return
	}
	if err := a.DB.SetPluginToken(tgt.ID, hash); err != nil {
		http.Error(w, "failed to rotate push token", http.StatusInternalServerError)
		return
	}
	render(w, pages.PluginTokenReveal(a.pluginVM(tgt, token)))
}

// pluginVM builds the plugin view model. token is non-empty ONLY on the reveal
// fragment (create/rotate) — it is never stored or shown again.
func (a *App) pluginVM(t *db.Target, token string) *pages.PluginVM {
	return &pages.PluginVM{
		TargetID: t.ID,
		Name:     t.Name,
		Endpoint: strings.TrimRight(a.Cfg.BaseURL, "/") + plugin.PushEndpoint,
		Token:    token,
	}
}

func startedOr(t *time.Time) time.Time {
	if t != nil {
		return *t
	}
	return time.Now().UTC()
}

func countPushURLs(pages []plugin.PushPage) int {
	seen := map[string]bool{}
	for _, p := range pages {
		seen[p.URL] = true
	}
	return len(seen)
}
