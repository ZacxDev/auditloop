package db

import "fmt"

// migration is one ordered, idempotent-at-the-runner-level DDL step. Statements
// are written portably (TEXT ids + TEXT RFC3339 timestamps) so the same DDL runs
// on SQLite and Postgres.
type migration struct {
	id  string
	sql string
}

var migrations = []migration{
	{
		id: "0001_schema_migrations",
		sql: `CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
	},
	{
		id: "0002_targets",
		sql: `CREATE TABLE IF NOT EXISTS targets (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL,
			base_url TEXT NOT NULL,
			verified_domains TEXT NOT NULL DEFAULT '[]',
			auth_mode TEXT NOT NULL DEFAULT 'none',
			created_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0003_targets_user_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_targets_user ON targets(user_id)`,
	},
	{
		id: "0004_runs",
		sql: `CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			target_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			trigger TEXT NOT NULL DEFAULT 'manual',
			prev_run_id TEXT,
			summary_json TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			started_at TEXT,
			finished_at TEXT
		)`,
	},
	{
		id:  "0005_runs_status_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status)`,
	},
	{
		id:  "0006_runs_target_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_runs_target ON runs(target_id)`,
	},
	{
		id: "0007_pages",
		sql: `CREATE TABLE IF NOT EXISTS pages (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			url TEXT NOT NULL,
			viewport TEXT NOT NULL,
			screenshot_key TEXT NOT NULL DEFAULT '',
			axe_key TEXT NOT NULL DEFAULT '',
			axe_violation_count INTEGER NOT NULL DEFAULT 0,
			console_first_party_count INTEGER NOT NULL DEFAULT 0,
			console_third_party_count INTEGER NOT NULL DEFAULT 0,
			network_first_party_count INTEGER NOT NULL DEFAULT 0,
			network_third_party_count INTEGER NOT NULL DEFAULT 0,
			load_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0008_pages_run_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_pages_run ON pages(run_id)`,
	},
	{
		id: "0009_findings",
		sql: `CREATE TABLE IF NOT EXISTS findings (
			id TEXT PRIMARY KEY,
			page_id TEXT NOT NULL,
			type TEXT NOT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			detail TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0010_findings_page_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_findings_page ON findings(page_id)`,
	},
	// --- P2 regression diffing ---
	// pages.diff_pct: percent (0–100) of pixels changed vs the matched baseline
	// page+viewport. pages.diff_key: object-storage key of the diff image (empty
	// when there is no baseline to diff against). REAL is portable across
	// SQLite and Postgres.
	{
		id:  "0011_pages_diff_pct",
		sql: `ALTER TABLE pages ADD COLUMN diff_pct REAL NOT NULL DEFAULT 0`,
	},
	{
		id:  "0012_pages_diff_key",
		sql: `ALTER TABLE pages ADD COLUMN diff_key TEXT NOT NULL DEFAULT ''`,
	},
	// runs.diff_json: the run-level regression summary (report.Diff) as JSON.
	// Empty string when the run has no baseline (first run for the target).
	{
		id:  "0013_runs_diff_json",
		sql: `ALTER TABLE runs ADD COLUMN diff_json TEXT NOT NULL DEFAULT ''`,
	},
	// --- P3 vision-LLM UX notes ---
	// page_notes holds one editable markdown UX-notes draft per (page, model).
	// A re-draft REPLACES the (page,model) row (upsert); a human edit sets edited=1.
	// `edited` is stored as INTEGER (0/1) rather than BOOLEAN for dual-dialect
	// portability (SQLite has no bool type; Postgres accepts 0/1 into a smallint-ish
	// INTEGER column all the same). Scoping is enforced via the run/target join
	// (page_id → pages.run_id → runs.user_id), matching the rest of the schema.
	{
		id: "0014_page_notes",
		sql: `CREATE TABLE IF NOT EXISTS page_notes (
			id TEXT PRIMARY KEY,
			page_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			model TEXT NOT NULL,
			notes TEXT NOT NULL DEFAULT '',
			edited INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0015_page_notes_run_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_page_notes_run ON page_notes(run_id)`,
	},
	// One row per (page, model): a UNIQUE index makes the upsert deterministic.
	{
		id:  "0016_page_notes_page_model_uidx",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uidx_page_notes_page_model ON page_notes(page_id, model)`,
	},
	// Async notes-job status on runs (mirrors how crawl runs report progress): a
	// poll endpoint reads these; a startup sweep (MarkGeneratingNotesFailed) settles
	// jobs orphaned in 'generating' by a restart.
	{
		id:  "0017_runs_notes_status",
		sql: `ALTER TABLE runs ADD COLUMN notes_status TEXT NOT NULL DEFAULT 'idle'`,
	},
	{
		id:  "0018_runs_notes_done",
		sql: `ALTER TABLE runs ADD COLUMN notes_done INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0019_runs_notes_total",
		sql: `ALTER TABLE runs ADD COLUMN notes_total INTEGER NOT NULL DEFAULT 0`,
	},
	// --- P4 login recipes (authenticated crawls) ---
	// login_recipes holds one authenticated-crawl recipe per target (target_id is
	// the PK/unique). steps_json is the canonical ordered step list (structure +
	// selectors + credential PLACEHOLDER refs, never values). creds_encrypted is
	// the AES-256-GCM-encrypted, base64-encoded credentials blob — the ONLY place
	// credential values live at rest; it is never rendered back or logged. Stored
	// as TEXT (base64) rather than a native BLOB for dual-dialect portability.
	// Scoping is via target_id → targets.user_id (no direct user_id column).
	{
		id: "0020_login_recipes",
		sql: `CREATE TABLE IF NOT EXISTS login_recipes (
			target_id TEXT PRIMARY KEY,
			login_url TEXT NOT NULL DEFAULT '',
			steps_json TEXT NOT NULL DEFAULT '[]',
			success_selector TEXT NOT NULL DEFAULT '',
			success_url_contains TEXT NOT NULL DEFAULT '',
			success_timeout_ms INTEGER NOT NULL DEFAULT 0,
			creds_encrypted TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	// --- P5 plugin push (push-only targets) ---
	// plugin_tokens holds ONE active push token per plugin target
	// (targets.auth_mode='plugin'). Only the sha256 hash (hex) of the token is
	// stored — the plaintext is shown to the user once at creation/rotation and
	// never persisted (tokens are not reversible from the DB). Rotating replaces
	// the hash (upsert on the target_id PK), invalidating the old token.
	{
		id: "0021_plugin_tokens",
		sql: `CREATE TABLE IF NOT EXISTS plugin_tokens (
			target_id TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	// Lookup path: hash the presented token → find the target by token_hash.
	{
		id:  "0022_plugin_tokens_hash_idx",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uidx_plugin_tokens_hash ON plugin_tokens(token_hash)`,
	},
	// runs.label carries the optional pushed-run label (empty for crawled runs)
	// so a pushed run is identifiable in report.json. A pushed run stamps the
	// existing runs.trigger column = 'plugin'.
	{
		id:  "0023_runs_label",
		sql: `ALTER TABLE runs ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
	},
	// --- P3 LLM cost tracking ---
	// OpenRouter reports the exact per-call USD cost + token counts when a request
	// opts in ("usage":{"include":true}). We persist them both per (page,model) note
	// cell and accumulated per run. REAL/INTEGER are portable across SQLite+Postgres;
	// the DEFAULT keeps existing rows valid (pre-cost notes read back as 0 → no badge).
	{
		id:  "0024_page_notes_cost_usd",
		sql: `ALTER TABLE page_notes ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`,
	},
	{
		id:  "0025_page_notes_prompt_tokens",
		sql: `ALTER TABLE page_notes ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0026_page_notes_completion_tokens",
		sql: `ALTER TABLE page_notes ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	// Per-run accumulators. AddNotesCost increments these DB-side (SET x = x + ?) so
	// the pass's concurrent per-(page,model) writes don't lose updates; ClaimNotesJob
	// RESETS them to 0 when a fresh pass starts, so a Regenerate reflects the LATEST
	// pass's cost rather than stacking every pass ever run.
	{
		id:  "0027_runs_notes_cost_usd",
		sql: `ALTER TABLE runs ADD COLUMN notes_cost_usd REAL NOT NULL DEFAULT 0`,
	},
	{
		id:  "0028_runs_notes_prompt_tokens",
		sql: `ALTER TABLE runs ADD COLUMN notes_prompt_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0029_runs_notes_completion_tokens",
		sql: `ALTER TABLE runs ADD COLUMN notes_completion_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	// --- Deterministic perf / web-vitals capture (per page+viewport) ---
	// LCP/TBT are milliseconds, CLS unitless, weight in bytes, plus the request
	// count. One ALTER per column (matching the existing dual-dialect pattern); the
	// DEFAULT 0 keeps pre-migration rows valid (they read back as 0 → no perf line).
	// tbt_ms is a headless LAB PROXY, not a real field metric (see crawler/report).
	{
		id:  "0030_pages_lcp_ms",
		sql: `ALTER TABLE pages ADD COLUMN lcp_ms INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0031_pages_cls",
		sql: `ALTER TABLE pages ADD COLUMN cls REAL NOT NULL DEFAULT 0`,
	},
	{
		id:  "0032_pages_tbt_ms",
		sql: `ALTER TABLE pages ADD COLUMN tbt_ms INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0033_pages_weight_bytes",
		sql: `ALTER TABLE pages ADD COLUMN weight_bytes INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0034_pages_req_count",
		sql: `ALTER TABLE pages ADD COLUMN req_count INTEGER NOT NULL DEFAULT 0`,
	},
	// --- Read API keys (machine-authenticated, read-only pulls) ---
	// api_keys holds per-user, read-only API keys used by autonomous agents/CLIs
	// to pull structured audit results WITHOUT a Supabase user JWT. Only the
	// sha256 hash (hex) of the key is stored — the plaintext is shown once at
	// creation and never persisted (keys are not reversible from the DB). A key
	// belongs to a user_id and may read ONLY runs/targets/artifacts owned by that
	// user (enforced by the DB join). scope is 'read' for now (future-proofing).
	// Rotation = revoke old (DELETE) + create new. Stored as TEXT ids + RFC3339
	// TEXT timestamps for dual-dialect portability.
	{
		id: "0035_api_keys",
		sql: `CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT 'read',
			created_at TEXT NOT NULL,
			last_used_at TEXT
		)`,
	},
	// Lookup path: hash the presented key → find the owning user by token_hash.
	{
		id:  "0036_api_keys_hash_uidx",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uidx_api_keys_hash ON api_keys(token_hash)`,
	},
	// List path: a user's keys, scoped by user_id.
	{
		id:  "0037_api_keys_user_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id)`,
	},
	// --- Pushed-run environment (perf-honesty) ---
	// runs.environment records a pushed run's declared environment (lab|staging|prod;
	// '' for crawled/unspecified). A 'lab' push measured perf on a hermetic localhost
	// stack, so its perf numbers are not field-representative — the ingest path
	// suppresses the misleading perf FINDINGS for it (raw perf columns kept). Stored
	// so the run view can label it. DEFAULT '' keeps pre-migration rows valid.
	{
		id:  "0038_runs_environment",
		sql: `ALTER TABLE runs ADD COLUMN environment TEXT NOT NULL DEFAULT ''`,
	},
	// --- Task-grounded persona-walkthrough evaluation (Phase 1) ---
	// page_evaluations holds one structured persona-walkthrough verdict per
	// (page, persona). findings_json is the report.PageEvaluation object (blockers/
	// frictions/top_fix + comprehension) — model-authored selector/evidence strings
	// are UNTRUSTED, stored as escaped JSON and rendered escaped. comprehension is
	// duplicated out as a column for cheap filtering/badging. A re-run REPLACES the
	// (page,persona) row (upsert). error is non-empty when the LLM call for that cell
	// failed (the row degrades in place rather than failing the whole pass). Scoping
	// is via the page→run→user join (no user_id column), like page_notes. Portable
	// DDL (TEXT ids + RFC3339 TEXT timestamps; REAL/INTEGER cost columns).
	{
		id: "0039_page_evaluations",
		sql: `CREATE TABLE IF NOT EXISTS page_evaluations (
			id TEXT PRIMARY KEY,
			page_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			persona TEXT NOT NULL,
			findings_json TEXT NOT NULL DEFAULT '',
			comprehension TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			cost_usd REAL NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0040_page_evaluations_run_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_page_evaluations_run ON page_evaluations(run_id)`,
	},
	// One row per (page, persona): a UNIQUE index makes the upsert deterministic.
	{
		id:  "0041_page_evaluations_page_persona_uidx",
		sql: `CREATE UNIQUE INDEX IF NOT EXISTS uidx_page_evaluations_page_persona ON page_evaluations(page_id, persona)`,
	},
	// Run-level synthesis "story" (report.EvalSynthesis JSON), the free-text job the
	// pass evaluated toward (shown + reused on re-run), and async job tracking on
	// runs mirroring the P3 notes columns: status (idle|generating|done|failed),
	// done/total progress, and the reset-per-pass LLM cost accumulators. DEFAULTs
	// keep pre-migration rows valid. One ALTER per column (dual-dialect pattern).
	{
		id:  "0042_runs_eval_synthesis_json",
		sql: `ALTER TABLE runs ADD COLUMN eval_synthesis_json TEXT NOT NULL DEFAULT ''`,
	},
	{
		id:  "0043_runs_eval_job",
		sql: `ALTER TABLE runs ADD COLUMN eval_job TEXT NOT NULL DEFAULT ''`,
	},
	{
		id:  "0044_runs_eval_status",
		sql: `ALTER TABLE runs ADD COLUMN eval_status TEXT NOT NULL DEFAULT 'idle'`,
	},
	{
		id:  "0045_runs_eval_done",
		sql: `ALTER TABLE runs ADD COLUMN eval_done INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0046_runs_eval_total",
		sql: `ALTER TABLE runs ADD COLUMN eval_total INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0047_runs_eval_cost_usd",
		sql: `ALTER TABLE runs ADD COLUMN eval_cost_usd REAL NOT NULL DEFAULT 0`,
	},
	{
		id:  "0048_runs_eval_prompt_tokens",
		sql: `ALTER TABLE runs ADD COLUMN eval_prompt_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	{
		id:  "0049_runs_eval_completion_tokens",
		sql: `ALTER TABLE runs ADD COLUMN eval_completion_tokens INTEGER NOT NULL DEFAULT 0`,
	},
	// Persona-walkthrough Phase 2: per-target audit config (one row per target,
	// target_id PK). Makes a target's audit goals first-class + reusable — a draft
	// is auto-INFERRED from a completed crawl (inferred=1), the owner CONFIRMs/edits
	// it (confirmed=1), and the evaluate trigger DEFAULTS from it. product_summary/
	// primary_job/primary_cta are one-liners; personas_json is a JSON array of
	// curated persona ids (a subset of eval.Personas). inferred/confirmed are
	// INTEGER (0/1) not BOOLEAN for dual-dialect portability. Scoped via target_id →
	// targets.user_id (no direct user_id column), like login_recipes.
	{
		id: "0050_target_audit_config",
		sql: `CREATE TABLE IF NOT EXISTS target_audit_config (
			target_id TEXT PRIMARY KEY,
			product_summary TEXT NOT NULL DEFAULT '',
			primary_job TEXT NOT NULL DEFAULT '',
			primary_cta TEXT NOT NULL DEFAULT '',
			personas_json TEXT NOT NULL DEFAULT '[]',
			inferred INTEGER NOT NULL DEFAULT 0,
			confirmed INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	// Persona-walkthrough Phase 3 (goal-directed DRIVER). The DETERMINISTIC success
	// condition lives alongside the goal on target_audit_config (goal + success are
	// authored together): a selector to appear OR a URL substring, within a timeout.
	{
		id:  "0051_audit_config_success_selector",
		sql: `ALTER TABLE target_audit_config ADD COLUMN success_selector TEXT NOT NULL DEFAULT ''`,
	},
	{
		id:  "0052_audit_config_success_url_contains",
		sql: `ALTER TABLE target_audit_config ADD COLUMN success_url_contains TEXT NOT NULL DEFAULT ''`,
	},
	{
		id:  "0053_audit_config_success_timeout_ms",
		sql: `ALTER TABLE target_audit_config ADD COLUMN success_timeout_ms INTEGER NOT NULL DEFAULT 0`,
	},
	// driving_enabled is the DEFAULT-OFF, opt-in per-target gate for the driver. A
	// walkthrough is refused at BOTH the route and the generator unless this is on.
	{
		id:  "0054_targets_driving_enabled",
		sql: `ALTER TABLE targets ADD COLUMN driving_enabled INTEGER NOT NULL DEFAULT 0`,
	},
	// walkthroughs: one async goal-directed driver pass. outcome ∈ ''|success|stuck|
	// failed; status ∈ idle|driving|done|failed. dry_run records whether the pass
	// ran under the (default) submit-guard. Scoped via target_id → targets.user_id.
	{
		id: "0055_walkthroughs",
		sql: `CREATE TABLE IF NOT EXISTS walkthroughs (
			id TEXT PRIMARY KEY,
			target_id TEXT NOT NULL,
			run_id TEXT NOT NULL DEFAULT '',
			goal TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL DEFAULT '',
			stuck_step INTEGER NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT '',
			dry_run INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'idle',
			steps_total INTEGER NOT NULL DEFAULT 0,
			steps_done INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	},
	// walkthrough_steps: the ordered deterministic trace. action_json + url +
	// planner_reason are UNTRUSTED (planner-authored) → stored raw, rendered escaped.
	{
		id: "0056_walkthrough_steps",
		sql: `CREATE TABLE IF NOT EXISTS walkthrough_steps (
			id TEXT PRIMARY KEY,
			walkthrough_id TEXT NOT NULL,
			idx INTEGER NOT NULL,
			action_json TEXT NOT NULL DEFAULT '',
			url TEXT NOT NULL DEFAULT '',
			screenshot_key TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL DEFAULT '',
			planner_reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
	},
	{
		id:  "0057_walkthrough_steps_idx",
		sql: `CREATE INDEX IF NOT EXISTS idx_walkthrough_steps_wid ON walkthrough_steps (walkthrough_id)`,
	},
	// allow_real_submit is the LOUD, separate, DEFAULT-OFF per-target flag that lets
	// the driver perform REAL form submissions (mutating requests). Off (the
	// default) → the driver runs in dry-run submit-guard mode (non-GET/HEAD requests
	// aborted at the network layer). Kept OUT of driving_enabled deliberately: two
	// independent opt-ins (drive at all vs. mutate live data).
	{
		id:  "0058_targets_allow_real_submit",
		sql: `ALTER TABLE targets ADD COLUMN allow_real_submit INTEGER NOT NULL DEFAULT 0`,
	},
	// favicon_key holds the storage key of the site favicon captured (best-effort,
	// SSRF-guarded) during the crawl — surfaced on the dashboard project card. Nullable
	// (DEFAULT '') so it is fully additive; runCols reads it via COALESCE(favicon_key,'').
	{
		id:  "0059_runs_favicon_key",
		sql: `ALTER TABLE runs ADD COLUMN favicon_key TEXT DEFAULT ''`,
	},
	// a11y_digest_key holds the storage key of the per-page DOM/accessibility digest
	// captured during the crawl (Phase 1 persona-evaluator grounding). NOT NULL DEFAULT ''
	// so it is fully additive: pre-0060 rows + pushed runs read back '' and the evaluator
	// degrades to screenshot-only (no deterministic drop).
	{
		id:  "0060_pages_a11y_digest_key",
		sql: `ALTER TABLE pages ADD COLUMN a11y_digest_key TEXT NOT NULL DEFAULT ''`,
	},
	// digest_json holds the bounded DOM/accessibility digest (a11y-digest.js output)
	// captured PER STEP during a goal-directed walkthrough drive (Phase 2 driven-path
	// grounding). NOT NULL DEFAULT '' so it is fully additive: pre-0061 walkthrough
	// steps read back '' and MaterializeWalkthroughRun sets no a11y_digest_key on their
	// synthetic pages → the persona evaluator degrades to screenshot-only.
	{
		id:  "0061_walkthrough_steps_digest_json",
		sql: `ALTER TABLE walkthrough_steps ADD COLUMN digest_json TEXT NOT NULL DEFAULT ''`,
	},
	// prev_walkthrough_id links a walkthrough to the target's PREVIOUS TERMINAL
	// walkthrough (the baseline for walkthrough-vs-walkthrough regression diffing,
	// Phase 4) — the exact mirror of runs.prev_run_id (P2). Stamped at CreateWalkthrough.
	// NOT NULL DEFAULT '' so it is fully additive: the target's first walkthrough (and
	// every pre-0062 row) reads back '' → no baseline → no diff.
	{
		id:  "0062_walkthroughs_prev_walkthrough_id",
		sql: `ALTER TABLE walkthroughs ADD COLUMN prev_walkthrough_id TEXT NOT NULL DEFAULT ''`,
	},
	// diff_json holds the run-level walkthrough regression summary (report.WalkthroughDiff
	// JSON) vs prev_walkthrough_id — the mirror of runs.diff_json (P2). NOT NULL DEFAULT ''
	// so it is fully additive; '' = no diff computed yet / no baseline.
	{
		id:  "0063_walkthroughs_diff_json",
		sql: `ALTER TABLE walkthroughs ADD COLUMN diff_json TEXT NOT NULL DEFAULT ''`,
	},
	// infra_failed marks a walkthrough that FAILED because the driver/browser could not
	// run (a watchdog-killed stall, a font-less render probe, a restart sweep) rather
	// than because the product regressed (#45). Such a pass observed nothing, so the
	// Phase-4 diff must not score it as a regression and it must never be a baseline.
	// INTEGER (not BOOLEAN) for dual-dialect portability; NOT NULL DEFAULT 0 so it is
	// fully additive — every pre-0064 row reads back 0 (see the one-time transition
	// note in CLAUDE.md).
	{
		id:  "0064_walkthroughs_infra_failed",
		sql: `ALTER TABLE walkthroughs ADD COLUMN infra_failed INTEGER NOT NULL DEFAULT 0`,
	},
}

// migrate applies any unapplied migrations in order.
func (d *DB) migrate() error {
	// Bootstrap the tracking table (first migration is the table itself; run it
	// unconditionally since we can't query the table before it exists).
	if _, err := d.exec(migrations[0].sql); err != nil {
		return fmt.Errorf("db: bootstrap migrations table: %w", err)
	}
	for _, m := range migrations {
		var count int
		if err := d.queryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, m.id).Scan(&count); err != nil {
			return fmt.Errorf("db: check migration %s: %w", m.id, err)
		}
		if count > 0 {
			continue
		}
		if _, err := d.exec(m.sql); err != nil {
			return fmt.Errorf("db: apply migration %s: %w", m.id, err)
		}
		if _, err := d.exec(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`, m.id, nowRFC()); err != nil {
			return fmt.Errorf("db: record migration %s: %w", m.id, err)
		}
	}
	return nil
}
