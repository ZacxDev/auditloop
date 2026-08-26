package pages

import (
	"github.com/ZacxDev/auditloop/components/partials"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// PluginVM is the view model for a push-only plugin target. Token is set ONLY on
// the one-time reveal fragment (create / rotate); it is never re-rendered.
type PluginVM struct {
	TargetID string
	Name     string
	Endpoint string // absolute POST endpoint (BaseURL + /api/plugins/runs)
	Token    string // plaintext push token — shown ONCE, never persisted
}

// PluginCreateForm is the dashboard card for creating a push-only plugin target.
// On submit it swaps in the one-time token reveal (so the token is shown once and
// the page is not refreshed away from it).
func PluginCreateForm() g.Node {
	return partials.Card(
		h.H2(h.Class("mb-1 section-title"), g.Text("Add a plugin target (push-only)")),
		h.P(h.Class("mb-3 text-xs text-muted"),
			g.Text("Use this when auditloop can't reach the site itself. Instead of auditloop crawling it, your own tooling (for example an automated test run) sends completed audit results here. You'll get a secret upload key, shown once.")),
		h.Form(
			g.Attr("hx-post", "/api/plugins/targets"),
			g.Attr("hx-target", "#plugin-create-result"),
			g.Attr("hx-swap", "innerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("flex flex-col gap-3 sm:flex-row sm:items-end"),
			h.Div(h.Class("flex-1"),
				h.Label(h.For("plugin_name"), h.Class("block text-sm font-medium mb-1"), g.Text("Name")),
				h.Input(h.ID("plugin_name"), h.Name("name"), h.Required(), h.Placeholder("Signup funnel (CI)"),
					h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
			),
			h.Div(h.Class("flex-1"),
				h.Label(h.For("plugin_label"), h.Class("block text-sm font-medium mb-1"), g.Text("Label (optional)")),
				h.Input(h.ID("plugin_label"), h.Name("label"), h.Placeholder("https://app.acme.com"),
					h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
			),
			h.Button(h.Type("submit"), h.Class("btn-secondary"),
				g.Text("Create plugin target")),
		),
		h.Div(h.ID("plugin-create-result"), h.Class("mt-4")),
	)
}

// PluginTokenReveal renders the one-time push-token panel (shown after create or
// rotate). The token is displayed once with a copy affordance; it can never be
// retrieved again — only rotated.
func PluginTokenReveal(vm *PluginVM) g.Node {
	if vm == nil {
		return g.Text("")
	}
	return h.Div(h.Class("rounded border border-emerald-500/30 bg-emerald-500/10 p-4 text-sm"),
		h.P(h.Class("font-semibold text-emerald-300"), g.Text("Upload key created for "+vm.Name)),
		h.P(h.Class("mt-1 text-xs text-muted"),
			g.Text("This is the secret your tooling uses to upload results. Copy it now — it is shown ONCE and cannot be retrieved again (only replaced).")),
		h.Div(h.Class("mt-3"),
			h.Label(h.For("plugin-token-"+vm.TargetID), h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Upload key")),
			copyField("plugin-token-"+vm.TargetID, "Upload key", vm.Token),
		),
		h.Div(h.Class("mt-3"),
			h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Push endpoint")),
			h.Code(h.Class("block break-all rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs"), g.Text(vm.Endpoint)),
		),
		h.P(h.Class("mt-3"),
			h.A(h.Href("/targets/"+vm.TargetID), h.Class("text-brand-light hover:underline text-sm"), g.Text("Go to the plugin target →")),
		),
	)
}

// PluginSection renders the push-only target's body: push instructions, an
// example uploader command, and a Rotate-token control. No "Run audit" button —
// the target is never crawled.
func PluginSection(vm *PluginVM) g.Node {
	example := "auditloop-push \\\n  --url " + trimEndpoint(vm.Endpoint) + " \\\n  --token <PUSH_TOKEN> \\\n  --meta metadata.json \\\n  --files ./artifacts"
	return partials.Card(
		h.Div(h.Class("mb-3 flex items-center justify-between gap-3"),
			h.H2(h.Class("section-title"), g.Text("Plugin push")),
			h.Span(h.Class("badge-info"),
				g.Text("Push-only — not crawled")),
		),
		h.P(h.Class("mb-3 text-sm text-muted"),
			g.Text("This target receives audit results uploaded by your own tooling, rather than being crawled by auditloop. Each upload becomes a run below and is compared against the previous one.")),
		h.Div(h.Class("space-y-3"),
			h.Div(
				h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Push endpoint")),
				h.Code(h.Class("block break-all rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs"), g.Text(vm.Endpoint)),
			),
			h.Div(
				h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Example (reference uploader)")),
				h.Pre(h.Class("overflow-x-auto rounded border border-line bg-surface px-3 py-2 font-mono text-xs"),
					h.Code(g.Text(example))),
			),
		),
		h.Div(h.Class("mt-4 flex items-center gap-3"),
			h.Button(h.Type("button"),
				g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/plugin-token/rotate"),
				g.Attr("hx-target", "#plugin-token-result"),
				g.Attr("hx-swap", "innerHTML"),
				g.Attr("hx-boost", "false"),
				g.Attr("hx-confirm", "Rotate the push token? The current token stops working immediately."),
				h.Class("rounded border border-line px-4 py-2 text-sm font-medium text-ink hover:bg-surface"),
				g.Text("Rotate token")),
			h.Span(h.Class("text-xs text-muted"), g.Text("Regenerates the token; the old one is invalidated.")),
		),
		h.Div(h.ID("plugin-token-result"), h.Class("mt-4")),
	)
}

// copyField renders a read-only value with a copy button (uses the app.js
// [data-copy] delegate if present; the field is selectable regardless). label is
// the input's accessible name (aria-label) so the read-only token/key field is
// never an unlabeled form control (axe a11y).
func copyField(id, label, value string) g.Node {
	return h.Div(h.Class("flex items-stretch gap-2"),
		h.Input(h.ID(id), h.Type("text"), h.ReadOnly(), h.Value(value),
			g.Attr("aria-label", label),
			g.Attr("onclick", "this.select()"),
			h.Class("w-full rounded border border-line bg-surface px-2 py-1.5 font-mono text-xs")),
		h.Button(h.Type("button"),
			g.Attr("data-copy", "#"+id),
			g.Attr("onclick", "navigator.clipboard&&navigator.clipboard.writeText(this.previousElementSibling.value)"),
			h.Class("shrink-0 rounded border border-line px-3 py-1.5 text-xs font-medium hover:bg-surface"),
			g.Text("Copy")),
	)
}

func trimEndpoint(endpoint string) string {
	// Strip the /api/plugins/runs suffix so the CLI --url is the app base.
	const suffix = "/api/plugins/runs"
	if len(endpoint) > len(suffix) && endpoint[len(endpoint)-len(suffix):] == suffix {
		return endpoint[:len(endpoint)-len(suffix)]
	}
	return endpoint
}
