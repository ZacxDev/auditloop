// Package partials holds reusable view fragments (also returned bare for htmx
// swaps).
package partials

import (
	"strings"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// Card is a bordered surface block.
func Card(children ...g.Node) g.Node {
	return h.Div(append([]g.Node{h.Class("rounded-lg border border-line bg-card p-5")}, children...)...)
}

// StatusPill renders a colored run-status badge.
func StatusPill(status string) g.Node {
	cls := "bg-slate-500/15 text-slate-300"
	switch status {
	case "queued":
		cls = "bg-amber-500/15 text-amber-400"
	case "running":
		cls = "bg-blue-500/15 text-blue-400"
	case "done":
		cls = "bg-emerald-500/15 text-emerald-400"
	case "failed":
		cls = "bg-red-500/15 text-red-400"
	}
	return h.Span(h.Class("badge "+cls),
		g.Text(strings.ToUpper(status[:1])+status[1:]))
}

// InfoIcon renders a small, progressive-enhancement info affordance: a superscript
// "ⓘ" carrying a native tooltip (title) plus an accessible name (aria-label), so a
// jargon term is defined inline for both mouse and assistive-tech users without any
// JavaScript. Renders nothing when there is no definition.
func InfoIcon(def string) g.Node {
	if def == "" {
		return g.Text("")
	}
	return h.Span(
		h.Class("ml-1 cursor-help text-[11px] leading-none text-muted hover:text-ink"),
		h.Title(def), g.Attr("aria-label", def), g.Attr("role", "img"),
		g.Text("ⓘ"),
	)
}

// InfoTerm renders a term immediately followed by its InfoIcon definition — the DRY
// building block for annotating a jargon label wherever it appears.
func InfoTerm(term, def string) g.Node {
	return h.Span(h.Class("inline-flex items-center whitespace-nowrap"),
		g.Text(term), InfoIcon(def))
}

// Count renders a small labeled count chip.
func Count(label string, n int, tone string) g.Node {
	tclass := "text-muted"
	if n > 0 {
		switch tone {
		case "bad":
			tclass = "text-red-400"
		case "warn":
			tclass = "text-amber-400"
		}
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-xs "+tclass),
		h.Span(h.Class("font-semibold"), g.Textf("%d", n)),
		h.Span(h.Class("text-muted"), g.Text(label)),
	)
}
