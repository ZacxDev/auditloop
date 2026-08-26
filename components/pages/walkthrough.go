package pages

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// WalkthroughVM is the Phase-3 goal-directed walkthrough section view model. The
// whole section is gated on Enabled (OpenRouterEnabled + the worker role) AND
// DrivingEnabled (the loud, default-off per-target opt-in). Planner-authored
// strings (action/url/reason) are rendered ESCAPED via g.Text (never g.Raw).
type WalkthroughVM struct {
	TargetID       string
	Enabled        bool // OpenRouter key present AND this role drives (worker)
	DrivingEnabled bool // the per-target opt-in gate
	DryRunDefault  bool // !allow_real_submit (what the NEXT run would use)

	HasGoal    bool
	HasSuccess bool
	Goal       string

	HasRun        bool
	WalkthroughID string
	Status        string // idle|driving|done|failed
	Outcome       string // ''|success|stuck|failed
	StuckStep     int
	Reason        string
	DryRun        bool // the mode the LATEST run used
	Done          int
	Total         int
	CostUSD       float64
	Steps         []WalkStepVM

	// Phase 4: the walkthrough-vs-previous-walkthrough regression summary. When the
	// latest walkthrough has a baseline this carries the outcome transition, stuck-step
	// movement, and new/resolved persona task-blockers; when there's no baseline it
	// renders a subtle "first walkthrough" note.
	Changes *WalkChangesVM

	// Phase-3 PR-B: persona evaluation over the DRIVEN trace. EvalEnabled gates the
	// whole block (OpenRouter key present); it only renders on a Terminal walkthrough
	// with recorded steps. When EvalStarted (a synthetic run has been materialized and
	// evaluated), Eval carries the embedded EvaluationSection VM; otherwise the trigger
	// form is shown, defaulting personas from EvalDefaultPersonas.
	EvalEnabled         bool
	Terminal            bool
	EvalPersonas        []PersonaOptVM
	EvalDefaultPersonas map[string]bool
	EvalStarted         bool
	EvalRunID           string
	Eval                *EvalVM
}

// WalkStepVM is one recorded step in the trace.
type WalkStepVM struct {
	Idx           int
	Action        string
	URL           string
	Outcome       string
	PlannerReason string
	ScreenshotURL string
}

// WalkChangesVM is the Phase-4 "Changes since last walkthrough" view model. All string
// fields (reasons, blocker keys) originate from planner/model text and are rendered
// ESCAPED via g.Text.
type WalkChangesVM struct {
	HasBaseline          bool // a previous terminal walkthrough exists
	HasDiff              bool // a computed diff_json was parsed (else the comparison is pending)
	PrevAtLabel          string
	OutcomeChanged       bool
	PrevOutcome          string
	Outcome              string
	IsRegression         bool
	Resolved             bool
	StuckStepDelta       int
	ReasonChanged        bool
	Reason               string
	NewTaskBlockers      []string
	ResolvedTaskBlockers []string
	BlockersCompared     bool
	// OutcomeCompared=false + InfraFailed=true mean the driver could not RUN (#45):
	// the card shows a neutral "could not run" state instead of a red Regression badge.
	OutcomeCompared bool
	InfraFailed     bool
}

// WalkthroughSection renders the opt-in goal-directed walkthrough block as one
// swappable fragment (id="walkthrough-section"). It self-polls every 3s while a
// pass is driving. Returns an empty node when the feature/opt-in is off.
func WalkthroughSection(vm *WalkthroughVM) g.Node {
	if vm == nil || !vm.Enabled || !vm.DrivingEnabled {
		return h.Div(h.ID("walkthrough-section"))
	}
	attrs := []g.Node{h.ID("walkthrough-section")}
	if vm.Status == "driving" {
		attrs = append(attrs,
			g.Attr("hx-get", "/targets/"+vm.TargetID+"/walkthrough-status"),
			g.Attr("hx-trigger", "load delay:3s"),
			g.Attr("hx-swap", "outerHTML"),
		)
	}

	header := h.Div(h.Class("flex items-start justify-between gap-3"),
		h.P(h.Class("text-sm text-muted"), g.Text("Drives the site toward the goal and reports whether it reached it.")),
		walkControls(vm),
	)

	var body g.Node
	switch {
	case vm.Status == "driving":
		body = walkProgress(vm)
	case !vm.HasRun:
		body = h.P(h.Class("text-sm text-muted"), g.Text("No walkthrough yet. Set a goal + success condition in the audit configuration, then “Run walkthrough”."))
	default:
		body = h.Div(h.Class("space-y-4"),
			walkOutcomeBadge(vm),
			walkCostLine(vm),
			walkChangesCard(vm.Changes),
			walkTrace(vm.Steps),
			walkEvalBlock(vm),
		)
	}

	return h.Div(append(attrs, h.Class("space-y-4"), header, body)...)
}

// walkControls renders the run button (disabled with a hint when goal/success is
// unset) + the dry-run/real-submit indicator.
func walkControls(vm *WalkthroughVM) g.Node {
	if vm.Status == "driving" {
		return g.Text("")
	}
	ready := vm.HasGoal && vm.HasSuccess
	label := "Run walkthrough"
	if vm.HasRun {
		label = "Re-run"
	}
	if !ready {
		return h.Div(h.Class("text-right"),
			h.Button(h.Type("button"), h.Disabled(),
				h.Class("rounded bg-surface px-4 py-2 text-sm font-medium text-muted cursor-not-allowed"),
				g.Text(label)),
			h.P(h.Class("mt-1 text-xs text-muted"), g.Text("Set a goal + success condition in the audit configuration first.")),
		)
	}
	mode := "dry-run (no real submissions)"
	if !vm.DryRunDefault {
		mode = "REAL submissions enabled"
	}
	return h.Div(h.Class("text-right"),
		h.Button(h.Type("button"),
			g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/walkthrough"),
			g.Attr("hx-target", "#walkthrough-section"),
			g.Attr("hx-swap", "outerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("btn-accent"),
			g.Text(label)),
		h.P(h.Class("mt-1 text-xs text-muted"), g.Text("Next run: "+mode)),
	)
}

func walkProgress(vm *WalkthroughVM) g.Node {
	pct := 0
	if vm.Total > 0 {
		pct = vm.Done * 100 / vm.Total
	}
	return h.Div(h.Class("space-y-2"),
		h.P(h.Class("text-sm text-muted"), g.Textf("Driving toward the goal — %d of %d step(s)…", vm.Done, vm.Total)),
		h.Div(h.Class("h-2 w-full overflow-hidden rounded bg-surface"),
			h.Div(h.Class("h-full bg-info transition-all"), h.Style("width:"+strconv.Itoa(pct)+"%")),
		),
	)
}

func walkOutcomeBadge(vm *WalkthroughVM) g.Node {
	var badgeCls, txt string
	switch vm.Outcome {
	case "success":
		badgeCls = "badge-success"
		txt = "Goal reached in " + strconv.Itoa(len(vm.Steps)) + " step(s)"
	case "stuck":
		badgeCls = "badge-warning"
		txt = "Stuck at step " + strconv.Itoa(vm.StuckStep)
	case "failed":
		badgeCls = "badge-danger"
		txt = "Failed"
	default:
		badgeCls = "badge bg-surface text-muted"
		txt = vm.Status
	}
	mode := "dry-run"
	if !vm.DryRun {
		mode = "real submissions"
	}
	return h.Div(h.Class("flex flex-wrap items-center gap-2"),
		h.Span(h.Class(badgeCls), g.Text(txt)),
		h.Span(h.Class("rounded bg-surface px-2 py-0.5 text-xs text-muted"), g.Text(mode)),
		g.If(vm.Reason != "", h.Span(h.Class("text-xs text-muted"), g.Text("— "+vm.Reason))),
	)
}

func walkCostLine(vm *WalkthroughVM) g.Node {
	if vm.CostUSD <= 0 {
		return g.Text("")
	}
	return h.P(h.Class("text-xs text-muted"), g.Text("Planner cost: ~"+formatUSD(vm.CostUSD)))
}

// walkTrace renders the ordered step trace. Action / URL / reason are planner-
// authored + UNTRUSTED → rendered escaped via g.Text.
func walkTrace(steps []WalkStepVM) g.Node {
	if len(steps) == 0 {
		return h.P(h.Class("text-sm text-muted"), g.Text("No steps recorded."))
	}
	rows := make([]g.Node, 0, len(steps))
	for _, s := range steps {
		rows = append(rows, walkStepRow(s))
	}
	return h.Div(h.Class("space-y-3"),
		h.P(h.Class("text-xs font-semibold text-muted uppercase tracking-wide"), g.Text("Step trace")),
		h.Div(h.Class("space-y-3"), g.Group(rows)),
	)
}

func walkStepRow(s WalkStepVM) g.Node {
	var shot g.Node = g.Text("")
	if s.ScreenshotURL != "" {
		shot = h.A(h.Href(s.ScreenshotURL), h.Target("_blank"),
			h.Img(h.Src(s.ScreenshotURL), h.Loading("lazy"), h.Alt("step screenshot"),
				h.Class("h-24 w-40 shrink-0 rounded border border-line object-cover object-top")))
	}
	return h.Div(h.Class("flex items-start gap-3 rounded border border-line p-2"),
		shot,
		h.Div(h.Class("min-w-0 space-y-0.5"),
			h.Div(h.Class("flex items-center gap-2"),
				h.Span(h.Class("flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-surface text-xs font-semibold text-muted"), g.Text(strconv.Itoa(s.Idx))),
				h.Span(h.Class("text-sm font-medium break-all"), g.Text(s.Action)),
				walkStepOutcomeBadge(s.Outcome),
			),
			g.If(s.URL != "", h.P(h.Class("text-xs text-muted break-all"), g.Text(s.URL))),
			g.If(s.PlannerReason != "", h.P(h.Class("text-xs text-muted italic"), g.Text("“"+s.PlannerReason+"”"))),
		),
	)
}

// walkEvalBlock renders the Phase-3 PR-B persona-evaluation control over the driven
// trace. Empty when the feature is off / no steps. When the evaluation has started it
// embeds the EXISTING EvaluationSection (which self-polls + carries its own re-run keyed
// on the synthetic run id); otherwise it shows the trigger form. The outer
// #walkthrough-eval div is the innerHTML swap target of the trigger POST.
func walkEvalBlock(vm *WalkthroughVM) g.Node {
	if !vm.EvalEnabled || len(vm.Steps) == 0 {
		return g.Text("")
	}
	var inner g.Node
	if vm.EvalStarted && vm.Eval != nil {
		inner = EvaluationSection(vm.Eval)
	} else {
		inner = walkEvalTrigger(vm)
	}
	return h.Div(h.ID("walkthrough-eval"), h.Class("border-t border-line pt-4"), inner)
}

// walkEvalTrigger renders the "Evaluate this walkthrough with personas" control: a
// persona multi-select (defaulting from the target's confirmed audit config) posting to
// the walkthrough evaluate route, which materializes a synthetic run and returns the
// EvaluationSection fragment (swapped into #walkthrough-eval).
func walkEvalTrigger(vm *WalkthroughVM) g.Node {
	checks := make([]g.Node, 0, len(vm.EvalPersonas))
	for i, p := range vm.EvalPersonas {
		attrs := []g.Node{h.Type("checkbox"), h.Name("personas"), h.Value(p.ID), h.Class("mt-0.5 accent-brand")}
		checked := i == 0
		if len(vm.EvalDefaultPersonas) > 0 {
			checked = vm.EvalDefaultPersonas[p.ID]
		}
		if checked {
			attrs = append(attrs, h.Checked())
		}
		checks = append(checks, h.Label(h.Class("flex items-start gap-2 text-sm"),
			h.Input(attrs...),
			h.Span(h.Class("font-medium"), g.Text(p.Label))))
	}
	return h.Details(h.Class("relative"),
		h.Summary(h.Class("cursor-pointer inline-flex btn-accent"),
			g.Text("Evaluate this walkthrough with personas")),
		h.Form(
			g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/walkthroughs/"+vm.WalkthroughID+"/evaluate"),
			g.Attr("hx-target", "#walkthrough-eval"),
			g.Attr("hx-swap", "innerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("mt-2 w-80 space-y-3 rounded-lg border border-line bg-card p-4 shadow-lg"),
			h.P(h.Class("text-xs text-muted"), g.Text("Each selected persona walks the driven step trace (one paid pass per step each).")),
			h.Div(h.Class("space-y-1.5"), g.Group(checks)),
			h.Button(h.Type("submit"),
				h.Class("w-full btn-accent"),
				g.Text("Run persona evaluation")),
		),
	)
}

// walkChangesCard renders the Phase-4 "Changes since last walkthrough" block: the
// outcome transition (green resolved / red regression), stuck-step movement, and
// new/resolved persona task-blockers. A subtle note when there is no baseline. Mirrors
// the P2 crawl "Changes since <prev run>" card. Blocker keys are model-authored → g.Text.
func walkChangesCard(c *WalkChangesVM) g.Node {
	if c == nil {
		return g.Text("")
	}
	if !c.HasBaseline {
		return h.Div(h.Class("rounded border border-line bg-card px-4 py-3 text-sm text-muted"),
			g.Text("First walkthrough — no baseline. Run it again later and this space will highlight whether the goal is still reachable and what changed."))
	}
	// A baseline exists but no diff was computed yet (the rare case where the non-fatal
	// drive-end refresh errored). Render a neutral pending note — NOT a misleading
	// "Stable" verdict over an empty outcome.
	if !c.HasDiff {
		return h.Div(h.Class("rounded border border-line bg-card px-4 py-3 text-sm text-muted"),
			g.Text("Comparison against the previous walkthrough is pending — run this walkthrough again to compare."))
	}

	// Nothing was compared (#45). TWO DISTINCT subjects, and the badge must name the
	// right one — a card headed "Could not run" above a note blaming the BASELINE is a
	// self-contradiction the reader has to resolve:
	//   - InfraFailed  → THIS walkthrough's driver could not run.
	//   - !OutcomeCompared alone → this one ran fine; the BASELINE is unusable.
	// Either way: never the red Regression badge, which would read as a product problem.
	if c.InfraFailed || !c.OutcomeCompared {
		title := "Changes since last walkthrough"
		if c.PrevAtLabel != "" {
			title = "Changes since " + c.PrevAtLabel
		}
		badgeText := "Not compared"
		note := "Not compared — the previous walkthrough did not produce a usable observation to compare against. Run this walkthrough again to get a comparison."
		if c.InfraFailed {
			badgeText = "Could not run"
			note = "This walkthrough could not run — the driver or browser failed before the goal could be observed, so nothing was compared. Re-run it."
		}
		var extra []g.Node
		// Driver-authored reason → rendered ESCAPED via g.Text (never g.Raw). Only this
		// walkthrough's own reason is shown, and only when it is the one that failed.
		if c.InfraFailed && strings.TrimSpace(c.Reason) != "" {
			extra = append(extra, h.P(h.Class("text-xs text-muted"),
				g.Text("Reason: "), h.Span(h.Class("font-medium"), g.Text(c.Reason))))
		}
		return h.Div(h.Class("rounded-lg border border-line bg-card p-4 space-y-2"),
			h.Div(h.Class("flex items-center gap-3"),
				h.P(h.Class("text-sm font-semibold"), g.Text(title)),
				h.Span(h.Class("badge-warning"), g.Text(badgeText)),
			),
			h.Div(h.Class("space-y-1"),
				h.P(h.Class("text-sm text-muted"), g.Text(note)),
				g.Group(extra),
			),
		)
	}

	// Outcome-transition badge.
	var badge g.Node
	switch {
	case c.IsRegression:
		badge = h.Span(h.Class("badge-danger"), g.Text("Regression"))
	case c.Resolved:
		badge = h.Span(h.Class("badge-success"), g.Text("Resolved"))
	case c.OutcomeChanged:
		badge = h.Span(h.Class("badge-warning"), g.Text("Changed"))
	default:
		badge = h.Span(h.Class("badge-success"), g.Text("Stable"))
	}

	var body []g.Node
	// Outcome line.
	if c.OutcomeChanged {
		body = append(body, h.P(h.Class("text-sm"),
			g.Text("Outcome: "), h.Span(h.Class("font-medium"), g.Text(c.PrevOutcome)),
			g.Text(" → "), h.Span(h.Class("font-medium"), g.Text(c.Outcome))))
	} else {
		body = append(body, h.P(h.Class("text-sm text-muted"),
			g.Text("Outcome unchanged ("+c.Outcome+")")))
	}
	// Stuck-step movement (both stuck).
	if c.StuckStepDelta != 0 {
		txt := "Got stuck later (+" + strconv.Itoa(c.StuckStepDelta) + " step(s)) — progress"
		cls := "text-success-fg"
		if c.StuckStepDelta < 0 {
			txt = "Got stuck " + strconv.Itoa(-c.StuckStepDelta) + " step(s) earlier — regression"
			cls = "text-danger-fg"
		}
		body = append(body, h.P(h.Class("text-sm "+cls), g.Text(txt)))
	}
	// Reason change — WHY it got stuck/failed changed. Only meaningful for a
	// non-success outcome (a success has no failure reason to report). Reason is
	// driver/planner text → rendered ESCAPED via g.Text (never g.Raw).
	if c.ReasonChanged && strings.TrimSpace(c.Reason) != "" && (c.Outcome == "failed" || c.Outcome == "stuck") {
		body = append(body, h.P(h.Class("text-sm text-muted"),
			g.Text("Reason: "), h.Span(h.Class("font-medium"), g.Text(c.Reason))))
	}
	// New task-blockers (regressions).
	if len(c.NewTaskBlockers) > 0 {
		body = append(body, walkBlockerList("New task-blockers (regressions)", c.NewTaskBlockers, "bg-danger/15 text-danger-fg"))
	}
	if len(c.ResolvedTaskBlockers) > 0 {
		body = append(body, walkBlockerList("Resolved task-blockers", c.ResolvedTaskBlockers, "bg-surface text-muted line-through"))
	}
	if c.BlockersCompared && len(c.NewTaskBlockers) == 0 && len(c.ResolvedTaskBlockers) == 0 {
		body = append(body, h.P(h.Class("text-xs text-muted"), g.Text("No change in persona task-blockers.")))
	}
	if !c.BlockersCompared {
		body = append(body, h.P(h.Class("text-xs text-muted"), g.Text("Persona task-blockers not compared (evaluate both walkthroughs to diff them).")))
	}

	title := "Changes since last walkthrough"
	if c.PrevAtLabel != "" {
		title = "Changes since " + c.PrevAtLabel
	}
	return h.Div(h.Class("rounded-lg border border-line bg-card p-4 space-y-2"),
		h.Div(h.Class("flex items-center gap-3"),
			h.P(h.Class("text-sm font-semibold"), g.Text(title)),
			badge,
		),
		h.Div(h.Class("space-y-1"), g.Group(body)),
	)
}

// walkBlockerList renders a labeled set of task-blocker identity keys as chips
// (model-authored → escaped via g.Text).
func walkBlockerList(label string, keys []string, tone string) g.Node {
	chips := make([]g.Node, 0, len(keys))
	for _, k := range keys {
		chips = append(chips, h.Span(h.Class("rounded px-2 py-0.5 text-xs "+tone), g.Text(prettyBlockerKey(k))))
	}
	return h.Div(
		h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text(label)),
		h.Div(h.Class("flex flex-wrap gap-2"), g.Group(chips)),
	)
}

// prettyBlockerKey renders a "<persona>\x1f<anchor>" identity key as "persona · anchor"
// for display (the raw unit-separator is not human-readable).
func prettyBlockerKey(k string) string {
	if i := strings.IndexByte(k, '\x1f'); i >= 0 {
		return k[:i] + " · " + k[i+1:]
	}
	return k
}

func walkStepOutcomeBadge(outcome string) g.Node {
	if outcome == "" {
		return g.Text("")
	}
	tone := "bg-surface text-muted"
	switch {
	case outcome == "ok" || outcome == "success":
		tone = "bg-success/15 text-success-fg"
	case outcome == "submit-blocked (dry-run)":
		tone = "bg-warning/15 text-warning-fg"
	case outcome == "ssrf-blocked":
		tone = "bg-danger/15 text-danger-fg"
	}
	return h.Span(h.Class("rounded px-1.5 py-0.5 text-[10px] font-medium "+tone), g.Text(outcome))
}
