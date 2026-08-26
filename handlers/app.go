// Package handlers wires the HTTP router: static assets, auth endpoints, the
// PWA shell, target/run pages + API, artifact proxy, health, and metrics. It
// also starts the background worker goroutine when the role includes it.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ZacxDev/auditloop/components"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/crypto"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/eval"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/notes"
	"github.com/ZacxDev/auditloop/internal/storage"
	"github.com/ZacxDev/auditloop/internal/walkthrough"
	"github.com/ZacxDev/auditloop/internal/worker"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// App holds shared server dependencies.
type App struct {
	Cfg        config.AppConfig
	DB         *db.DB
	Store      storage.Store
	Verifier   *auth.Verifier
	JSVersion  string
	CSSVersion string
	StaticDir  string
	// Notes is the P3 vision-LLM UX-notes generator, non-nil only when
	// OpenRouterEnabled() (a server-side API key is configured).
	Notes *notes.Generator
	// Eval is the persona-walkthrough evaluator (Phase 1), non-nil only when
	// OpenRouterEnabled(). Parallel to Notes; the two LLM passes coexist.
	Eval *eval.Generator
	// Walk is the Phase-3 goal-directed walkthrough driver, non-nil only when
	// OpenRouterEnabled() AND this role runs the worker (it drives chromedp).
	Walk *walkthrough.Generator
	// Cipher encrypts/decrypts P4 login-recipe credentials. Non-nil only when a
	// valid AUDITLOOP_ENCRYPTION_KEY is configured (LoginRecipesEnabled); when nil
	// the auth UI is hidden and the auth routes return 503.
	Cipher *crypto.Cipher
	// loginTestLimiter throttles POST /login-test (spawns a headless Chromium).
	loginTestLimiter *minIntervalLimiter
	// pluginPushLimiter throttles POST /api/plugins/runs per target (P5).
	pluginPushLimiter *minIntervalLimiter
	// apiReadLimiter throttles the read API per key (token bucket, generous for
	// agent polling).
	apiReadLimiter *tokenBucketLimiter
}

// LoginRecipesEnabled reports whether the P4 feature is live (key present AND it
// parsed into a usable cipher).
func (a *App) LoginRecipesEnabled() bool { return a.Cipher != nil }

// NewRouter builds the fully-wired HTTP handler. It runs the startup stale-run
// sweep and starts the worker goroutine when the role includes the worker.
func NewRouter(ctx context.Context, cfg config.AppConfig, database *db.DB, store storage.Store) (http.Handler, error) {
	staticDir := "static"
	app := &App{
		Cfg:        cfg,
		DB:         database,
		Store:      store,
		Verifier:   auth.NewVerifier(cfg.SupabaseJWTSecret, cfg.DevMode),
		JSVersion:  assetVersion(staticDir + "/js/app.js"),
		CSSVersion: assetVersion(staticDir + "/output.css"),
		StaticDir:  staticDir,
		// Throttle login-test to at most one headless-browser probe per user every
		// 5s (SSRF-brute / DoS guard; see minIntervalLimiter).
		loginTestLimiter: newMinIntervalLimiter(5 * time.Second),
		// Throttle plugin pushes to at most one every 2s per target (abuse/DoS
		// guard; a healthy CI push cadence is far slower).
		pluginPushLimiter: newMinIntervalLimiter(2 * time.Second),
		// Read API: up to 10 requests/second per key with a burst of 20 — generous
		// enough for an agent to walk a run's pages/artifacts, but caps abuse.
		apiReadLimiter: newTokenBucketLimiter(10, 20),
	}

	// P3: build the vision-LLM notes generator when an OpenRouter key is set.
	// Phase 1: build the persona-walkthrough evaluator on the SAME key, bound to the
	// first curated model (personas — not models — are the axis).
	if cfg.OpenRouterEnabled() {
		client := llm.New(cfg.OpenRouterBaseURL, cfg.OpenRouterAPIKey, cfg.LLMMaxTokens)
		app.Notes = notes.New(database, store, client)
		app.Eval = eval.New(database, store, client, cfg.Models()[0])
		// Three completion-budget tiers, all via per-call llm.WithMaxTokens: the P3
		// notes calls keep the client default (LLMMaxTokens, 1024); the per-page
		// eval generation+verification calls get a larger middle tier (2000, a verbose
		// verdict overflows 1024 and truncates → lost findings); the run-level
		// synthesis call gets the largest budget (3000, its ranked JSON is bigger still).
		app.Eval.SynthMaxTokens = cfg.LLMSynthMaxTokens
		app.Eval.GenMaxTokens = cfg.LLMEvalMaxTokens
		// Phase 3: the goal-directed driver needs chromedp, so build it only when this
		// role runs the worker. Cipher is wired below (after it is built).
		if cfg.RunsWorker() {
			app.Walk = walkthrough.New(database, store, client, cfg.Models()[0])
			app.Walk.MaxTokens = cfg.LLMDriveMaxTokens
			app.Walk.ChromiumPath = cfg.ChromiumPath
			app.Walk.AllowLoopback = cfg.CrawlAllowLoopback
			app.Walk.InternalAllowHosts = cfg.InternalAllowHosts
		}
	}

	// P4: build the credential cipher when a valid encryption key is set. A
	// present-but-invalid key logs and disables the feature (fail-safe: never
	// silently store credentials under a bad key).
	if cfg.LoginRecipesEnabled() {
		cipher, err := crypto.NewFromString(cfg.EncryptionKey)
		if err != nil {
			log.Printf("startup: AUDITLOOP_ENCRYPTION_KEY invalid (%v) — login recipes DISABLED", err)
		} else {
			app.Cipher = cipher
		}
	}
	// P4 cipher powers authenticated driving too (decrypt login creds).
	if app.Walk != nil {
		app.Walk.Cipher = app.Cipher
	}

	// Startup sweep: settle runs orphaned in 'running' by a restart.
	if n, err := database.RecoverStaleRuns(); err != nil {
		log.Printf("startup: recover stale runs: %v", err)
	} else if n > 0 {
		log.Printf("startup: swept %d stale run(s) → failed", n)
	}
	// P3: settle notes jobs orphaned in 'generating' by a restart.
	if n, err := database.MarkGeneratingNotesFailed(); err != nil {
		log.Printf("startup: recover stale notes jobs: %v", err)
	} else if n > 0 {
		log.Printf("startup: swept %d stale notes job(s) → failed", n)
	}
	// Phase 1: settle persona-walkthrough jobs orphaned in 'generating' by a restart.
	if n, err := database.MarkGeneratingEvalFailed(); err != nil {
		log.Printf("startup: recover stale eval jobs: %v", err)
	} else if n > 0 {
		log.Printf("startup: swept %d stale eval job(s) → failed", n)
	}
	// Phase 3: settle goal-directed walkthroughs orphaned in 'driving' by a restart.
	if n, err := database.MarkDrivingWalkthroughsFailed(); err != nil {
		log.Printf("startup: recover stale walkthroughs: %v", err)
	} else if n > 0 {
		log.Printf("startup: swept %d stale walkthrough(s) → failed", n)
	}

	// Worker goroutine (role=worker|all).
	if cfg.RunsWorker() {
		w := worker.New(database, store, cfg.ChromiumPath, cfg.CrawlMaxPages, cfg.CrawlMaxDepth)
		w.AllowLoopback = cfg.CrawlAllowLoopback
		w.InternalAllowHosts = cfg.InternalAllowHosts
		w.Cipher = app.Cipher // P4: decrypt login-recipe creds (nil when disabled)
		go w.Run(ctx)
	}

	r := mux.NewRouter()

	// Public, unauthenticated endpoints.
	r.HandleFunc("/healthz", app.handleHealthz).Methods("GET")
	r.HandleFunc("/readyz", app.handleReadyz).Methods("GET")
	r.Handle("/metrics", promhttp.Handler()).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))
	r.HandleFunc("/manifest.webmanifest", app.handleManifest).Methods("GET")
	r.HandleFunc("/sw.js", app.handleServiceWorker).Methods("GET")
	r.HandleFunc("/login", app.handleLogin).Methods("GET")
	// P5 plugin push: PUBLIC route authenticated by the target's bearer push token
	// (NOT Supabase auth). Registered outside the auth subrouter.
	r.HandleFunc("/api/plugins/runs", app.handlePluginPush).Methods("POST")

	// Read API: PUBLIC routes authenticated by a per-user read-only API key
	// (Bearer), NOT a Supabase JWT. Registered outside the Supabase subrouter, on
	// their own subrouter guarded by apiKeyAuth. Every read is scoped to the key's
	// owner (ownership enforced in the DB join). READ-ONLY — no mutation routes.
	audit := r.NewRoute().Subrouter()
	audit.Use(app.apiKeyAuth)
	audit.HandleFunc("/api/audit/targets/{id}/runs", app.handleAuditRunList).Methods("GET")
	audit.HandleFunc("/api/audit/targets/{id}/runs/latest", app.handleAuditLatestRun).Methods("GET")
	audit.HandleFunc("/api/audit/runs/{run_id}", app.handleAuditRunReport).Methods("GET")
	audit.HandleFunc("/api/audit/runs/{run_id}/evaluation", app.handleAuditRunEvaluation).Methods("GET")
	audit.HandleFunc("/api/audit/targets/{id}/audit-config", app.handleAuditTargetConfig).Methods("GET")
	audit.HandleFunc("/api/audit/walkthroughs/{id}", app.handleAuditWalkthrough).Methods("GET")
	audit.HandleFunc("/api/audit/artifacts/{key:.+}", app.handleAuditArtifact).Methods("GET")

	// Everything below is auth-aware (Middleware injects claims; RequireAuth gates).
	sub := r.NewRoute().Subrouter()
	sub.Use(app.Verifier.Middleware)

	// Auth sync/signout only need the middleware (sync sets the cookie itself).
	sub.HandleFunc("/api/auth/sync", app.handleAuthSync).Methods("POST")
	sub.HandleFunc("/api/auth/signout", app.handleSignout).Methods("POST")

	// Gated app routes.
	gated := sub.NewRoute().Subrouter()
	gated.Use(auth.RequireAuth)

	gated.HandleFunc("/", app.handleIndex).Methods("GET")
	gated.HandleFunc("/dashboard", app.handleDashboard).Methods("GET")
	gated.HandleFunc("/targets/{id}", app.handleTargetView).Methods("GET")
	gated.HandleFunc("/runs/{id}", app.handleRunView).Methods("GET")
	gated.HandleFunc("/runs/{id}/status", app.handleRunStatus).Methods("GET")
	gated.HandleFunc("/runs/{id}/notes-status", app.handleNotesStatus).Methods("GET")
	gated.HandleFunc("/runs/{id}/eval-status", app.handleEvalStatus).Methods("GET")
	gated.HandleFunc("/targets/{id}/walkthrough-status", app.handleWalkthroughStatus).Methods("GET")

	gated.HandleFunc("/api/targets", app.handleCreateTarget).Methods("POST")
	gated.HandleFunc("/api/targets/{id}/runs", app.handleCreateRun).Methods("POST")
	// P5 plugin targets (push-only): create + rotate token.
	gated.HandleFunc("/api/plugins/targets", app.handleCreatePluginTarget).Methods("POST")
	gated.HandleFunc("/api/targets/{id}/plugin-token/rotate", app.handleRotatePluginToken).Methods("POST")
	// P4 login recipes (authenticated crawl).
	gated.HandleFunc("/api/targets/{id}/auth", app.handleSaveAuth).Methods("POST")
	gated.HandleFunc("/api/targets/{id}/login-test", app.handleLoginTest).Methods("POST")
	// Persona-walkthrough Phase 2: per-target audit config (infer draft + save/confirm).
	gated.HandleFunc("/api/targets/{id}/audit-config/infer", app.handleInferAuditConfig).Methods("POST")
	gated.HandleFunc("/api/targets/{id}/audit-config", app.handleSaveAuditConfig).Methods("POST")
	// Phase 3: goal-directed walkthrough driver (gated: driving_enabled + goal/success).
	gated.HandleFunc("/api/targets/{id}/walkthrough", app.handleStartWalkthrough).Methods("POST")
	// Phase 3 PR-B: evaluate a completed walkthrough's driven trace with personas
	// (materialize a synthetic run → reuse the Phase-1 eval stack).
	gated.HandleFunc("/api/targets/{id}/walkthroughs/{wid}/evaluate", app.handleEvaluateWalkthrough).Methods("POST")
	gated.HandleFunc("/api/runs/{id}/notes", app.handleGenerateNotes).Methods("POST")
	gated.HandleFunc("/api/runs/{id}/evaluate", app.handleGenerateEval).Methods("POST")
	// Read-API key management (human mints/revokes keys; Supabase-authed).
	gated.HandleFunc("/api/keys", app.handleCreateAPIKey).Methods("POST")
	gated.HandleFunc("/api/keys/{id}/revoke", app.handleRevokeAPIKey).Methods("POST")
	// {model:.+} because curated model ids contain slashes (e.g. anthropic/claude-haiku-4.5).
	gated.HandleFunc("/api/pages/{pageId}/notes/{model:.+}", app.handleSaveNote).Methods("POST")

	// Artifact proxy (filesystem backend; S3 backend redirects to presigned URL).
	gated.HandleFunc("/artifacts/{key:.*}", app.handleArtifact).Methods("GET")

	// Global defense-in-depth response headers on every route. Deliberately
	// conservative: nosniff + clickjacking + referrer trimming, but NO
	// Content-Security-Policy (the app uses htmx, inline scripts/styles, and
	// supabase-js — a strict CSP would break it; a broken app is worse than a
	// missing CSP). Header-only, so redirects/302s/htmx swaps are unaffected.
	return secureHeaders(r), nil
}

// secureHeaders wraps a handler and sets conservative security headers on every
// response. It only adds headers (never touches the body or status), so it is
// transparent to redirects, presigned-URL 302s, and htmx swaps.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// ctx builds a PageContext from the request.
func (a *App) pageCtx(r *http.Request, title string) components.PageContext {
	email := ""
	if c, ok := auth.ClaimsFrom(r.Context()); ok {
		email = c.Email
	}
	return components.PageContext{
		Title:           title,
		UserEmail:       email,
		DevMode:         a.Cfg.DevMode,
		SupabaseURL:     a.Cfg.SupabaseURL,
		SupabaseAnonKey: a.Cfg.SupabaseAnonKey,
		JSVersion:       a.JSVersion,
		CSSVersion:      a.CSSVersion,
	}
}

// OpenStore builds the configured storage backend (S3 when an endpoint is set,
// else the local filesystem backend).
func OpenStore(ctx context.Context, cfg config.AppConfig) (storage.Store, error) {
	if cfg.StorageIsLocal() {
		return storage.NewFS(cfg.S3Local)
	}
	return storage.NewS3(ctx, storage.S3Config{
		Endpoint:     cfg.S3Endpoint,
		Bucket:       cfg.S3Bucket,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		Region:       cfg.S3Region,
		UseSSL:       cfg.S3UseSSL,
		UsePathStyle: cfg.S3UsePathStyle,
	})
}

// assetVersion returns a short content hash of a file (cache-busting), or a
// time-based fallback if the file can't be read.
func assetVersion(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Now().Format("20060102150405")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}
