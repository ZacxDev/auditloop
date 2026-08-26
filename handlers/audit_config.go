package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/eval"
	"github.com/ZacxDev/auditloop/internal/metrics"

	"github.com/gorilla/mux"
)

// formChecked reports whether a checkbox form field was submitted checked
// ("1"/"true"). An absent checkbox submits nothing → false.
func formChecked(r *http.Request, name string) bool {
	v := strings.TrimSpace(r.FormValue(name))
	return v == "1" || v == "true" || v == "on"
}

// maxAuditFieldLen caps each free-text audit-config field (defensive; the fields
// are stored and re-rendered escaped). Matches maxEvalJobLen for the job.
const maxAuditFieldLen = 300

// inferTimeout bounds the synchronous single-call inference (one LLM round-trip).
const inferTimeout = 60 * time.Second

// auditConfigVM builds the Phase-2 audit-config view model for a target. It loads
// any stored config (owner-scoped) and whether the target has a completed run
// (gates the infer button). The whole card is gated on OpenRouterEnabled().
func (a *App) auditConfigVM(uid string, t *db.Target) *pages.AuditConfigVM {
	vm := &pages.AuditConfigVM{
		TargetID:        t.ID,
		Enabled:         a.Cfg.OpenRouterEnabled(),
		DrivingEnabled:  t.DrivingEnabled,
		AllowRealSubmit: t.AllowRealSubmit,
	}
	if !vm.Enabled {
		return vm
	}
	// Completed-run check (gates "Infer from latest run").
	if run, err := a.DB.LatestDoneRunForTargetOwned(uid, t.ID); err == nil && run != nil {
		vm.HasDoneRun = true
	}

	checked := map[string]bool{}
	cfg, found, err := a.DB.GetTargetAuditConfig(uid, t.ID)
	if err == nil && found {
		vm.HasConfig = true
		vm.Inferred = cfg.Inferred
		vm.Confirmed = cfg.Confirmed
		vm.ProductSummary = cfg.ProductSummary
		vm.PrimaryJob = cfg.PrimaryJob
		vm.PrimaryCTA = cfg.PrimaryCTA
		vm.SuccessSelector = cfg.SuccessSelector
		vm.SuccessURLContains = cfg.SuccessURLContains
		vm.SuccessTimeoutMs = cfg.SuccessTimeoutMs
		for _, p := range cfg.Personas {
			checked[p] = true
		}
	}
	for _, p := range eval.Personas {
		vm.Personas = append(vm.Personas, pages.AuditPersonaVM{ID: p.ID, Label: p.Label, Checked: checked[p.ID]})
	}
	return vm
}

// handleInferAuditConfig runs the synchronous goal-inference pass against the
// target's latest completed run and stores the draft (inferred=1, confirmed=0).
// 503 when OpenRouter is disabled; 404 for a foreign target; 409 when the target
// has no completed run yet. Returns the pre-filled config-card fragment.
func (a *App) handleInferAuditConfig(w http.ResponseWriter, r *http.Request) {
	if a.Eval == nil || !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "audit-config inference is not enabled (no OpenRouter API key configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := a.DB.LatestDoneRunForTargetOwned(uid, t.ID)
	if err != nil {
		http.Error(w, "failed to look up runs", http.StatusInternalServerError)
		return
	}
	if run == nil {
		http.Error(w, "need a completed run first — run an audit before inferring the config", http.StatusConflict)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), inferTimeout)
	defer cancel()
	draft, err := a.Eval.InferConfig(ctx, run.ID)
	if err != nil {
		http.Error(w, "inference failed — try again", http.StatusBadGateway)
		return
	}

	cfg := &db.TargetAuditConfig{
		TargetID:       t.ID,
		ProductSummary: capField(draft.ProductSummary),
		PrimaryJob:     capField(draft.PrimaryJob),
		PrimaryCTA:     capField(draft.PrimaryCTA),
		Personas:       draft.Audiences, // already filtered to the allowlist by ParseInferredConfig
		Inferred:       true,
		Confirmed:      false,
	}
	if err := a.DB.SetTargetAuditConfig(cfg); err != nil {
		http.Error(w, "failed to store the inferred config", http.StatusInternalServerError)
		return
	}
	metrics.EvalGenerated.WithLabelValues("_infer", "ok").Inc()
	render(w, pages.AuditConfigSection(a.auditConfigVM(uid, t)))
}

// handleSaveAuditConfig saves/confirms a target's audit config from the form
// fields. 503 when disabled; 404 for a foreign target; 400 on an unknown persona.
// Sets confirmed=1. Returns the updated card fragment.
func (a *App) handleSaveAuditConfig(w http.ResponseWriter, r *http.Request) {
	if !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "audit configuration is not enabled (no OpenRouter API key configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}
	personas, ok := validatePersonas(r.Form["personas"])
	if !ok {
		http.Error(w, "unknown persona requested", http.StatusBadRequest)
		return
	}

	// Phase-3 driver success condition (goal + success authored together on the
	// config). Length-capped defensively; the timeout is parsed leniently.
	successTO, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("success_timeout_ms")))
	if successTO < 0 {
		successTO = 0
	}
	cfg := &db.TargetAuditConfig{
		TargetID:           t.ID,
		ProductSummary:     capField(strings.TrimSpace(r.FormValue("product_summary"))),
		PrimaryJob:         capField(strings.TrimSpace(r.FormValue("primary_job"))),
		PrimaryCTA:         capField(strings.TrimSpace(r.FormValue("primary_cta"))),
		Personas:           personas,
		SuccessSelector:    capField(strings.TrimSpace(r.FormValue("success_selector"))),
		SuccessURLContains: capField(strings.TrimSpace(r.FormValue("success_url_contains"))),
		SuccessTimeoutMs:   successTO,
		// Preserve the inferred provenance flag if the target was inferred before.
		Inferred:  false,
		Confirmed: true,
	}
	if prev, found, perr := a.DB.GetTargetAuditConfig(uid, t.ID); perr == nil && found {
		cfg.Inferred = prev.Inferred
	}
	if err := a.DB.SetTargetAuditConfig(cfg); err != nil {
		http.Error(w, "failed to save the audit config", http.StatusInternalServerError)
		return
	}
	// Phase-3 driving opt-ins live on the target (two independent, default-off
	// gates: drive at all vs. mutate live data).
	drivingEnabled := formChecked(r, "driving_enabled")
	allowRealSubmit := formChecked(r, "allow_real_submit")
	if err := a.DB.SetDrivingConfig(uid, t.ID, drivingEnabled, allowRealSubmit); err != nil {
		http.Error(w, "failed to save the driving config", http.StatusInternalServerError)
		return
	}
	// Reflect the new flags in the rendered fragment.
	t.DrivingEnabled = drivingEnabled
	t.AllowRealSubmit = allowRealSubmit
	render(w, pages.AuditConfigSection(a.auditConfigVM(uid, t)))
}

// capField trims + length-caps a stored free-text field defensively.
func capField(s string) string {
	// Fast path: a byte length within the cap guarantees a rune count within it
	// too (every rune is ≥1 byte), so no truncation is needed.
	if len(s) <= maxAuditFieldLen {
		return s
	}
	// Truncate on a RUNE boundary — a byte slice (s[:n]) can split a multibyte
	// UTF-8 rune, and Postgres `text` rejects invalid UTF-8 (→ 500 on save/infer),
	// even though SQLite tolerates it. Model-authored fields can be multibyte.
	r := []rune(s)
	if len(r) > maxAuditFieldLen {
		return string(r[:maxAuditFieldLen])
	}
	return s
}

// --- Read API: machine-readable audit config ---

// apiAuditConfig is the JSON shape for GET /api/audit/targets/{id}/audit-config.
type apiAuditConfig struct {
	TargetID       string   `json:"target_id"`
	ProductSummary string   `json:"product_summary"`
	PrimaryJob     string   `json:"primary_job"`
	PrimaryCTA     string   `json:"primary_cta"`
	Personas       []string `json:"personas"`
	Inferred       bool     `json:"inferred"`
	Confirmed      bool     `json:"confirmed"`
}

// handleAuditTargetConfig serves a target's audit config as JSON so a machine
// consumer/agent can see the target's job + audiences. Owner-scoped (UUID-or-name
// via resolveTarget) → 404 for a foreign/unknown target or one with no config.
func (a *App) handleAuditTargetConfig(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	param := mux.Vars(r)["id"]
	t, err := a.resolveTarget(uid, param)
	if err != nil {
		a.apiNotFound(w)
		return
	}
	cfg, found, err := a.DB.GetTargetAuditConfig(uid, t.ID)
	if err != nil {
		metrics.APIReads.WithLabelValues("error").Inc()
		http.Error(w, "failed to load audit config", http.StatusInternalServerError)
		return
	}
	if !found {
		a.apiNotFound(w)
		return
	}
	personas := cfg.Personas
	if personas == nil {
		personas = []string{}
	}
	metrics.APIReads.WithLabelValues("ok").Inc()
	a.writeJSON(w, apiAuditConfig{
		TargetID:       cfg.TargetID,
		ProductSummary: cfg.ProductSummary,
		PrimaryJob:     cfg.PrimaryJob,
		PrimaryCTA:     cfg.PrimaryCTA,
		Personas:       personas,
		Inferred:       cfg.Inferred,
		Confirmed:      cfg.Confirmed,
	})
}
