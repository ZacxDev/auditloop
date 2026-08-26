package eval

import (
	"strings"

	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
)

// DETERMINISTIC DOM-GROUNDED VERIFY (scoping-doc §5.2 — the highest-leverage piece).
//
// The persona evaluator is a competent VISIBLE-UX critic but a dangerous a11y critic:
// it can't see an sr-only label, an <a> styled as a card, or a resolved accessible
// name, so it re-derives mechanical accessibility from pixels and gets it wrong (the
// meta-audit measured ~20% precision on objective a11y claims; the "Verify findings"
// LLM pass re-reads the SAME screenshots so it dropped none of them).
//
// This is a pre/around-LLM deterministic gate: for each model-authored finding that
// makes an OBJECTIVE a11y claim (missing label / not keyboard-operable), look its
// selector up in the captured DOM digest and DROP the finding ONLY when the digest
// POSITIVELY REFUTES it — a UNIQUE concrete selector (#id / [name=…]) that resolves to
// an element carrying the very property the finding says is missing. It runs ONLY when
// a non-empty digest exists (pre-0060 + pushed runs degrade to screenshot-only — never
// a drop), and it touches ONLY findings classified as objective a11y claims, so
// subjective/visible-UX findings pass through untouched.
//
// CONSERVATIVE BY DESIGN (over-dropping is worse than the FP it fixes): it NEVER
// infers absence ("selector not in the digest" is never a drop — the digest is capped
// and visibility-filtered, so an absent selector proves nothing), NEVER refutes off a
// weak fact (a bare `focusable`, a placeholder, or textContent-derived name), NEVER
// drops a VISIBLE-label or label-QUALITY complaint, and only indexes positive facts
// under UNIQUE concrete keys (never a shared class selector).

// dropContradicted removes findings the digest deterministically contradicts. It is a
// no-op (returns the evaluation unchanged) when the digest is empty/absent — the
// load-bearing backward-compat guard. It never mutates the input's slices in place.
func dropContradicted(pe report.PageEvaluation, d *report.A11yDigest) report.PageEvaluation {
	if d.IsEmpty() {
		return pe
	}
	idx := buildDigestIndex(d)
	pe.Blockers = filterContradicted(pe.Blockers, idx)
	pe.Frictions = filterContradicted(pe.Frictions, idx)
	// A top_fix carries a model-authored selector/change too and can be the same kind
	// of contradicted a11y FP — clear it (nil renders cleanly in the UI) when refuted.
	if pe.TopFix != nil && idx.contradicts(topFixAsFinding(*pe.TopFix)) {
		metrics.EvalFindingsGated.WithLabelValues("contradicted").Inc()
		pe.TopFix = nil
	}
	return pe
}

// topFixAsFinding adapts a top_fix into an EvalFinding so it runs through the same
// contradiction classifier as blockers/frictions (Change is the actionable claim,
// Rationale the supporting evidence).
func topFixAsFinding(tf report.EvalTopFix) report.EvalFinding {
	return report.EvalFinding{Issue: tf.Change, Selector: tf.Selector, Evidence: tf.Rationale}
}

func filterContradicted(in []report.EvalFinding, idx digestIndex) []report.EvalFinding {
	if len(in) == 0 {
		return in
	}
	out := make([]report.EvalFinding, 0, len(in))
	for _, f := range in {
		if idx.contradicts(f) {
			metrics.EvalFindingsGated.WithLabelValues("contradicted").Inc()
			continue
		}
		out = append(out, f)
	}
	return out
}

// digestElem is the merged view of an element across the interactive + form-control
// lists (a form input appears in both), keyed by its UNIQUE concrete selector.
type digestElem struct {
	tag           string
	hasProgLabel  bool // form control with a PROGRAMMATIC label (for/aria-*/wrapping)
	isFormControl bool
	// Operability facts, populated ONLY from the interactive list (a form-control row
	// carries none). `tag` alone is NOT sufficient to refute a not-operable finding:
	// on the crawl path `tag=a` implies a real `a[href]` because the digest's query IS
	// `a[href]`, but a PUSHED digest has no query behind it and can simply CLAIM
	// tag="a" on an inert element. So the drop additionally requires the digest's own
	// focusable/disabled facts to agree — which the crawl path's JS computes honestly
	// (`tabIndex >= 0 && !disabled`), so this costs the crawl path nothing.
	interactive bool
	focusable   bool
	disabled    bool
}

// operable reports whether the digest's OWN facts say this element is keyboard
// reachable. A claimed <a>/<button> that the same digest says is not focusable (or is
// disabled) is self-contradictory — refute nothing.
func (e digestElem) operable() bool {
	return e.interactive && e.focusable && !e.disabled
}

type digestIndex struct {
	bySelector map[string]digestElem
	nonEmpty   bool
}

// buildDigestIndex indexes each element's positive facts ONLY under its UNIQUE concrete
// keys (#id / [name=…]). A class-only / compound selector is NOT indexed — two distinct
// elements can share such a key (e.g. two `<a>`s under `a.client-card`), and OR-merging
// their facts would risk a too-eager positive refute on a non-unique selector. Only a
// unique concrete anchor can soundly drive a deterministic drop.
func buildDigestIndex(d *report.A11yDigest) digestIndex {
	idx := digestIndex{bySelector: map[string]digestElem{}, nonEmpty: !d.IsEmpty()}
	put := func(sel string, base digestElem) {
		// A selector can yield MORE THAN ONE concrete key (`input#card[name=pwn]` gives
		// both `#card` and `[name=pwn]`). Each key must be merged from the ROW'S OWN
		// facts — hence the `e := base` snapshot per iteration. Mutating a shared `e`
		// across the loop leaked facts merged at the first key into the second, so a row
		// that never claimed anything about `[name=pwn]` could inherit another element's
		// "operable"/"labelled" fact and license a drop on that anchor.
		for _, k := range concreteKeys(sel) {
			e := base
			// Merge onto the SAME concrete key (an <input> appears in both lists, and
			// a form-control row carries the authoritative label fact): prefer positive
			// facts and keep a non-empty tag.
			if ex, ok := idx.bySelector[k]; ok {
				if e.tag == "" {
					e.tag = ex.tag
				}
				e.hasProgLabel = e.hasProgLabel || ex.hasProgLabel
				e.isFormControl = e.isFormControl || ex.isFormControl
				// Operability merges CONSERVATIVELY (unlike the label OR-merge): two
				// interactive rows on one concrete key must BOTH be operable to refute
				// — ambiguity refutes nothing. A form-control row carries no
				// operability facts, so it must not clobber an interactive row's.
				switch {
				case e.interactive && ex.interactive:
					e.focusable = e.focusable && ex.focusable
					e.disabled = e.disabled || ex.disabled
				case ex.interactive:
					e.interactive, e.focusable, e.disabled = true, ex.focusable, ex.disabled
				}
			}
			idx.bySelector[k] = e
		}
	}
	for _, e := range d.Interactive {
		put(e.Selector, digestElem{
			tag: strings.ToLower(strings.TrimSpace(e.Tag)),
			// #36: an interactive element (button/link/widget) with a PROGRAMMATIC label
			// source carries a real, screen-reader-announced accessible name — the same
			// authoritative fact a form control's HasLabel gives. This lets the gate soundly
			// refute an accessible-name-absence finding on an aria-labelled button too (not
			// just a labelled input). A text-content / placeholder / none source is NOT a
			// programmatic label (an icon <button>★</button> has text-content, no real name).
			hasProgLabel: isProgrammaticLabelSource(e.LabelSource),
			interactive:  true,
			focusable:    e.Focusable,
			disabled:     e.Disabled,
		})
	}
	for _, c := range d.FormControls {
		put(c.Selector, digestElem{
			hasProgLabel:  c.HasLabel,
			isFormControl: true,
		})
	}
	return idx
}

// contradicts reports whether the digest deterministically contradicts f. Only findings
// that make an objective a11y claim (missing label / not operable) are ever candidates —
// everything else is left alone — and a drop requires a POSITIVE refutation against a
// unique concrete selector (never absence-inference).
func (idx digestIndex) contradicts(f report.EvalFinding) bool {
	if !idx.nonEmpty {
		return false
	}
	missingLabel := claimsMissingLabel(f)
	notOperable := claimsNotOperable(f)
	if !missingLabel && !notOperable {
		return false // not an objective a11y claim → never a deterministic drop
	}
	el, ok := idx.lookup(f.Selector)
	if !ok {
		// The selector is not a UNIQUE concrete anchor in the digest. The digest is
		// CAPPED (≤40 interactive / ≤30 controls) AND visibility-filtered, so an absent
		// selector can NEVER soundly prove the element doesn't exist (over-cap, hidden,
		// below-fold, inside a collapsed accordion). Absence is never a drop.
		return false
	}
	if missingLabel && el.hasProgLabel {
		// The finding claims a label/ARIA name is absent, but the digest resolved a real
		// PROGRAMMATIC label (for / aria-label / aria-labelledby / wrapping <label>) for
		// this form control — the label may simply be visually hidden (sr-only). This is
		// the meta-audit's "no label" FP class.
		return true
	}
	if notOperable && (el.tag == "a" || el.tag == "button") && el.operable() {
		// The finding claims the element isn't keyboard-operable, but the digest shows a
		// natively operable element — an <a>/<button> that the digest ALSO reports as
		// focusable and not disabled. A merely `focusable` element (e.g. <div
		// role=button tabindex=0>) is NOT sufficient and is not dropped. This is the
		// meta-audit's <a>-as-card FP class.
		//
		// The el.operable() conjunct is load-bearing for UNTRUSTED (plugin-pushed)
		// digests: `tag` has the same drop power as `has_label`, and unlike the crawl
		// path — where tag=a implies a real `a[href]` because the digest's query IS
		// `a[href]` — a producer can simply CLAIM tag="a" on an inert element. Requiring
		// the digest's own focusable/disabled facts to AGREE makes a self-contradictory
		// claim ("it's an <a>, and it's not focusable, and it's disabled") refute
		// nothing. The crawl path computes both honestly, so it is unaffected.
		return true
	}
	return false
}

func (idx digestIndex) lookup(selector string) (digestElem, bool) {
	for _, k := range concreteKeys(selector) {
		if e, ok := idx.bySelector[k]; ok {
			return e, true
		}
	}
	return digestElem{}, false
}

// concreteKeys returns the UNIQUE concrete lookup keys for a selector — only its id
// (`#id`, matching a digest `input#id`) and/or `[name=…]`. A bare tag / class selector
// yields no key: it is not specific enough to soundly anchor a deterministic drop.
//
// The implementation lives in internal/report (report.ConcreteKeys) — ONE definition
// shared by this read-side gate AND internal/plugin's ingest-time duplicate-conflict
// check. They MUST normalise identically: keying the ingest check on the raw selector
// string instead let `"input#e"` and `"#e"` pass validation as "different" selectors
// and then collapse to the same `#e` key here, where the OR-merge resolved their
// conflicting label facts permissively and licensed a drop.
func concreteKeys(selector string) []string { return report.ConcreteKeys(selector) }

// isProgrammaticLabelSource reports whether an A11yInteractive.LabelSource / an
// A11yFormControl.LabelSource is a TRUE programmatic label association — one a screen
// reader announces as the element's name (#36). placeholder / text-content / value /
// none are NOT programmatic (they don't make a real, robust accessible name), so a
// finding claiming the accessible name is absent is NOT refuted by them.
//
// The set itself lives in internal/report (report.IsProgrammaticLabelSource) — ONE
// definition shared by this read-side gate AND internal/plugin's validation of an
// externally-pushed digest, so the two can never drift.
func isProgrammaticLabelSource(src string) bool {
	return report.IsProgrammaticLabelSource(src)
}

// claimsMissingLabel classifies a finding as asserting a MISSING PROGRAMMATIC label /
// ARIA name / accessible name — the FP class an sr-only or aria label invisibly refutes.
// It is deliberately NARROW: a VISIBLE-label complaint or a label-QUALITY complaint
// ("not descriptive", "clearer", "confusing", …) is legitimate even when a programmatic
// label is present, so it is NOT classified here (never a deterministic drop).
func claimsMissingLabel(f report.EvalFinding) bool {
	t := findingText(f)
	// A label-QUALITY / ENHANCEMENT complaint is about an EXISTING label (make it clearer,
	// more descriptive, less confusing) — a programmatic sr-only label does not refute
	// THAT, so bail before classifying it as an absence claim (this is the ONLY-keep guard
	// per #36, which closes the "add a clearer label" false-drop). NOTE: "visible" is
	// deliberately NO LONGER a bail word (#36) — the fragile "visible"-anywhere bail was
	// dropped once the structural label_source signal landed. A programmatic label present
	// + a "no accessible name/association" claim is SAFE to drop regardless of the word
	// "visible" (the accessible name provably exists); we only ever drop on a POSITIVE
	// programmatic-label refutation (contradicts()), never on the word alone.
	if mentionsAny(t, "descriptive", "clearer", "unclear", "clarity", "better", "confusing", "quality", "vague", "ambiguous") {
		return false
	}
	if !mentionsAny(t, "label", "aria-label", "aria label", "aria-labelledby", "accessible name", "screen reader", "screen-reader") {
		return false
	}
	// Genuine ABSENCE phrasings only (narrowed from the earlier broad "no"/"not"/"add"
	// heuristic, which swept in quality/visible complaints).
	return mentionsAny(t,
		"no label", "no labels", "no accessible", "no aria", "no programmatic",
		"lacks a label", "lacks label", "lack a label", "lack labels",
		"has no label", "have no label",
		"missing label", "missing a label", "missing labels", "missing accessible", "missing aria",
		"without a label", "without label", "without an accessible",
		"unlabeled", "unlabelled", "un-labeled", "un-labelled",
		"not labeled", "not labelled",
		"needs a label", "needs an aria-label", "add a label", "add an aria-label", "add aria-label", "provide a label",
	)
}

// claimsNotOperable classifies a finding as asserting the element isn't keyboard
// operable / isn't a real button-or-link / isn't focusable — the <a>-as-card FP class.
func claimsNotOperable(f report.EvalFinding) bool {
	t := findingText(f)
	if mentionsAny(t, "not keyboard", "not keyboard-operable", "no keyboard", "keyboard-inaccessible", "keyboard inaccessible", "not focusable", "cannot be focused", "can't be focused", "not reachable by keyboard", "not tabbable", "no tab", "not operable") {
		return true
	}
	// "make it a semantic button", "it's a div not a button/link", "not a real button/link".
	if mentionsAny(t, "semantic button", "semantic element", "not a button", "not a link", "not a real button", "not a real link", "use a button", "use an anchor", "should be a button", "should be an anchor") &&
		mentionsAny(t, "button", "link", "anchor", "focus", "keyboard", "operable") {
		return true
	}
	return false
}

// findingText concatenates the model-authored fields we classify over.
func findingText(f report.EvalFinding) string {
	return strings.ToLower(f.Issue + " " + f.Evidence)
}

func mentionsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
