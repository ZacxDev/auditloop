package pages

import (
	"strings"

	"github.com/ZacxDev/auditloop/components"
	"github.com/ZacxDev/auditloop/components/layouts"
	"github.com/ZacxDev/auditloop/internal/db"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// CardShot is one screenshot thumbnail in a project card's carousel: a pre-built
// (authed-proxy) URL plus its accessible alt text.
type CardShot struct {
	URL string
	Alt string
}

// DashboardCardVM is the per-target "first-class project" card view model, assembled
// owner-scoped in the handler (latest done run + a few screenshot keys + favicon key +
// cheap stats). All URLs are pre-built app-origin artifact-proxy URLs — the component
// never constructs them (it can't reach the storage key scheme).
type DashboardCardVM struct {
	Target     *db.Target
	FaviconURL string     // "" → fall back to a name monogram
	Shots      []CardShot // latest done run's screenshot thumbnails ("No runs" → empty)
	// Latest-run summary (all zero/empty when the target has no done run yet).
	HasRun         bool
	Status         string // latest run status (done)
	RunDate        string // pre-formatted date of the latest done run
	Pages          int
	A11yViolations int
	Regressions    int // P2 same-size visual regressions on the latest run
}

// Dashboard lists the user's targets as first-class PROJECT CARDS (identity +
// screenshot carousel + summary). Creation/setup stays collapsed behind one primary
// "＋ New target" disclosure so a returning user isn't buried under setup forms.
func Dashboard(ctx components.PageContext, cards []DashboardCardVM, apiKeys []*db.APIKey) g.Node {
	empty := len(cards) == 0
	return layouts.App(ctx,
		h.Div(h.Class("mb-4 flex items-center justify-between gap-3"),
			h.H1(h.Class("text-xl font-bold"), g.Text("Targets")),
		),
		newTargetDisclosure(apiKeys, empty),
		h.Div(h.Class("mt-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-3"), ProjectCards(cards)),
	)
}

// newTargetDisclosure is the single, visually-primary "＋ New target" affordance
// that collapses the three creation/setup cards (add target, plugin push, API
// access). Open by default for a brand-new user (no targets yet). Native
// <details> — progressive disclosure, no JS.
func newTargetDisclosure(apiKeys []*db.APIKey, open bool) g.Node {
	attrs := []g.Node{h.Class("group rounded-lg border border-line bg-card")}
	if open {
		attrs = append(attrs, h.Open())
	}
	summary := h.Summary(
		h.Class("flex cursor-pointer list-none items-center gap-2 rounded-lg bg-brand px-4 py-3 font-medium text-white hover:opacity-90"),
		h.Span(h.Class("text-lg leading-none"), g.Text("＋")),
		g.Text("New target"),
	)
	body := h.Div(h.Class("space-y-6 border-t border-line p-5"),
		addTargetForm(),
		h.Details(h.Class("rounded-lg border border-line"),
			h.Summary(h.Class("cursor-pointer px-4 py-3 text-sm font-medium text-muted hover:text-ink"),
				g.Text("Advanced — plugin push & API access")),
			h.Div(h.Class("space-y-6 border-t border-line p-4"),
				PluginCreateForm(),
				APIAccessCard(apiKeys),
			),
		),
	)
	return h.Details(append(attrs, summary, body)...)
}

// addTargetForm is the common-case "register a site you own" form (unchanged
// fields/action/htmx — layout only).
func addTargetForm() g.Node {
	return h.Div(
		h.H2(h.Class("mb-3 section-title"), g.Text("Add a target")),
		h.Form(
			g.Attr("hx-post", "/api/targets"),
			g.Attr("hx-swap", "none"),
			h.Class("flex flex-col gap-3 sm:flex-row sm:items-end"),
			h.Div(h.Class("flex-1"),
				h.Label(h.For("name"), h.Class("block text-sm font-medium mb-1"), g.Text("Name")),
				h.Input(h.ID("name"), h.Name("name"), h.Required(), h.Placeholder("Acme marketing site"),
					h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
			),
			h.Div(h.Class("flex-1"),
				h.Label(h.For("base_url"), h.Class("block text-sm font-medium mb-1"), g.Text("Base URL")),
				h.Input(h.ID("base_url"), h.Name("base_url"), h.Type("url"), h.Required(), h.Placeholder("https://acme.com"),
					h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
			),
			h.Button(h.Type("submit"), h.Class("btn-primary"),
				g.Text("Add target")),
		),
		h.P(h.Class("mt-2 text-xs text-muted"),
			g.Text("You must own the site. Crawls are same-origin only and refuse private/internal addresses.")),
	)
}

// ProjectCards renders the responsive project-card grid.
func ProjectCards(cards []DashboardCardVM) g.Node {
	if len(cards) == 0 {
		return h.Div(h.Class("col-span-full rounded-lg border border-dashed border-line p-8 text-center text-muted"),
			g.Text("No targets yet. Use “＋ New target” above to register a site and run your first audit."))
	}
	nodes := make([]g.Node, 0, len(cards))
	for _, vm := range cards {
		nodes = append(nodes, projectCard(vm))
	}
	return g.Group(nodes)
}

// projectCard renders one target as a first-class project card: identity (favicon or
// monogram + name + auth badge), a screenshot carousel, cheap summary stats, and the
// primary action. `.card-interactive` gives the hover-lift; the whole card enters with
// motion-safe:animate-enter.
func projectCard(vm DashboardCardVM) g.Node {
	t := vm.Target
	isPlugin := t.AuthMode == db.AuthPlugin
	href := "/targets/" + t.ID

	return h.Div(
		h.Class("card-interactive flex flex-col gap-3 motion-safe:animate-enter"),
		cardIdentity(vm, isPlugin, href),
		cardCarousel(vm),
		cardSummary(vm, isPlugin),
		cardActions(vm, isPlugin, href),
	)
}

// cardIdentity is the favicon/monogram + name + base URL + auth-mode badge.
func cardIdentity(vm DashboardCardVM, isPlugin bool, href string) g.Node {
	return h.Div(h.Class("flex items-start gap-3"),
		cardAvatar(vm),
		h.Div(h.Class("min-w-0 flex-1"),
			h.Div(h.Class("flex items-center justify-between gap-2"),
				h.A(h.Href(href), h.Class("truncate font-semibold hover:text-brand-light"), g.Text(vm.Target.Name)),
				authBadge(vm.Target.AuthMode),
			),
			h.P(h.Class("mt-0.5 truncate text-sm text-muted"), g.Text(cardSubtitle(vm.Target, isPlugin))),
		),
	)
}

// cardAvatar shows the captured favicon, or a name monogram when none was captured.
func cardAvatar(vm DashboardCardVM) g.Node {
	if vm.FaviconURL != "" {
		return h.Img(
			h.Src(vm.FaviconURL), h.Alt(""), g.Attr("aria-hidden", "true"), h.Loading("lazy"),
			h.Class("h-9 w-9 shrink-0 rounded border border-line bg-surface object-contain p-1"),
		)
	}
	return h.Div(
		h.Class("flex h-9 w-9 shrink-0 items-center justify-center rounded border border-line bg-surface text-sm font-semibold text-brand-light"),
		g.Attr("aria-hidden", "true"),
		g.Text(monogram(vm.Target.Name)),
	)
}

// cardCarousel is the accessible scroll-snap screenshot strip (favicon-led identity is
// in the header). Degrades to a "No runs yet" panel when the target has no screenshots.
func cardCarousel(vm DashboardCardVM) g.Node {
	if len(vm.Shots) == 0 {
		return h.Div(
			h.Class("flex h-32 items-center justify-center rounded-lg border border-dashed border-line bg-surface text-sm text-muted"),
			g.Text("No runs yet"),
		)
	}
	label := "Screenshots of " + vm.Target.Name
	slides := make([]g.Node, 0, len(vm.Shots))
	for _, s := range vm.Shots {
		slides = append(slides,
			h.Div(h.Class("relative w-40 shrink-0 snap-start"), g.Attr("data-carousel-slide", ""),
				h.Img(h.Src(s.URL), h.Alt(s.Alt), h.Loading("lazy"),
					h.Class("h-32 w-40 rounded-lg border border-line bg-surface object-cover object-top")),
			),
		)
	}
	track := h.Div(
		h.Class("flex snap-x snap-mandatory gap-3 overflow-x-auto scroll-smooth rounded-lg focus:outline-none focus:ring-2 focus:ring-brand-light/50"),
		g.Attr("data-carousel-track", ""),
		// Keyboard-scrollable region (axe scrollable-region-focusable): a labeled,
		// focusable group so arrow keys scroll the strip.
		h.TabIndex("0"), g.Attr("role", "group"), g.Attr("aria-label", label),
		g.Group(slides),
	)
	// Only show prev/next controls when there's more than one thumbnail to scroll to.
	var controls g.Node = g.Text("")
	if len(vm.Shots) > 1 {
		controls = h.Div(h.Class("mt-1 flex justify-end gap-1"),
			carouselButton("prev", "Scroll screenshots left", "‹"),
			carouselButton("next", "Scroll screenshots right", "›"),
		)
	}
	return h.Div(h.Class("min-w-0"), g.Attr("data-carousel", ""), track, controls)
}

func carouselButton(dir, label, glyph string) g.Node {
	return h.Button(h.Type("button"), g.Attr("data-carousel-"+dir, ""),
		g.Attr("aria-label", label),
		h.Class("flex h-6 w-6 items-center justify-center rounded border border-line text-muted transition-colors hover:bg-card-hover hover:text-ink"),
		g.Text(glyph),
	)
}

// cardSummary is the last-run status + date + cheap stat chips.
func cardSummary(vm DashboardCardVM, isPlugin bool) g.Node {
	if !vm.HasRun {
		hint := "Not yet audited"
		if isPlugin {
			hint = "Awaiting first push"
		}
		return h.P(h.Class("text-xs text-muted"), g.Text(hint))
	}
	stats := []g.Node{
		cardStatChip(g.Textf("%d", vm.Pages), "pages", ""),
		cardStatChip(g.Textf("%d", vm.A11yViolations), "a11y", toneFor(vm.A11yViolations)),
	}
	if vm.Regressions > 0 {
		stats = append(stats, cardStatChip(g.Textf("%d", vm.Regressions), "regressions", "danger"))
	}
	return h.Div(h.Class("flex flex-col gap-2"),
		h.Div(h.Class("flex items-center gap-2 text-xs text-muted"),
			h.Span(h.Class("badge-success"), g.Text("Done")),
			g.Text(vm.RunDate),
		),
		h.Div(h.Class("flex flex-wrap items-center gap-x-4 gap-y-1"), g.Group(stats)),
	)
}

func cardStatChip(n g.Node, label, tone string) g.Node {
	numCls := "font-semibold text-ink"
	switch tone {
	case "danger":
		numCls = "font-semibold text-danger-fg"
	case "warning":
		numCls = "font-semibold text-warning-fg"
	}
	return h.Span(h.Class("inline-flex items-center gap-1 text-xs"),
		h.Span(h.Class(numCls), n),
		h.Span(h.Class("text-muted"), g.Text(label)),
	)
}

func cardActions(vm DashboardCardVM, isPlugin bool, href string) g.Node {
	viewLink := h.A(h.Href(href), h.Class("text-sm text-muted hover:text-ink"), g.Text("View runs"))
	if isPlugin {
		return h.Div(h.Class("mt-auto flex items-center gap-3 pt-1"),
			h.A(h.Href(href), h.Class("btn-accent"), g.Text("Push instructions")),
			viewLink,
		)
	}
	return h.Div(h.Class("mt-auto flex items-center gap-3 pt-1"),
		h.Button(
			g.Attr("hx-post", "/api/targets/"+vm.Target.ID+"/runs"),
			g.Attr("hx-swap", "none"),
			h.Class("btn-primary text-sm"),
			g.Text("Run audit"),
		),
		viewLink,
	)
}

// authBadge maps a target's auth mode to a design-system status badge.
func authBadge(mode string) g.Node {
	switch mode {
	case db.AuthPlugin:
		return h.Span(h.Class("badge-info"), g.Text("Plugin"))
	case db.AuthLogin:
		return h.Span(h.Class("badge-warning"), g.Text("Login"))
	default:
		return h.Span(h.Class("badge-success"), g.Text("Crawl"))
	}
}

func cardSubtitle(t *db.Target, isPlugin bool) string {
	if isPlugin && t.BaseURL == "" {
		return "Push-only plugin target"
	}
	return t.BaseURL
}

// monogram returns 1–2 uppercase initials from a target name for the avatar fallback.
func monogram(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return "?"
	}
	if len(fields) == 1 {
		r := []rune(fields[0])
		if len(r) >= 2 {
			return strings.ToUpper(string(r[:2]))
		}
		return strings.ToUpper(string(r))
	}
	return strings.ToUpper(string([]rune(fields[0])[:1]) + string([]rune(fields[1])[:1]))
}

func toneFor(n int) string {
	if n > 0 {
		return "warning"
	}
	return ""
}
