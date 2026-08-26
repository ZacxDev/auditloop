// Package layouts holds the page shells (authenticated App + public Login).
package layouts

import (
	"github.com/ZacxDev/auditloop/components"

	g "maragu.dev/gomponents"
	c "maragu.dev/gomponents/components"
	h "maragu.dev/gomponents/html"
)

// App is the authenticated application shell.
func App(ctx components.PageContext, body ...g.Node) g.Node {
	return c.HTML5(c.HTML5Props{
		Title:    title(ctx),
		Language: "en",
		Head:     head(ctx),
		Body: []g.Node{
			h.Class("min-h-screen bg-surface text-ink antialiased"),
			g.Attr("hx-boost", "true"),
			topBar(ctx),
			h.Main(h.Class("mx-auto w-full max-w-screen-xl px-4 sm:px-6 py-6"), g.Group(body)),
			scripts(ctx),
		},
	})
}

// Public is the logged-out shell (login page).
func Public(ctx components.PageContext, body ...g.Node) g.Node {
	return c.HTML5(c.HTML5Props{
		Title:    title(ctx),
		Language: "en",
		Head:     head(ctx),
		Body: []g.Node{
			h.Class("min-h-screen bg-surface text-ink antialiased flex items-center justify-center px-4"),
			// A <main> landmark so all page content sits inside a landmark region
			// (axe landmark-one-main + region). The authenticated App shell already
			// wraps its body in <main>; Public (login) had none.
			h.Main(h.Class("w-full flex items-center justify-center"), g.Group(body)),
			scripts(ctx),
		},
	})
}

func title(ctx components.PageContext) string {
	if ctx.Title == "" {
		return "auditloop"
	}
	return ctx.Title + " · auditloop"
}

func head(ctx components.PageContext) []g.Node {
	css := "/static/output.css"
	if ctx.CSSVersion != "" {
		css += "?v=" + ctx.CSSVersion
	}
	return []g.Node{
		h.Meta(h.Charset("utf-8")),
		h.Meta(h.Name("viewport"), h.Content("width=device-width, initial-scale=1")),
		h.Meta(h.Name("theme-color"), h.Content("#0f172a")),
		// PWA: web manifest makes the app installable.
		h.Link(h.Rel("manifest"), h.Href("/manifest.webmanifest")),
		h.Link(h.Rel("icon"), h.Type("image/svg+xml"), h.Href("/static/img/icon.svg")),
		h.Link(h.Rel("apple-touch-icon"), h.Href("/static/img/icon.svg")),
		h.Link(h.Rel("stylesheet"), h.Href(css)),
		h.Script(h.Src("https://unpkg.com/htmx.org@2.0.4"), g.Attr("defer", "true")),
	}
}

func scripts(ctx components.PageContext) g.Node {
	return g.Group([]g.Node{
		supabaseBridge(ctx),
		// Cache-busted app.js (Cloudflare-style hours-long /static caching → the
		// ?v hash forces a fresh copy after a deploy). Wires htmx auth bridge +
		// PWA service-worker registration.
		h.Script(h.Src("/static/js/app.js?v="+ctx.JSVersion), g.Attr("defer", "true"), g.Attr("data-cfasync", "false")),
	})
}

func topBar(ctx components.PageContext) g.Node {
	return h.Header(
		h.Class("sticky top-0 z-40 h-14 border-b border-line bg-card/90 backdrop-blur"),
		h.Div(
			h.Class("mx-auto flex h-full max-w-screen-xl items-center justify-between px-4 sm:px-6"),
			h.A(h.Href("/dashboard"), h.Class("flex items-center gap-2 font-semibold text-lg hover:opacity-80"),
				h.Span(h.Class("inline-block h-6 w-6 rounded bg-brand text-white text-center leading-6 text-sm font-bold"), g.Text("A")),
				g.Text("auditloop"),
			),
			h.Nav(g.Attr("aria-label", "Primary"), h.Class("flex items-center gap-4 text-sm"),
				h.A(h.Href("/dashboard"), h.Class("text-muted hover:text-ink"), g.Text("Targets")),
				g.If(ctx.UserEmail != "", h.Span(h.Class("text-muted hidden sm:inline"), g.Text(ctx.UserEmail))),
				h.Button(g.Attr("hx-boost", "false"), g.Attr("data-logout", ""), h.Type("button"),
					h.Class("text-muted hover:text-ink"), g.Text("Sign out")),
			),
		),
	)
}
