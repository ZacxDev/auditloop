package pages

import (
	"time"

	"github.com/ZacxDev/auditloop/components"
	"github.com/ZacxDev/auditloop/components/layouts"
	"github.com/ZacxDev/auditloop/components/partials"
	"github.com/ZacxDev/auditloop/internal/db"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// TargetOverviewVM is the at-a-glance status strip in the target overview header.
// It is derived from the target's most recent run (newest-first) — HasRun is false
// for a brand-new target, HasStats false when that run has no parsed summary yet
// (e.g. still queued/running). It degrades gracefully in both cases.
type TargetOverviewVM struct {
	HasRun     bool
	LastStatus string
	LastRunAt  time.Time
	LastRunID  string
	HasStats   bool
	Pages      int
	A11y       int
	Console    int
	Network    int
}

// TargetView is the redesigned target detail page. It establishes a clear
// hierarchy so the page no longer reads as a wall of equally-heavy cards:
//
//  1. Overview header — identity + auth-mode badge + the primary "Run audit" CTA
//     + an at-a-glance status strip (last run + key stats).
//  2. Runs — the PRIMARY content: recent runs as interactive cards, newest first.
//  3. Findings trend.
//  4. Configuration — secondary, tucked behind a native <details> accordion so the
//     Authentication / Audit-configuration / Walkthrough controls no longer compete
//     for attention. Each section renders under EXACTLY its existing gate.
//
// A plugin (push-only) target has no "Run audit" and no accordion — it shows the
// PluginSection (push instructions) directly.
func TargetView(ctx components.PageContext, t *db.Target, runs []*db.Run, trend []db.TrendPoint, overview *TargetOverviewVM, authVM *AuthVM, pluginVM *PluginVM, auditCfgVM *AuditConfigVM, walkVM *WalkthroughVM) g.Node {
	isPlugin := t.AuthMode == db.AuthPlugin

	return layouts.App(ctx,
		h.Div(h.Class("mb-3"),
			h.A(h.Href("/dashboard"), h.Class("text-sm text-muted hover:text-ink"), g.Text("← Targets")),
		),
		overviewHeader(t, overview, isPlugin),
		h.Div(h.Class("mt-6 space-y-8 motion-safe:animate-enter"),
			runsSection(runs, isPlugin),
			FindingTrend(trend),
			configArea(isPlugin, pluginVM, authVM, auditCfgVM, walkVM),
		),
	)
}

// overviewHeader is the hero: target identity, an auth-mode badge, the primary CTA
// and a status strip. It fades+rises in on load (motion-safe).
func overviewHeader(t *db.Target, ov *TargetOverviewVM, isPlugin bool) g.Node {
	subtitle := t.BaseURL
	if isPlugin {
		subtitle = "Plugin target (push-only)"
		if t.BaseURL != "" {
			subtitle = "Plugin target · " + t.BaseURL
		}
	}

	var runAudit g.Node = g.Text("")
	if !isPlugin {
		runAudit = h.Button(
			g.Attr("hx-post", "/api/targets/"+t.ID+"/runs"),
			g.Attr("hx-swap", "none"),
			h.Class("btn-primary shrink-0"),
			g.Text("Run audit"),
		)
	}

	return h.Div(h.Class("rounded-xl border border-line bg-card p-5 sm:p-6 motion-safe:animate-enter"),
		h.Div(h.Class("flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between"),
			h.Div(h.Class("min-w-0 space-y-1"),
				h.Div(h.Class("flex flex-wrap items-center gap-2"),
					h.H1(h.Class("truncate text-xl font-bold text-ink"), g.Text(t.Name)),
					authModeBadge(t),
				),
				h.P(h.Class("break-all text-sm text-muted"), g.Text(subtitle)),
			),
			runAudit,
		),
		statusStrip(ov, isPlugin),
	)
}

// authModeBadge maps the target's auth mode to a semantic .badge-* pill.
func authModeBadge(t *db.Target) g.Node {
	switch t.AuthMode {
	case db.AuthPlugin:
		return h.Span(h.Class("badge-info"), g.Text("Push-only"))
	case db.AuthLogin:
		return h.Span(h.Class("badge-success"), g.Text("Authenticated"))
	default:
		return h.Span(h.Class("badge bg-surface text-muted"), g.Text("Public crawl"))
	}
}

// statusStrip is the at-a-glance last-run summary under the header. Degrades to a
// short prompt when there are no runs yet.
func statusStrip(ov *TargetOverviewVM, isPlugin bool) g.Node {
	if ov == nil || !ov.HasRun {
		msg := "No runs yet — click “Run audit” to start one."
		if isPlugin {
			msg = "No runs yet — this target receives pushed runs."
		}
		return h.Div(h.Class("mt-4 border-t border-line pt-4"),
			h.P(h.Class("text-sm text-muted"), g.Text(msg)))
	}

	chips := []g.Node{
		h.Div(h.Class("flex items-center gap-2"),
			h.Span(h.Class("text-xs uppercase tracking-wide text-muted"), g.Text("Last run")),
			partials.StatusPill(ov.LastStatus),
			h.Span(h.Class("text-sm text-muted"), g.Text(ov.LastRunAt.Format("Jan 2, 2006 15:04"))),
		),
	}
	if ov.HasStats {
		chips = append(chips,
			statChip(ov.Pages, "pages"),
			statChip(ov.A11y, "a11y issues"),
			statChip(ov.Console+ov.Network, "errors"),
		)
	}
	return h.Div(h.Class("mt-4 flex flex-wrap items-center gap-x-6 gap-y-2 border-t border-line pt-4"),
		g.Group(chips))
}

// statChip renders one big-number + label stat.
func statChip(n int, label string) g.Node {
	return h.Div(h.Class("flex items-baseline gap-1.5"),
		h.Span(h.Class("text-lg font-semibold text-ink"), g.Textf("%d", n)),
		h.Span(h.Class("text-xs text-muted"), g.Text(label)),
	)
}

// runsSection is the PRIMARY content: a labeled stack of run cards.
func runsSection(runs []*db.Run, isPlugin bool) g.Node {
	return h.Section(h.Class("space-y-3"),
		h.H2(h.Class("section-title"), g.Text("Runs")),
		RunList(runs, isPlugin),
	)
}

// RunList renders a target's runs (bare for htmx swap). Each run is a
// .card-interactive row (subtle hover-lift, motion-safe) on the app surface, so
// the list reads as the main content. isPlugin picks the empty-state copy.
func RunList(runs []*db.Run, isPlugin bool) g.Node {
	if len(runs) == 0 {
		msg := "No runs yet. Click “Run audit” to start one."
		if isPlugin {
			msg = "No runs yet. This target receives pushed runs — see the push instructions below."
		}
		return h.Div(h.Class("card"),
			h.P(h.Class("text-sm text-muted"), g.Text(msg)))
	}
	rows := make([]g.Node, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, runRow(r))
	}
	return h.Div(h.Class("space-y-2"), g.Group(rows))
}

func runRow(r *db.Run) g.Node {
	meta := []g.Node{
		partials.StatusPill(r.Status),
		h.Span(h.Class("text-sm text-muted"), g.Text(r.CreatedAt.Format("Jan 2, 2006 15:04"))),
	}
	if r.Label != "" {
		meta = append(meta, h.Span(h.Class("badge bg-surface text-muted"), g.Text(r.Label)))
	}
	return h.A(h.Href("/runs/"+r.ID),
		h.Class("card-interactive flex items-center justify-between gap-3"),
		h.Div(h.Class("flex flex-wrap items-center gap-3"), g.Group(meta)),
		h.Span(h.Class("shrink-0 text-sm text-muted"), g.Text("View →")),
	)
}

// configArea groups the secondary configuration controls out of the primary flow.
// For a plugin target it is the PluginSection (push instructions) directly. For a
// crawlable target it is a native <details> accordion — one collapsible item per
// ENABLED config section, each rendering under exactly its existing gate. Native
// <details>/<summary> gives correct keyboard + screen-reader semantics with no ARIA.
func configArea(isPlugin bool, pluginVM *PluginVM, authVM *AuthVM, auditCfgVM *AuditConfigVM, walkVM *WalkthroughVM) g.Node {
	if isPlugin {
		return PluginSection(pluginVM)
	}

	var items []g.Node
	if authVM != nil && authVM.Enabled {
		// Open when a login recipe is configured so the state (and a just-saved
		// result) is visible after the auth-save full reload (HX-Refresh) — which
		// flips auth_mode and re-renders the whole page; otherwise the section would
		// re-collapse and hide the confirmation. A "none"/unconfigured target stays
		// collapsed (nothing to show).
		items = append(items, configItem("Authentication",
			"Crawl logged-in pages via a login recipe", AuthSection(authVM), authVM.Mode == "login"))
	}
	if auditCfgVM != nil && auditCfgVM.Enabled {
		items = append(items, configItem("Audit configuration",
			"Goal, audiences & driving for persona walkthroughs", AuditConfigSection(auditCfgVM), false))
	}
	if walkVM != nil && walkVM.Enabled && walkVM.DrivingEnabled {
		// Open the item while a pass is driving so the live progress (and its htmx
		// self-poll) is visible without a click.
		items = append(items, configItem("Goal-directed walkthrough",
			"Drive the site toward the goal and report if it's reached",
			WalkthroughSection(walkVM), walkVM.Status == "driving"))
	}
	if len(items) == 0 {
		return g.Text("")
	}

	return h.Section(h.Class("space-y-3"),
		h.H2(h.Class("section-title"), g.Text("Configuration")),
		h.P(h.Class("text-xs text-muted"), g.Text("Optional setup — expand a section to configure it.")),
		h.Div(h.Class("space-y-2"), g.Group(items)),
	)
}

// configItem is one collapsible accordion row: a summary (title + one-line hint +
// chevron) over the section body. The body is the existing section fragment
// (unchanged ids + htmx targets) — the accordion supplies the card container so the
// sections no longer double-wrap.
func configItem(title, hint string, body g.Node, open bool) g.Node {
	attrs := []g.Node{h.Class("card overflow-hidden p-0")}
	if open {
		attrs = append(attrs, g.Attr("open", ""))
	}
	summary := h.Summary(
		h.Class("accordion-summary flex cursor-pointer select-none items-center justify-between gap-3 px-4 py-3 transition-colors hover:bg-card-hover"),
		h.Div(h.Class("min-w-0"),
			h.Span(h.Class("block font-medium text-ink"), g.Text(title)),
			h.Span(h.Class("block text-xs text-muted"), g.Text(hint)),
		),
		chevron(),
	)
	return h.Details(append(attrs,
		summary,
		h.Div(h.Class("border-t border-line p-4"), body),
	)...)
}

// chevron is the accordion disclosure indicator; CSS rotates it 90° when the
// parent <details> is open (motion is auto-neutralized under prefers-reduced-motion).
func chevron() g.Node {
	return g.El("svg",
		h.Class("accordion-chevron h-4 w-4 shrink-0 text-muted"),
		g.Attr("viewBox", "0 0 20 20"), g.Attr("fill", "none"),
		g.Attr("stroke", "currentColor"), g.Attr("stroke-width", "2"),
		g.Attr("aria-hidden", "true"),
		g.El("path", g.Attr("stroke-linecap", "round"), g.Attr("stroke-linejoin", "round"),
			g.Attr("d", "M7 5l6 5-6 5")),
	)
}
