package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/eval"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/gorilla/mux"
)

// maxEvalJobLen caps the free-text job/task string (defensive; it is stored and
// re-rendered escaped).
const maxEvalJobLen = 300

// evalVM builds the persona-walkthrough view model for a run: the enabled flag +
// curated persona set, the async job status/progress, the run-level synthesis
// "story", and the per-URL, per-persona structured verdicts persisted so far.
func (a *App) evalVM(run *db.Run) *pages.EvalVM {
	vm := &pages.EvalVM{
		RunID:   run.ID,
		Enabled: a.Cfg.OpenRouterEnabled(),
		Status:  run.EvalStatus,
		Done:    run.EvalDone,
		Total:   run.EvalTotal,
		Job:     run.EvalJob,
		CostUSD: run.EvalCostUSD,
	}
	for _, p := range eval.Personas {
		vm.Personas = append(vm.Personas, pages.PersonaOptVM{ID: p.ID, Label: p.Label, Cares: p.Cares})
	}
	if !vm.Enabled {
		return vm
	}

	// Phase 2: default the evaluate FORM from the target's confirmed audit config
	// (so the owner doesn't re-type the job/personas). Only applied when this run
	// has not itself been evaluated yet (run.EvalJob empty) — a re-run reuses what
	// the last pass evaluated toward. The per-run job+personas override stays intact.
	if strings.TrimSpace(run.EvalJob) == "" {
		if cfg, found, err := a.DB.GetTargetAuditConfig(run.UserID, run.TargetID); err == nil && found && cfg.Confirmed {
			if strings.TrimSpace(cfg.PrimaryJob) != "" {
				vm.Job = cfg.PrimaryJob
			}
			if len(cfg.Personas) > 0 {
				vm.DefaultPersonas = map[string]bool{}
				for _, p := range cfg.Personas {
					vm.DefaultPersonas[p] = true
				}
			}
		}
	}

	// Run-level synthesis story.
	if strings.TrimSpace(run.EvalSynthesisJSON) != "" {
		var items []report.EvalSynthItem
		if err := json.Unmarshal([]byte(run.EvalSynthesisJSON), &items); err == nil {
			vm.Synthesis = items
		}
	}

	// Map page_id → URL (for grouping) preserving page/URL order.
	pageRows, err := a.DB.ListPages(run.ID)
	if err != nil {
		return vm
	}
	urlByPage := map[string]string{}
	var urlOrder []string
	seen := map[string]bool{}
	for _, p := range pageRows {
		urlByPage[p.ID] = p.URL
		if !seen[p.URL] {
			seen[p.URL] = true
			urlOrder = append(urlOrder, p.URL)
		}
	}

	evalRows, err := a.DB.ListPageEvaluations(run.ID)
	if err != nil {
		return vm
	}
	byURL := map[string][]pages.EvalCellVM{}
	costByPersona := map[string]float64{}
	var personaOrder []string
	for _, e := range evalRows {
		url := urlByPage[e.PageID]
		if url == "" {
			continue // orphaned (page removed) — skip
		}
		var pe report.PageEvaluation
		if e.FindingsJSON != "" {
			_ = json.Unmarshal([]byte(e.FindingsJSON), &pe)
		}
		byURL[url] = append(byURL[url], pages.EvalCellVM{
			Persona:       e.Persona,
			PersonaLabel:  eval.PersonaLabel(e.Persona),
			Comprehension: e.Comprehension,
			Error:         e.Error,
			Eval:          pe,
			CostUSD:       e.CostUSD,
			Tokens:        e.PromptTokens + e.CompletionTokens,
		})
		if e.CostUSD > 0 {
			if _, ok := costByPersona[e.Persona]; !ok {
				personaOrder = append(personaOrder, e.Persona)
			}
			costByPersona[e.Persona] += e.CostUSD
			vm.CellCount++
		}
	}
	for _, p := range personaOrder {
		vm.ByPersona = append(vm.ByPersona, pages.PersonaCostVM{Label: eval.PersonaLabel(p), CostUSD: costByPersona[p]})
	}
	for _, url := range urlOrder {
		if cells := byURL[url]; len(cells) > 0 {
			vm.Pages = append(vm.Pages, pages.EvalPageVM{URL: url, Cells: cells})
		}
	}
	return vm
}

// handleEvalStatus returns the eval-section fragment (htmx poll target while a
// pass is generating).
func (a *App) handleEvalStatus(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, pages.EvaluationSection(a.evalVM(run)))
}

// handleGenerateEval enqueues the async persona-walkthrough pass for a completed
// run. Body carries personas[] (validated against the curated allowlist), an
// optional job string, and an optional verify flag (default on). 503 when the
// feature is disabled; 400 on an empty/unknown persona set; run must be done.
func (a *App) handleGenerateEval(w http.ResponseWriter, r *http.Request) {
	if a.Eval == nil || !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "persona walkthrough is not enabled (no OpenRouter API key configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if run.Status != db.RunDone {
		http.Error(w, "the run must be complete before a persona walkthrough", http.StatusBadRequest)
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
		http.Error(w, "select at least one valid persona", http.StatusBadRequest)
		return
	}
	job := strings.TrimSpace(r.FormValue("job"))
	if len(job) > maxEvalJobLen {
		job = job[:maxEvalJobLen]
	}
	// verify defaults on: absent (no re-run form) OR "1"/"true" → on.
	verify := true
	if r.Form.Has("verify") {
		verify = r.FormValue("verify") == "1" || r.FormValue("verify") == "true"
	}

	total, err := a.Eval.CountUnits(run.ID, personas)
	if err != nil {
		http.Error(w, "failed to plan the walkthrough", http.StatusInternalServerError)
		return
	}
	won, err := a.DB.ClaimEvalJob(run.ID, job, total)
	if err != nil {
		http.Error(w, "failed to start the walkthrough", http.StatusInternalServerError)
		return
	}
	if won {
		go func(runID string, personas []string, opts eval.Options) {
			_ = a.Eval.Run(context.Background(), runID, personas, opts)
		}(run.ID, personas, eval.Options{Job: job, Verify: verify})
	}

	run.EvalStatus = db.EvalGenerating
	run.EvalTotal = total
	run.EvalDone = 0
	run.EvalJob = job
	render(w, pages.EvaluationSection(a.evalVM(run)))
}

// validatePersonas returns the de-duplicated, order-preserving set of requested
// personas. ok is false if ANY requested (non-blank) persona is outside the
// curated allowlist.
func validatePersonas(requested []string) (personas []string, ok bool) {
	seen := map[string]bool{}
	for _, p := range requested {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if !eval.PersonaAllowed(p) {
			return nil, false
		}
		seen[p] = true
		personas = append(personas, p)
	}
	return personas, true
}

// --- Read API: machine-readable evaluations ---

// apiRunEvaluation is the JSON shape for GET /api/audit/runs/{run_id}/evaluation.
type apiRunEvaluation struct {
	RunID     string                 `json:"run_id"`
	Status    string                 `json:"eval_status"`
	Job       string                 `json:"job,omitempty"`
	Synthesis []report.EvalSynthItem `json:"synthesis"`
	Pages     []apiPageEvaluation    `json:"pages"`
}

type apiPageEvaluation struct {
	URL        string                 `json:"url"`
	Persona    string                 `json:"persona"`
	Error      string                 `json:"error,omitempty"`
	Evaluation *report.PageEvaluation `json:"evaluation,omitempty"`
}

// handleAuditRunEvaluation serves a run's structured persona-walkthrough
// evaluations + synthesis as JSON (owner-scoped → 404 for a foreign run), so a
// coding agent/CI can pull the machine layer. Reuses apiKeyAuth + owner scoping.
func (a *App) handleAuditRunEvaluation(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	runID := mux.Vars(r)["run_id"]
	run, err := a.DB.GetRun(uid, runID)
	if err != nil {
		a.apiNotFound(w)
		return
	}
	out := apiRunEvaluation{RunID: run.ID, Status: run.EvalStatus, Job: run.EvalJob, Synthesis: []report.EvalSynthItem{}, Pages: []apiPageEvaluation{}}
	if strings.TrimSpace(run.EvalSynthesisJSON) != "" {
		var items []report.EvalSynthItem
		if err := json.Unmarshal([]byte(run.EvalSynthesisJSON), &items); err == nil {
			out.Synthesis = items
		}
	}
	// page_id → URL for the machine payload.
	urlByPage := map[string]string{}
	if pageRows, err := a.DB.ListPages(run.ID); err == nil {
		for _, p := range pageRows {
			urlByPage[p.ID] = p.URL
		}
	}
	rows, err := a.DB.ListPageEvaluations(run.ID)
	if err != nil {
		metrics.APIReads.WithLabelValues("error").Inc()
		http.Error(w, "failed to load evaluations", http.StatusInternalServerError)
		return
	}
	for _, e := range rows {
		item := apiPageEvaluation{URL: urlByPage[e.PageID], Persona: e.Persona, Error: e.Error}
		if e.FindingsJSON != "" {
			var pe report.PageEvaluation
			if err := json.Unmarshal([]byte(e.FindingsJSON), &pe); err == nil {
				item.Evaluation = &pe
			}
		}
		out.Pages = append(out.Pages, item)
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.writeJSON(w, out)
}
