package pages

import (
	"strconv"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// AuthVM is the P4 Authentication view model for a target. It NEVER carries
// credential values — only whether creds are set (write-only UI). Enabled is
// false when no encryption key is configured (the whole section is hidden).
type AuthVM struct {
	TargetID           string
	Enabled            bool
	Mode               string // none|login
	HasRecipe          bool
	HasCreds           bool
	IsGuided           bool // steps match the canonical guided shape → prefill guided form
	LoginURL           string
	UsernameSelector   string
	PasswordSelector   string
	SubmitSelector     string
	SuccessSelector    string
	SuccessURLContains string
	SuccessTimeoutMs   int
	StepsJSON          string // canonical steps (pretty) for the advanced editor
}

// LoginTestVM is the result of a login test (login-test route).
type LoginTestVM struct {
	OK            bool
	EndURL        string
	FailReason    string
	ScreenshotURL string
	ScreenshotKey string
	Err           string // infra error (couldn't run at all)
}

// AuthSection renders the Authentication config body (the accordion supplies the
// section title + card container). When the feature is disabled it renders nothing
// (the target simply crawls unauthenticated).
func AuthSection(vm *AuthVM) g.Node {
	if vm == nil || !vm.Enabled {
		return g.Text("")
	}
	isLogin := vm.Mode == "login"

	var badge g.Node = g.Text("")
	if isLogin && vm.HasRecipe {
		badge = h.Div(h.Class("flex justify-end"),
			h.Span(h.Class("badge-success"), g.Text("Login recipe set")))
	}

	intro := h.P(h.Class("mb-4 text-sm text-muted"),
		g.Text("Crawl gated pages by logging in first. Same-domain form login only — third-party SSO is not supported. Credentials are encrypted at rest and never shown again."))

	// Login-recipe body (guided form + advanced + credentials). Progressive
	// disclosure: hidden by default, revealed only when the "Login recipe" radio
	// is chosen (see wireAuthMode in app.js). "No authentication" shows just the
	// two radios + the explainer — not a wall of selector fields.
	recipeBodyClass := "space-y-4"
	if !isLogin {
		recipeBodyClass += " hidden"
	}
	recipeBody := h.Div(
		g.Attr("data-auth-recipe", ""),
		h.Class(recipeBodyClass),
		guidedFields(vm),
		advancedEditor(vm),
		credentialFields(vm),
	)

	form := h.Form(
		g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/auth"),
		g.Attr("hx-swap", "none"),
		g.Attr("hx-boost", "false"),
		g.Attr("data-auth-form", ""),
		h.Class("space-y-4"),

		// Auth mode toggle.
		h.Div(h.Class("flex items-center gap-4"),
			radio("auth_mode", "none", "No authentication", !isLogin),
			radio("auth_mode", "login", "Login recipe", isLogin),
		),

		recipeBody,

		h.Div(h.Class("flex items-center gap-3"),
			h.Button(h.Type("submit"),
				h.Class("btn-secondary text-sm"),
				g.Text("Save authentication")),
			g.If(isLogin && vm.HasRecipe,
				h.Button(h.Type("button"),
					g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/login-test"),
					g.Attr("hx-target", "#login-test-result"),
					g.Attr("hx-swap", "innerHTML"),
					g.Attr("hx-boost", "false"),
					h.Class("rounded border border-line px-4 py-2 text-sm font-medium text-ink hover:bg-surface"),
					g.Text("Test login")),
			),
		),
	)

	return h.Div(h.Class("space-y-4"),
		badge,
		intro,
		form,
		h.Div(h.ID("login-test-result"), h.Class("mt-4")),
	)
}

func guidedFields(vm *AuthVM) g.Node {
	return h.Div(h.Class("space-y-3 rounded-lg border border-line p-4"),
		h.Div(h.Class("flex items-center gap-2"),
			radio("recipe_mode", "guided", "Guided form", !vm.HasRecipe || vm.IsGuided),
			radio("recipe_mode", "advanced", "Advanced (edit steps)", vm.HasRecipe && !vm.IsGuided),
		),
		h.Div(h.Class("grid gap-3 md:grid-cols-2"),
			authField("login_url", "Login page URL", "https://app.example.com/login", vm.LoginURL),
			authField("submit_selector", "Submit button selector", "button[type=submit]", vm.SubmitSelector),
			authField("username_selector", "Username field selector", "#email", vm.UsernameSelector),
			authField("password_selector", "Password field selector", "#password", vm.PasswordSelector),
			authField("success_selector", "Success element selector (optional)", "nav.dashboard", vm.SuccessSelector),
			authField("success_url_contains", "…or success URL contains (optional)", "/dashboard", vm.SuccessURLContains),
			authField("success_timeout_ms", "Success timeout (ms)", "15000", timeoutStr(vm.SuccessTimeoutMs)),
		),
		h.P(h.Class("text-xs text-muted"), g.Text("Set a success element OR a URL substring so a failed login is detected instead of crawling the login wall.")),
	)
}

func advancedEditor(vm *AuthVM) g.Node {
	return h.Details(h.Class("rounded-lg border border-line p-4"),
		h.Summary(h.Class("cursor-pointer text-sm font-medium"), g.Text("Advanced — edit canonical steps (JSON)")),
		h.P(h.Class("mt-2 text-xs text-muted"),
			g.Text(`Step types: goto{"url"}, fill{"selector","value_ref":"username|password"}, click{"selector"}, waitFor{"selector"|"url_contains","timeout_ms"}. No script/eval steps. Select "Advanced (edit steps)" above to save this instead of the guided form.`)),
		h.Textarea(h.Name("steps_json"),
			h.Class("mt-2 h-56 w-full resize-y rounded border border-line bg-surface p-2 font-mono text-xs"),
			g.Text(vm.StepsJSON)),
	)
}

func credentialFields(vm *AuthVM) g.Node {
	userPH, passPH := "username", "password"
	if vm.HasCreds {
		userPH, passPH = "•••• (set — leave blank to keep)", "•••• (set — leave blank to keep)"
	}
	return h.Div(h.Class("grid gap-3 md:grid-cols-2"),
		h.Div(
			h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Username / email")),
			h.Input(h.Type("text"), h.Name("username"), h.AutoComplete("off"),
				h.Placeholder(userPH),
				h.Class("w-full rounded border border-line bg-surface p-2 text-sm")),
		),
		h.Div(
			h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text("Password")),
			h.Input(h.Type("password"), h.Name("password"), h.AutoComplete("new-password"),
				h.Placeholder(passPH),
				h.Class("w-full rounded border border-line bg-surface p-2 text-sm")),
		),
	)
}

// LoginTestResult renders the login-test outcome fragment (swapped into
// #login-test-result).
func LoginTestResult(vm *LoginTestVM) g.Node {
	if vm == nil {
		return g.Text("")
	}
	if vm.Err != "" {
		return h.Div(h.Class("rounded border border-warning/30 bg-warning/10 p-3 text-sm text-warning-fg"),
			g.Text(vm.Err))
	}
	var head g.Node
	if vm.OK {
		head = h.Div(h.Class("rounded border border-success/30 bg-success/10 p-3 text-sm text-success-fg"),
			h.P(h.Class("font-medium"), g.Text("Login succeeded")),
			g.If(vm.EndURL != "", h.P(h.Class("mt-1 break-all text-xs"), g.Text("Landed on: "+vm.EndURL))),
		)
	} else {
		head = h.Div(h.Class("rounded border border-danger/30 bg-danger/10 p-3 text-sm text-danger-fg"),
			h.P(h.Class("font-medium"), g.Text("Login failed")),
			g.If(vm.FailReason != "", h.P(h.Class("mt-1"), g.Text(vm.FailReason))),
			g.If(vm.EndURL != "", h.P(h.Class("mt-1 break-all text-xs"), g.Text("Ended on: "+vm.EndURL))),
		)
	}
	var shot g.Node = g.Text("")
	if vm.ScreenshotURL != "" {
		shot = h.Div(h.Class("mt-3"),
			h.P(h.Class("mb-1 text-xs text-muted"), g.Text("End-state screenshot")),
			h.Img(h.Src(vm.ScreenshotURL), h.Alt("Login test end state"),
				h.Class("max-h-96 w-auto rounded border border-line")),
		)
	}
	return h.Div(h.Class("space-y-2"), head, shot)
}

// --- small form helpers ---

func radio(name, val, label string, checked bool) g.Node {
	attrs := []g.Node{h.Type("radio"), h.Name(name), h.Value(val), h.Class("accent-brand")}
	if checked {
		attrs = append(attrs, h.Checked())
	}
	return h.Label(h.Class("flex items-center gap-2 text-sm"),
		h.Input(attrs...), h.Span(g.Text(label)))
}

func authField(name, label, placeholder, val string) g.Node {
	return h.Div(
		h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text(label)),
		h.Input(h.Type("text"), h.Name(name), h.Value(val), h.Placeholder(placeholder),
			h.AutoComplete("off"),
			h.Class("w-full rounded border border-line bg-surface p-2 font-mono text-xs")),
	)
}

func timeoutStr(ms int) string {
	if ms <= 0 {
		return ""
	}
	return strconv.Itoa(ms)
}
