package walkthrough

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/crypto"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/recipe"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// DefaultMaxActions is the per-pass action budget (also the initial steps_total).
const DefaultMaxActions = 20

// DriveFunc is the driver entrypoint (injectable so tests stub the browser).
type DriveFunc func(ctx context.Context, opts crawler.DriveOptions) (*crawler.DriveTrace, error)

// Generator runs the async goal-directed walkthrough pass for a target.
type Generator struct {
	DB            *db.DB
	Store         storage.Store
	LLM           Drafter
	Model         string
	MaxTokens     int
	Cipher        *crypto.Cipher // decrypts P4 login creds (nil → auth_mode=login walkthroughs fail clearly)
	ChromiumPath  string
	AllowLoopback bool
	// InternalAllowHosts is the exact-match internal-host SSRF allowlist (see
	// crawler.GuardConfig.InternalAllowHosts) — empty in normal deployments.
	InternalAllowHosts []string
	MaxActions         int
	// Drive is the driver entrypoint (defaults to crawler.Drive; tests stub it).
	Drive DriveFunc
}

// New builds a Generator bound to a single model (the first curated model), with
// the real crawler.Drive wired in.
func New(database *db.DB, store storage.Store, client Drafter, model string) *Generator {
	return &Generator{
		DB:         database,
		Store:      store,
		LLM:        client,
		Model:      model,
		MaxTokens:  DefaultMaxTokens,
		MaxActions: DefaultMaxActions,
		Drive:      crawler.Drive,
	}
}

// Run executes the walkthrough pass for a walkthrough id: it loads the target's
// goal + deterministic success condition + (optional) login recipe, drives toward
// the goal with the LLM planner, persists each step's screenshot + record, and
// finalizes the deterministic outcome (success|stuck|failed). Per-step persistence
// degrades in place; only an infra/config failure fails the whole pass.
func (g *Generator) Run(ctx context.Context, walkthroughID string) error {
	t0 := time.Now()
	wk, err := g.DB.GetWalkthroughByID(walkthroughID)
	if err != nil {
		return err
	}
	tgt, err := g.DB.GetTargetByID(wk.TargetID)
	if err != nil {
		// Setup failure: the drive never ran, so this is not evidence about the product.
		return g.fail(wk.ID, "target lookup failed", true)
	}
	// Defense in depth: the driving gate is enforced at the route AND here.
	if !tgt.DrivingEnabled {
		// Config failure: nothing was driven — not a product regression.
		return g.fail(wk.ID, "driving is not enabled for this target", true)
	}

	cfg, found, err := g.DB.GetTargetAuditConfig(tgt.UserID, tgt.ID)
	if err != nil || !found {
		// Config failure: no goal to drive toward — the product was never exercised.
		return g.fail(wk.ID, "no audit configuration — set a goal and success condition first", true)
	}
	success := action.SuccessAssertion{
		Selector:    strings.TrimSpace(cfg.SuccessSelector),
		URLContains: strings.TrimSpace(cfg.SuccessURLContains),
		TimeoutMs:   cfg.SuccessTimeoutMs,
	}
	if success.IsZero() {
		// Config failure: with no success assertion nothing could be observed.
		return g.fail(wk.ID, "no success condition configured — cannot detect goal completion", true)
	}
	goal := strings.TrimSpace(wk.Goal)
	if goal == "" {
		goal = strings.TrimSpace(cfg.PrimaryJob)
	}

	loginCfg, err := g.buildLogin(tgt)
	if err != nil {
		// Setup failure (missing key / undecryptable creds / malformed stored recipe):
		// the browser never drove the funnel, so this says nothing about the product.
		return g.fail(wk.ID, err.Error(), true)
	}

	planner := &Planner{
		LLM:       g.LLM,
		Model:     g.Model,
		MaxTokens: g.MaxTokens,
		OnUsage: func(u llm.Usage) {
			if u.CostUSD == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
				return
			}
			if err := g.DB.AddWalkthroughCost(wk.ID, u.CostUSD, u.PromptTokens, u.CompletionTokens); err != nil {
				log.Printf("walkthrough: add cost %s: %v", wk.ID, err)
			}
			metrics.WalkthroughCostUSD.Add(u.CostUSD)
		},
	}

	maxActions := g.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultMaxActions
	}
	drive := g.Drive
	if drive == nil {
		drive = crawler.Drive
	}

	trace, derr := drive(ctx, crawler.DriveOptions{
		BaseURL:      tgt.BaseURL,
		AllowedHosts: allowedHosts(tgt),
		Login:        loginCfg,
		Goal:         goal,
		Success:      success,
		Planner:      planner,
		MaxActions:   maxActions,
		// Defense in depth: never LOOSEN dry-run. Re-derive from the LIVE target
		// (loaded above) and only ever TIGHTEN — a stored wk.DryRun can only be
		// overridden toward MORE guarding, never toward real submits. So even if a
		// walkthrough row was created dry_run=false, a target that is not currently
		// allow_real_submit still drives under the submit-guard.
		DryRun:             wk.DryRun || !tgt.AllowRealSubmit,
		AllowLoopback:      g.AllowLoopback,
		InternalAllowHosts: g.InternalAllowHosts,
		ChromiumPath:       g.ChromiumPath,
	})
	// #45: "the driver could not run" is a STRUCTURAL signal, never a substring match
	// on the reason prose — either the trace says so in band, or the out-of-band error
	// is an infrastructure error (crawler.ErrBrowserStalled / crawler.ErrDriverInfra).
	infra := (trace != nil && trace.InfraFailed) || crawler.IsInfraFailure(derr)
	if derr != nil {
		if ctx.Err() != nil {
			// A cancelled pass observed nothing either.
			_ = g.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeFailed, 0, "cancelled", true)
			return ctx.Err()
		}
		// maxDriverErr, not 200: the #41 render-probe error is ~344 chars and its
		// ACTIONABLE half ("Install fontconfig plus a font package…") lives at the
		// END, so a 200-char cut silently discarded the only part the operator can
		// act on and left them staring at "…page loads never settle".
		return g.fail(wk.ID, "driver error: "+truncate(derr.Error(), maxDriverErr), infra)
	}

	// Persist the deterministic trace: one screenshot per step + the row, updating
	// progress as we go.
	_ = g.DB.UpdateWalkthroughProgress(wk.ID, 0)
	for i, step := range trace.Steps {
		shotKey := ""
		if len(step.ScreenshotPNG) > 0 {
			shotKey = storage.WalkthroughStepKey(tgt.ID, wk.ID, step.Idx)
			if perr := g.Store.Put(ctx, shotKey, "image/png", bytes.NewReader(step.ScreenshotPNG), int64(len(step.ScreenshotPNG))); perr != nil {
				log.Printf("walkthrough: upload step %d shot: %v", step.Idx, perr)
				shotKey = ""
			}
		}
		actionJSON, _ := json.Marshal(step.Action)
		if _, perr := g.DB.InsertWalkthroughStep(&db.WalkthroughStep{
			WalkthroughID: wk.ID,
			Idx:           step.Idx,
			ActionJSON:    string(actionJSON),
			URL:           step.URL,
			ScreenshotKey: shotKey,
			Outcome:       step.Outcome,
			PlannerReason: step.PlannerReason,
			// Phase 2: persist the per-step DOM/a11y digest so materialization can carry
			// it into the synthetic run's pages for driven-path evaluator grounding.
			DigestJSON: string(step.A11yDigestJSON),
		}); perr != nil {
			log.Printf("walkthrough: insert step %d: %v", step.Idx, perr)
		}
		metrics.WalkthroughSteps.WithLabelValues(stepOutcomeLabel(step.Outcome)).Inc()
		_ = g.DB.UpdateWalkthroughProgress(wk.ID, i+1)
	}

	outcome := trace.Outcome
	if outcome == "" {
		outcome = db.WalkOutcomeStuck
	}
	if err := g.DB.FinishWalkthrough(wk.ID, outcome, trace.StuckStep, trace.Reason, infra); err != nil {
		log.Printf("walkthrough: finish %s: %v", wk.ID, err)
	}
	// Phase 4: diff this walkthrough against the target's previous terminal walkthrough
	// (the baseline stamped at CreateWalkthrough). The deterministic outcome/stuck delta
	// is available now; the persona-blocker delta degrades (no eval yet) until the trace
	// is evaluated. Non-fatal — a diff failure never fails the drive. This is the ONLY
	// place the outcome/stuck regression metric is bumped (counted once per walkthrough).
	if d, derr := RefreshWalkthroughDiff(ctx, g.DB, wk.ID); derr != nil {
		log.Printf("walkthrough: diff %s: %v", wk.ID, derr)
	} else if d != nil && d.IsRegression {
		metrics.WalkthroughRegressions.Inc()
	}
	metrics.WalkthroughRuns.WithLabelValues(outcome).Inc()
	metrics.WalkthroughDuration.Observe(time.Since(t0).Seconds())
	log.Printf("walkthrough: %s done outcome=%s steps=%d (%s)", wk.ID, outcome, len(trace.Steps), time.Since(t0).Round(time.Millisecond))
	return nil
}

// buildLogin resolves a target's login recipe into a crawler.LoginConfig with
// DECRYPTED credentials when auth_mode=login. Mirrors worker.buildLogin; credential
// values are never logged. Returns (nil, nil) for a non-login target.
func (g *Generator) buildLogin(tgt *db.Target) (*crawler.LoginConfig, error) {
	if tgt.AuthMode != db.AuthLogin {
		return nil, nil
	}
	if g.Cipher == nil {
		return nil, errors.New("target requires login but no encryption key is configured (set AUDITLOOP_ENCRYPTION_KEY)")
	}
	lr, err := g.DB.GetLoginRecipe(tgt.ID)
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
	plain, err := g.Cipher.DecryptFromBase64(lr.CredsEncrypted)
	if err != nil {
		return nil, errors.New("could not decrypt login credentials (key rotated?)")
	}
	creds, err := recipe.ParseCredentials(plain)
	if err != nil {
		return nil, errors.New("stored login credentials are malformed")
	}
	return &crawler.LoginConfig{Steps: steps, Credentials: creds.Map()}, nil
}

// fail settles a walkthrough as failed. infra says the DRIVE never ran (a setup/config
// failure or an infrastructure failure), so the Phase-4 diff must not score it as a
// product regression (#45). Every call site sets it deliberately.
func (g *Generator) fail(id, reason string, infra bool) error {
	if err := g.DB.FinishWalkthrough(id, db.WalkOutcomeFailed, 0, reason, infra); err != nil {
		log.Printf("walkthrough: mark failed %s: %v", id, err)
	}
	metrics.WalkthroughRuns.WithLabelValues(db.WalkOutcomeFailed).Inc()
	return nil
}

func allowedHosts(t *db.Target) []string {
	if len(t.VerifiedDomains) > 0 {
		return t.VerifiedDomains
	}
	if h := hostOf(t.BaseURL); h != "" {
		return []string{h}
	}
	return nil
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// stepOutcomeLabel buckets a per-step outcome into a low-cardinality metric label.
func stepOutcomeLabel(outcome string) string {
	switch {
	case outcome == "ok":
		return "ok"
	case outcome == "success":
		return "success"
	case strings.HasPrefix(outcome, "submit-blocked"):
		return "submit-blocked"
	case strings.HasPrefix(outcome, "rejected"):
		return "rejected"
	case outcome == "ssrf-blocked" || strings.HasPrefix(outcome, "refused"):
		return "blocked"
	case strings.HasPrefix(outcome, "error"):
		return "error"
	default:
		return "other"
	}
}

// maxDriverErr bounds a persisted driver error. It must comfortably fit the
// longest ACTIONABLE message the driver can produce — currently the #41
// font-less render-probe diagnosis (~344 chars, remedy at the end).
const maxDriverErr = 600

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
