package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/eval"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/walkthrough"

	"github.com/gorilla/mux"
)

// Phase 3 — goal-directed walkthrough DRIVER routes. The driver is gated behind
// TWO independent, loud, default-OFF opt-ins: driving_enabled (may we drive at
// all) enforced at BOTH the route and the generator, and allow_real_submit (may we
// perform REAL mutating submissions vs. the default dry-run submit-guard). A
// walkthrough also requires a goal + a deterministic success condition (409).

// walkthroughVM builds the target-page walkthrough section view model. It is gated
// on OpenRouterEnabled() && driving_enabled (non-plugin targets). It surfaces the
// latest pass's outcome + ordered step trace (planner-authored strings rendered
// ESCAPED by the component).
func (a *App) walkthroughVM(uid string, t *db.Target) *pages.WalkthroughVM {
	vm := &pages.WalkthroughVM{
		TargetID:       t.ID,
		Enabled:        a.Walk != nil && a.Cfg.OpenRouterEnabled(),
		DrivingEnabled: t.DrivingEnabled,
		DryRunDefault:  !t.AllowRealSubmit,
	}
	if !vm.Enabled || !vm.DrivingEnabled || t.AuthMode == db.AuthPlugin {
		return vm
	}
	// A walkthrough needs a goal + a success condition to run.
	if cfg, found, err := a.DB.GetTargetAuditConfig(uid, t.ID); err == nil && found {
		vm.HasGoal = strings.TrimSpace(cfg.PrimaryJob) != ""
		vm.HasSuccess = strings.TrimSpace(cfg.SuccessSelector) != "" || strings.TrimSpace(cfg.SuccessURLContains) != ""
		vm.Goal = cfg.PrimaryJob
	}

	wk, err := a.DB.LatestWalkthroughForTarget(uid, t.ID)
	if err != nil || wk == nil {
		return vm
	}
	vm.HasRun = true
	vm.WalkthroughID = wk.ID
	vm.Status = wk.Status
	vm.Outcome = wk.Outcome
	vm.StuckStep = wk.StuckStep
	vm.Reason = wk.Reason
	vm.DryRun = wk.DryRun
	vm.Done = wk.StepsDone
	vm.Total = wk.StepsTotal
	vm.CostUSD = wk.CostUSD
	if wk.Goal != "" {
		vm.Goal = wk.Goal
	}

	if steps, err := a.DB.ListWalkthroughSteps(wk.ID); err == nil {
		for _, s := range steps {
			vm.Steps = append(vm.Steps, pages.WalkStepVM{
				Idx:           s.Idx + 1,
				Action:        prettyAction(s.ActionJSON),
				URL:           s.URL,
				Outcome:       s.Outcome,
				PlannerReason: s.PlannerReason,
				ScreenshotURL: artifactURL(s.ScreenshotKey),
			})
		}
	}

	vm.Changes = walkChangesVM(wk)

	// Phase-3 PR-B: the persona-evaluation control over the DRIVEN trace. Gated on the
	// OpenRouter key AND a terminal walkthrough with recorded steps (nothing to evaluate
	// before then). The trigger materializes a synthetic run then reuses the whole
	// Phase-1 eval stack; once a synthetic run exists we embed the existing
	// EvaluationSection (which self-polls + carries its own re-run keyed on the run id).
	a.attachWalkthroughEval(uid, t.ID, wk, vm)
	return vm
}

// attachWalkthroughEval wires the persona-evaluation block into the walkthrough view
// model: the curated persona options + the target's confirmed default personas, and —
// once a synthetic run has been materialized and its eval started — the embedded
// EvaluationSection view model for that run.
func (a *App) attachWalkthroughEval(uid, targetID string, wk *db.Walkthrough, vm *pages.WalkthroughVM) {
	vm.EvalEnabled = a.Eval != nil && a.Cfg.OpenRouterEnabled()
	vm.Terminal = wk.Status == db.WalkDone || wk.Status == db.WalkFailed
	if !vm.EvalEnabled || !vm.Terminal || len(vm.Steps) == 0 {
		return
	}
	for _, p := range eval.Personas {
		vm.EvalPersonas = append(vm.EvalPersonas, pages.PersonaOptVM{ID: p.ID, Label: p.Label, Cares: p.Cares})
	}
	if cfg, found, err := a.DB.GetTargetAuditConfig(uid, targetID); err == nil && found && len(cfg.Personas) > 0 {
		vm.EvalDefaultPersonas = map[string]bool{}
		for _, p := range cfg.Personas {
			vm.EvalDefaultPersonas[p] = true
		}
	}
	// If the synthetic run exists, embed the live EvaluationSection (results or poll).
	if wk.RunID != "" {
		if run, err := a.DB.GetRun(uid, wk.RunID); err == nil {
			vm.EvalRunID = run.ID
			vm.EvalStarted = run.EvalStatus != db.EvalIdle
			vm.Eval = a.evalVM(run)
		}
	}
}

// walkChangesVM builds the Phase-4 "Changes since last walkthrough" view model from a
// walkthrough's baseline link + persisted diff_json. No baseline → HasBaseline=false
// (renders the "first walkthrough" note). A diff not yet computed (empty diff_json but a
// baseline exists) → the baseline note too (the drive-end refresh normally fills it).
func walkChangesVM(wk *db.Walkthrough) *pages.WalkChangesVM {
	c := &pages.WalkChangesVM{HasBaseline: wk.PrevWalkthroughID != ""}
	if !c.HasBaseline || wk.DiffJSON == "" {
		return c
	}
	var d report.WalkthroughDiff
	if err := json.Unmarshal([]byte(wk.DiffJSON), &d); err != nil {
		return c
	}
	applyDiffCompat(wk.DiffJSON, &d)
	c.HasDiff = true
	c.OutcomeChanged = d.OutcomeChanged
	c.PrevOutcome = d.PrevOutcome
	c.Outcome = d.Outcome
	c.IsRegression = d.IsRegression
	c.Resolved = d.Resolved
	c.StuckStepDelta = d.StuckStepDelta
	c.ReasonChanged = d.ReasonChanged
	c.Reason = d.Reason
	c.NewTaskBlockers = d.NewTaskBlockers
	c.ResolvedTaskBlockers = d.ResolvedTaskBlockers
	c.BlockersCompared = d.BlockersCompared
	c.OutcomeCompared = d.OutcomeCompared
	c.InfraFailed = d.InfraFailed
	if !d.PrevAt.IsZero() {
		c.PrevAtLabel = d.PrevAt.Format("2006-01-02 15:04")
	}
	return c
}

// applyDiffCompat back-fills the #45 fields on a walkthrough diff persisted BEFORE
// migration 0064 existed. Such a blob has no "outcome_compared" key, so it decodes to
// false — which would misread every historical, legitimately-scored diff as "the driver
// could not run". Key ABSENCE is the discriminator: a pre-#45 diff was always an outcome
// comparison, so absence means compared. A blob written after the change always carries
// the key explicitly and is left untouched.
func applyDiffCompat(raw string, d *report.WalkthroughDiff) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return
	}
	if _, ok := keys["outcome_compared"]; !ok {
		d.OutcomeCompared = true
		d.InfraFailed = false
	}
}

// prettyAction renders a stored action JSON as a short human string (escaped by
// the component). Falls back to the raw JSON if it can't parse.
func prettyAction(actionJSON string) string {
	var a struct {
		Type      string `json:"type"`
		Selector  string `json:"selector"`
		Text      string `json:"text"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		Direction string `json:"direction"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal([]byte(actionJSON), &a); err != nil {
		return actionJSON
	}
	switch a.Type {
	case "click":
		return "click " + a.Selector
	case "type":
		return "type into " + a.Selector
	case "select":
		return "select " + a.Value + " in " + a.Selector
	case "press":
		return "press " + a.Key
	case "scroll":
		if a.Direction == "" {
			a.Direction = "down"
		}
		return "scroll " + a.Direction
	case "waitFor":
		return "waitFor " + a.Selector + a.URL
	case "navigate":
		return "navigate " + a.URL
	case "finish":
		return "finish"
	default:
		return a.Type
	}
}

// handleWalkthroughStatus returns the walkthrough section fragment (htmx poll
// target while a pass is driving).
func (a *App) handleWalkthroughStatus(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, pages.WalkthroughSection(a.walkthroughVM(uid, t)))
}

// handleStartWalkthrough enqueues an async goal-directed driver pass for a target.
// 503 when the feature is disabled; 403 when driving is not enabled for the
// target; 400 for a plugin target; 409 when the target has no goal + success
// condition. Claims the job + spawns the goroutine, returns the driving fragment.
func (a *App) handleStartWalkthrough(w http.ResponseWriter, r *http.Request) {
	if a.Walk == nil || !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "walkthrough driving is not enabled (no OpenRouter API key configured, or this role does not run the worker)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if t.AuthMode == db.AuthPlugin {
		http.Error(w, "plugin (push-only) targets cannot be driven", http.StatusBadRequest)
		return
	}
	if !t.DrivingEnabled {
		http.Error(w, "driving is not enabled for this target — turn it on in the audit configuration first", http.StatusForbidden)
		return
	}
	cfg, found, err := a.DB.GetTargetAuditConfig(uid, t.ID)
	if err != nil {
		http.Error(w, "failed to load the audit configuration", http.StatusInternalServerError)
		return
	}
	if !found || strings.TrimSpace(cfg.PrimaryJob) == "" ||
		(strings.TrimSpace(cfg.SuccessSelector) == "" && strings.TrimSpace(cfg.SuccessURLContains) == "") {
		http.Error(w, "set a primary job (goal) and a success condition before running a walkthrough", http.StatusConflict)
		return
	}

	dryRun := !t.AllowRealSubmit
	wk, err := a.DB.CreateWalkthrough(t.ID, "", cfg.PrimaryJob, dryRun)
	if err != nil {
		http.Error(w, "failed to create the walkthrough", http.StatusInternalServerError)
		return
	}
	won, err := a.DB.ClaimWalkthroughJob(wk.ID, walkthrough.DefaultMaxActions)
	if err != nil {
		http.Error(w, "failed to start the walkthrough", http.StatusInternalServerError)
		return
	}
	if won {
		go func(id string) {
			// The driver touches untrusted planner JSON + chromedp; a panic here would
			// otherwise crash the process. Recover, log it, and settle the walkthrough
			// to 'failed' (the same terminal state a driver error takes) so a poll
			// doesn't hang forever.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("walkthrough: panic driving %s: %v", id, r)
					// infra=true: a panic in OUR driver is not evidence about the
					// audited product, so it must not score as a regression (#45).
					if err := a.DB.FinishWalkthrough(id, db.WalkOutcomeFailed, 0, "internal error", true); err != nil {
						log.Printf("walkthrough: mark failed after panic %s: %v", id, err)
					}
					metrics.WalkthroughRuns.WithLabelValues(db.WalkOutcomeFailed).Inc()
				}
			}()
			_ = a.Walk.Run(context.Background(), id)
		}(wk.ID)
	}

	render(w, pages.WalkthroughSection(a.walkthroughVM(uid, t)))
}

// handleEvaluateWalkthrough runs the EXISTING Phase-1 persona evaluator over a
// COMPLETED walkthrough's DRIVEN trace (Phase-3 PR-B). It materializes the trace as a
// synthetic run + one page per step (idempotent), then reuses the entire eval stack:
// ClaimEvalJob + eval.Generator.Run keyed on the synthetic run id → the eval-status
// poll + the read-API /evaluation endpoint already work unchanged. 503 when the feature
// is disabled; 404 for a foreign/unknown walkthrough (owner-scoped, no existence leak);
// 409 when the walkthrough is not done or has no steps. Personas default from the
// target's confirmed audit config, else the Phase-1 default. Returns the eval fragment.
func (a *App) handleEvaluateWalkthrough(w http.ResponseWriter, r *http.Request) {
	if a.Eval == nil || !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "persona walkthrough is not enabled (no OpenRouter API key configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	vars := mux.Vars(r)
	targetID, wid := vars["id"], vars["wid"]

	wk, err := a.DB.GetWalkthrough(uid, wid) // owner-scoped → ErrNotFound for a foreign walkthrough
	if err != nil || wk.TargetID != targetID {
		http.NotFound(w, r)
		return
	}
	if wk.Status != db.WalkDone {
		http.Error(w, "the walkthrough must finish before it can be evaluated", http.StatusConflict)
		return
	}
	steps, err := a.DB.ListWalkthroughSteps(wid)
	if err != nil {
		http.Error(w, "failed to load the walkthrough trace", http.StatusInternalServerError)
		return
	}
	if len(steps) == 0 {
		http.Error(w, "the walkthrough has no steps to evaluate", http.StatusConflict)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	personas, ok := validatePersonas(r.Form["personas"])
	if !ok {
		http.Error(w, "unknown persona requested", http.StatusBadRequest)
		return
	}
	if len(personas) == 0 {
		personas = a.defaultWalkthroughPersonas(uid, targetID)
	}

	// Materialize the driven trace as a synthetic run + pages (idempotent).
	runID, err := a.DB.MaterializeWalkthroughRun(r.Context(), a.Store, uid, wid)
	if err != nil {
		http.Error(w, "failed to prepare the walkthrough for evaluation", http.StatusInternalServerError)
		return
	}

	job := strings.TrimSpace(wk.Goal)
	total, err := a.Eval.CountUnits(runID, personas)
	if err != nil {
		http.Error(w, "failed to plan the evaluation", http.StatusInternalServerError)
		return
	}
	won, err := a.DB.ClaimEvalJob(runID, job, total)
	if err != nil {
		http.Error(w, "failed to start the evaluation", http.StatusInternalServerError)
		return
	}
	if won {
		go func(runID string, personas []string, opts eval.Options) {
			// The pass touches untrusted model text; a panic would otherwise crash the
			// process and leave the job stuck 'generating'. Recover + settle → failed.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("walkthrough-eval: panic %s: %v", runID, rec)
					if err := a.DB.FinishEvalJob(runID, db.EvalFailed); err != nil {
						log.Printf("walkthrough-eval: mark failed after panic %s: %v", runID, err)
					}
				}
			}()
			_ = a.Eval.Run(context.Background(), runID, personas, opts)
			// Phase 4: now that the driven trace has persona evaluations, re-diff this
			// walkthrough vs its baseline so NewTaskBlockers fills in. No metric bump —
			// the outcome/stuck regression was already counted once at drive end.
			if _, derr := walkthrough.RefreshWalkthroughDiff(context.Background(), a.DB, wid); derr != nil {
				log.Printf("walkthrough-eval: diff refresh %s: %v", wid, derr)
			}
		}(runID, personas, eval.Options{Job: job, Verify: true})
	}

	run, err := a.DB.GetRun(uid, runID)
	if err != nil {
		http.Error(w, "failed to load the evaluation", http.StatusInternalServerError)
		return
	}
	run.EvalStatus = db.EvalGenerating
	run.EvalTotal = total
	run.EvalDone = 0
	run.EvalJob = job
	render(w, pages.EvaluationSection(a.evalVM(run)))
}

// defaultWalkthroughPersonas returns the persona set to evaluate a walkthrough with when
// the request omitted them: the target's confirmed audit-config personas (validated
// against the allowlist) if any, else the Phase-1 default (the first curated persona).
func (a *App) defaultWalkthroughPersonas(uid, targetID string) []string {
	if cfg, found, err := a.DB.GetTargetAuditConfig(uid, targetID); err == nil && found && len(cfg.Personas) > 0 {
		if out, ok := validatePersonas(cfg.Personas); ok && len(out) > 0 {
			return out
		}
	}
	return []string{eval.Personas[0].ID}
}

// --- Read API: machine-readable walkthrough trace ---

type apiWalkthrough struct {
	ID        string `json:"id"`
	Goal      string `json:"goal"`
	Outcome   string `json:"outcome"`
	StuckStep int    `json:"stuck_step"`
	Reason    string `json:"reason,omitempty"`
	DryRun    bool   `json:"dry_run"`
	Status    string `json:"status"`
	// InfraFailed says this walkthrough failed because the DRIVER could not run (a
	// killed browser, a setup/config failure, a restart sweep) rather than because the
	// product regressed (#45). It is TOP-LEVEL, not only inside `regression`, because
	// the paths that produce it most often have NO diff at all: a config failure never
	// reaches the diff step, and a target's FIRST walkthrough has no baseline. A CI
	// consumer must always be able to tell "the driver could not run" from a verdict.
	InfraFailed bool `json:"infra_failed"`
	// EvalRunID is the synthetic run the persona evaluator ran over (set once the
	// walkthrough has been evaluated); a machine consumer pulls the persona findings via
	// GET /api/audit/runs/{eval_run_id}/evaluation.
	EvalRunID string `json:"eval_run_id,omitempty"`
	// Regression is the walkthrough-vs-previous-terminal-walkthrough diff (Phase 4),
	// present once a baseline exists.
	//
	// RECOMMENDED CONSUMER PREDICATE (CHANGED by #45 — read this before gating):
	//
	//	if infra_failed || (regression && !regression.outcome_compared) → INFRASTRUCTURE
	//	    error: the driver never observed the goal. RETRY; do not report a verdict.
	//	else if regression && (regression.is_regression ||
	//	                       len(regression.new_task_blockers) > 0) → REGRESSION: fail.
	//
	// The old predicate was `is_regression || len(new_task_blockers) > 0` alone. Before
	// #45 a browser stall surfaced as is_regression=true, so that predicate FAILED the
	// build — a false alarm, but safe. Now it is is_regression=false with empty
	// new_task_blockers, so a gate that ignores the infra fields PASSES on a walkthrough
	// that never ran. The failure direction moved from false-alarm to SILENT PASS: a
	// consumer that does not add the infra branch is strictly worse off than before.
	Regression *apiWalkRegression `json:"regression,omitempty"`
	Steps      []apiWalkStep      `json:"steps"`
}

// apiWalkRegression is the machine-readable walkthrough regression block for the CI
// gate. Mirrors report.WalkthroughDiff (the persisted walkthroughs.diff_json).
type apiWalkRegression struct {
	PrevWalkthroughID    string   `json:"prev_walkthrough_id"`
	OutcomeChanged       bool     `json:"outcome_changed"`
	PrevOutcome          string   `json:"prev_outcome"`
	Outcome              string   `json:"outcome"`
	IsRegression         bool     `json:"is_regression"`
	Resolved             bool     `json:"resolved"`
	StuckStepDelta       int      `json:"stuck_step_delta"`
	ReasonChanged        bool     `json:"reason_changed"`
	Reason               string   `json:"reason,omitempty"`
	NewTaskBlockers      []string `json:"new_task_blockers"`
	ResolvedTaskBlockers []string `json:"resolved_task_blockers,omitempty"`
	BlockersCompared     bool     `json:"blockers_compared"`
	// OutcomeCompared=false + InfraFailed=true mean the DRIVER could not run (#45) —
	// the goal was never observed, so is_regression is false because nothing was
	// compared, NOT because the product is fine. A CI gate should treat this as
	// "infrastructure error, re-run", never as a pass or a regression.
	OutcomeCompared bool `json:"outcome_compared"`
	InfraFailed     bool `json:"infra_failed"`
}

type apiWalkStep struct {
	Idx           int    `json:"idx"`
	Action        string `json:"action"`
	URL           string `json:"url"`
	Outcome       string `json:"outcome"`
	ScreenshotKey string `json:"screenshot_key,omitempty"`
}

// handleAuditWalkthrough serves a walkthrough's deterministic trace as JSON
// (owner-scoped → 404 for a foreign walkthrough). action_json is untrusted →
// re-serialized (not raw HTML); a consumer reads it as data.
func (a *App) handleAuditWalkthrough(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	wk, err := a.DB.GetWalkthrough(uid, id)
	if err != nil {
		a.apiNotFound(w)
		return
	}
	out := apiWalkthrough{
		ID: wk.ID, Goal: wk.Goal, Outcome: wk.Outcome, StuckStep: wk.StuckStep,
		Reason: wk.Reason, DryRun: wk.DryRun, Status: wk.Status, InfraFailed: wk.InfraFailed,
		EvalRunID: wk.RunID, Steps: []apiWalkStep{},
	}
	if wk.DiffJSON != "" {
		var d report.WalkthroughDiff
		if err := json.Unmarshal([]byte(wk.DiffJSON), &d); err == nil {
			applyDiffCompat(wk.DiffJSON, &d)
			out.Regression = &apiWalkRegression{
				PrevWalkthroughID: d.PrevWalkthroughID, OutcomeChanged: d.OutcomeChanged,
				PrevOutcome: d.PrevOutcome, Outcome: d.Outcome, IsRegression: d.IsRegression,
				Resolved: d.Resolved, StuckStepDelta: d.StuckStepDelta,
				ReasonChanged: d.ReasonChanged, Reason: d.Reason,
				NewTaskBlockers: d.NewTaskBlockers, ResolvedTaskBlockers: d.ResolvedTaskBlockers,
				BlockersCompared: d.BlockersCompared,
				OutcomeCompared:  d.OutcomeCompared, InfraFailed: d.InfraFailed,
			}
		}
	}
	if steps, err := a.DB.ListWalkthroughSteps(wk.ID); err == nil {
		for _, s := range steps {
			out.Steps = append(out.Steps, apiWalkStep{
				Idx: s.Idx, Action: s.ActionJSON, URL: s.URL, Outcome: s.Outcome, ScreenshotKey: s.ScreenshotKey,
			})
		}
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.writeJSON(w, out)
}
