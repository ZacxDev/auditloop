package db

import "time"

// Auth modes. Only "none" is exercised now; the column exists as a seam for
// P4 (login recipes) and P5 (plugin push).
const (
	AuthNone   = "none"
	AuthLogin  = "login"
	AuthPlugin = "plugin"
)

// Run statuses.
const (
	RunQueued  = "queued"
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
)

// Finding types.
const (
	FindingA11y    = "a11y"
	FindingConsole = "console"
	FindingNetwork = "network"
	FindingPerf    = "perf"
	FindingLayout  = "layout"
)

// Target is a site the user owns and audits.
type Target struct {
	ID              string
	UserID          string
	Name            string
	BaseURL         string
	VerifiedDomains []string
	AuthMode        string
	// DrivingEnabled is the DEFAULT-OFF, opt-in per-target gate for the Phase-3
	// goal-directed walkthrough DRIVER. A walkthrough is refused unless this is on.
	DrivingEnabled bool
	// AllowRealSubmit is the LOUD, separate, DEFAULT-OFF flag that lets the driver
	// perform REAL (mutating) form submissions. Off → dry-run submit-guard.
	AllowRealSubmit bool
	CreatedAt       time.Time
}

// Run is one on-demand audit of a target.
type Run struct {
	ID          string
	TargetID    string
	UserID      string
	Status      string
	Trigger     string // manual|plugin
	Label       string // optional pushed-run label ("" for crawled runs)
	Environment string // pushed-run environment: lab|staging|prod ("" for crawled/unspecified)
	PrevRunID   string // nullable; "" when none (baseline for P2 diffing)
	FaviconKey  string // storage key of the captured site favicon; "" when none
	SummaryJSON string
	DiffJSON    string // run-level regression summary (report.Diff); "" when no baseline
	Error       string
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	// P3 async notes-job tracking.
	NotesStatus string // idle|generating|done|failed
	NotesDone   int
	NotesTotal  int
	// P3 LLM cost accounting, accumulated across the pass's (page,model) calls and
	// RESET when a fresh pass is claimed (reflects the latest pass, not all-time).
	NotesCostUSD          float64
	NotesPromptTokens     int
	NotesCompletionTokens int
	// Persona-walkthrough evaluation (Phase 1) — async job tracking + the free-text
	// job the pass evaluated toward + the run-level synthesis "story" (JSON) + the
	// reset-per-pass LLM cost accumulators (mirrors the P3 notes fields).
	EvalStatus           string // idle|generating|done|failed
	EvalDone             int
	EvalTotal            int
	EvalJob              string // free-text task/job the walkthrough evaluated toward
	EvalSynthesisJSON    string // report.EvalSynthesis JSON ("" until synthesis runs)
	EvalCostUSD          float64
	EvalPromptTokens     int
	EvalCompletionTokens int
}

// Notes-job statuses (P3).
const (
	NotesIdle       = "idle"
	NotesGenerating = "generating"
	NotesDone       = "done"
	NotesFailed     = "failed"
)

// Persona-walkthrough evaluation job statuses (Phase 1). Same lifecycle as the
// notes job (idle→generating→done|failed).
const (
	EvalIdle       = "idle"
	EvalGenerating = "generating"
	EvalDone       = "done"
	EvalFailed     = "failed"
)

// PageEvaluation is one structured persona-walkthrough verdict for a (page,
// persona) pair (Phase 1). FindingsJSON holds the report.PageEvaluation object
// (blockers/frictions/top_fix); Comprehension is duplicated out for cheap
// filtering. A re-run replaces the row. Error is non-empty when the LLM call for
// this (page,persona) failed (the row degrades in place). Cost fields carry the
// per-cell LLM accounting (0 for failed cells / providers without usage).
type PageEvaluation struct {
	ID               string
	PageID           string
	RunID            string
	Persona          string
	FindingsJSON     string // report.PageEvaluation JSON; untrusted → rendered escaped
	Comprehension    string // clear|unclear|blocked
	Error            string
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// PageNote is one editable markdown UX-notes draft for a (page, model) pair (P3).
// A re-draft replaces the row (Edited=false); a human edit sets Edited=true. Error
// is non-empty when the LLM call for this (page,model) failed (the row degrades in
// place rather than failing the whole pass).
type PageNote struct {
	ID     string
	PageID string
	RunID  string
	Model  string
	Notes  string
	Edited bool
	Error  string
	// P3 LLM cost accounting for this (page,model) call (0 for pre-cost/legacy rows
	// and for providers that don't report usage).
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// LoginRecipe is a target's stored authenticated-crawl recipe (P4). One per
// target (target_id is the primary key). StepsJSON is the canonical ordered step
// list (structure + selectors + credential PLACEHOLDER refs — never values).
// CredsEncrypted is the AES-256-GCM-encrypted, base64-encoded credentials blob
// ({username,password,…}); it is the ONLY place credential values live and is
// never rendered back to the UI or logged. The success_* columns mirror the
// guided form's success condition for convenient display/editing.
type LoginRecipe struct {
	TargetID           string
	LoginURL           string
	StepsJSON          string
	SuccessSelector    string
	SuccessURLContains string
	SuccessTimeoutMs   int
	CredsEncrypted     string // base64(AES-GCM(nonce||ct)); NEVER logged/rendered
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TargetAuditConfig is the persona-walkthrough Phase-2 per-target audit config
// (one row per target). It makes the audit goals first-class + reusable: a draft
// is auto-inferred from a completed crawl (Inferred), the owner confirms/edits it
// (Confirmed), and the evaluate trigger defaults from it. ProductSummary/PrimaryJob/
// PrimaryCTA are model-authored on infer → always rendered escaped. Personas is a
// subset of the curated eval persona ids (stored as a JSON array in personas_json).
type TargetAuditConfig struct {
	TargetID       string
	ProductSummary string
	PrimaryJob     string
	PrimaryCTA     string
	Personas       []string
	Inferred       bool
	Confirmed      bool
	// Phase-3 goal-directed DRIVER success condition (goal + success authored
	// together): the goal is deterministically reached when SuccessSelector becomes
	// visible OR the URL contains SuccessURLContains, within SuccessTimeoutMs.
	SuccessSelector    string
	SuccessURLContains string
	SuccessTimeoutMs   int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Walkthrough-driver job statuses (Phase 3). Same lifecycle shape as the notes/
// eval jobs (idle→driving→done|failed).
const (
	WalkIdle    = "idle"
	WalkDriving = "driving"
	WalkDone    = "done"
	WalkFailed  = "failed"
)

// Walkthrough outcomes (Phase 3). "" until the pass finalizes.
const (
	WalkOutcomeSuccess = "success"
	WalkOutcomeStuck   = "stuck"
	WalkOutcomeFailed  = "failed"
)

// Walkthrough is one async goal-directed DRIVER pass for a target (Phase 3). It
// tracks the deterministic outcome (success|stuck|failed), where it got stuck, a
// credential-free reason, whether it ran in dry-run (submit-guard) mode, async job
// progress, and the reset-per-pass planner LLM cost. Scoped via target→user (no
// direct user_id column). RunID is optional (a walkthrough may be tied to a run for
// context; "" when standalone).
type Walkthrough struct {
	ID               string
	TargetID         string
	RunID            string
	Goal             string
	Outcome          string // ""|success|stuck|failed
	StuckStep        int
	Reason           string
	DryRun           bool
	Status           string // idle|driving|done|failed
	StepsTotal       int
	StepsDone        int
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
	// PrevWalkthroughID links this walkthrough to the target's previous TERMINAL
	// walkthrough — the baseline for Phase-4 walkthrough-vs-walkthrough regression
	// diffing (mirror of runs.prev_run_id). "" for the target's first walkthrough.
	PrevWalkthroughID string
	// DiffJSON is the report.WalkthroughDiff regression summary vs PrevWalkthroughID
	// (mirror of runs.diff_json). "" when there is no baseline / not yet computed.
	DiffJSON string
	// InfraFailed is true when this walkthrough failed because the DRIVER could not run
	// (browser stall/start failure, a config/setup failure, or a restart sweep) rather
	// than because the audited product regressed (#45). It never observed the goal, so
	// the Phase-4 diff does not score it and it is never a regression BASELINE.
	InfraFailed bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// WalkthroughStep is one recorded action in a walkthrough's deterministic trace.
// ActionJSON, URL and PlannerReason are planner-authored + UNTRUSTED → stored raw,
// rendered ESCAPED. ScreenshotKey points at the post-action capture in the Store.
type WalkthroughStep struct {
	ID            string
	WalkthroughID string
	Idx           int
	ActionJSON    string
	URL           string
	ScreenshotKey string
	Outcome       string
	PlannerReason string
	// DigestJSON is the bounded DOM/accessibility digest (a11y-digest.js output,
	// report.A11yDigest shape) captured on this step's page during the drive (Phase 2).
	// Empty ("") for pre-0061 walkthroughs or a capture failure → the synthetic page
	// gets no a11y_digest_key and the persona eval degrades to screenshot-only.
	DigestJSON string
	CreatedAt  time.Time
}

// Page is one crawled URL at one viewport within a run.
type Page struct {
	ID                     string
	RunID                  string
	URL                    string
	Viewport               string
	ScreenshotKey          string
	AxeKey                 string
	A11yDigestKey          string // Phase 1: storage key of the DOM/a11y digest ("" pre-0060 / pushed)
	AxeViolationCount      int
	ConsoleFirstPartyCount int
	ConsoleThirdPartyCount int
	NetworkFirstPartyCount int
	NetworkThirdPartyCount int
	LoadMS                 int64
	DiffPct                float64 // P2: % pixels changed vs baseline (0 when no baseline)
	DiffKey                string  // P2: object-storage key of the diff image ("" when none)
	// Deterministic perf / web-vitals capture (0 for pre-migration rows). TBTMs is
	// a headless LAB PROXY (no field input), not a real field metric.
	LCPMs       int64
	CLS         float64
	TBTMs       int64
	WeightBytes int64
	ReqCount    int
	CreatedAt   time.Time
}

// Finding is one normalized issue attached to a page.
type Finding struct {
	ID        string
	PageID    string
	Type      string
	Severity  string
	Detail    string // JSON
	CreatedAt time.Time
}
