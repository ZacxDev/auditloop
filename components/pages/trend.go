package pages

import (
	"fmt"
	"strings"

	"github.com/ZacxDev/auditloop/components/partials"
	"github.com/ZacxDev/auditloop/internal/db"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// trendSeries describes one plottable finding type: how to pull its per-run
// count, its display label, its SVG stroke color, and the Tailwind text class
// used for the legend swatch/label (fixed hexes read on the app's dark surface).
type trendSeries struct {
	label    string
	stroke   string // SVG stroke hex
	swatchBg string // Tailwind bg class for the legend dot (literal so Tailwind scans it)
	get      func(db.TrendPoint) int
}

// trendSeriesDefs is the ordered, fixed set of series. a11y is the primary debt
// signal and is ALWAYS drawn; the others are drawn only when they carry a
// nonzero value somewhere in the window (avoids a clutter of flat-zero lines).
var trendSeriesDefs = []trendSeries{
	{"a11y", "#818cf8", "bg-indigo-400", func(p db.TrendPoint) int { return p.A11y }},
	{"layout", "#fbbf24", "bg-amber-400", func(p db.TrendPoint) int { return p.Layout }},
	{"console", "#fb7185", "bg-rose-400", func(p db.TrendPoint) int { return p.Console }},
	{"network", "#22d3ee", "bg-cyan-400", func(p db.TrendPoint) int { return p.Network }},
	{"perf", "#34d399", "bg-emerald-400", func(p db.TrendPoint) int { return p.Perf }},
}

// SVG geometry (viewBox units; the element itself is width:100% so it scales).
const (
	trendVBWidth  = 320
	trendVBHeight = 72
	trendPad      = 6
)

// FindingTrend renders the per-target findings-count-over-time card: a compact
// multi-line sparkline SVG (one line per finding type) plus a text summary of
// each series' latest value and its delta vs the first run. It is nil-safe and
// deterministic (no time.Now/random in the render):
//
//   - 0 completed runs  → nothing (the caller's run list already covers it).
//   - 1 completed run   → a subtle "trend appears after 2+ runs" note, no chart.
//   - 2+ completed runs → the sparkline + summary.
func FindingTrend(points []db.TrendPoint) g.Node {
	if len(points) == 0 {
		return g.Text("")
	}
	if len(points) < 2 {
		return partials.Card(
			trendHeading(),
			h.P(h.Class("text-sm text-muted"),
				g.Text("A trend appears once this target has 2 or more completed runs.")),
		)
	}

	// Which series to draw: a11y always, others only if nonzero somewhere.
	var active []trendSeries
	for _, s := range trendSeriesDefs {
		if s.label == "a11y" || seriesMax(points, s) > 0 {
			active = append(active, s)
		}
	}

	// Shared y-scale across all drawn series so lines are comparable.
	yMax := 1
	for _, s := range active {
		if m := seriesMax(points, s); m > yMax {
			yMax = m
		}
	}

	return partials.Card(
		trendHeading(),
		trendSVG(points, active, yMax),
		h.Div(h.Class("mt-3 flex flex-wrap gap-x-5 gap-y-1"),
			g.Group(trendSummaries(points, active)),
		),
	)
}

func trendHeading() g.Node {
	return h.H2(h.Class("mb-3 section-title"),
		g.Text("Findings trend"))
}

// trendSVG builds the inline multi-line sparkline (pure gomponents SVG elements,
// no JS, no external chart lib). x is evenly spaced by run index (oldest→newest,
// left→right); y is the shared linear scale.
func trendSVG(points []db.TrendPoint, active []trendSeries, yMax int) g.Node {
	n := len(points)
	x := func(i int) float64 {
		if n == 1 {
			return trendPad
		}
		return trendPad + float64(i)*float64(trendVBWidth-2*trendPad)/float64(n-1)
	}
	y := func(v int) float64 {
		// Higher count → higher on screen (smaller y). Leave a little headroom.
		frac := float64(v) / float64(yMax)
		return trendVBHeight - trendPad - frac*float64(trendVBHeight-2*trendPad)
	}

	var lines []g.Node
	for _, s := range active {
		var pts strings.Builder
		for i, p := range points {
			if i > 0 {
				pts.WriteByte(' ')
			}
			fmt.Fprintf(&pts, "%.1f,%.1f", x(i), y(s.get(p)))
		}
		lines = append(lines,
			g.El("polyline",
				g.Attr("points", pts.String()),
				g.Attr("fill", "none"),
				g.Attr("stroke", s.stroke),
				g.Attr("stroke-width", "1.5"),
				g.Attr("stroke-linejoin", "round"),
				g.Attr("stroke-linecap", "round"),
			),
			// End dot on the latest value.
			g.El("circle",
				g.Attr("cx", fmt.Sprintf("%.1f", x(n-1))),
				g.Attr("cy", fmt.Sprintf("%.1f", y(s.get(points[n-1])))),
				g.Attr("r", "2"),
				g.Attr("fill", s.stroke),
			),
		)
	}

	return g.El("svg",
		g.Attr("viewBox", fmt.Sprintf("0 0 %d %d", trendVBWidth, trendVBHeight)),
		g.Attr("preserveAspectRatio", "none"),
		g.Attr("role", "img"),
		g.Attr("aria-label", "Findings count over the target's completed runs"),
		h.Class("w-full h-16"),
		g.Group(lines),
	)
}

// trendSummaries renders one legend/summary line per drawn series:
// "● a11y 12 ▲3 over 5 runs". The delta arrow is colored by direction — up is
// worse (debt rising, red), down is better (green), flat is muted.
func trendSummaries(points []db.TrendPoint, active []trendSeries) []g.Node {
	n := len(points)
	var out []g.Node
	for _, s := range active {
		first := s.get(points[0])
		last := s.get(points[n-1])
		delta := last - first

		arrow, deltaCls := "±0", "text-muted"
		switch {
		case delta > 0:
			arrow, deltaCls = fmt.Sprintf("▲%d", delta), "text-red-400"
		case delta < 0:
			arrow, deltaCls = fmt.Sprintf("▼%d", -delta), "text-emerald-400"
		}

		out = append(out, h.Span(h.Class("inline-flex items-center gap-1.5 text-xs"),
			// Colored swatch (matches the SVG line color).
			h.Span(h.Class("inline-block h-2 w-2 rounded-full "+s.swatchBg)),
			h.Span(h.Class("text-muted"), g.Text(s.label)),
			h.Span(h.Class("font-semibold text-ink"), g.Textf("%d", last)),
			h.Span(h.Class(deltaCls), g.Text(arrow)),
			h.Span(h.Class("text-muted"), g.Textf("over %d runs", n)),
		))
	}
	return out
}

func seriesMax(points []db.TrendPoint, s trendSeries) int {
	m := 0
	for _, p := range points {
		if v := s.get(p); v > m {
			m = v
		}
	}
	return m
}
