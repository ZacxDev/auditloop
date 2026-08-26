// Package metrics holds the Prometheus collectors for auditloop.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RunsTotal counts finished runs by terminal status.
	RunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_runs_total",
		Help: "Total audit runs by terminal status (done|failed).",
	}, []string{"status"})

	// PagesCrawled counts individual page×viewport captures.
	PagesCrawled = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_pages_crawled_total",
		Help: "Total page/viewport captures across all runs.",
	})

	// URLsBlocked counts URLs refused by the SSRF guard.
	URLsBlocked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_urls_blocked_total",
		Help: "Total URLs refused by the SSRF/abuse guard.",
	})

	// CrawlDuration observes end-to-end crawl wall time in seconds.
	CrawlDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auditloop_crawl_duration_seconds",
		Help:    "End-to-end crawl duration per run in seconds.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
	})

	// VisualRegressions counts page+viewport captures flagged as visual
	// regressions (diff_pct ≥ threshold) across all diffed runs (P2).
	VisualRegressions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_visual_regressions_total",
		Help: "Total page/viewport captures flagged as visual regressions vs the baseline run.",
	})

	// BrowserStalls counts times the #41 hard watchdog fired — a chromedp call
	// blew past its wall-clock budget while ignoring its context, and the browser
	// was killed to unwind it. Labelled by the phase that stalled
	// (probe|capture|login|drive) so a font-less/stalling environment is
	// observable rather than only visible in the logs. Any non-zero value means
	// runs are failing on infrastructure, not on the audited site.
	BrowserStalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_browser_stalls_total",
		Help: "Times the browser watchdog killed a stalled chromium (see issue #41), by phase.",
	}, []string{"phase"})

	// RunPagesChanged is the number of visually-changed pages in the most
	// recently diffed run (P2). A gauge — inspect the last run at a glance.
	RunPagesChanged = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "auditloop_run_pages_changed",
		Help: "Pages with a visual change vs the baseline in the most recently diffed run.",
	})

	// NotesGenerated counts vision-LLM UX-note drafts by model + outcome (P3).
	NotesGenerated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_notes_generated_total",
		Help: "Total vision-LLM UX-note drafts by model and status (ok|error).",
	}, []string{"model", "status"})

	// LoginAttempts counts authenticated-crawl login attempts by outcome (P4):
	// status is ok|failed for the pre-crawl login, and probe_ok|probe_failed for
	// the standalone login-test.
	LoginAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_login_attempts_total",
		Help: "Total login-recipe attempts by status (ok|failed|probe_ok|probe_failed).",
	}, []string{"status"})

	// PluginPushes counts plugin-push ingestion attempts by outcome (P5):
	// ok|rejected|unauthorized|rate_limited|too_large|error.
	PluginPushes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_plugin_pushes_total",
		Help: "Total plugin-push ingestion attempts by status (ok|rejected|unauthorized|rate_limited|too_large|error).",
	}, []string{"status"})

	// PluginPagesIngested counts page/viewport captures ingested via plugin push (P5).
	PluginPagesIngested = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_plugin_pages_ingested_total",
		Help: "Total page/viewport captures ingested via plugin push.",
	})

	// APIReads counts read-API requests by outcome:
	// ok|unauthorized|forbidden|not_found|rate_limited|error.
	APIReads = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_api_reads_total",
		Help: "Total read-API requests by status (ok|unauthorized|not_found|rate_limited|error).",
	}, []string{"status"})

	// NotesDuration observes per-(page,model) draft latency in seconds (P3).
	NotesDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auditloop_notes_duration_seconds",
		Help:    "Vision-LLM UX-note draft latency per (page,model) in seconds.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 45, 90},
	})

	// NotesCostUSD sums the exact per-call OpenRouter cost (USD) by model (P3).
	// Cumulative; a dashboard depends on this exact name.
	NotesCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_notes_cost_usd_total",
		Help: "Cumulative vision-LLM UX-note cost in USD by model (from OpenRouter usage accounting).",
	}, []string{"model"})

	// NotesPromptTokens sums prompt tokens consumed by model (P3).
	NotesPromptTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_notes_prompt_tokens_total",
		Help: "Cumulative vision-LLM UX-note prompt tokens by model.",
	}, []string{"model"})

	// NotesCompletionTokens sums completion tokens produced by model (P3).
	NotesCompletionTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_notes_completion_tokens_total",
		Help: "Cumulative vision-LLM UX-note completion tokens by model.",
	}, []string{"model"})

	// --- Task-grounded persona-walkthrough evaluation (Phase 1) ---

	// EvalGenerated counts per-(page,persona) evaluation cells by persona + outcome.
	EvalGenerated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_eval_generated_total",
		Help: "Total persona-walkthrough evaluation cells by persona and status (ok|error).",
	}, []string{"persona", "status"})

	// EvalDuration observes per-(page,persona) evaluation latency (generation +
	// optional verification) in seconds.
	EvalDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auditloop_eval_duration_seconds",
		Help:    "Persona-walkthrough evaluation latency per (page,persona) in seconds.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 45, 90},
	})

	// EvalCostUSD sums the exact per-call OpenRouter cost (USD) by persona (the
	// synthesis pass reports under the "_synthesis" label). Cumulative.
	EvalCostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_eval_cost_usd_total",
		Help: "Cumulative persona-walkthrough evaluation cost in USD by persona.",
	}, []string{"persona"})

	// EvalPromptTokens sums prompt tokens consumed by persona.
	EvalPromptTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_eval_prompt_tokens_total",
		Help: "Cumulative persona-walkthrough evaluation prompt tokens by persona.",
	}, []string{"persona"})

	// EvalCompletionTokens sums completion tokens produced by persona.
	EvalCompletionTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_eval_completion_tokens_total",
		Help: "Cumulative persona-walkthrough evaluation completion tokens by persona.",
	}, []string{"persona"})

	// EvalFindingsGated counts model-authored findings the DETERMINISTIC DOM/a11y
	// gate acted on, by action:
	//   contradicted — the captured digest POSITIVELY REFUTED the claim (verify.go)
	//   ungrounded   — a mechanical a11y claim citing a selector the digest does not
	//                  list, on a non-truncated digest → dropped (ground.go)
	//   reanchored   — an invented selector rewritten onto the digest's own, via the
	//                  accessible name the finding quoted (ground.go)
	// This is the ONE observable that answers "is the DOM grounding load-bearing or
	// decorative?" — a flat zero over a pass means the digest changed nothing.
	EvalFindingsGated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_eval_findings_gated_total",
		Help: "Model-authored evaluation findings acted on by the deterministic DOM/a11y gate (contradicted|ungrounded|reanchored).",
	}, []string{"action"})

	// --- Goal-directed walkthrough driver (Phase 3) ---

	// WalkthroughRuns counts completed driver passes by outcome
	// (success|stuck|failed).
	WalkthroughRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_walkthrough_runs_total",
		Help: "Total goal-directed walkthrough driver passes by outcome (success|stuck|failed).",
	}, []string{"outcome"})

	// WalkthroughSteps counts individual driver actions by outcome
	// (ok|error|submit-blocked|...).
	WalkthroughSteps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "auditloop_walkthrough_steps_total",
		Help: "Total goal-directed walkthrough driver steps by outcome.",
	}, []string{"outcome"})

	// WalkthroughDuration observes end-to-end driver wall time in seconds.
	WalkthroughDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "auditloop_walkthrough_duration_seconds",
		Help:    "End-to-end goal-directed walkthrough driver duration per pass in seconds.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
	})

	// WalkthroughCostUSD sums the exact per-call OpenRouter planner cost (USD).
	WalkthroughCostUSD = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_walkthrough_cost_usd_total",
		Help: "Cumulative goal-directed walkthrough planner cost in USD (from OpenRouter usage accounting).",
	})

	// WalkthroughRegressions counts walkthroughs whose deterministic outcome/stuck
	// signal REGRESSED vs the target's previous terminal walkthrough (Phase 4). The
	// mirror of VisualRegressions (P2): counted once per walkthrough at drive end.
	WalkthroughRegressions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auditloop_walkthrough_regressions_total",
		Help: "Total walkthroughs flagged as an outcome/stuck regression vs the previous terminal walkthrough.",
	})
)
