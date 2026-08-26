package pages

import (
	"strconv"
	"strings"

	"github.com/ZacxDev/auditloop/components/partials"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// NotesVM is the P3 "AI UX notes" view model for a run. Enabled is false when no
// OpenRouter key is configured (the whole section then renders nothing). Status
// mirrors runs.notes_status (idle|generating|done|failed); Pages carries the
// per-URL, per-model drafts (empty until a pass has run).
type NotesVM struct {
	RunID   string
	Enabled bool
	Status  string
	Done    int
	Total   int
	Models  []string // curated allowlist for the picker (first is default-checked)
	Pages   []NotePageVM
	// P3 cost accounting for the latest pass (0 for pre-cost runs → no cost line).
	CostUSD    float64
	DraftCount int           // number of successful (cost-bearing) drafts this pass
	ByModel    []ModelCostVM // per-model breakdown (stable order)
}

// ModelCostVM is one model's summed cost across the pass.
type ModelCostVM struct {
	Model   string
	CostUSD float64
}

// NotePageVM groups a logical page's per-model notes.
type NotePageVM struct {
	URL   string
	Notes []PageNoteVM
}

// PageNoteVM is one (page, model) note cell.
type PageNoteVM struct {
	PageID string
	Model  string
	Notes  string
	Edited bool
	Error  string
	// P3 per-cell cost (USD) + total tokens; 0 for pre-cost/legacy or failed drafts
	// (no badge rendered).
	CostUSD float64
	Tokens  int
}

// NotesSection renders the whole opt-in UX-notes block as a single swappable
// fragment (id="notes-section"). It polls itself every 3s while a pass is
// generating. Returns an empty node when the feature is disabled.
func NotesSection(vm *NotesVM) g.Node {
	if vm == nil || !vm.Enabled {
		return h.Div(h.ID("notes-section"))
	}
	// No animate-fade here: this fragment self-swaps every 3s while generating
	// (hx-swap outerHTML), so a fade would re-fire on each poll = a blink.
	attrs := []g.Node{h.ID("notes-section"), h.Class("block")}
	if vm.Status == "generating" {
		attrs = append(attrs,
			g.Attr("hx-get", "/runs/"+vm.RunID+"/notes-status"),
			g.Attr("hx-trigger", "load delay:3s"),
			g.Attr("hx-swap", "outerHTML"),
		)
	}

	header := h.Div(h.Class("mb-3 flex items-center justify-between gap-3"),
		h.Div(
			h.H2(h.Class("text-lg font-semibold"), g.Text("AI UX notes")),
			notesCostSummary(vm),
		),
		notesControls(vm),
	)

	var body g.Node
	switch {
	case vm.Status == "generating":
		body = notesProgress(vm)
	case len(vm.Pages) == 0:
		note := "No UX notes yet — use “Draft UX notes” to generate a first pass."
		if vm.Status == "failed" {
			note = "The last notes pass failed. Try “Draft UX notes” again."
		}
		body = h.P(h.Class("text-sm text-muted"), g.Text(note))
	default:
		blocks := make([]g.Node, 0, len(vm.Pages))
		for _, p := range vm.Pages {
			blocks = append(blocks, notePageBlock(p))
		}
		body = h.Div(h.Class("space-y-6"), g.Group(blocks))
	}

	return h.Div(append(attrs, partials.Card(header, body))...)
}

// notesControls renders the picker/regenerate affordance (hidden while generating).
func notesControls(vm *NotesVM) g.Node {
	if vm.Status == "generating" {
		return g.Text("")
	}
	label := "Draft UX notes"
	if len(vm.Pages) > 0 {
		label = "Regenerate"
	}
	checks := make([]g.Node, 0, len(vm.Models))
	for i, m := range vm.Models {
		attrs := []g.Node{h.Type("checkbox"), h.Name("models"), h.Value(m), h.Class("accent-blue-500")}
		if i == 0 {
			attrs = append(attrs, h.Checked())
		}
		checks = append(checks, h.Label(h.Class("flex items-center gap-2 text-sm"),
			h.Input(attrs...), h.Span(g.Text(m))))
	}
	return h.Details(h.Class("relative"),
		h.Summary(h.Class("cursor-pointer btn-accent"),
			g.Text(label)),
		h.Form(
			g.Attr("hx-post", "/api/runs/"+vm.RunID+"/notes"),
			g.Attr("hx-target", "#notes-section"),
			g.Attr("hx-swap", "outerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("absolute right-0 z-10 mt-2 w-72 space-y-3 rounded-lg border border-line bg-card p-4 shadow-lg"),
			h.P(h.Class("text-xs font-semibold text-muted"), g.Text("Models (each is a paid per-page call)")),
			h.Div(h.Class("space-y-1.5"), g.Group(checks)),
			h.Button(h.Type("submit"),
				h.Class("w-full btn-accent"),
				g.Text("Draft notes")),
		),
	)
}

// notesCostSummary renders the per-run total ("This pass: ~$0.0123 · 8 drafts")
// plus a per-model breakdown, when the latest pass reported any cost. Renders
// nothing while generating or when no cost is recorded (pre-cost runs).
func notesCostSummary(vm *NotesVM) g.Node {
	if vm.Status == "generating" || vm.CostUSD <= 0 {
		return g.Text("")
	}
	line := "This pass: ~" + formatUSD(vm.CostUSD)
	if vm.DraftCount > 0 {
		line += " · " + strconv.Itoa(vm.DraftCount) + " draft" + plural(vm.DraftCount)
	}
	kids := []g.Node{h.Span(g.Text(line))}
	if len(vm.ByModel) > 0 {
		parts := make([]string, 0, len(vm.ByModel))
		for _, mc := range vm.ByModel {
			parts = append(parts, shortModel(mc.Model)+" "+formatUSD(mc.CostUSD))
		}
		kids = append(kids, h.Span(h.Class("text-muted/70"), g.Text(" — "+strings.Join(parts, " · "))))
	}
	return h.P(h.Class("mt-0.5 text-xs text-muted"), g.Group(kids))
}

func notesProgress(vm *NotesVM) g.Node {
	pct := 0
	if vm.Total > 0 {
		pct = vm.Done * 100 / vm.Total
	}
	return h.Div(h.Class("space-y-2"),
		h.P(h.Class("text-sm text-muted"), g.Textf("Drafting UX notes — %d of %d done…", vm.Done, vm.Total)),
		h.Div(h.Class("h-2 w-full overflow-hidden rounded bg-surface"),
			h.Div(h.Class("h-full bg-blue-500 transition-all"), h.Style("width:"+strconv.Itoa(pct)+"%")),
		),
	)
}

func notePageBlock(p NotePageVM) g.Node {
	cols := make([]g.Node, 0, len(p.Notes))
	for _, n := range p.Notes {
		cols = append(cols, noteCell(n))
	}
	return h.Div(h.Class("space-y-2"),
		h.H3(h.Class("font-semibold break-all text-sm"), g.Text(p.URL)),
		h.Div(h.Class("grid gap-4 md:grid-cols-2"), g.Group(cols)),
	)
}

// NoteCell renders one editable (page, model) note. It is also returned bare from
// the save route to swap just this cell.
func NoteCell(n PageNoteVM) g.Node {
	label := h.Div(h.Class("flex items-center gap-2 border-b border-line bg-surface px-3 py-2 text-xs"),
		h.Span(h.Class("font-medium"), g.Text(n.Model)),
		g.If(n.Edited, h.Span(h.Class("rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-400"), g.Text("edited"))),
		g.If(n.Error != "", h.Span(h.Class("rounded bg-red-500/15 px-1.5 py-0.5 text-red-400"), g.Text("draft failed"))),
		noteCostBadge(n),
	)

	var inner g.Node
	if n.Error != "" && n.Notes == "" {
		inner = h.Div(h.Class("px-3 py-3 text-xs text-muted"),
			h.P(g.Text("This model could not draft notes for this page:")),
			h.P(h.Class("mt-1 break-all text-red-400"), g.Text(truncate(n.Error, 300))),
			h.P(h.Class("mt-1"), g.Text("Use “Regenerate” to try again.")),
		)
	} else {
		inner = h.Form(
			g.Attr("hx-post", "/api/pages/"+n.PageID+"/notes/"+n.Model),
			g.Attr("hx-target", "closest [data-note-cell]"),
			g.Attr("hx-swap", "outerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("p-3"),
			h.Textarea(h.Name("notes"), h.Class("h-56 w-full resize-y rounded border border-line bg-surface p-2 font-mono text-xs"),
				g.Text(n.Notes)),
			h.Div(h.Class("mt-2 flex justify-end"),
				h.Button(h.Type("submit"),
					h.Class("rounded bg-surface px-3 py-1 text-xs font-medium text-ink hover:bg-line"),
					g.Text("Save")),
			),
		)
	}

	return h.Div(g.Attr("data-note-cell", "1"), h.Class("rounded border border-line overflow-hidden"),
		label, inner)
}

func noteCell(n PageNoteVM) g.Node { return NoteCell(n) }

// noteCostBadge renders a subtle "$0.0021 · 1.2k tok" badge for a cell, only when a
// cost was recorded (>0). Pre-cost/failed cells render nothing. Text is escaped by
// gomponents.
func noteCostBadge(n PageNoteVM) g.Node {
	if n.CostUSD <= 0 {
		return g.Text("")
	}
	txt := formatUSD(n.CostUSD)
	if n.Tokens > 0 {
		txt += " · " + formatTokens(n.Tokens) + " tok"
	}
	return h.Span(h.Class("ml-auto rounded bg-surface px-1.5 py-0.5 text-muted"), g.Text(txt))
}

// formatUSD renders a small dollar amount with enough precision that sub-cent
// costs don't collapse to "$0.00" (≥4 decimals; more for very small values).
func formatUSD(v float64) string {
	switch {
	case v <= 0:
		return "$0"
	case v >= 1:
		return "$" + strconv.FormatFloat(v, 'f', 2, 64)
	case v >= 0.0001:
		return "$" + strconv.FormatFloat(v, 'f', 4, 64)
	default:
		return "$" + strconv.FormatFloat(v, 'f', 6, 64)
	}
}

// formatTokens renders a token count compactly (1200 → "1.2k").
func formatTokens(n int) string {
	if n >= 1000 {
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	}
	return strconv.Itoa(n)
}

// shortModel trims a "provider/model" id to its model segment for the breakdown.
func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 && i < len(m)-1 {
		return m[i+1:]
	}
	return m
}
