package pages

import (
	"strconv"

	"github.com/ZacxDev/auditloop/components/partials"
	"github.com/ZacxDev/auditloop/internal/report"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// EvalVM is the persona-walkthrough evaluation view model for a run (Phase 1).
// Enabled is false when no OpenRouter key is configured (the section renders
// nothing). Status mirrors runs.eval_status (idle|generating|done|failed).
type EvalVM struct {
	RunID    string
	Enabled  bool
	Status   string
	Done     int
	Total    int
	Personas []PersonaOptVM // the curated persona set for the multi-select
	Job      string         // the job the last pass evaluated toward (reused on re-run)
	// DefaultPersonas pre-checks the persona boxes from the target's confirmed
	// Phase-2 audit config (nil → fall back to first-persona-checked). Only set on a
	// not-yet-evaluated run; a re-run keeps the last pass's selection empty here.
	DefaultPersonas map[string]bool
	Synthesis       []report.EvalSynthItem
	Pages           []EvalPageVM
	// Cost accounting for the latest pass (0 → no cost line).
	CostUSD   float64
	CellCount int
	ByPersona []PersonaCostVM
}

// PersonaOptVM is one curated persona option in the picker.
type PersonaOptVM struct {
	ID    string
	Label string
	Cares string
}

// PersonaCostVM is one persona's summed cost across the pass.
type PersonaCostVM struct {
	Label   string
	CostUSD float64
}

// EvalPageVM groups a logical page's per-persona evaluation cells.
type EvalPageVM struct {
	URL   string
	Cells []EvalCellVM
}

// EvalCellVM is one (page, persona) evaluation cell.
type EvalCellVM struct {
	Persona       string
	PersonaLabel  string
	Comprehension string
	Error         string
	Eval          report.PageEvaluation
	CostUSD       float64
	Tokens        int
}

// EvaluationSection renders the whole opt-in persona-walkthrough block as a single
// swappable fragment (id="eval-section"). It polls itself every 3s while a pass is
// generating. Returns an empty node when the feature is disabled.
func EvaluationSection(vm *EvalVM) g.Node {
	if vm == nil || !vm.Enabled {
		return h.Div(h.ID("eval-section"))
	}
	// No animate-fade here: this fragment self-swaps every 3s while generating
	// (hx-swap outerHTML), so a fade would re-fire on each poll = a blink.
	attrs := []g.Node{h.ID("eval-section"), h.Class("block")}
	if vm.Status == "generating" {
		attrs = append(attrs,
			g.Attr("hx-get", "/runs/"+vm.RunID+"/eval-status"),
			g.Attr("hx-trigger", "load delay:3s"),
			g.Attr("hx-swap", "outerHTML"),
		)
	}

	header := h.Div(h.Class("mb-3 flex items-center justify-between gap-3"),
		h.Div(
			h.H2(h.Class("text-lg font-semibold"), g.Text("Persona walkthrough")),
			evalCostSummary(vm),
		),
		evalControls(vm),
	)

	var body g.Node
	switch {
	case vm.Status == "generating":
		body = evalProgress(vm)
	case len(vm.Pages) == 0 && len(vm.Synthesis) == 0:
		note := "No persona walkthrough yet — pick personas and a task, then “Run persona walkthrough”."
		if vm.Status == "failed" {
			note = "The last persona walkthrough failed. Try again."
		}
		body = h.P(h.Class("text-sm text-muted"), g.Text(note))
	default:
		var blocks []g.Node
		if syn := evalSynthesis(vm.Synthesis); syn != nil {
			blocks = append(blocks, syn)
		}
		for _, p := range vm.Pages {
			blocks = append(blocks, evalPageBlock(p))
		}
		body = h.Div(h.Class("space-y-6"), g.Group(blocks))
	}

	return h.Div(append(attrs, partials.Card(header, body))...)
}

// evalControls renders the persona multi-select + job field + verify toggle.
func evalControls(vm *EvalVM) g.Node {
	if vm.Status == "generating" {
		return g.Text("")
	}
	label := "Run persona walkthrough"
	if len(vm.Pages) > 0 || len(vm.Synthesis) > 0 {
		label = "Re-run"
	}
	checks := make([]g.Node, 0, len(vm.Personas))
	for i, p := range vm.Personas {
		attrs := []g.Node{h.Type("checkbox"), h.Name("personas"), h.Value(p.ID), h.Class("mt-0.5 accent-blue-500")}
		// Pre-check from the target's confirmed audit config when present; otherwise
		// fall back to the Phase-1 default (first persona checked).
		checked := i == 0
		if len(vm.DefaultPersonas) > 0 {
			checked = vm.DefaultPersonas[p.ID]
		}
		if checked {
			attrs = append(attrs, h.Checked())
		}
		checks = append(checks, h.Label(h.Class("flex items-start gap-2 text-sm"),
			h.Input(attrs...),
			h.Span(h.Span(h.Class("font-medium"), g.Text(p.Label)))))
	}
	return h.Details(h.Class("relative"),
		h.Summary(h.Class("cursor-pointer btn-accent"),
			g.Text(label)),
		h.Form(
			g.Attr("hx-post", "/api/runs/"+vm.RunID+"/evaluate"),
			g.Attr("hx-target", "#eval-section"),
			g.Attr("hx-swap", "outerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("absolute right-0 z-10 mt-2 w-80 space-y-3 rounded-lg border border-line bg-card p-4 shadow-lg"),
			h.Div(
				h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text("Task / job (optional)")),
				h.Input(h.Type("text"), h.Name("job"), h.Value(vm.Job),
					h.Placeholder("e.g. sign up and create a first project"),
					h.Class("w-full rounded border border-line bg-surface px-2 py-1 text-sm")),
			),
			h.Div(
				h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text("Personas (one paid pass per page each)")),
				h.Div(h.Class("space-y-1.5"), g.Group(checks)),
			),
			h.Label(h.Class("flex items-center gap-2 text-xs text-muted"),
				h.Input(h.Type("checkbox"), h.Name("verify"), h.Value("1"), h.Checked(), h.Class("accent-blue-500")),
				h.Span(g.Text("Verify findings (extra call per page — drops unsubstantiated findings)"))),
			h.Button(h.Type("submit"),
				h.Class("w-full btn-accent"),
				g.Text("Run walkthrough")),
		),
	)
}

func evalCostSummary(vm *EvalVM) g.Node {
	if vm.Status == "generating" || vm.CostUSD <= 0 {
		return g.Text("")
	}
	line := "This pass: ~" + formatUSD(vm.CostUSD)
	if vm.CellCount > 0 {
		line += " · " + strconv.Itoa(vm.CellCount) + " evaluation" + plural(vm.CellCount)
	}
	return h.P(h.Class("mt-0.5 text-xs text-muted"), g.Text(line))
}

func evalProgress(vm *EvalVM) g.Node {
	pct := 0
	if vm.Total > 0 {
		pct = vm.Done * 100 / vm.Total
	}
	return h.Div(h.Class("space-y-2"),
		h.P(h.Class("text-sm text-muted"), g.Textf("Walking the flow as each persona — %d of %d done…", vm.Done, vm.Total)),
		h.Div(h.Class("h-2 w-full overflow-hidden rounded bg-surface"),
			h.Div(h.Class("h-full bg-blue-500 transition-all"), h.Style("width:"+strconv.Itoa(pct)+"%")),
		),
	)
}

// evalSynthesis renders the run-level ranked "story" at the top.
func evalSynthesis(items []report.EvalSynthItem) g.Node {
	if len(items) == 0 {
		return nil
	}
	rows := make([]g.Node, 0, len(items))
	for i, it := range items {
		rows = append(rows, h.Li(h.Class("flex items-start gap-3 py-2"),
			h.Span(h.Class("mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-blue-500/15 text-xs font-semibold text-blue-400"), g.Text(strconv.Itoa(i+1))),
			h.Div(h.Class("space-y-0.5"),
				h.Div(h.Class("flex items-center gap-2"),
					h.Span(h.Class("text-sm font-medium"), g.Text(it.Title)),
					impactBadge(it.Impact),
				),
				g.If(it.Rationale != "", h.P(h.Class("text-xs text-muted"), g.Text(it.Rationale))),
				g.If(it.Selector != "", h.Code(h.Class("text-[11px] text-muted break-all"), g.Text(it.Selector))),
			),
		))
	}
	return h.Div(h.Class("rounded border border-blue-500/20 bg-blue-500/5 p-4"),
		h.P(h.Class("mb-1 text-sm font-semibold"), g.Text("Top improvements across the flow")),
		h.Ul(h.Class("divide-y divide-line"), g.Group(rows)),
	)
}

func evalPageBlock(p EvalPageVM) g.Node {
	cols := make([]g.Node, 0, len(p.Cells))
	for _, c := range p.Cells {
		cols = append(cols, evalCell(c))
	}
	return h.Div(h.Class("space-y-2"),
		h.H3(h.Class("font-semibold break-all text-sm"), g.Text(p.URL)),
		h.Div(h.Class("grid gap-4 md:grid-cols-2"), g.Group(cols)),
	)
}

// EvalCell renders one (page, persona) evaluation cell. Model-authored strings are
// rendered via g.Text (escaped).
func EvalCell(c EvalCellVM) g.Node {
	label := h.Div(h.Class("flex items-center gap-2 border-b border-line bg-surface px-3 py-2 text-xs"),
		h.Span(h.Class("font-medium"), g.Text(c.PersonaLabel)),
		comprehensionBadge(c.Comprehension),
		g.If(c.Error != "", h.Span(h.Class("rounded bg-red-500/15 px-1.5 py-0.5 text-red-400"), g.Text("failed"))),
		evalCostBadge(c),
	)

	var inner g.Node
	if c.Error != "" {
		inner = h.Div(h.Class("px-3 py-3 text-xs text-muted"),
			h.P(g.Text("This persona could not be evaluated for this page:")),
			h.P(h.Class("mt-1 break-all text-red-400"), g.Text(truncate(c.Error, 300))),
			h.P(h.Class("mt-1"), g.Text("Use “Re-run” to try again.")),
		)
	} else {
		inner = h.Div(h.Class("space-y-3 p-3"),
			findingList("Blockers", c.Eval.Blockers, "text-red-400"),
			findingList("Frictions", c.Eval.Frictions, "text-amber-400"),
			topFixBlock(c.Eval.TopFix),
			g.If(len(c.Eval.Blockers) == 0 && len(c.Eval.Frictions) == 0 && c.Eval.TopFix == nil,
				h.P(h.Class("text-xs text-muted"), g.Text("No blockers or frictions found for this persona."))),
		)
	}

	return h.Div(g.Attr("data-eval-cell", "1"), h.Class("rounded border border-line overflow-hidden"),
		label, inner)
}

func evalCell(c EvalCellVM) g.Node { return EvalCell(c) }

// findingList renders a labeled list of blockers/frictions with escaped
// model-authored issue/selector/evidence text.
func findingList(title string, findings []report.EvalFinding, tone string) g.Node {
	if len(findings) == 0 {
		return g.Text("")
	}
	items := make([]g.Node, 0, len(findings))
	for _, f := range findings {
		var meta []g.Node
		if f.Selector != "" {
			meta = append(meta, h.Code(h.Class("text-[11px] text-muted break-all"), g.Text(f.Selector)))
		}
		if f.Evidence != "" {
			meta = append(meta, h.Span(h.Class("text-[11px] text-muted italic"), g.Text("“"+f.Evidence+"”")))
		}
		items = append(items, h.Li(h.Class("space-y-0.5"),
			h.Div(h.Class("flex items-start gap-1.5"),
				g.If(!f.Verified, h.Span(h.Class("mt-0.5 rounded bg-surface px-1 text-[10px] text-muted"), h.Title("not run through the verification pass"), g.Text("unverified"))),
				h.Span(h.Class("text-xs"), g.Text(f.Issue)),
			),
			g.If(len(meta) > 0, h.Div(h.Class("flex flex-col gap-0.5 pl-1"), g.Group(meta))),
		))
	}
	return h.Div(
		h.P(h.Class("mb-1 text-xs font-semibold "+tone), g.Textf("%s (%d)", title, len(findings))),
		h.Ul(h.Class("space-y-1.5"), g.Group(items)),
	)
}

func topFixBlock(t *report.EvalTopFix) g.Node {
	if t == nil {
		return g.Text("")
	}
	return h.Div(h.Class("rounded bg-surface px-2.5 py-2"),
		h.Div(h.Class("mb-0.5 flex items-center gap-2"),
			h.Span(h.Class("text-xs font-semibold text-ink"), g.Text("Top fix")),
			impactBadge(t.Impact),
		),
		h.P(h.Class("text-xs"), g.Text(t.Change)),
		g.If(t.Rationale != "", h.P(h.Class("mt-0.5 text-[11px] text-muted"), g.Text(t.Rationale))),
		g.If(t.Selector != "", h.Code(h.Class("mt-0.5 block text-[11px] text-muted break-all"), g.Text(t.Selector))),
	)
}

func comprehensionBadge(c string) g.Node {
	if c == "" {
		return g.Text("")
	}
	tone := "bg-surface text-muted"
	switch c {
	case "clear":
		tone = "bg-emerald-500/15 text-emerald-400"
	case "unclear":
		tone = "bg-amber-500/15 text-amber-400"
	case "blocked":
		tone = "bg-red-500/15 text-red-400"
	}
	return h.Span(h.Class("rounded px-1.5 py-0.5 font-medium "+tone), g.Text(c))
}

func impactBadge(impact string) g.Node {
	if impact == "" {
		return g.Text("")
	}
	tone := "bg-surface text-muted"
	switch impact {
	case "high":
		tone = "bg-red-500/15 text-red-400"
	case "medium":
		tone = "bg-amber-500/15 text-amber-400"
	case "low":
		tone = "bg-surface text-muted"
	}
	return h.Span(h.Class("rounded px-1.5 py-0.5 text-[10px] font-medium "+tone), g.Text(impact+" impact"))
}

func evalCostBadge(c EvalCellVM) g.Node {
	if c.CostUSD <= 0 {
		return g.Text("")
	}
	txt := formatUSD(c.CostUSD)
	if c.Tokens > 0 {
		txt += " · " + formatTokens(c.Tokens) + " tok"
	}
	return h.Span(h.Class("ml-auto rounded bg-surface px-1.5 py-0.5 text-muted"), g.Text(txt))
}
