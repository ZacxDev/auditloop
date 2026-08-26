// Package worker runs the background crawl loop: it atomically claims queued
// runs, drives the crawler for the target, persists pages+findings, uploads
// artifacts + a run-level report.json to object storage, and finalizes the run.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"time"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/crypto"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/recipe"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// CrawlFunc is the crawl entrypoint (injectable so tests can stub the browser).
type CrawlFunc func(ctx context.Context, opts crawler.Options) (*crawler.Result, error)

// Worker processes queued runs.
type Worker struct {
	DB            *db.DB
	Store         storage.Store
	Crawl         CrawlFunc
	ChromiumPath  string
	MaxPages      int
	MaxDepth      int
	AllowLoopback bool
	// InternalAllowHosts is the exact-match internal-host SSRF allowlist (see
	// crawler.GuardConfig.InternalAllowHosts) — empty in normal deployments.
	InternalAllowHosts []string
	Poll               time.Duration
	ToolVersion        string
	// Cipher decrypts login-recipe credentials (P4). Nil when the encryption key
	// is unset — a target in auth_mode=login then fails the run with a clear
	// message (the feature is gated off).
	Cipher *crypto.Cipher
}

// New builds a worker with sensible defaults.
func New(database *db.DB, store storage.Store, chromiumPath string, maxPages, maxDepth int) *Worker {
	return &Worker{
		DB:           database,
		Store:        store,
		Crawl:        crawler.Crawl,
		ChromiumPath: chromiumPath,
		MaxPages:     maxPages,
		MaxDepth:     maxDepth,
		Poll:         3 * time.Second,
		ToolVersion:  "dev",
	}
}

// Run is the worker loop. It returns when ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("worker: started (maxPages=%d maxDepth=%d backend=%s)", w.MaxPages, w.MaxDepth, w.Store.Backend())
	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: stopping")
			return
		default:
		}
		run, err := w.DB.ClaimNextQueuedRun()
		if err != nil {
			log.Printf("worker: claim: %v", err)
			w.sleep(ctx)
			continue
		}
		if run == nil {
			w.sleep(ctx)
			continue
		}
		if err := w.ProcessRun(ctx, run); err != nil {
			log.Printf("worker: run %s failed: %v", run.ID, err)
		}
	}
}

func (w *Worker) sleep(ctx context.Context) {
	t := time.NewTimer(w.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// ProcessRun crawls a claimed run's target and persists results.
func (w *Worker) ProcessRun(ctx context.Context, run *db.Run) error {
	t0 := time.Now()
	tgt, err := w.DB.GetTargetByID(run.TargetID)
	if err != nil {
		return w.fail(run, "target lookup: "+err.Error())
	}

	allowed := tgt.VerifiedDomains
	if len(allowed) == 0 {
		if h := hostOf(tgt.BaseURL); h != "" {
			allowed = []string{h}
		}
	}

	// P4: resolve a login recipe (decrypt creds) for an authenticated crawl.
	loginCfg, err := w.buildLogin(tgt)
	if err != nil {
		return w.fail(run, err.Error())
	}

	crawlRes, err := w.Crawl(ctx, crawler.Options{
		BaseURL:            tgt.BaseURL,
		AllowedHosts:       allowed,
		MaxPages:           w.MaxPages,
		MaxDepth:           w.MaxDepth,
		ChromiumPath:       w.ChromiumPath,
		AllowLoopback:      w.AllowLoopback,
		InternalAllowHosts: w.InternalAllowHosts,
		Login:              loginCfg,
	})
	if err != nil {
		metrics.RunsTotal.WithLabelValues(db.RunFailed).Inc()
		// A login-wall failure is distinct from a general crawl error: report it
		// with the recipe hint and DO NOT crawl the login wall.
		var loginErr *crawler.ErrLoginFailed
		if errors.As(err, &loginErr) {
			metrics.LoginAttempts.WithLabelValues("failed").Inc()
			return w.fail(run, "login recipe failed — check selectors/credentials: "+loginErr.Reason)
		}
		return w.fail(run, "crawl: "+err.Error())
	}
	if loginCfg != nil {
		metrics.LoginAttempts.WithLabelValues("ok").Inc()
	}

	targetSlug := storage.Slug(tgt.Name)
	if targetSlug == "root" {
		targetSlug = storage.Slug(hostOf(tgt.BaseURL))
	}

	rep := &report.Report{
		Schema:      report.SchemaVersion,
		Tool:        "auditloop",
		ToolVersion: w.ToolVersion,
		RunID:       run.ID,
		TargetID:    tgt.ID,
		TargetName:  tgt.Name,
		BaseURL:     tgt.BaseURL,
		AuthMode:    tgt.AuthMode,
		StartedAt:   deref(run.StartedAt, t0),
		Status:      db.RunDone,
		Versions:    report.ToolVersions{Auditloop: w.ToolVersion, AxeCore: crawler.AxeVersion},
	}
	rep.Summary.URLsDiscovered = crawlRes.URLsDiscovered
	rep.Summary.URLsBlocked = crawlRes.URLsBlocked

	crawledURLs := map[string]bool{}
	for _, pr := range crawlRes.Pages {
		crawledURLs[pr.URL] = true
		if err := w.persistPage(ctx, run, targetSlug, pr, rep); err != nil {
			log.Printf("worker: persist page %s: %v", pr.URL, err)
		}
		metrics.PagesCrawled.Inc()
	}
	rep.Summary.PagesCrawled = len(crawledURLs)
	metrics.URLsBlocked.Add(float64(crawlRes.URLsBlocked))
	rep.FinishedAt = time.Now().UTC()

	// Best-effort site-favicon upload (captured SSRF-guarded during the crawl). A
	// failure never fails the run — the dashboard card degrades to a name monogram.
	if len(crawlRes.Favicon) > 0 {
		fkey := storage.FaviconKey(targetSlug, run.ID, crawlRes.FaviconExt)
		ct := storage.FaviconContentType(crawlRes.FaviconExt)
		if err := w.Store.Put(ctx, fkey, ct, bytes.NewReader(crawlRes.Favicon), int64(len(crawlRes.Favicon))); err != nil {
			log.Printf("worker: upload favicon: %v", err)
		} else if err := w.DB.SetRunFavicon(run.ID, fkey); err != nil {
			log.Printf("worker: persist favicon key: %v", err)
		}
	}

	// P2 diff phase: compare this run to its baseline (prev_run_id). Persists
	// per-page diff_pct + diff images and returns the run-level summary; nil when
	// there is no baseline (first run for the target).
	var diffJSON string
	if d := w.runDiff(ctx, run, targetSlug); d != nil {
		rep.Diff = d
		if b, err := json.Marshal(d); err == nil {
			diffJSON = string(b)
		}
		log.Printf("worker: run %s diff: +%d/-%d pages, %d changed, %d new a11y rules",
			run.ID, len(d.PagesAdded), len(d.PagesRemoved), d.PagesChanged, len(d.NewA11yRules))
	}

	// Upload the run-level report.json (the forward-compat contract, now with the
	// optional diff block).
	if b, err := rep.Marshal(); err == nil {
		key := storage.ReportKey(targetSlug, run.ID)
		if err := w.Store.Put(ctx, key, "application/json", bytes.NewReader(b), int64(len(b))); err != nil {
			log.Printf("worker: upload report.json: %v", err)
		}
	}

	summary, _ := json.Marshal(rep.Summary)
	if err := w.DB.FinishRun(run.ID, db.RunDone, string(summary), ""); err != nil {
		return err
	}
	if diffJSON != "" {
		if err := w.DB.SetRunDiff(run.ID, diffJSON); err != nil {
			log.Printf("worker: persist run diff: %v", err)
		}
	}
	metrics.RunsTotal.WithLabelValues(db.RunDone).Inc()
	metrics.CrawlDuration.Observe(time.Since(t0).Seconds())
	log.Printf("worker: run %s done (%d pages, %d blocked, %s)", run.ID, rep.Summary.PagesCrawled, rep.Summary.URLsBlocked, time.Since(t0).Round(time.Millisecond))
	return nil
}

// buildLogin resolves a target's login recipe into a crawler.LoginConfig with
// DECRYPTED credentials, when the target is in auth_mode=login. It returns
// (nil, nil) for a non-login target. Credential values are never logged here.
func (w *Worker) buildLogin(tgt *db.Target) (*crawler.LoginConfig, error) {
	if tgt.AuthMode != db.AuthLogin {
		return nil, nil
	}
	if w.Cipher == nil {
		return nil, errors.New("target requires login but no encryption key is configured (set AUDITLOOP_ENCRYPTION_KEY)")
	}
	lr, err := w.DB.GetLoginRecipe(tgt.ID)
	if err != nil {
		return nil, errors.New("login recipe not found for target")
	}
	steps, err := recipe.ParseSteps(lr.StepsJSON)
	if err != nil {
		return nil, errors.New("stored login recipe is invalid: " + err.Error())
	}
	if err := recipe.Validate(steps); err != nil {
		return nil, errors.New("stored login recipe is invalid: " + err.Error())
	}
	plain, err := w.Cipher.DecryptFromBase64(lr.CredsEncrypted)
	if err != nil {
		return nil, errors.New("could not decrypt login credentials (key rotated?)")
	}
	creds, err := recipe.ParseCredentials(plain)
	if err != nil {
		return nil, errors.New("stored login credentials are malformed")
	}
	return &crawler.LoginConfig{Steps: steps, Credentials: creds.Map()}, nil
}

func (w *Worker) persistPage(ctx context.Context, run *db.Run, targetSlug string, pr crawler.PageResult, rep *report.Report) error {
	pageSlug := storage.PageSlug(pr.URL)

	// Upload artifacts.
	shotKey := storage.ScreenshotKey(targetSlug, run.ID, pageSlug, pr.Viewport.Name)
	if len(pr.ScreenshotPNG) > 0 {
		_ = w.Store.Put(ctx, shotKey, "image/png", bytes.NewReader(pr.ScreenshotPNG), int64(len(pr.ScreenshotPNG)))
	}
	var axeKey string
	if len(pr.AxeJSON) > 0 {
		axeKey = storage.AxeKey(targetSlug, run.ID, pageSlug)
		_ = w.Store.Put(ctx, axeKey, "application/json", bytes.NewReader(pr.AxeJSON), int64(len(pr.AxeJSON)))
	}
	var netKey string
	if len(pr.NetworkLogJSON) > 0 {
		netKey = storage.NetworkKey(targetSlug, run.ID, pageSlug)
		_ = w.Store.Put(ctx, netKey, "application/json", bytes.NewReader(pr.NetworkLogJSON), int64(len(pr.NetworkLogJSON)))
	}
	// DOM/accessibility digest (Phase 1 persona-evaluator grounding). Best-effort:
	// an empty digest (capture failed) simply leaves the key unset → the eval degrades.
	var a11yKey string
	if len(pr.A11yDigestJSON) > 0 {
		a11yKey = storage.A11yDigestKey(targetSlug, run.ID, pageSlug)
		_ = w.Store.Put(ctx, a11yKey, "application/json", bytes.NewReader(pr.A11yDigestJSON), int64(len(pr.A11yDigestJSON)))
	}

	// Classify counts.
	var cFirst, cThird, nFirst, nThird int
	for _, ce := range pr.ConsoleErrors {
		if ce.FirstPart {
			cFirst++
		} else {
			cThird++
		}
	}
	for _, ne := range pr.NetworkErrors {
		if ne.FirstPart {
			nFirst++
		} else {
			nThird++
		}
	}

	pageID, err := w.DB.InsertPage(&db.Page{
		RunID: run.ID, URL: pr.URL, Viewport: pr.Viewport.Name,
		ScreenshotKey: shotKey, AxeKey: axeKey, A11yDigestKey: a11yKey,
		AxeViolationCount:      pr.AxeViolations,
		ConsoleFirstPartyCount: cFirst, ConsoleThirdPartyCount: cThird,
		NetworkFirstPartyCount: nFirst, NetworkThirdPartyCount: nThird,
		LoadMS:      pr.LoadMS,
		LCPMs:       pr.LCPMs,
		CLS:         pr.CLS,
		TBTMs:       pr.TBTMs,
		WeightBytes: pr.WeightBytes,
		ReqCount:    pr.ReqCount,
	})
	if err != nil {
		return err
	}

	// Findings (single source of truth for both the DB rows and the report):
	// a11y + console + network (status-aware) + perf + layout smells.
	findings := w.pageFindings(pr)
	for _, f := range findings {
		_, _ = w.DB.InsertFinding(&db.Finding{PageID: pageID, Type: f.Type, Severity: f.Severity, Detail: string(f.Detail)})
	}

	// Roll up into the report.
	prp := report.PageReport{
		URL: pr.URL, Viewport: pr.Viewport.Name, Width: pr.Viewport.Width,
		ScreenshotKey: shotKey, AxeKey: axeKey, A11yDigestKey: a11yKey, NetworkKey: netKey, LoadMS: pr.LoadMS,
		Console: report.Origins{FirstParty: cFirst, ThirdParty: cThird},
		Network: report.Origins{FirstParty: nFirst, ThirdParty: nThird},
		A11y:    report.A11y{ViolationCount: pr.AxeViolations, NodeCount: pr.AxeNodes},
		Perf: &report.Perf{
			LCPMs: pr.LCPMs, CLS: pr.CLS, TBTMs: pr.TBTMs,
			WeightBytes: pr.WeightBytes, ReqCount: pr.ReqCount,
		},
		Layout: &report.LayoutSmells{
			HorizontalOverflow: pr.Layout.HorizontalOverflow, ScrollWidth: pr.Layout.ScrollWidth,
			InnerWidth: pr.Layout.InnerWidth, SmallTapTargets: pr.Layout.SmallTapTargets,
			SmallText: pr.Layout.SmallText, MissingViewportMeta: pr.Layout.MissingViewportMeta,
			ImagesNoDims: pr.Layout.ImagesNoDims, Examples: pr.Layout.Examples,
		},
	}
	prp.Findings = findings
	rep.Pages = append(rep.Pages, prp)
	rep.Summary.A11yViolations += pr.AxeViolations
	rep.Summary.ConsoleFirst += cFirst
	rep.Summary.ConsoleThird += cThird
	rep.Summary.NetworkFirst += nFirst
	rep.Summary.NetworkThird += nThird
	return nil
}

// pageFindings normalizes every deterministic signal on a crawled page+viewport
// into report.Findings — a11y violations, first/third-party console + status-aware
// network errors, web-vitals perf breaches, and DOM layout smells. It is the single
// source of truth: persistPage inserts one db.Finding per entry AND rolls the same
// list into report.json (so the DB and report never drift).
func (w *Worker) pageFindings(pr crawler.PageResult) []report.Finding {
	var out []report.Finding
	if len(pr.AxeJSON) > 0 {
		var ar struct {
			Violations []json.RawMessage `json:"violations"`
		}
		if json.Unmarshal(pr.AxeJSON, &ar) == nil {
			for _, v := range ar.Violations {
				var meta struct {
					Impact string `json:"impact"`
				}
				sev := "moderate"
				if json.Unmarshal(v, &meta) == nil && meta.Impact != "" {
					sev = meta.Impact
				}
				out = append(out, report.Finding{Type: "a11y", Severity: sev, Detail: v})
			}
		}
	}
	for _, ce := range pr.ConsoleErrors {
		det, _ := json.Marshal(ce)
		out = append(out, report.Finding{Type: "console", Severity: sevForConsole(ce.FirstPart), Detail: det})
	}
	for _, ne := range pr.NetworkErrors {
		det, _ := json.Marshal(ne)
		out = append(out, report.Finding{Type: "network", Severity: sevForNetwork(ne), Detail: det})
	}
	// Deterministic perf + layout signals (web-vitals breaches, DOM smells).
	out = append(out, perfFindings(pr)...)
	out = append(out, layoutFindings(pr, pr.Viewport.Mobile)...)
	return out
}

func (w *Worker) fail(run *db.Run, msg string) error {
	_ = w.DB.FinishRun(run.ID, db.RunFailed, "{}", msg)
	metrics.RunsTotal.WithLabelValues(db.RunFailed).Inc()
	return nil
}

func sevForConsole(firstParty bool) string {
	if firstParty {
		return "serious"
	}
	return "info"
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func deref(t *time.Time, def time.Time) time.Time {
	if t != nil {
		return *t
	}
	return def
}
