package pages

import (
	"github.com/ZacxDev/auditloop/components/partials"
	"github.com/ZacxDev/auditloop/internal/db"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// APIKeyRevealVM carries the one-time API-key reveal (create). Token is set ONLY
// on the reveal fragment; it is never re-rendered or persisted in plaintext.
type APIKeyRevealVM struct {
	Name    string
	Token   string
	BaseURL string // app base (e.g. https://auditloop.example.com) for the example curl
}

// APIAccessCard is the dashboard "API access" panel: it lists the user's
// read-only API keys (name, created, last used, revoke) and a create form that
// swaps in a one-time token reveal. No hash or plaintext is ever rendered after
// creation.
func APIAccessCard(keys []*db.APIKey) g.Node {
	return partials.Card(
		h.H2(h.Class("mb-1 section-title"), g.Text("API access (read-only)")),
		h.P(h.Class("mb-3 text-xs text-muted"),
			g.Text("Mint a read-only API key so an agent or CLI can pull audit results (run lists, report.json, artifacts) with a Bearer token — no login. A key reads only your own targets. Shown once; rotate = revoke + create.")),
		h.Form(
			g.Attr("hx-post", "/api/keys"),
			g.Attr("hx-target", "#apikey-create-result"),
			g.Attr("hx-swap", "innerHTML"),
			g.Attr("hx-boost", "false"),
			h.Class("flex flex-col gap-3 sm:flex-row sm:items-end"),
			h.Div(h.Class("flex-1"),
				h.Label(h.For("apikey_name"), h.Class("block text-sm font-medium mb-1"), g.Text("Name (optional)")),
				h.Input(h.ID("apikey_name"), h.Name("name"), h.Placeholder("ci-agent"),
					h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
			),
			h.Button(h.Type("submit"), h.Class("btn-secondary"),
				g.Text("Create API key")),
		),
		h.Div(h.ID("apikey-create-result"), h.Class("mt-4")),
		h.Div(h.Class("mt-4"), apiKeyList(keys)),
	)
}

func apiKeyList(keys []*db.APIKey) g.Node {
	if len(keys) == 0 {
		return h.P(h.Class("text-xs text-muted"), g.Text("No API keys yet."))
	}
	rows := make([]g.Node, 0, len(keys))
	for _, k := range keys {
		name := k.Name
		if name == "" {
			name = "(unnamed)"
		}
		lastUsed := "never"
		if k.LastUsedAt != nil {
			lastUsed = k.LastUsedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		rows = append(rows, h.Div(h.Class("flex items-center justify-between gap-3 border-t border-line py-2 text-sm"),
			h.Div(
				h.Div(h.Class("font-medium"), g.Text(name)),
				h.Div(h.Class("text-xs text-muted"),
					g.Text("created "+k.CreatedAt.UTC().Format("2006-01-02")+" · last used "+lastUsed)),
			),
			h.Button(h.Type("button"),
				g.Attr("hx-post", "/api/keys/"+k.ID+"/revoke"),
				g.Attr("hx-boost", "false"),
				g.Attr("hx-confirm", "Revoke this API key? Any client using it stops working immediately."),
				h.Class("shrink-0 rounded border border-line px-3 py-1.5 text-xs font-medium text-red-300 hover:bg-surface"),
				g.Text("Revoke")),
		))
	}
	return h.Div(rows...)
}

// APIKeyReveal renders the one-time key panel (shown after create): the token
// with a copy affordance + an example curl. The key can never be retrieved
// again — only revoked and re-created.
func APIKeyReveal(vm *APIKeyRevealVM) g.Node {
	if vm == nil {
		return g.Text("")
	}
	label := vm.Name
	if label == "" {
		label = "read-only API key"
	}
	example := "curl -H 'Authorization: Bearer <API_KEY>' \\\n  " + vm.BaseURL + "/api/audit/targets/<TARGET_ID>/runs/latest"
	return h.Div(h.Class("rounded border border-emerald-500/30 bg-emerald-500/10 p-4 text-sm"),
		h.P(h.Class("font-semibold text-emerald-300"), g.Text("API key created: "+label)),
		h.P(h.Class("mt-1 text-xs text-muted"),
			g.Text("Copy it now — it is shown ONCE and cannot be retrieved again (only revoked).")),
		h.Div(h.Class("mt-3"),
			h.Label(h.For("api-key-reveal"), h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("API key")),
			copyField("api-key-reveal", "API key", vm.Token),
		),
		h.Div(h.Class("mt-3"),
			h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Example")),
			h.Pre(h.Class("overflow-x-auto rounded border border-line bg-surface px-3 py-2 font-mono text-xs"),
				h.Code(g.Text(example))),
		),
		h.P(h.Class("mt-3 text-xs text-muted"),
			g.Text("Reload the page to see it in the list below.")),
	)
}
