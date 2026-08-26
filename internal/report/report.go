// Package report defines the forward-compatible run-level report.json contract.
//
// report.json is the artifact a run writes to object storage summarizing every
// crawled page and finding. It is the SAME schema a future plugin-push (P5,
// where a customer's app POSTs its own audit results instead of us crawling)
// will emit, so it is versioned and self-describing. Keep it additive.
package report

import (
	"encoding/json"
	"io"
	"time"
)

// SchemaVersion is bumped only on breaking changes to the report.json shape.
const SchemaVersion = 1

// Report is the top-level run-level artifact.
type Report struct {
	Schema      int    `json:"schema"`
	Tool        string `json:"tool"`         // "auditloop"
	ToolVersion string `json:"tool_version"` // build/version string
	RunID       string `json:"run_id"`
	TargetID    string `json:"target_id"`
	TargetName  string `json:"target_name"`
	BaseURL     string `json:"base_url"`
	AuthMode    string `json:"auth_mode"`       // none|login|plugin (seam for P4/P5)
	Label       string `json:"label,omitempty"` // optional pushed-run label (P5)
	// Environment is the OPTIONAL declared environment of a pushed run
	// (lab|staging|prod). A "lab" push measured perf on a hermetic localhost stack,
	// so its perf numbers are not field-representative — perf FINDINGS are suppressed
	// for it (raw perf is still recorded). Additive/omitempty: pre-change reports
	// (no environment) still decode, older readers ignore it. Empty for crawled runs.
	Environment string       `json:"environment,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	FinishedAt  time.Time    `json:"finished_at"`
	Status      string       `json:"status"` // done|failed
	Error       string       `json:"error,omitempty"`
	Summary     Summary      `json:"summary"`
	Pages       []PageReport `json:"pages"`
	Versions    ToolVersions `json:"versions"`
	// Diff is the P2 regression block: how this run compares to the target's
	// previous completed run. Optional and additive — a pointer with omitempty,
	// so pre-P2 reports (no diff) still decode cleanly and older readers ignore it.
	Diff *Diff `json:"diff,omitempty"`
	// NotesCost is the optional P3 vision-LLM UX-notes cost rollup for the run (USD
	// + token totals from OpenRouter usage accounting). Additive/omitempty: report.json
	// is written at crawl-finish BEFORE the opt-in notes pass runs, so the crawl worker
	// leaves it nil — a forward-compat SEAM a future re-emit (or a P5 plugin push) can
	// populate from runs.notes_cost_usd/notes_*_tokens. See CLAUDE.md.
	NotesCost *NotesCost `json:"notes_cost,omitempty"`
	// EvalSynthesis is the run-level, ranked "story" from the persona-walkthrough
	// evaluator's synthesis pass (Phase 1). Additive/omitempty forward-compat SEAM —
	// like Notes, report.json is written at crawl-finish BEFORE the opt-in eval pass,
	// so the crawl worker leaves it nil; the DB + read API are the source of truth
	// for Phase 1. See CLAUDE.md.
	EvalSynthesis []EvalSynthItem `json:"eval_synthesis,omitempty"`
	// EvalCost is the run-level LLM cost rollup for the persona-walkthrough pass
	// (USD + token totals). Additive/omitempty SEAM (nil at crawl-finish).
	EvalCost *NotesCost `json:"eval_cost,omitempty"`
}

// NotesCost is the run-level LLM cost rollup for the UX-notes pass (P3).
type NotesCost struct {
	CostUSD          float64 `json:"cost_usd"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
}

// --- Task-grounded persona-walkthrough evaluation (Phase 1) ---
//
// These types are the STRUCTURED, machine-readable output of the persona
// walkthrough evaluator (internal/eval). They live here (not in internal/eval)
// so they are the canonical, additive-only report.json contract AND so
// internal/eval can reference them without a report→eval import cycle (eval
// already imports report for its deterministic grounding). Model-authored
// selector/evidence/issue strings are UNTRUSTED — they are stored as escaped
// JSON and rendered escaped in the UI (same rule as layout `examples`).

// PageEvaluation is one persona's structured walkthrough verdict for one logical
// page (URL) in a run. Comprehension answers "can this persona tell what to do
// next here?" (clear|unclear|blocked). Blockers STOP the persona's task;
// Frictions slow/annoy but don't stop; TopFix is the single highest-leverage
// change. It is the value stored per (page,persona) in page_evaluations.findings_json.
type PageEvaluation struct {
	Comprehension string        `json:"comprehension"` // clear|unclear|blocked
	Blockers      []EvalFinding `json:"blockers,omitempty"`
	Frictions     []EvalFinding `json:"frictions,omitempty"`
	TopFix        *EvalTopFix   `json:"top_fix,omitempty"`
}

// EvalFinding is one task-blocking/friction issue the model surfaced, anchored to
// a page element. Selector + Evidence are model-authored and untrusted (rendered
// escaped). Verified is set by the anti-vagueness verification pass — only
// substantiated findings are kept.
type EvalFinding struct {
	Issue    string `json:"issue"`
	Selector string `json:"selector,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Verified bool   `json:"verified"`
}

// EvalTopFix is the single highest-impact change for a (page,persona). Impact is
// high|medium|low.
type EvalTopFix struct {
	Selector  string `json:"selector,omitempty"`
	Change    string `json:"change"`
	Rationale string `json:"rationale,omitempty"`
	Impact    string `json:"impact"` // high|medium|low
}

// EvalSynthItem is one ranked run-level improvement produced by the synthesis
// pass — the holistic "story" layer across all pages+personas.
type EvalSynthItem struct {
	Title            string   `json:"title"`
	Rationale        string   `json:"rationale,omitempty"`
	Impact           string   `json:"impact"` // high|medium|low
	AffectedURLs     []string `json:"affected_urls,omitempty"`
	AffectedPersonas []string `json:"affected_personas,omitempty"`
	Selector         string   `json:"selector,omitempty"`
}

// VisualRegressionThreshold is the diff_pct (percent of changed pixels) at or
// above which a page+viewport is flagged as a visual regression.
const VisualRegressionThreshold = 1.0

// Diff is the run-level regression summary versus PrevRunID (P2). It mirrors the
// value persisted in runs.diff_json.
type Diff struct {
	PrevRunID string    `json:"prev_run_id"`
	PrevRunAt time.Time `json:"prev_run_at,omitempty"`
	// Page-set delta (by URL).
	PagesAdded   []string `json:"pages_added"`
	PagesRemoved []string `json:"pages_removed"`
	// PagesChanged counts page+viewport captures flagged as VISUAL REGRESSIONS:
	// SAME capture dimensions (size_changed=false) AND diff_pct at or above
	// VisualRegressionThreshold. This is the only place a raw full-page pixel
	// diff is trustworthy (aligned comparison), so it is the signal that drives
	// the regression metric + UI regression badge.
	PagesChanged int `json:"pages_changed"`
	// PagesSizeChanged counts captures whose dimensions changed (page height/width
	// shifted — a new top item, consent banner, lazy-loaded/added content). Their
	// inflated diff_pct is NOT comparable, so they are reported separately as a
	// softer "layout/size changed" signal and NOT counted as visual regressions.
	PagesSizeChanged int `json:"pages_size_changed"`
	// a11y rule-set delta (axe rule ids), first-party counts, and violation
	// totals. Deltas are current − previous (positive = regression).
	NewA11yRules      []string `json:"new_a11y_rules"`
	ResolvedA11yRules []string `json:"resolved_a11y_rules,omitempty"`
	A11yDelta         int      `json:"a11y_delta"`
	ConsoleDelta      int      `json:"console_delta"`
	NetworkDelta      int      `json:"network_delta"`
	// ChangedPages lists the visually-changed captures (worst-first), each with a
	// diff-image key discoverable in object storage.
	ChangedPages []ChangedPage `json:"changed_pages,omitempty"`
}

// ChangedPage is one page+viewport that differs from the baseline — either a
// true visual regression, a layout/size change, or a capture too large to
// visualize. Distinguish via SizeChanged / NotCompared / IsRegression.
type ChangedPage struct {
	URL      string  `json:"url"`
	Viewport string  `json:"viewport"`
	DiffPct  float64 `json:"diff_pct"`
	DiffKey  string  `json:"diff_key"`
	// SizeChanged is true when the capture dimensions differ from the baseline —
	// the diff_pct is inflated/incomparable, so this is a layout change, NOT a
	// visual regression.
	SizeChanged bool `json:"size_changed"`
	// NotCompared is true when the capture exceeded the visualization size cap, so
	// no diff image was produced (diff_pct is still populated).
	NotCompared bool `json:"not_compared,omitempty"`
}

// IsRegression reports whether a changed page is a trustworthy visual regression:
// same capture dimensions (so the pixel comparison is aligned) AND diff_pct at or
// above the threshold. A size/layout change is deliberately NOT a regression.
func (c ChangedPage) IsRegression() bool {
	return !c.SizeChanged && c.DiffPct >= VisualRegressionThreshold
}

// WalkthroughDiff is the walkthrough-vs-walkthrough regression summary (Phase 4):
// this walkthrough's deterministic outcome + persona task-blockers compared to the
// target's PREVIOUS terminal walkthrough (PrevWalkthroughID). It is the exact
// walkthrough analogue of the P2 crawl Diff, persisted in walkthroughs.diff_json and
// served by the read-API so a CI gate can fail on
// IsRegression || len(NewTaskBlockers) > 0.
type WalkthroughDiff struct {
	PrevWalkthroughID string    `json:"prev_walkthrough_id"`
	PrevAt            time.Time `json:"prev_at,omitempty"`
	// Deterministic outcome transition. Rank: success > stuck > failed.
	PrevOutcome    string `json:"prev_outcome"`
	Outcome        string `json:"outcome"`
	OutcomeChanged bool   `json:"outcome_changed"`
	// IsRegression is the OUTCOME/stuck-axis regression: the outcome rank dropped
	// (success→stuck|failed, stuck→failed) OR — both stuck — it got stuck EARLIER
	// (StuckStepDelta < 0). New task-blockers are a SEPARATE gate signal
	// (NewTaskBlockers); the CI gate ORs the two.
	IsRegression bool `json:"is_regression"`
	// Resolved is the opposite: the outcome rank IMPROVED (stuck|failed→success).
	Resolved bool `json:"resolved"`
	// StuckStepDelta = Outcome - Prev stuck step, only when BOTH walkthroughs are
	// stuck (else 0). Negative = got stuck earlier (regression); positive = progress.
	StuckStepDelta int    `json:"stuck_step_delta"`
	PrevStuckStep  int    `json:"prev_stuck_step,omitempty"`
	StuckStep      int    `json:"stuck_step,omitempty"`
	ReasonChanged  bool   `json:"reason_changed"`
	PrevReason     string `json:"prev_reason,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// Task-blocker delta over the persona evaluations of each walkthrough's driven
	// trace. Identity key = "<persona>\x1f<normalized selector|issue>". NewTaskBlockers
	// (present in cur, absent in baseline) are the task-blocker REGRESSIONS the CI gate
	// keys on. Empty (both) when either side has no completed evaluation (degrade).
	NewTaskBlockers      []string `json:"new_task_blockers"`
	ResolvedTaskBlockers []string `json:"resolved_task_blockers,omitempty"`
	// BlockersCompared is false when the blocker delta was skipped because one or both
	// walkthroughs had no completed persona evaluation (so empty NewTaskBlockers means
	// "not compared", not "no new blockers").
	BlockersCompared bool `json:"blockers_compared"`
	// OutcomeCompared is false when the OUTCOME axis was not scored because the current
	// (or, defensively, the baseline) walkthrough was an INFRASTRUCTURE failure — a
	// stalled/killed browser, a setup failure, a restart sweep (#45). A non-observation
	// cannot be a regression, so IsRegression/Resolved/StuckStepDelta are forced off and
	// false IsRegression here means "not compared", not "no regression". The descriptive
	// fields (prev/cur outcome, reasons, OutcomeChanged) are still populated.
	OutcomeCompared bool `json:"outcome_compared"`
	// InfraFailed says the CURRENT walkthrough failed on infrastructure rather than on
	// the product, so a CI gate can report "the driver could not run" distinctly from
	// "the goal regressed".
	InfraFailed bool `json:"infra_failed"`
}

// Summary carries run-level rollup counts.
type Summary struct {
	PagesCrawled   int `json:"pages_crawled"`
	URLsDiscovered int `json:"urls_discovered"`
	URLsBlocked    int `json:"urls_blocked"` // refused by the SSRF guard
	A11yViolations int `json:"a11y_violations"`
	ConsoleFirst   int `json:"console_first_party"`
	ConsoleThird   int `json:"console_third_party"`
	NetworkFirst   int `json:"network_first_party"`
	NetworkThird   int `json:"network_third_party"`
}

// PageReport is one crawled URL at one viewport.
type PageReport struct {
	URL           string `json:"url"`
	Viewport      string `json:"viewport"` // mobile|desktop
	Width         int    `json:"width"`
	ScreenshotKey string `json:"screenshot_key"`
	AxeKey        string `json:"axe_key,omitempty"`
	// A11yDigestKey is the optional storage key of this page's DOM/accessibility
	// digest (Phase 1 persona-evaluator grounding). Additive/omitempty — pre-0060 and
	// pushed reports omit it and readers ignore it.
	A11yDigestKey string  `json:"a11y_digest_key,omitempty"`
	NetworkKey    string  `json:"network_key,omitempty"`
	LoadMS        int64   `json:"load_ms"`
	Console       Origins `json:"console"`
	Network       Origins `json:"network"`
	A11y          A11y    `json:"a11y"`
	// Perf is the optional deterministic web-vitals / page-weight block. Additive
	// (pointer + omitempty), so pre-perf reports still decode. TBTMs is a headless
	// LAB PROXY (no field input) — see Perf.
	Perf *Perf `json:"perf,omitempty"`
	// Layout is the optional deterministic DOM layout-smell block (per viewport).
	Layout   *LayoutSmells `json:"layout,omitempty"`
	Findings []Finding     `json:"findings"`
	// Notes is the optional P3 vision-LLM UX-notes block: model id → markdown draft.
	// It is additive and backward-compatible (omitempty). NOTE: report.json is
	// written at crawl-finish time, BEFORE the opt-in notes pass runs, so this is a
	// forward-compat SEAM — the crawl worker does not populate it. A future
	// enhancement can re-emit report.json after a notes pass (or a P5 plugin push
	// can supply notes directly). See CLAUDE.md.
	Notes map[string]string `json:"notes,omitempty"`
	// Evaluations is the optional persona-walkthrough block: persona id → the
	// structured verdict for this page (Phase 1). Additive/omitempty forward-compat
	// SEAM — the crawl worker does not populate it (the DB + read API are the Phase 1
	// source of truth); a future re-emit or plugin push may supply it. See CLAUDE.md.
	Evaluations map[string]PageEvaluation `json:"evaluations,omitempty"`
}

// Web-vitals rating thresholds (shared by the worker's finding logic and the UI's
// colour coding, so they never drift). The "good"/"poor" boundaries mirror Google's
// Core Web Vitals guidance: at/under Good is fine, between Good and Poor is
// "needs improvement", above Poor is poor. TBT is a headless LAB PROXY (no field
// input), so its rating approximates main-thread blocking, not a real field metric.
const (
	LCPGoodMs = 2500 // Largest Contentful Paint (ms)
	LCPPoorMs = 4000
	CLSGood   = 0.1 // Cumulative Layout Shift (unitless)
	CLSPoor   = 0.25
	TBTGoodMs = 200 // Total Blocking Time (ms) — lab proxy
	TBTPoorMs = 600
	// Soft page-budget thresholds → one informational finding when breached.
	WeightWarnBytes = 3 * 1024 * 1024 // 3 MB transferred
	ReqCountWarn    = 100             // requests dispatched
)

// Rating classifies a value against a good/poor boundary pair → "good" |
// "needs-improvement" | "poor". Used for the UI's perf colour coding.
func Rating(value, good, poor float64) string {
	switch {
	case value > poor:
		return "poor"
	case value > good:
		return "needs-improvement"
	default:
		return "good"
	}
}

// Perf is the deterministic web-vitals / page-weight capture for one page+viewport.
// LCP/TBT are milliseconds, CLS is unitless. TBTMs is a headless LAB PROXY — there
// is no field input in a crawl, so it approximates main-thread blocking rather than
// a real field Total Blocking Time (labeled honestly in the UI + comments).
type Perf struct {
	LCPMs       int64   `json:"lcp_ms,omitempty"`
	CLS         float64 `json:"cls,omitempty"`
	TBTMs       int64   `json:"tbt_ms,omitempty"` // lab proxy (headless, no field input)
	WeightBytes int64   `json:"weight_bytes,omitempty"`
	ReqCount    int     `json:"req_count,omitempty"`
}

// LayoutSmells is the deterministic DOM layout-smell rollup for one page+viewport.
// It mirrors crawler.LayoutSmells. Tap-target / horizontal-overflow are primarily
// mobile concerns; the worker only emits findings for them on the mobile viewport.
type LayoutSmells struct {
	HorizontalOverflow  bool                `json:"horizontal_overflow,omitempty"`
	ScrollWidth         int                 `json:"scroll_width,omitempty"`
	InnerWidth          int                 `json:"inner_width,omitempty"`
	SmallTapTargets     int                 `json:"small_tap_targets,omitempty"`
	SmallText           int                 `json:"small_text,omitempty"`
	MissingViewportMeta bool                `json:"missing_viewport_meta,omitempty"`
	ImagesNoDims        int                 `json:"images_no_dims,omitempty"`
	Examples            map[string][]string `json:"examples,omitempty"`
}

// Origins holds first/third-party split counts for one signal type.
type Origins struct {
	FirstParty int `json:"first_party"`
	ThirdParty int `json:"third_party"`
}

// A11y holds axe-core scan rollups.
type A11y struct {
	ViolationCount int `json:"violation_count"`
	NodeCount      int `json:"node_count"`
}

// Finding is a single normalized issue.
type Finding struct {
	Type     string          `json:"type"`     // a11y|console|network|perf|layout
	Severity string          `json:"severity"` // critical|serious|moderate|minor|info
	Detail   json.RawMessage `json:"detail"`
}

// ToolVersions records the versions used to produce the report (provenance).
type ToolVersions struct {
	Auditloop string `json:"auditloop"`
	Chromium  string `json:"chromium,omitempty"`
	AxeCore   string `json:"axe_core,omitempty"`
}

// Encode writes r as pretty JSON.
func (r *Report) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Marshal returns r as pretty JSON bytes.
func (r *Report) Marshal() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Decode reads a report.json.
func Decode(r io.Reader) (*Report, error) {
	var rep Report
	if err := json.NewDecoder(r).Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
