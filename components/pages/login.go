package pages

import (
	"github.com/ZacxDev/auditloop/components"
	"github.com/ZacxDev/auditloop/components/layouts"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// Login renders the sign-in-only page (signup is invite-only, handled in
// Supabase). Under DevMode a notice explains auth is bypassed.
func Login(ctx components.PageContext) g.Node {
	return layouts.Public(ctx,
		h.Div(h.Class("w-full max-w-sm"),
			h.Div(h.Class("mb-6 text-center"),
				h.H1(h.Class("text-2xl font-bold"), g.Text("auditloop")),
				h.P(h.Class("mt-1 text-sm text-muted"), g.Text("Generic UX-audit crawler")),
			),
			h.Div(h.Class("rounded-lg border border-line bg-card p-6"),
				g.If(ctx.DevMode,
					h.Div(h.Class("mb-4 rounded border border-amber-500/30 bg-amber-500/10 p-3 text-sm text-amber-300"),
						g.Text("DEV_MODE — auth is bypassed. "),
						h.A(h.Href("/dashboard"), h.Class("underline font-medium"), g.Text("Enter dashboard")),
					),
				),
				h.Form(g.Attr("hx-boost", "false"), g.Attr("data-login-form", ""), h.Class("space-y-3"),
					field("email", "Email", "email", "you@company.com"),
					field("password", "Password", "password", "••••••••"),
					h.Button(h.Type("submit"), h.Class("w-full rounded bg-brand px-4 py-2 font-medium text-white hover:opacity-90"),
						g.Text("Sign in")),
					h.P(g.Attr("data-login-error", ""), h.Class("hidden text-sm text-red-400")),
				),
				h.P(h.Class("mt-4 text-center text-xs text-muted"),
					g.Text("Access is invite-only. Ask an admin to create your account.")),
			),
		),
	)
}

func field(name, label, typ, placeholder string) g.Node {
	return h.Div(
		h.Label(h.For(name), h.Class("block text-sm font-medium mb-1"), g.Text(label)),
		h.Input(h.ID(name), h.Name(name), h.Type(typ), h.Placeholder(placeholder), h.AutoComplete("on"),
			h.Class("w-full rounded border border-line bg-surface px-3 py-2 text-sm focus:border-brand focus:outline-none")),
	)
}
