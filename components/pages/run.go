package pages

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/ZacxDev/auditloop/components"
	"github.com/ZacxDev/auditloop/components/layouts"
	"github.com/ZacxDev/auditloop/components/partials"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// PageVM is one crawled page/viewport with its presigned screenshot URL.
type PageVM struct {
	Page          *db.Page
	ScreenshotURL string
	Findings      []*db.Finding
}

// PageGroup groups a URL's viewports together.
type PageGroup struct {
	URL       string
	Viewports []PageVM
}

// DiffVM is the P2 "changes since the previous run" view model. PrevRunID is set
// whenever the run has a baseline; Diff is the computed regression summary (nil
// when this is the first run for the target). ChangedShots carries the
// presigned diff-image URLs (resolved by the handler, worst-first).
type DiffVM struct {
	PrevRunID    string
	Diff         *report.Diff
	ChangedShots []ChangedShotVM
}

// ChangedShotVM is one changed page+viewport with its diff image URL. Regression
// is true only for a trustworthy same-size visual regression; SizeChanged marks a
// layout/height change (incomparable pixel diff); NotCompared marks a capture too
// large to visualize.
type ChangedShotVM struct {
	URL         string
	Viewport    string
	DiffPct     float64
	DiffURL     string
	SizeChanged bool
	NotCompared bool
	Regression  bool
}

// RunView is the full run page. While the run is queued/running it polls the
// status fragment every 3s.
func RunView(ctx components.PageContext, run *db.Run, t *db.Target, groups []PageGroup, diff *DiffVM, notes *NotesVM, evalVM *EvalVM) g.Node {
	return layouts.App(ctx,
		h.Div(h.Class("mb-4"),
			h.A(h.Href("/targets/"+run.TargetID), h.Class("text-sm text-muted hover:text-ink"), g.Text("← "+t.Name)),
		),
		h.Div(h.ID("run-body"), RunBody(run, t, groups, diff, notes, evalVM)),
	)
}

// RunBody is the swappable run content (page/poll share it). For a completed run it
// reads as a professional REPORT — a report header (identity + status), an executive
// summary (verdict), the P2 "changes since" section, then per-page findings as
// collapsible cards (detail behind interaction), and finally the deeper-analysis
// sections (persona walkthrough + AI UX notes). Queued/running show the live pending
// state; failed shows the error.
func RunBody(run *db.Run, t *db.Target, groups []PageGroup, diff *DiffVM, notes *NotesVM, evalVM *EvalVM) g.Node {
	polling := run.Status == db.RunQueued || run.Status == db.RunRunning
	attrs := []g.Node{h.ID("run-body")}
	if polling {
		attrs = append(attrs,
			g.Attr("hx-get", "/runs/"+run.ID+"/status"),
			g.Attr("hx-trigger", "load delay:3s"),
			g.Attr("hx-swap", "outerHTML"),
		)
	}

	var body g.Node
	switch run.Status {
	case db.RunQueued:
		// No entry animation here: this region self-swaps every 3s (hx-swap
		// outerHTML), so any fade/rise would re-fire on every poll = a blink.
		body = h.Div(h.Class("mt-6"),
			pendingNotice("Queued", "Waiting for a worker to pick up this audit. This page updates automatically — no need to refresh."))
	case db.RunRunning:
		body = h.Div(h.Class("mt-6"),
			pendingNotice("Auditing your site", "Capturing screenshots, checking accessibility, and recording errors across each page and viewport. This page updates automatically as it goes."))
	case db.RunFailed:
		body = h.Div(h.Class("mt-6 rounded-lg border border-danger/30 bg-danger/10 p-4 text-sm text-danger-fg"),
			h.P(h.Class("font-medium"), g.Text("Run failed")),
			g.If(run.Error != "", h.P(h.Class("mt-1"), g.Text(run.Error))),
		)
	default:
		body = h.Div(h.Class("mt-6 space-y-8 motion-safe:animate-enter"),
			runSummary(groups, evalVM),
			labEnvNote(run),
			changesSection(diff),
			pageReportSection(groups),
			analysisSection(evalVM, notes),
		)
	}
	return h.Div(append(attrs, runHeader(run, t), body)...)
}

// runHeader is the report identity line, shown in every state: an "Audit report"
// eyebrow, the target name as the title, the status pill + date, and the base URL.
// t may be nil on a degraded poll fragment — it falls back to a generic label.
func runHeader(run *db.Run, t *db.Target) g.Node {
	name := "Audit run"
	sub := ""
	if t != nil {
		if t.Name != "" {
			name = t.Name
		}
		sub = t.BaseURL
	}
	return h.Div(h.Class("space-y-1"),
		h.P(h.Class("section-title"), g.Text("Audit report")),
		h.Div(h.Class("flex flex-wrap items-center gap-3"),
			h.H1(h.Class("text-xl font-bold text-ink"), g.Text(name)),
			partials.StatusPill(run.Status),
			h.Span(h.Class("text-sm text-muted"), g.Text(run.CreatedAt.Format("Jan 2, 2006 15:04"))),
		),
		g.If(sub != "", h.P(h.Class("break-all text-sm text-muted"), g.Text(sub))),
	)
}

// changesSection renders the P2 "Changes since <prev run>" block. It shows a
// subtle first-run note when there is no baseline, and nothing when the diff is
// unavailable.
func changesSection(diff *DiffVM) g.Node {
	if diff == nil || diff.Diff == nil {
		if diff != nil && diff.PrevRunID == "" {
			return h.Div(h.Class("rounded border border-line bg-card px-4 py-3 text-sm text-muted"),
				g.Text("This is the first audit for this target — the findings below are your starting point. Run it again later and this space will highlight what changed since."))
		}
		return g.Text("")
	}
	d := diff.Diff
	var body []g.Node

	// Page-set delta.
	if len(d.PagesAdded) > 0 {
		body = append(body, pageList("New pages", d.PagesAdded, "text-success-fg"))
	}
	if len(d.PagesRemoved) > 0 {
		body = append(body, pageList("Removed pages", d.PagesRemoved, "text-warning-fg"))
	}

	// Signal deltas.
	body = append(body, h.Div(h.Class("flex flex-wrap gap-4"),
		deltaChip("a11y violations", d.A11yDelta),
		deltaChip("first-party console", d.ConsoleDelta),
		deltaChip("first-party network", d.NetworkDelta),
	))

	// New a11y rules (regressions).
	if len(d.NewA11yRules) > 0 {
		chips := make([]g.Node, 0, len(d.NewA11yRules))
		for _, rule := range d.NewA11yRules {
			chips = append(chips, h.Span(h.Class("rounded bg-danger/15 px-2 py-0.5 text-xs font-medium text-danger-fg"), g.Text(rule)))
		}
		body = append(body, h.Div(
			h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text("New accessibility violations (regressions)")),
			h.Div(h.Class("flex flex-wrap gap-2"), g.Group(chips)),
		))
	}
	if len(d.ResolvedA11yRules) > 0 {
		chips := make([]g.Node, 0, len(d.ResolvedA11yRules))
		for _, rule := range d.ResolvedA11yRules {
			chips = append(chips, h.Span(h.Class("rounded bg-surface px-2 py-0.5 text-xs text-muted line-through"), g.Text(rule)))
		}
		body = append(body, h.Div(
			h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text("Resolved accessibility violations")),
			h.Div(h.Class("flex flex-wrap gap-2"), g.Group(chips)),
		))
	}

	// Changed captures (worst-first): true visual regressions (with diff images),
	// layout/size changes, and too-large captures each render with their own label.
	if len(diff.ChangedShots) > 0 {
		shots := make([]g.Node, 0, len(diff.ChangedShots))
		for _, s := range diff.ChangedShots {
			shots = append(shots, changedShot(s))
		}
		body = append(body, h.Div(
			h.P(h.Class("mb-2 text-xs font-semibold text-muted"), g.Textf("Visual changes & layout shifts (%d)", len(diff.ChangedShots))),
			h.Div(h.Class("grid gap-4 sm:grid-cols-2"), g.Group(shots)),
		))
	}

	title := "Changes since previous run"
	if !d.PrevRunAt.IsZero() {
		title = "Changes since " + d.PrevRunAt.Format("2006-01-02 15:04")
	}
	return partials.Card(
		h.Div(h.Class("mb-3 flex items-center gap-3"),
			h.H2(h.Class("text-lg font-semibold"), g.Text(title)),
			g.If(d.PagesChanged > 0,
				h.Span(h.Class("badge-danger"),
					g.Textf("%d regression%s", d.PagesChanged, plural(d.PagesChanged))),
			),
			g.If(d.PagesSizeChanged > 0,
				h.Span(h.Class("badge-warning"),
					g.Textf("%d layout change%s", d.PagesSizeChanged, plural(d.PagesSizeChanged))),
			),
		),
		h.Div(h.Class("space-y-4"), g.Group(body)),
	)
}

func pageList(label string, urls []string, tone string) g.Node {
	items := make([]g.Node, 0, len(urls))
	for _, u := range urls {
		items = append(items, h.Li(h.Class("break-all "+tone), g.Text(u)))
	}
	return h.Div(
		h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Textf("%s (%d)", label, len(urls))),
		h.Ul(h.Class("space-y-0.5 text-xs"), g.Group(items)),
	)
}

// deltaChip shows a signed count delta: ▲ (red) for an increase/regression, ▼
// (muted) for a decrease, and a neutral "no change" for zero.
func deltaChip(label string, delta int) g.Node {
	arrow, tone, text := "—", "text-muted", "no change"
	switch {
	case delta > 0:
		arrow, tone, text = "▲", "text-danger-fg", fmt.Sprintf("+%d", delta)
	case delta < 0:
		arrow, tone, text = "▼", "text-muted", fmt.Sprintf("%d", delta)
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-sm"),
		h.Span(h.Class("font-semibold "+tone), g.Text(arrow+" "+text)),
		h.Span(h.Class("text-muted text-xs"), g.Text(label)),
	)
}

func changedShot(s ChangedShotVM) g.Node {
	// Choose the badge by category. A size/layout change gets a SOFT label and no
	// scary ~100% diff badge (the pixel diff is not comparable across heights); a
	// too-large capture is labeled "not compared"; only a true same-size
	// regression shows the red diff-percent badge.
	var badge g.Node
	switch {
	case s.SizeChanged:
		badge = h.Span(h.Class("rounded px-1.5 py-0.5 font-medium bg-warning/15 text-warning-fg"),
			g.Text("Layout changed — page height differs, pixel diff not comparable"))
	case s.NotCompared:
		badge = h.Span(h.Class("rounded px-1.5 py-0.5 font-medium bg-surface text-muted"),
			g.Text("Not compared — capture too large to visualize"))
	case s.Regression:
		badge = h.Span(h.Class("rounded px-1.5 py-0.5 font-semibold bg-danger/15 text-danger-fg"),
			g.Textf("%.1f%% changed", s.DiffPct))
	default:
		badge = h.Span(h.Class("rounded px-1.5 py-0.5 font-semibold bg-warning/15 text-warning-fg"),
			g.Textf("%.1f%% changed", s.DiffPct))
	}
	header := h.Div(h.Class("flex items-center justify-between gap-2 bg-surface px-3 py-2 text-xs"),
		h.Span(h.Class("font-medium"), g.Text(s.Viewport)),
		h.Div(h.Class("flex items-center gap-2"), badge),
	)
	var img g.Node = g.Text("")
	if s.DiffURL != "" {
		img = h.A(h.Href(s.DiffURL), h.Target("_blank"),
			h.Img(h.Src(s.DiffURL), h.Alt("Visual diff of "+s.URL+" at "+s.Viewport),
				h.Class("block w-full max-h-96 object-cover object-top bg-black")),
		)
	}
	return h.Div(h.Class("rounded border border-line overflow-hidden"),
		header,
		img,
		h.Div(h.Class("px-3 py-1.5 text-xs text-muted break-all"), g.Text(s.URL)),
	)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func notice(msg string) g.Node {
	return h.Div(h.Class("rounded border border-line bg-card p-6 text-center text-muted"),
		g.Text(msg))
}

// pendingNotice is the queued/running state: a live activity indicator (spinner) +
// a title + friendly explanation, so an in-progress run reads as "working, live"
// rather than an empty/broken page. The surrounding fragment already self-polls.
func pendingNotice(title, msg string) g.Node {
	return h.Div(h.Class("flex items-start gap-4 rounded border border-line bg-card p-6"),
		h.Div(h.Class("mt-0.5 h-5 w-5 shrink-0 motion-safe:animate-spin rounded-full border-2 border-line border-t-brand-light"),
			g.Attr("role", "status"), g.Attr("aria-label", "In progress")),
		h.Div(h.Class("space-y-1"),
			h.P(h.Class("flex items-center gap-2 font-medium text-ink"),
				g.Text(title),
				h.Span(h.Class("inline-flex items-center gap-1 rounded-full bg-info/15 px-2 py-0.5 text-[10px] font-medium text-info-fg"),
					h.Span(h.Class("motion-safe:animate-live"), g.Text("●")),
					g.Text("Live — auto-updating")),
			),
			h.P(h.Class("text-sm text-muted"), g.Text(msg)),
		),
	)
}

// termDefs is the SINGLE source of the plain-language jargon definitions surfaced as
// inline tooltips throughout the run view (reused across every viewport/page).
var termDefs = map[string]string{
	"a11y":        "Accessibility — how usable the page is for people relying on assistive technology (screen readers, keyboard navigation, etc.). Based on an automated axe-core scan.",
	"perf":        "Performance — how quickly and smoothly the page loads, measured from an automated headless browser.",
	"LCP":         "Largest Contentful Paint — how long until the biggest piece of content appears. Under 2.5s is good.",
	"CLS":         "Cumulative Layout Shift — how much the page visually jumps around while loading. Lower is better (under 0.1 is good).",
	"TBT":         "Total Blocking Time — roughly how long the page was unresponsive to taps/clicks while loading. A lab estimate (headless, no real-user input), under 200ms is good.",
	"weight":      "Page weight — the total size of everything the page downloaded (HTML, images, scripts, fonts).",
	"reqs":        "Requests — how many separate files the page fetched to load.",
	"first-party": "Errors coming from your own site's code — the ones you can fix.",
	"third-party": "Errors coming from embedded third-party services (analytics, ads, CDNs) — usually outside your control.",
}

// severityRank maps a finding severity label to a comparable weight (higher = worse)
// so the summary can surface the most serious issues first.
func severityRank(sev string) int {
	switch sev {
	case "critical":
		return 4
	case "serious":
		return 3
	case "moderate":
		return 2
	case "minor":
		return 1
	default:
		return 0
	}
}

// summaryIssue is one deduplicated issue rolled up for the plain-language summary: a
// finding rule that may recur across pages/viewports, tracked by its worst severity
// and the set of pages it appears on.
type summaryIssue struct {
	typ   string
	rank  int
	sev   string
	label string
	pages map[string]bool
}

// runSummary renders the plain-language "what was audited / what to look at" box at
// the top of a completed run. It aggregates the ALREADY-computed per-page findings
// (a11y / layout / perf) into a severity rollup + a short top-issues list + a clear
// next step. Nil-safe: renders nothing when there are no captured pages (the
// per-page results section shows its own empty state).
func runSummary(groups []PageGroup, evalVM *EvalVM) g.Node {
	if len(groups) == 0 {
		return g.Text("")
	}

	viewports := map[string]bool{}
	firstPartyErrors := 0
	worstPerf := "good"
	issues := map[string]*summaryIssue{} // key: type|ruleKey

	addIssue := func(typ, key, sev, label string) {
		if label == "" {
			return
		}
		k := typ + "|" + key
		it, ok := issues[k]
		if !ok {
			it = &summaryIssue{typ: typ, label: label, pages: map[string]bool{}}
			issues[k] = it
		}
		if r := severityRank(sev); r > it.rank {
			it.rank, it.sev = r, sev
		}
	}

	for _, grp := range groups {
		grpErrors := 0
		for _, vm := range grp.Viewports {
			p := vm.Page
			if p.Viewport != "" {
				viewports[p.Viewport] = true
			}
			// First-party console+network errors are stored per viewport-row; the
			// SAME logical error fires on both the mobile AND desktop capture, so
			// take the per-URL MAX across viewports (matching how the finding
			// rollups below dedupe per URL) rather than summing — otherwise a
			// 2-viewport run doubles the count shown in the summary chip.
			if e := p.ConsoleFirstPartyCount + p.NetworkFirstPartyCount; e > grpErrors {
				grpErrors = e
			}
			if r := worstPerfRating(p); ratingWorse(r, worstPerf) {
				worstPerf = r
			}
			for _, f := range vm.Findings {
				switch f.Type {
				case db.FindingA11y:
					key, label := a11yFriendly(f.Detail)
					addIssue(db.FindingA11y, key, f.Severity, label)
					if it := issues[db.FindingA11y+"|"+key]; it != nil {
						it.pages[grp.URL] = true
					}
				case db.FindingLayout:
					key, label := layoutFriendly(f.Detail)
					addIssue(db.FindingLayout, key, f.Severity, label)
					if it := issues[db.FindingLayout+"|"+key]; it != nil {
						it.pages[grp.URL] = true
					}
				case db.FindingPerf:
					key, label := perfFriendly(f.Detail)
					addIssue(db.FindingPerf, key, f.Severity, label)
					if it := issues[db.FindingPerf+"|"+key]; it != nil {
						it.pages[grp.URL] = true
					}
				}
			}
		}
		firstPartyErrors += grpErrors
	}

	a11yTotal, a11ySerious, layoutTotal := 0, 0, 0
	for _, it := range issues {
		switch it.typ {
		case db.FindingA11y:
			a11yTotal++
			if it.rank >= 3 {
				a11ySerious++
			}
		case db.FindingLayout:
			layoutTotal++
		}
	}

	// Headline: what was audited.
	pageWord := "page"
	if len(groups) != 1 {
		pageWord = "pages"
	}
	vpWord := "viewport"
	if len(viewports) != 1 {
		vpWord = "viewports"
	}
	headline := fmt.Sprintf("This run audited %d %s at %d %s.", len(groups), pageWord, len(viewports), vpWord)

	clean := a11yTotal == 0 && layoutTotal == 0 && firstPartyErrors == 0 && worstPerf == "good"

	// Severity rollup chips.
	rollup := h.Div(h.Class("mt-3 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-sm"),
		a11yRollupChip(a11yTotal, a11ySerious),
		layoutRollupChip(layoutTotal),
		errorsRollupChip(firstPartyErrors),
		perfRollupChip(worstPerf),
	)

	children := []g.Node{
		h.P(h.Class("text-base font-medium text-ink"), g.Text(headline)),
		rollup,
	}

	// Top issues (worst-first, capped).
	if top := topIssuesList(issues); top != nil {
		children = append(children, top)
	} else if clean {
		children = append(children, h.P(h.Class("mt-3 text-sm text-success-fg"),
			g.Text("✓ No accessibility, layout, performance, or first-party error issues to review.")))
	}

	// Next step.
	next := "Next: expand a page below to see its full screenshots, performance, and findings."
	if evalVM != nil && evalVM.Enabled {
		next += " You can also draft AI UX notes or run a persona walkthrough for a deeper review."
	}
	children = append(children,
		h.P(h.Class("mt-3 border-t border-line pt-3 text-sm text-muted"), g.Text(next)))

	return partials.Card(children...)
}

// a11yRollupChip renders the accessibility rollup with the serious/critical count
// highlighted.
func a11yRollupChip(total, serious int) g.Node {
	if total == 0 {
		return h.Span(h.Class("inline-flex items-center gap-1 text-success-fg"),
			g.Text("✓ no accessibility issues"), partials.InfoIcon(termDefs["a11y"]))
	}
	tone := "text-warning-fg"
	if serious > 0 {
		tone = "text-danger-fg"
	}
	txt := fmt.Sprintf("⚠ %d accessibility issue%s", total, plural(total))
	if serious > 0 {
		txt += fmt.Sprintf(" (%d serious)", serious)
	}
	return h.Span(h.Class("inline-flex items-center gap-1 "+tone),
		g.Text(txt), partials.InfoIcon(termDefs["a11y"]))
}

func layoutRollupChip(total int) g.Node {
	if total == 0 {
		return g.Text("")
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-brand-light"),
		g.Text(fmt.Sprintf("• %d layout issue%s", total, plural(total))))
}

func errorsRollupChip(n int) g.Node {
	if n == 0 {
		return h.Span(h.Class("inline-flex items-center gap-1 text-success-fg"),
			g.Text("✓ no first-party errors"), partials.InfoIcon(termDefs["first-party"]))
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-danger-fg"),
		g.Text(fmt.Sprintf("• %d first-party error%s", n, plural(n))), partials.InfoIcon(termDefs["first-party"]))
}

func perfRollupChip(rating string) g.Node {
	tone, label := "text-success-fg", "good"
	switch rating {
	case "needs-improvement":
		tone, label = "text-warning-fg", "needs work"
	case "poor":
		tone, label = "text-danger-fg", "poor"
	}
	return h.Span(h.Class("inline-flex items-center gap-1 "+tone),
		g.Text("• performance: "+label), partials.InfoIcon(termDefs["perf"]))
}

// topIssuesList renders up to 5 worst-first plain-language issue lines, each naming
// the affected page(s). Returns nil when there are no findings.
func topIssuesList(issues map[string]*summaryIssue) g.Node {
	if len(issues) == 0 {
		return nil
	}
	list := make([]*summaryIssue, 0, len(issues))
	for _, it := range issues {
		list = append(list, it)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].rank != list[j].rank {
			return list[i].rank > list[j].rank
		}
		return len(list[i].pages) > len(list[j].pages)
	})
	if len(list) > 5 {
		list = list[:5]
	}
	rows := make([]g.Node, 0, len(list))
	for i, it := range list {
		line := it.label
		if pgs := summaryPages(it.pages); pgs != "" {
			line += " (" + pgs + ")"
		}
		rows = append(rows, h.Li(h.Class("flex items-start gap-2 py-0.5"),
			h.Span(h.Class("mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full bg-surface text-[10px] font-semibold text-muted"), g.Text(fmt.Sprintf("%d", i+1))),
			h.Span(h.Class("text-sm"), g.Text(line)),
		))
	}
	return h.Div(h.Class("mt-4"),
		h.P(h.Class("mb-1 text-xs font-semibold uppercase tracking-wide text-muted"), g.Text("Top issues to review")),
		h.Ul(h.Class("space-y-0.5"), g.Group(rows)),
	)
}

// summaryPages renders a compact, sorted list of affected page paths (capped).
func summaryPages(pages map[string]bool) string {
	if len(pages) == 0 {
		return ""
	}
	paths := make([]string, 0, len(pages))
	for u := range pages {
		paths = append(paths, shortPath(u))
	}
	sort.Strings(paths)
	if len(paths) > 3 {
		return strings.Join(paths[:3], ", ") + fmt.Sprintf(" +%d more", len(paths)-3)
	}
	return strings.Join(paths, ", ")
}

// shortPath reduces a full URL to a readable path (host root → "/").
func shortPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		if err == nil && u.Host != "" {
			return "/"
		}
		return raw
	}
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

// a11yFriendly turns a stored axe-violation detail into (ruleKey, plain-language
// label). It prefers the axe "help" text and falls back to a humanized rule id.
func a11yFriendly(detail string) (string, string) {
	var v struct {
		ID   string `json:"id"`
		Help string `json:"help"`
	}
	_ = json.Unmarshal([]byte(detail), &v)
	key := v.ID
	if key == "" {
		key = "a11y"
	}
	if label, ok := a11yLabels[v.ID]; ok {
		return key, label
	}
	if v.Help != "" {
		return key, v.Help
	}
	return key, "Accessibility: " + humanize(v.ID)
}

// a11yLabels maps common axe rule ids to short, plain-language descriptions.
var a11yLabels = map[string]string{
	"color-contrast":     "Text contrast is below the 4.5:1 minimum",
	"label":              "Form fields are missing labels",
	"link-name":          "Links have no descriptive text",
	"button-name":        "Buttons have no accessible label",
	"image-alt":          "Images are missing alt text",
	"region":             "Some content sits outside a page landmark",
	"landmark-one-main":  "The page has no main landmark",
	"html-has-lang":      "The page is missing a language attribute",
	"heading-order":      "Headings are out of order",
	"list":               "List markup is malformed",
	"aria-required-attr": "An ARIA element is missing a required attribute",
}

// layoutFriendly turns a stored layout-smell detail into (smellKey, plain label).
func layoutFriendly(detail string) (string, string) {
	var d struct {
		Smell string `json:"smell"`
	}
	_ = json.Unmarshal([]byte(detail), &d)
	key := d.Smell
	if key == "" {
		key = "layout"
	}
	if label, ok := layoutLabels[d.Smell]; ok {
		return key, label
	}
	return key, "Layout: " + humanize(d.Smell)
}

var layoutLabels = map[string]string{
	"horizontal-overflow":       "Page scrolls sideways on mobile",
	"small-tap-targets":         "Some tap targets are too small for mobile",
	"small-text":                "Some text is too small to read comfortably",
	"missing-viewport-meta":     "Missing mobile viewport tag — the page won't scale on phones",
	"images-without-dimensions": "Images lack width/height, causing layout shift",
}

// perfFriendly turns a stored perf detail into (metricKey, plain label).
func perfFriendly(detail string) (string, string) {
	var d struct {
		Metric string `json:"metric"`
	}
	_ = json.Unmarshal([]byte(detail), &d)
	key := d.Metric
	if key == "" {
		key = "perf"
	}
	if label, ok := perfLabels[d.Metric]; ok {
		return key, label
	}
	return key, "Performance: " + humanize(d.Metric)
}

var perfLabels = map[string]string{
	"LCP":         "Main content is slow to appear (LCP)",
	"CLS":         "Content shifts around while loading (CLS)",
	"TBT":         "Page is slow to respond while loading (TBT)",
	"page-weight": "Page is heavy — large download or many requests",
}

// humanize turns a hyphenated rule id into spaced Title-ish text as a last resort.
func humanize(s string) string {
	if s == "" {
		return "issue"
	}
	return strings.ReplaceAll(s, "-", " ")
}

// worstPerfRating computes the worst rating across a page's captured web-vitals /
// weight metrics ("good" when no perf capture exists).
func worstPerfRating(p *db.Page) string {
	if p.LCPMs == 0 && p.CLS == 0 && p.TBTMs == 0 && p.WeightBytes == 0 && p.ReqCount == 0 {
		return "good"
	}
	worst := "good"
	consider := func(r string) {
		if ratingWorse(r, worst) {
			worst = r
		}
	}
	consider(report.Rating(float64(p.LCPMs), report.LCPGoodMs, report.LCPPoorMs))
	consider(report.Rating(p.CLS, report.CLSGood, report.CLSPoor))
	consider(report.Rating(float64(p.TBTMs), report.TBTGoodMs, report.TBTPoorMs))
	consider(weightRating(p.WeightBytes))
	consider(reqRating(p.ReqCount))
	return worst
}

// ratingWorse reports whether rating a is worse than rating b (good < needs < poor).
func ratingWorse(a, b string) bool {
	order := map[string]int{"good": 0, "needs-improvement": 1, "poor": 2}
	return order[a] > order[b]
}

// pageReportSection renders the per-page findings as a stack of collapsible cards,
// worst-first, so the report reads at a glance and the full detail (both
// screenshots, perf chips, console/network breakdown, findings) is revealed only on
// interaction. Empty state degrades to a plain notice.
func pageReportSection(groups []PageGroup) g.Node {
	if len(groups) == 0 {
		return notice("No pages were captured.")
	}
	// Order worst-first (highest severity, then most findings) so the pages that
	// need attention surface at the top; keep it stable for equal scores.
	sorted := make([]PageGroup, len(groups))
	copy(sorted, groups)
	sort.SliceStable(sorted, func(i, j int) bool {
		return groupSeverityScore(sorted[i]) > groupSeverityScore(sorted[j])
	})
	cards := make([]g.Node, 0, len(sorted))
	for _, grp := range sorted {
		cards = append(cards, pageGroupCard(grp))
	}
	return h.Section(h.Class("space-y-3"),
		h.H2(h.Class("section-title"), g.Text("Pages")),
		h.P(h.Class("text-xs text-muted"), g.Text("Worst-first — expand a page for its full screenshots, performance, and findings.")),
		h.Div(h.Class("space-y-2"), g.Group(cards)),
	)
}

// pageGroupCard is one logical page (URL) as a collapsible <details> card. The
// always-visible summary row carries a lead thumbnail, the URL, and at-a-glance
// severity/perf/error badges; the detail (revealed on click) holds the full
// screenshots + perf chips + console/network breakdown + findings list. Native
// <details>/<summary> gives correct keyboard + screen-reader semantics (matching the
// #31 accordion pattern). A left accent bar keys the card to its worst severity.
func pageGroupCard(grp PageGroup) g.Node {
	// Findings are shared across viewports for the URL; show from the first.
	var findings []*db.Finding
	if len(grp.Viewports) > 0 {
		findings = grp.Viewports[0].Findings
	}
	a11yTotal, a11ySerious, layoutTotal := findingCounts(findings)
	rep := representativePage(grp)
	perf := "good"
	if rep != nil {
		perf = worstPerfRating(rep)
	}
	errs := firstPartyErrorMax(grp)

	shots := make([]g.Node, 0, len(grp.Viewports))
	for _, vm := range grp.Viewports {
		shots = append(shots, viewportShot(vm))
	}

	summary := h.Summary(
		h.Class("accordion-summary flex cursor-pointer select-none items-center gap-4 px-4 py-3 transition-colors hover:bg-card-hover"),
		leadThumb(grp),
		h.Div(h.Class("min-w-0 flex-1 space-y-1.5"),
			h.Span(h.Class("block truncate text-sm font-medium text-ink"), g.Text(shortPath(grp.URL))),
			h.Div(h.Class("flex flex-wrap items-center gap-1.5"),
				g.Group(pageSummaryBadges(a11yTotal, a11ySerious, layoutTotal, perf, errs)),
			),
		),
		chevron(),
	)
	detail := h.Div(h.Class("space-y-4 border-t border-line p-4"),
		h.P(h.Class("break-all text-xs text-muted"), g.Text(grp.URL)),
		h.Div(h.Class("grid gap-4 sm:grid-cols-2"), g.Group(shots)),
		findingsSection(findings),
	)
	return h.Details(
		h.Class("card overflow-hidden p-0 border-l-4 "+severityAccent(a11yTotal, a11ySerious, errs)),
		summary, detail,
	)
}

// groupSeverityScore ranks a page group for worst-first ordering: serious a11y
// dominates, then any a11y, then first-party errors, then layout, then perf.
func groupSeverityScore(grp PageGroup) int {
	var findings []*db.Finding
	if len(grp.Viewports) > 0 {
		findings = grp.Viewports[0].Findings
	}
	a11yTotal, a11ySerious, layoutTotal := findingCounts(findings)
	errs := firstPartyErrorMax(grp)
	perf := "good"
	if rep := representativePage(grp); rep != nil {
		perf = worstPerfRating(rep)
	}
	score := a11ySerious*100000 + a11yTotal*1000 + errs*100 + layoutTotal*10
	switch perf {
	case "poor":
		score += 5
	case "needs-improvement":
		score += 2
	}
	return score
}

// findingCounts rolls up a page's shared findings into (total a11y, serious a11y,
// layout) counts for the summary badges.
func findingCounts(findings []*db.Finding) (a11yTotal, a11ySerious, layoutTotal int) {
	for _, f := range findings {
		switch f.Type {
		case db.FindingA11y:
			a11yTotal++
			if severityRank(f.Severity) >= 3 {
				a11ySerious++
			}
		case db.FindingLayout:
			layoutTotal++
		}
	}
	return
}

// representativePage picks the desktop viewport (preferred) or the first viewport as
// the page's canonical row for perf/lead-thumbnail purposes.
func representativePage(grp PageGroup) *db.Page {
	for _, vm := range grp.Viewports {
		if vm.Page != nil && vm.Page.Viewport == "desktop" {
			return vm.Page
		}
	}
	if len(grp.Viewports) > 0 {
		return grp.Viewports[0].Page
	}
	return nil
}

// firstPartyErrorMax is the per-URL first-party console+network error count, taken as
// the MAX across viewports (the same logical error fires on both captures — matching
// the executive-summary rollup rule).
func firstPartyErrorMax(grp PageGroup) int {
	max := 0
	for _, vm := range grp.Viewports {
		if vm.Page == nil {
			continue
		}
		if e := vm.Page.ConsoleFirstPartyCount + vm.Page.NetworkFirstPartyCount; e > max {
			max = e
		}
	}
	return max
}

// leadThumb renders a small lead screenshot for the summary row (the desktop capture,
// preferred). Falls back to a neutral placeholder when there is no shot.
func leadThumb(grp PageGroup) g.Node {
	url := ""
	for _, vm := range grp.Viewports {
		if vm.Page != nil && vm.Page.Viewport == "desktop" && vm.ScreenshotURL != "" {
			url = vm.ScreenshotURL
			break
		}
	}
	if url == "" {
		for _, vm := range grp.Viewports {
			if vm.ScreenshotURL != "" {
				url = vm.ScreenshotURL
				break
			}
		}
	}
	if url == "" {
		return h.Div(h.Class("hidden h-14 w-20 shrink-0 rounded border border-line bg-surface sm:block"),
			g.Attr("aria-hidden", "true"))
	}
	return h.Img(h.Src(url), h.Alt(""), g.Attr("aria-hidden", "true"), h.Loading("lazy"),
		h.Class("hidden h-14 w-20 shrink-0 rounded border border-line object-cover object-top bg-white sm:block"))
}

// pageSummaryBadges renders the always-visible at-a-glance chips for a page card:
// a11y, layout, perf, and first-party errors. When everything is clean it collapses
// to a single success badge.
func pageSummaryBadges(a11yTotal, a11ySerious, layoutTotal int, perf string, errs int) []g.Node {
	var out []g.Node
	if a11yTotal > 0 {
		cls := "badge-warning"
		if a11ySerious > 0 {
			cls = "badge-danger"
		}
		out = append(out, h.Span(h.Class(cls), g.Textf("%d a11y", a11yTotal)))
	}
	if errs > 0 {
		out = append(out, h.Span(h.Class("badge-danger"), g.Textf("%d error%s", errs, plural(errs))))
	}
	if layoutTotal > 0 {
		out = append(out, h.Span(h.Class("badge bg-brand-light/15 text-brand-light"), g.Textf("%d layout", layoutTotal)))
	}
	if perf != "good" {
		label, cls := "perf: needs work", "badge-warning"
		if perf == "poor" {
			label, cls = "perf: poor", "badge-danger"
		}
		out = append(out, h.Span(h.Class(cls), g.Text(label)))
	}
	if len(out) == 0 {
		out = append(out, h.Span(h.Class("badge-success"), g.Text("✓ clean")))
	}
	return out
}

// severityAccent picks the left-border accent color for a page card by its worst
// signal, so the most-flagged pages read at a glance in the stack.
func severityAccent(a11yTotal, a11ySerious, errs int) string {
	switch {
	case a11ySerious > 0 || errs > 0:
		return "border-l-danger/60"
	case a11yTotal > 0:
		return "border-l-warning/60"
	default:
		return "border-l-success/40"
	}
}

func viewportShot(vm PageVM) g.Node {
	p := vm.Page
	return h.Div(h.Class("rounded border border-line overflow-hidden"),
		h.Div(h.Class("flex items-center justify-between bg-surface px-3 py-2 text-xs"),
			h.Span(h.Class("font-medium"), g.Text(p.Viewport)),
			h.Span(h.Class("text-muted"), g.Textf("%dms", p.LoadMS)),
		),
		g.If(vm.ScreenshotURL != "",
			h.A(h.Href(vm.ScreenshotURL), h.Target("_blank"),
				h.Img(h.Src(vm.ScreenshotURL), h.Alt("Screenshot of "+p.URL+" at "+p.Viewport),
					h.Class("block w-full max-h-96 object-cover object-top bg-white")),
			),
		),
		h.Div(h.Class("flex flex-wrap items-center gap-3 px-3 py-2"),
			partials.Count("a11y", p.AxeViolationCount, "bad"),
			partials.InfoIcon(termDefs["a11y"]),
		),
		perfLine(p),
		h.Div(h.Class("border-t border-line px-3 py-2"),
			h.P(h.Class("mb-1 flex items-center text-xs font-semibold text-muted"),
				g.Text("First-party (your site)"), partials.InfoIcon(termDefs["first-party"])),
			h.Div(h.Class("flex flex-wrap gap-3"),
				partials.Count("console", p.ConsoleFirstPartyCount, "bad"),
				partials.Count("network", p.NetworkFirstPartyCount, "bad"),
			),
			h.P(h.Class("mb-1 mt-2 flex items-center text-xs font-semibold text-muted"),
				g.Text("Third-party"), partials.InfoIcon(termDefs["third-party"])),
			h.Div(h.Class("flex flex-wrap gap-3"),
				partials.Count("console", p.ConsoleThirdPartyCount, "warn"),
				partials.Count("network", p.NetworkThirdPartyCount, "warn"),
			),
		),
	)
}

// labEnvNote surfaces a subtle banner when a pushed run declared the "lab"
// environment: its perf numbers were measured on a hermetic localhost stack (no
// CDN/latency/compression), so they are NOT field-representative and auditloop
// suppresses the perf FINDINGS for it (the raw numbers are still shown for
// reference/trend). Renders nothing for staging/prod/crawled runs.
func labEnvNote(run *db.Run) g.Node {
	if run.Environment != plugin.EnvLab {
		return g.Text("")
	}
	return h.Div(h.Class("rounded-lg border border-warning/30 bg-warning/10 px-4 py-2 text-sm text-warning-fg"),
		g.Text("Lab environment — perf measured on a hermetic localhost stack, so LCP/TBT/page-weight are not field-representative. Perf findings are suppressed for this run (the raw numbers are shown for reference)."))
}

// perfLine is the compact deterministic web-vitals / weight row shown under a
// capture: LCP / CLS / TBT / weight / requests, each colour-coded good/needs-work/
// poor against the shared report thresholds. It renders nothing when the page has
// no perf capture (all zero — e.g. a pre-migration or pushed row).
func perfLine(p *db.Page) g.Node {
	if p.LCPMs == 0 && p.CLS == 0 && p.TBTMs == 0 && p.WeightBytes == 0 && p.ReqCount == 0 {
		return g.Text("")
	}
	metrics := []g.Node{
		perfChip("LCP", termDefs["LCP"], fmt.Sprintf("%dms", p.LCPMs), report.Rating(float64(p.LCPMs), report.LCPGoodMs, report.LCPPoorMs)),
		perfChip("CLS", termDefs["CLS"], fmt.Sprintf("%.3f", p.CLS), report.Rating(p.CLS, report.CLSGood, report.CLSPoor)),
		// TBT is a headless LAB PROXY (no field input) — labeled with a ~ and title.
		perfChip("TBT~", termDefs["TBT"], fmt.Sprintf("%dms", p.TBTMs), report.Rating(float64(p.TBTMs), report.TBTGoodMs, report.TBTPoorMs)),
		perfChip("weight", termDefs["weight"], formatBytes(p.WeightBytes), weightRating(p.WeightBytes)),
		perfChip("reqs", termDefs["reqs"], fmt.Sprintf("%d", p.ReqCount), reqRating(p.ReqCount)),
	}
	return h.Div(h.Class("flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line px-3 py-2"),
		h.Span(h.Class("flex items-center text-xs font-semibold text-muted"),
			g.Text("Performance"), partials.InfoIcon(termDefs["perf"])),
		g.Group(metrics),
		h.Span(h.Class("text-[10px] text-muted"), h.Title("TBT is a headless lab proxy — no field input; approximates main-thread blocking, not a real field metric."), g.Text("TBT~ = lab proxy")),
	)
}

// perfChip renders one metric label + value with a rating colour. def (when set)
// attaches an inline tooltip defining the metric.
func perfChip(label, def, value, rating string) g.Node {
	tone := "text-success-fg"
	switch rating {
	case "needs-improvement":
		tone = "text-warning-fg"
	case "poor":
		tone = "text-danger-fg"
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-xs"),
		h.Span(h.Class("inline-flex items-center text-muted"), g.Text(label), partials.InfoIcon(def)),
		h.Span(h.Class("font-semibold "+tone), g.Text(value)),
	)
}

func weightRating(b int64) string {
	switch {
	case b > report.WeightWarnBytes:
		return "poor"
	case b > report.WeightWarnBytes*2/3:
		return "needs-improvement"
	default:
		return "good"
	}
}

func reqRating(n int) string {
	switch {
	case n > report.ReqCountWarn:
		return "poor"
	case n > report.ReqCountWarn*2/3:
		return "needs-improvement"
	default:
		return "good"
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.0fKB", float64(b)/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// findingTone maps a finding type → a badge colour class so perf/layout/network
// findings read at a glance alongside a11y/console.
func findingTone(typ string) string {
	switch typ {
	case db.FindingA11y:
		return "bg-danger/15 text-danger-fg"
	case db.FindingPerf:
		return "bg-warning/15 text-warning-fg"
	case db.FindingLayout:
		return "bg-brand-light/15 text-brand-light"
	case db.FindingNetwork:
		return "bg-orange-500/15 text-orange-300"
	case db.FindingConsole:
		return "bg-yellow-500/15 text-yellow-300"
	default:
		return "bg-surface text-muted"
	}
}

// findingsSection renders the page's findings list. It lives inside the already-
// collapsed page card, so it renders INLINE (not a nested <details>) — the page
// disclosure is the single interaction that reveals it.
func findingsSection(findings []*db.Finding) g.Node {
	if len(findings) == 0 {
		return g.Text("")
	}
	items := make([]g.Node, 0, len(findings))
	for _, f := range findings {
		items = append(items, h.Li(h.Class("flex items-start gap-2 px-3 py-2"),
			h.Span(h.Class("mt-0.5 rounded px-1.5 py-0.5 text-xs font-medium "+findingTone(f.Type)), g.Text(f.Type)),
			h.Span(h.Class("mt-0.5 rounded bg-surface px-1.5 py-0.5 text-[10px] font-medium text-muted"), g.Text(f.Severity)),
			h.Span(h.Class("text-xs text-muted break-all"), g.Text(truncate(f.Detail, 200))),
		))
	}
	return h.Div(
		h.P(h.Class("section-title mb-2"), g.Textf("Findings (%d)", len(findings))),
		h.Ul(h.Class("divide-y divide-line rounded border border-line"), g.Group(items)),
	)
}

// analysisSection groups the opt-in LLM report sections (persona walkthrough + AI UX
// notes) under a clear "Deeper analysis" heading, kept out of the primary metric flow
// but easy to find. Each carries its own controls, forms, and 3s self-poll unchanged.
// When neither is enabled the (empty) fragment divs still render so their htmx ids
// exist, but without the heading.
func analysisSection(evalVM *EvalVM, notes *NotesVM) g.Node {
	enabled := (evalVM != nil && evalVM.Enabled) || (notes != nil && notes.Enabled)
	if !enabled {
		return h.Div(EvaluationSection(evalVM), NotesSection(notes))
	}
	return h.Section(h.Class("space-y-4"),
		h.H2(h.Class("section-title"), g.Text("Deeper analysis")),
		EvaluationSection(evalVM),
		NotesSection(notes),
	)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
