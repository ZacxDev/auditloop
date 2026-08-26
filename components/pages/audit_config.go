package pages

import (
	"strconv"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// AuditConfigVM is the persona-walkthrough Phase-2 per-target audit-config view
// model. The whole card is gated on Enabled (OpenRouterEnabled) — the same gate as
// the evaluate section, since the primary value (auto-infer + grounded personas)
// needs the LLM. ProductSummary/PrimaryJob/PrimaryCTA are model-authored on infer
// → always rendered escaped (g.Text). Personas carries the curated set with a
// per-persona Checked flag reflecting the stored config.
type AuditConfigVM struct {
	TargetID       string
	Enabled        bool
	HasConfig      bool
	HasDoneRun     bool // an "Infer from latest run" needs ≥1 completed run
	Inferred       bool
	Confirmed      bool
	ProductSummary string
	PrimaryJob     string
	PrimaryCTA     string
	Personas       []AuditPersonaVM
	// Phase-3 goal-directed DRIVER controls. DrivingEnabled + AllowRealSubmit are
	// the two loud, default-OFF opt-ins; the Success* fields are the deterministic
	// success condition the driver observes.
	DrivingEnabled     bool
	AllowRealSubmit    bool
	SuccessSelector    string
	SuccessURLContains string
	SuccessTimeoutMs   int
}

// AuditPersonaVM is one curated persona checkbox in the config card.
type AuditPersonaVM struct {
	ID      string
	Label   string
	Checked bool
}

// AuditConfigSection renders the "Audit configuration" body (id
// audit-config-section; the accordion supplies the section title + card container).
// Returns an empty node when the feature is disabled (no OpenRouter key). The infer
// + save routes return this same fragment for in-place swap — so the status badge
// lives INSIDE this fragment and stays fresh across swaps.
func AuditConfigSection(vm *AuditConfigVM) g.Node {
	if vm == nil || !vm.Enabled {
		return h.Div(h.ID("audit-config-section"))
	}

	var badge g.Node = g.Text("")
	if vm.HasConfig && vm.Inferred && !vm.Confirmed {
		badge = h.Div(h.Class("flex justify-end"),
			h.Span(h.Class("badge-warning"), g.Text("inferred — review & confirm")))
	} else if vm.HasConfig && vm.Confirmed {
		badge = h.Div(h.Class("flex justify-end"),
			h.Span(h.Class("badge-success"), g.Text("confirmed")))
	}

	intro := h.P(h.Class("mb-4 text-sm text-muted"),
		g.Text("The audit goal + audiences for this target. Auto-inferred from a completed crawl, then confirm/edit. The persona walkthrough defaults its task + personas from this."))

	var body g.Node
	if !vm.HasConfig {
		body = auditConfigEmpty(vm)
	} else {
		body = auditConfigForm(vm)
	}

	return h.Div(h.ID("audit-config-section"), h.Class("space-y-4"),
		badge,
		intro,
		body,
	)
}

// auditConfigEmpty is the no-config state: an infer button (disabled with a hint
// when the target has no completed run yet).
func auditConfigEmpty(vm *AuditConfigVM) g.Node {
	if !vm.HasDoneRun {
		return h.Div(h.Class("space-y-2"),
			h.P(h.Class("text-sm text-muted"), g.Text("No audit configuration yet.")),
			h.Button(h.Type("button"), h.Disabled(),
				h.Class("rounded bg-surface px-4 py-2 text-sm font-medium text-muted cursor-not-allowed"),
				g.Text("Infer from latest run")),
			h.P(h.Class("text-xs text-muted"), g.Text("Run an audit first — inference reads a completed crawl's landing page + pages.")),
		)
	}
	return h.Div(h.Class("space-y-2"),
		h.P(h.Class("text-sm text-muted"), g.Text("No audit configuration yet. Infer a draft from the latest completed run, then review it.")),
		inferButton(vm, "Infer from latest run"),
	)
}

// auditConfigForm is the editable config: text fields + persona checkboxes + Save
// (confirm) + Re-infer.
func auditConfigForm(vm *AuditConfigVM) g.Node {
	checks := make([]g.Node, 0, len(vm.Personas))
	for _, p := range vm.Personas {
		attrs := []g.Node{h.Type("checkbox"), h.Name("personas"), h.Value(p.ID), h.Class("mt-0.5 accent-brand")}
		if p.Checked {
			attrs = append(attrs, h.Checked())
		}
		checks = append(checks, h.Label(h.Class("flex items-start gap-2 text-sm"),
			h.Input(attrs...),
			h.Span(h.Class("font-medium"), g.Text(p.Label))))
	}

	form := h.Form(
		g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/audit-config"),
		g.Attr("hx-target", "#audit-config-section"),
		g.Attr("hx-swap", "outerHTML"),
		g.Attr("hx-boost", "false"),
		h.Class("space-y-4"),

		auditTextField("product_summary", "Product summary", "What this product is / does", vm.ProductSummary),
		auditTextField("primary_job", "Primary job / task", "e.g. sign up and create a first project", vm.PrimaryJob),
		auditTextField("primary_cta", "Primary call-to-action", "e.g. Sign up", vm.PrimaryCTA),

		h.Div(
			h.P(h.Class("mb-1 text-xs font-semibold text-muted"), g.Text("Audiences (personas the walkthrough defaults to)")),
			h.Div(h.Class("space-y-1.5"), g.Group(checks)),
		),

		drivingControls(vm),

		h.Div(h.Class("flex items-center gap-3"),
			h.Button(h.Type("submit"),
				h.Class("btn-secondary text-sm"),
				g.Text("Save configuration")),
			g.If(vm.HasDoneRun, inferButton(vm, "Re-infer")),
		),
	)
	return form
}

// inferButton posts the infer route and swaps the whole section in place.
func inferButton(vm *AuditConfigVM, label string) g.Node {
	return h.Button(h.Type("button"),
		g.Attr("hx-post", "/api/targets/"+vm.TargetID+"/audit-config/infer"),
		g.Attr("hx-target", "#audit-config-section"),
		g.Attr("hx-swap", "outerHTML"),
		g.Attr("hx-boost", "false"),
		h.Class("rounded border border-line px-4 py-2 text-sm font-medium text-ink hover:bg-surface"),
		g.Text(label))
}

// drivingControls renders the Phase-3 goal-directed DRIVER settings: the two loud,
// default-off opt-ins (driving_enabled, allow_real_submit) and the deterministic
// success condition. Guidance copy foregrounds the safety model.
func drivingControls(vm *AuditConfigVM) g.Node {
	drivingAttrs := []g.Node{h.Type("checkbox"), h.Name("driving_enabled"), h.Value("1"), h.Class("mt-0.5 accent-brand")}
	if vm.DrivingEnabled {
		drivingAttrs = append(drivingAttrs, h.Checked())
	}
	realAttrs := []g.Node{h.Type("checkbox"), h.Name("allow_real_submit"), h.Value("1"), h.Class("mt-0.5 accent-danger")}
	if vm.AllowRealSubmit {
		realAttrs = append(realAttrs, h.Checked())
	}
	to := ""
	if vm.SuccessTimeoutMs > 0 {
		to = strconv.Itoa(vm.SuccessTimeoutMs)
	}
	return h.Div(h.Class("space-y-3 rounded border border-line bg-surface/40 p-3"),
		h.P(h.Class("text-xs font-semibold text-muted uppercase tracking-wide"), g.Text("Goal-directed walkthrough (drive to goal)")),
		h.Label(h.Class("flex items-start gap-2 text-sm"),
			h.Input(drivingAttrs...),
			h.Span(h.Span(h.Class("font-medium"), g.Text("Enable driving")),
				h.Span(h.Class("block text-xs text-muted"), g.Text("Let auditloop drive this site toward the goal (headless). Off by default. Use a dedicated/disposable test account for authenticated driving.")))),
		h.Label(h.Class("flex items-start gap-2 text-sm"),
			h.Input(realAttrs...),
			h.Span(h.Span(h.Class("font-medium text-danger-fg"), g.Text("Allow real form submissions (mutates live data — off = dry-run)")),
				h.Span(h.Class("block text-xs text-muted"), g.Text("Off (default): mutating requests (POST/PUT/PATCH/DELETE) are blocked at the network layer — the driver explores without submitting. On: real submissions run. Point at STAGING for real-submit.")))),
		h.Div(h.Class("grid gap-3 sm:grid-cols-3"),
			auditTextField("success_selector", "Success: selector", "e.g. #signup-complete", vm.SuccessSelector),
			auditTextField("success_url_contains", "Success: URL contains", "e.g. /welcome", vm.SuccessURLContains),
			smallNumberField("success_timeout_ms", "Success: timeout (ms)", "8000", to),
		),
		h.P(h.Class("text-xs text-muted"), g.Text("The goal is reached when the selector appears OR the URL contains that substring, within the timeout. This deterministic check — not the model's word — decides success.")),
	)
}

func smallNumberField(name, label, placeholder, val string) g.Node {
	return h.Div(
		h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text(label)),
		h.Input(h.Type("number"), h.Name(name), h.Value(val), h.Placeholder(placeholder),
			h.Min("0"), h.AutoComplete("off"),
			h.Class("w-full rounded border border-line bg-surface p-2 text-sm")),
	)
}

func auditTextField(name, label, placeholder, val string) g.Node {
	return h.Div(
		h.Label(h.Class("mb-1 block text-xs font-medium text-muted"), g.Text(label)),
		h.Input(h.Type("text"), h.Name(name), h.Value(val), h.Placeholder(placeholder),
			h.AutoComplete("off"),
			h.Class("w-full rounded border border-line bg-surface p-2 text-sm")),
	)
}
