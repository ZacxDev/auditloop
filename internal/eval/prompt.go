// Package eval runs the task-grounded PERSONA WALKTHROUGH evaluator (Phase 1): a
// parallel, opt-in LLM pass (separate from the P3 subjective-visual notes) that
// evaluates a completed run's ALREADY-CAPTURED pages as a task + persona
// walkthrough and emits STRUCTURED, selector-anchored, actionable findings plus a
// synthesized run-level "story".
//
// Phase 1 does NOT drive the app and does NOT infer goals — it reasons over the
// captured screenshots + the run's DETERMINISTIC signals (a11y rule ids, perf
// ratings, layout smells, console/network counts) as GROUNDING, so the model
// reasons about task completion for a given persona rather than re-listing the
// signals (those are captured deterministically and shown separately).
//
// Axis of variation is the PERSONA, not the model: Phase 1 uses ONE model (the
// first curated model in AUDITLOOP_LLM_MODELS) across all personas. The pass runs
// per (page × persona) with flow context, then a per-cell VERIFICATION pass
// (anti-vagueness) keeps only substantiated findings, then ONE run-level SYNTHESIS
// pass ranks the top improvements.
package eval

import (
	"fmt"
	"strings"

	"github.com/ZacxDev/auditloop/internal/report"
)

// Persona is one curated evaluation lens. The set is CLOSED and defined in code
// (documented, reviewable) — like notes.SystemPrompt — and validated server-side
// against user input. Cares is embedded verbatim into the prompt so the model
// evaluates the page through that persona's concerns.
type Persona struct {
	ID    string
	Label string
	Cares string
}

// Personas is the curated Phase-1 persona set (exactly four). The IDs are the
// stable contract used by the API + DB.
var Personas = []Persona{
	{
		ID:    "first-time-nontechnical",
		Label: "First-time, non-technical visitor",
		Cares: "a first-time, non-technical visitor who has never seen this product. They care about: what is this, is it trustworthy, what do I do first? They are easily confused by jargon, overwhelmed by density, and afraid of committing (signing up, paying, sharing data) before they understand the value.",
	},
	{
		ID:    "returning-power-user",
		Label: "Returning power user",
		Cares: "a frequent, expert user who uses this product often. They care about efficiency, information density, keyboard shortcuts, and fast paths to the thing they came to do. They resent hand-holding, redundant confirmations, and hidden/slow navigation that a novice-first design imposes on them.",
	},
	{
		ID:    "skeptical-evaluator",
		Label: "Skeptical evaluator",
		Cares: "a comparison-shopping evaluator deciding whether to commit versus a competitor. They care about credibility, concrete proof (social proof, evidence, specifics over adjectives), pricing and claims clarity, and risk (what happens if this goes wrong, can I get out). Vague marketing, missing pricing, and unsubstantiated claims make them bounce.",
	},
	{
		ID:    "accessibility-constrained",
		Label: "Accessibility-constrained user",
		Cares: "a user operating with a constraint — low vision, keyboard-only, screen-reader, or motor difficulty. They care about whether the page is PERCEIVABLE and OPERABLE from a lived-experience angle: can they find and reach the primary action, is focus order sensible, are things distinguishable and reachable without a mouse. Judge the LIVED experience, complementing (not repeating) the mechanical axe checks shown as grounding.",
	},
}

// PersonaByID returns the curated persona with the given id (ok=false if unknown).
func PersonaByID(id string) (Persona, bool) {
	for _, p := range Personas {
		if p.ID == id {
			return p, true
		}
	}
	return Persona{}, false
}

// PersonaAllowed reports whether id is one of the curated personas.
func PersonaAllowed(id string) bool {
	_, ok := PersonaByID(id)
	return ok
}

// PersonaLabel returns the human label for an id (falls back to the id).
func PersonaLabel(id string) string {
	if p, ok := PersonaByID(id); ok {
		return p.Label
	}
	return id
}

// GenSystemPrompt is the generation-pass role/instruction prompt. It forces a
// strict JSON response and explicitly forbids re-listing the deterministic signals
// (they are grounding, not findings to echo).
const GenSystemPrompt = `You are a rigorous UX evaluator running a TASK-GROUNDED PERSONA WALKTHROUGH of a web page.
You are given full-page screenshots of ONE page (desktop and mobile viewports), the persona you must
embody, the task/job the visitor is trying to complete, this page's position in the flow (step N of M,
with the previous/next page URLs), and the page's already-computed DETERMINISTIC signals (accessibility
rule ids, performance ratings, layout smells, console/network error counts).

Walk the page AS THAT PERSONA trying to complete that task at this step. Decide:
- comprehension: can this persona tell what to do next here? "clear" | "unclear" | "blocked".
- blockers: things that STOP this persona from completing the task at this step.
- frictions: things that slow, annoy, or erode confidence but do NOT stop them.
- top_fix: the single highest-leverage change to make on THIS page for THIS persona.

Anchor every blocker/friction to a concrete on-page element with a CSS selector (best effort) and a short
evidence string quoting the visible text/label/state you are reacting to. Be specific — no vague advice.

CRITICAL RULES:
- The deterministic signals are GROUNDING for your reasoning about task completion. Do NOT re-list or
  re-narrate them as findings (accessibility violations, console/network errors, perf metrics, layout
  smells are captured automatically and shown separately — echoing them is wasted output). Use them only
  to inform whether the persona can complete the task.
- Reason from what is VISIBLE in the screenshots for this specific persona and task. Do not invent copy,
  features, or elements that are not shown.
- The SEMANTIC STRUCTURE block (when present) is derived from the live DOM / accessibility tree and is
  AUTHORITATIVE for questions of label, ARIA name, role, focus, and keyboard-operability. You MUST NOT
  assert that a label, ARIA name, accessible name, role, or keyboard/focus affordance is MISSING based on
  the screenshot — an sr-only label, an anchor styled like a card, or a resolved accessible name is
  invisible in pixels but present in that block. If the block shows an element has a resolved accessible
  name, or is a link/button, or is focusable, DEFER to it — do not claim otherwise. Mechanical
  accessibility (labels/roles/contrast/focus) is MEASURED by axe and shown as grounding; do not re-derive
  it from pixels. Your remit is the LIVED experience: comprehension, ordering, wait-state anxiety, and
  whether the primary action is discoverable and reachable.
- Respond with ONLY a single JSON object, no prose, no markdown fences, matching EXACTLY this shape:
  {"comprehension":"clear|unclear|blocked",
   "blockers":[{"issue":"...","selector":"...","evidence":"..."}],
   "frictions":[{"issue":"...","selector":"...","evidence":"..."}],
   "top_fix":{"selector":"...","change":"...","rationale":"...","impact":"high|medium|low"}}
- If the page is clear and blocker-free for this persona, return empty blockers/frictions and say so via
  comprehension "clear"; still provide the single best refinement as top_fix.`

// VerifySystemPrompt is the anti-vagueness verification pass. It re-reads the same
// screenshots plus the drafted findings and returns ONLY the substantiated ones,
// each marked verified. Bounds cost to ONE extra call per cell.
const VerifySystemPrompt = `You are a strict fact-checker for a UX evaluation. You are given the SAME page screenshots
(desktop + mobile) and a DRAFT set of findings (blockers, frictions, and a top_fix) another evaluator produced.

For EACH drafted blocker and friction, verify it against what is ACTUALLY VISIBLE in the screenshots:
point to the specific element/text that substantiates it. DROP any finding you cannot substantiate from the
screenshots (speculation, generic best-practice advice not tied to a visible element, or an invented element).

The SEMANTIC STRUCTURE block (when present) is derived from the live DOM / accessibility tree and is
AUTHORITATIVE for label/ARIA-name/role/focus/keyboard questions. DROP any finding that claims a label, ARIA
name, role, or keyboard/focus affordance is MISSING when that block shows it is present (a resolved
accessible name, a link/button element, or a focusable element) — those facts are invisible in a screenshot.
Do not re-derive mechanical accessibility from pixels; keep only findings about the LIVED experience.

Respond with ONLY a single JSON object, no prose, no markdown fences, matching EXACTLY this shape:
  {"comprehension":"clear|unclear|blocked",
   "blockers":[{"issue":"...","selector":"...","evidence":"...","verified":true}],
   "frictions":[{"issue":"...","selector":"...","evidence":"...","verified":true}],
   "top_fix":{"selector":"...","change":"...","rationale":"...","impact":"high|medium|low"}}
Keep comprehension and top_fix from the draft (adjust only if the evidence contradicts them). Include ONLY
findings you verified, each with "verified":true. If nothing is substantiated, return empty blockers/frictions.`

// SynthSystemPrompt is the run-level synthesis pass: rank the top improvements
// across all pages+personas into a holistic "story".
const SynthSystemPrompt = `You are a product lead synthesizing a UX audit. You are given the VERIFIED per-(page,persona)
findings from a walkthrough of a whole flow toward a task. Produce a RANKED list of the most important, highest-leverage
improvements for the WHOLE flow — the story of where this journey breaks down and what to fix first.

Rank by impact on task completion across personas. Merge duplicates across pages/personas into one item. Cap the list at
8 items. Respond with ONLY a single JSON object, no prose, no markdown fences, matching EXACTLY this shape:
  {"improvements":[{"title":"...","rationale":"...","impact":"high|medium|low",
    "affected_urls":["..."],"affected_personas":["..."],"selector":"..."}]}
If there is nothing material to fix, return an empty improvements array.`

// MaxSynthItems caps the synthesis list length defensively (the prompt also asks
// for ≤8).
const MaxSynthItems = 8

// MaxSynthCells bounds how many per-(page,persona) verdicts are fed INTO the
// synthesis prompt on a large run (e.g. 50 pages × 4 personas = 200 cells), so the
// synthesis prompt-token cost + context size stay bounded. Cells are truncated in
// flow order (entry funnel first, where drop-off matters most).
const MaxSynthCells = 120

// Grounding is the per-page context fed to the model alongside the screenshots. It
// carries the flow position, the task/job, and the page's deterministic signals —
// so the model reasons about task completion, NOT so it re-lists the signals.
type Grounding struct {
	URL       string
	Viewports []string
	Job       string // free-text task/job the walkthrough evaluates toward
	FlowPos   int    // 1-based step index in the flow
	FlowTotal int    // total steps in the flow
	PrevURL   string
	NextURL   string
	// Deterministic signals (grounding only — never to be re-listed as findings).
	A11yRuleIDs  []string
	A11yCount    int
	ConsoleFirst int
	ConsoleThird int
	NetworkFirst int
	NetworkThird int
	Perf         *report.Perf
	LayoutSmells []string
	// A11yRuleNodes maps an axe rule id → up to a few offending node selectors (from
	// the stored axe violation detail). Surfaces WHERE each mechanical a11y issue is,
	// so the persona doesn't guess selectors. Empty when no digest/axe detail.
	A11yRuleNodes map[string][]string
	// A11yDigest is the page's DOM/accessibility digest — the AUTHORITATIVE semantic
	// facts (labels, roles, accessible names, focusability) a screenshot cannot show.
	// nil ⇒ no digest (pre-0060 / pushed run) ⇒ screenshot-only, no semantic block,
	// and the deterministic verify never drops a finding.
	A11yDigest *report.A11yDigest
}

// GenUserPrompt builds the per-(page,persona) generation user prompt.
func (g Grounding) GenUserPrompt(p Persona) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PERSONA: %s\n", p.Cares)
	job := strings.TrimSpace(g.Job)
	if job == "" {
		job = "explore the product and decide whether to proceed"
	}
	fmt.Fprintf(&b, "\nTASK / JOB: %s\n", job)
	if g.FlowTotal > 0 {
		fmt.Fprintf(&b, "\nFLOW POSITION: this is step %d of %d toward the task.\n", g.FlowPos, g.FlowTotal)
	}
	fmt.Fprintf(&b, "This page URL: %s\n", g.URL)
	if g.PrevURL != "" {
		fmt.Fprintf(&b, "Previous step URL: %s\n", g.PrevURL)
	}
	if g.NextURL != "" {
		fmt.Fprintf(&b, "Next step URL: %s\n", g.NextURL)
	}
	if len(g.Viewports) > 0 {
		fmt.Fprintf(&b, "Viewports shown: %s\n", strings.Join(g.Viewports, ", "))
	}

	b.WriteString("\nDETERMINISTIC SIGNALS (grounding only — do NOT re-list these as findings):\n")
	if g.A11yCount > 0 {
		fmt.Fprintf(&b, "- accessibility: %d axe violation(s)", g.A11yCount)
		if len(g.A11yRuleIDs) > 0 {
			fmt.Fprintf(&b, " [rules: %s]", strings.Join(g.a11yRuleLabels(), ", "))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- accessibility: no axe violations detected\n")
	}
	fmt.Fprintf(&b, "- console errors: %d first-party, %d third-party\n", g.ConsoleFirst, g.ConsoleThird)
	fmt.Fprintf(&b, "- network errors: %d first-party, %d third-party\n", g.NetworkFirst, g.NetworkThird)
	if g.Perf != nil {
		p := g.Perf
		fmt.Fprintf(&b, "- perf: LCP %dms (%s), CLS %.3f (%s), TBT~ %dms (%s, lab proxy), weight %d bytes, %d requests\n",
			p.LCPMs, report.Rating(float64(p.LCPMs), report.LCPGoodMs, report.LCPPoorMs),
			p.CLS, report.Rating(p.CLS, report.CLSGood, report.CLSPoor),
			p.TBTMs, report.Rating(float64(p.TBTMs), report.TBTGoodMs, report.TBTPoorMs),
			p.WeightBytes, p.ReqCount)
	}
	if len(g.LayoutSmells) > 0 {
		fmt.Fprintf(&b, "- layout smells: %s\n", strings.Join(g.LayoutSmells, ", "))
	}

	b.WriteString(g.semanticBlock())

	b.WriteString("\nWalk this page as the persona and return the JSON verdict now.")
	return b.String()
}

// a11yRuleLabels renders each axe rule id, appending up to a few offending node
// selectors when known (e.g. "label (input#email), color-contrast (.btn)").
func (g Grounding) a11yRuleLabels() []string {
	out := make([]string, 0, len(g.A11yRuleIDs))
	for _, id := range g.A11yRuleIDs {
		if sels := g.A11yRuleNodes[id]; len(sels) > 0 {
			// %q on the joined selectors: these come from the stored axe violation
			// detail (nodes[].target), which on a PUSHED run is the producer's raw axe
			// object stored verbatim by plugin.MapPage — i.e. untrusted, uncapped text
			// interpolated into the prompt. Unquoted, an embedded newline forges its own
			// instruction line, the same steer the semantic block was hardened against.
			out = append(out, fmt.Sprintf("%s (%q)", id, strings.Join(sels, ", ")))
		} else {
			out = append(out, id)
		}
	}
	return out
}

// semanticBlock renders the AUTHORITATIVE DOM/accessibility digest as compact lines,
// or "" when no digest is present (pre-0060 / pushed run → screenshot-only behavior,
// backward-compatible). Both the generation and verification prompts include it.
func (g Grounding) semanticBlock() string {
	if g.A11yDigest.IsEmpty() {
		return ""
	}
	d := g.A11yDigest
	var b strings.Builder
	b.WriteString("\nSEMANTIC STRUCTURE (from the DOM/accessibility tree — AUTHORITATIVE for label/role/focus/keyboard questions):\n")
	for _, e := range d.Interactive {
		role := e.Role
		if role == "" {
			role = defaultRole(e.Tag)
		}
		// %q on the page/producer-authored selector + role: this text is UNTRUSTED and
		// lands inside a block the prompt declares AUTHORITATIVE, so an embedded newline
		// would let it forge its own instruction lines — a steer that bypasses the
		// deterministic gate entirely. Quoting escapes newlines/quotes (and the pushed
		// path also strips control characters at ingest). Same rule the accessible name
		// and landmark text already follow.
		fmt.Fprintf(&b, "- %q — role=%q", elemLabel(e.Tag, e.Selector), role)
		if e.AccessibleName != "" {
			fmt.Fprintf(&b, `, accessible name %q`, e.AccessibleName)
		} else {
			b.WriteString(", NO accessible name")
		}
		if e.Focusable {
			b.WriteString(", focusable")
		} else {
			b.WriteString(", not focusable")
		}
		if e.Disabled {
			b.WriteString(", disabled")
		}
		b.WriteString("\n")
	}
	if len(d.FormControls) > 0 {
		b.WriteString("Form controls (label association is AUTHORITATIVE — a present label may be visually hidden):\n")
		for _, c := range d.FormControls {
			// %q on the untrusted selector — see the interactive loop above.
			if c.HasLabel {
				fmt.Fprintf(&b, "- %q — labelled %q (via %s)\n", c.Selector, c.AccessibleName, labelSourceLabel(c.LabelSource))
			} else if c.LabelSource == "placeholder" && c.AccessibleName != "" {
				fmt.Fprintf(&b, "- %q — NO programmatic label (only a placeholder %q)\n", c.Selector, c.AccessibleName)
			} else {
				fmt.Fprintf(&b, "- %q — NO programmatic label (labelSource: none)\n", c.Selector)
			}
		}
	}
	if len(d.Landmarks) > 0 {
		var parts []string
		for _, l := range d.Landmarks {
			// %q on the untrusted tag/role too — l.Text was already quoted.
			label := fmt.Sprintf("%q", l.Tag)
			if l.Role != "" {
				label = fmt.Sprintf("%q", l.Role)
			}
			if l.Text != "" {
				label = fmt.Sprintf("%s %q", label, l.Text)
			}
			parts = append(parts, label)
		}
		fmt.Fprintf(&b, "Page structure: %s\n", strings.Join(parts, " › "))
	}
	// Emit the rule ONLY where the gate will actually enforce it — the same predicate,
	// not a lookalike condition. The rule tells the model an unlisted-element a11y claim
	// "is discarded automatically"; on a page where that is false (a landmarks-only
	// digest, or any list at its cap, where the gate must not drop) saying it makes the
	// model self-censor for nothing.
	if digestVocabularyComplete(d) {
		b.WriteString(selectorRule)
	}
	return b.String()
}

// selectorRule constrains the model's SELECTOR VOCABULARY to the digest's own list.
// The evaluator reasons from PIXELS, so it cannot know a real id and — left alone —
// invents an anchor (`h3:contains('…')`, a placeholder attribute, a class). An
// invented anchor is unverifiable by construction, which is exactly what left the
// captured digest decorative: measured on a real pushed run, 4 of 4 objective a11y
// claims cited invented anchors, so the deterministic gate had nothing to look up.
//
// This text is a CONVENIENCE for the model; groundSelectors (ground.go) is what makes
// it true — a mechanical a11y claim citing an unlisted selector is re-anchored onto
// the digest's own selector or dropped, whether or not the model complied. It is
// emitted only when a digest exists, so digest-less runs are untouched.
const selectorRule = "SELECTOR RULE: the elements listed above are the ONLY ones you may cite by selector. " +
	"When a finding is about one of them, copy its selector VERBATIM from this list — never invent, " +
	"abbreviate or construct a selector (no :contains(), no placeholder/class guesses). " +
	"A finding asserting a MECHANICAL accessibility fact (a missing label or accessible name, or that " +
	"something is not keyboard-operable) about an element NOT in this list is discarded automatically, " +
	"so only make such a claim about a listed element — mechanical accessibility is already measured " +
	"deterministically and reported separately. Everything else (what confuses, blocks or slows this " +
	"persona) is your job and is judged on the screenshots.\n"

// elemLabel prefers the selector (it is concrete) but falls back to the tag.
func elemLabel(tag, selector string) string {
	if selector != "" {
		return selector
	}
	return tag
}

// defaultRole gives the implicit ARIA role for a few common tags (grounding only).
func defaultRole(tag string) string {
	switch tag {
	case "a":
		return "link"
	case "button":
		return "button"
	case "input", "textarea":
		return "textbox"
	case "select":
		return "combobox"
	default:
		return tag
	}
}

func labelSourceLabel(src string) string {
	switch src {
	case "for":
		return "<label for>"
	case "wrapping-label":
		return "wrapping <label>"
	case "aria-label":
		return "aria-label"
	case "aria-labelledby":
		return "aria-labelledby"
	default:
		return src
	}
}

// VerifyUserPrompt builds the verification-pass user prompt from a drafted verdict.
func VerifyUserPrompt(g Grounding, p Persona, draftJSON string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PERSONA: %s\n", p.Cares)
	job := strings.TrimSpace(g.Job)
	if job == "" {
		job = "explore the product and decide whether to proceed"
	}
	fmt.Fprintf(&b, "TASK / JOB: %s\n", job)
	fmt.Fprintf(&b, "Page URL: %s\n", g.URL)
	b.WriteString(g.semanticBlock())
	b.WriteString("\nDRAFT FINDINGS to verify against the screenshots:\n")
	b.WriteString(draftJSON)
	b.WriteString("\n\nReturn ONLY the substantiated findings as the JSON object described.")
	return b.String()
}

// SynthUserPrompt builds the run-level synthesis prompt from the per-cell verdicts.
func SynthUserPrompt(job string, cells []SynthCell) string {
	var b strings.Builder
	job = strings.TrimSpace(job)
	if job == "" {
		job = "complete the primary journey"
	}
	fmt.Fprintf(&b, "TASK / JOB the flow was evaluated toward: %s\n\n", job)
	b.WriteString("VERIFIED per-(page,persona) findings:\n")
	// Only the SALIENT parts per cell — comprehension, the blockers (what stops the
	// task), and the top_fix. Frictions are intentionally omitted to keep the
	// synthesis prompt bounded on large runs (they rarely change the run-level story).
	for _, c := range cells {
		fmt.Fprintf(&b, "\n# %s — persona: %s (comprehension: %s)\n", c.URL, PersonaLabel(c.Persona), c.Eval.Comprehension)
		for _, f := range c.Eval.Blockers {
			fmt.Fprintf(&b, "  BLOCKER: %s [%s]\n", f.Issue, f.Selector)
		}
		if c.Eval.TopFix != nil && c.Eval.TopFix.Change != "" {
			fmt.Fprintf(&b, "  top_fix (%s): %s\n", c.Eval.TopFix.Impact, c.Eval.TopFix.Change)
		}
	}
	b.WriteString("\nSynthesize the ranked top improvements as the JSON object described.")
	return b.String()
}

// SynthCell is one (page,persona) verdict fed into the synthesis prompt.
type SynthCell struct {
	URL     string
	Persona string
	Eval    report.PageEvaluation
}
