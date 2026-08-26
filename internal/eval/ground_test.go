package eval

import (
	"fmt"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
)

// pipeline is applyDOMGate — the PRODUCTION composition, not a re-implementation of
// it. This indirection is deliberate: an earlier revision composed the two stages
// itself, so swapping them in generator.go passed the whole suite (the tests pinned a
// copy of the pipeline). Everything below goes through the same function generator.go
// calls, so an order swap is a red test.
func pipeline(pe report.PageEvaluation, d *report.A11yDigest) report.PageEvaluation {
	return applyDOMGate(pe, d)
}

func blocker(issue, selector string) report.EvalFinding {
	return report.EvalFinding{Issue: issue, Selector: selector, Verified: true}
}

func selectors(fs []report.EvalFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Selector)
	}
	return out
}

// --- The measured regression this change exists to fix -----------------------
//
// Real shape from pushed run 9d322473 (target civitai-manager-funnel): the digest
// carried the refuting facts, but every model-authored a11y claim cited a
// placeholder attribute, a class, or an invented pseudo-selector — so the
// refutation gate had nothing to look up and 4 of 4 claims survived. The CONTROL
// half of this test pins that old behaviour (dropContradicted alone still keeps all
// four); the assertion half pins the new pipeline dropping them.

func civitaiDigest() *report.A11yDigest {
	return &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "input", Selector: "input#subscribe-q", AccessibleName: "Search civitai to subscribe", Focusable: true, LabelSource: "for"},
			{Tag: "input", Selector: "input#f-model", AccessibleName: "Model id or civitai.com/models/… URL", Focusable: true, LabelSource: "for"},
			{Tag: "input", Selector: `input[name="auto_download"]`, AccessibleName: "Auto-download", Focusable: true, LabelSource: "wrapping-label"},
		},
		FormControls: []report.A11yFormControl{
			{Selector: "input#subscribe-q", AccessibleName: "Search civitai to subscribe", HasLabel: true, LabelSource: "for"},
			{Selector: "input#f-model", AccessibleName: "Model id or civitai.com/models/… URL", HasLabel: true, LabelSource: "for"},
			{Selector: `input[name="auto_download"]`, AccessibleName: "Auto-download", HasLabel: true, LabelSource: "wrapping-label"},
		},
		Landmarks: []report.A11yLandmark{{Tag: "h1", Text: "Civitai manager"}},
	}
}

// The four real prior claims, verbatim selectors from the handoff measurement.
func civitaiClaims() report.PageEvaluation {
	return report.PageEvaluation{
		Comprehension: "unclear",
		Blockers: []report.EvalFinding{
			blocker("The search field has no label — a screen reader announces nothing", `input[placeholder='Search by name, tag, ...']`),
			blocker("This input is missing a label", `input[placeholder='Leave blank for your home directory']`),
			blocker("The main region has no accessible name for screen readers", ".main-content"),
			blocker("Card heading is not keyboard-operable and has no accessible name", "h3:contains('SDXL Portrait')"),
		},
	}
}

func TestGroundSelectorsFixesTheDecorativeGate(t *testing.T) {
	d := civitaiDigest()

	// CONTROL — the pre-change behaviour: the refutation gate alone drops NOTHING,
	// because not one cited selector is a concrete anchor it can look up.
	if got := dropContradicted(civitaiClaims(), d); len(got.Blockers) != 4 {
		t.Fatalf("control: dropContradicted alone should still keep all 4 claims, kept %d (%v)",
			len(got.Blockers), selectors(got.Blockers))
	}

	// NEW — with selector grounding the same input collapses.
	got := pipeline(civitaiClaims(), d)
	if len(got.Blockers) != 0 {
		t.Errorf("want all 4 ungrounded a11y claims gone, %d survived: %v",
			len(got.Blockers), selectors(got.Blockers))
	}
}

// --- Keep: the model cited a real, listed element ----------------------------

func TestGroundKeepsListedSelectorVerbatim(t *testing.T) {
	d := civitaiDigest()
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		// A listed control that the digest ALSO says is unlabelled would survive both
		// stages; use one whose label fact is absent from the digest index instead.
		blocker("This control has no label", "input#not-in-digest-but-listed"),
	}}
	// Sanity: the above is NOT listed, so it must drop — the positive case follows.
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Fatalf("unlisted selector should not survive: %v", selectors(got.Blockers))
	}

	pe = report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("This control has no label", `input[name="auto_download"]`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != `input[name="auto_download"]` {
		t.Fatalf("a verbatim-listed selector must survive grounding unchanged, got %v", selectors(got.Blockers))
	}
}

func TestGroundAcceptsConcreteAnchorSpelling(t *testing.T) {
	d := civitaiDigest()
	// The digest writes `input#subscribe-q`; the model wrote the bare id. Same element.
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("Not keyboard operable", "#subscribe-q"),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 {
		t.Fatalf("a bare #id matching a digest `tag#id` entry must be treated as listed, got %v", selectors(got.Blockers))
	}
}

// --- Re-anchor: an invented selector that quotes a real accessible name ------

func TestGroundReanchorsByQuotedAccessibleName(t *testing.T) {
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#promo", AccessibleName: "Promo code", HasLabel: false, LabelSource: "none"},
		},
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("This field has no label", `input[placeholder="Promo code"]`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 {
		t.Fatalf("a TRUE finding whose only sin was the anchor must be re-anchored, not dropped")
	}
	if got.Blockers[0].Selector != "input#promo" {
		t.Errorf("selector = %q, want it rewritten to the digest's own input#promo", got.Blockers[0].Selector)
	}
	// And it SURVIVES the refutation stage — the digest agrees it is unlabelled.
	if len(pipeline(pe, d).Blockers) != 1 {
		t.Error("a re-anchored TRUE missing-label finding must survive the full pipeline")
	}
}

func TestReanchoredFalsePositiveIsThenRefuted(t *testing.T) {
	// Same shape, but the element IS labelled (sr-only) — re-anchoring hands it to
	// dropContradicted, which refutes it. This is why the order is ground→contradict.
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#email", AccessibleName: "Email address", HasLabel: true, LabelSource: "for"},
		},
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("This field has no label", `input[placeholder="Email address"]`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#email" {
		t.Fatalf("expected re-anchor to input#email, got %v", selectors(got.Blockers))
	}
	if got := pipeline(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a re-anchored FP must then be refuted by the digest, survived: %v", selectors(got.Blockers))
	}
}

func TestGroundReanchorsTruncatedQuote(t *testing.T) {
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#q", AccessibleName: "Search by name, tag or creator", HasLabel: false, LabelSource: "placeholder"},
		},
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("No label on the search box", `input[placeholder='Search by name, tag, ...']`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#q" {
		t.Errorf("a model-truncated quote should still re-anchor, got %v", selectors(got.Blockers))
	}
}

func TestGroundDoesNotReanchorAmbiguousName(t *testing.T) {
	// Two elements share the name — rewriting would move the finding onto an
	// innocent element, so the finding is dropped instead.
	d := &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "button", Selector: "button#save-top", AccessibleName: "Save", Focusable: true, LabelSource: "text-content"},
			{Tag: "button", Selector: "button#save-bottom", AccessibleName: "Save", Focusable: true, LabelSource: "text-content"},
		},
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("This control has no accessible name", `button:contains("Save")`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("an ambiguous name must not re-anchor (and the claim stays ungrounded), got %v", selectors(got.Blockers))
	}
}

func TestGroundDoesNotReanchorOnAShortLiteral(t *testing.T) {
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#qty", AccessibleName: "Qty", HasLabel: false, LabelSource: "none"},
		},
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("missing label", `input[placeholder="Qty"]`), // 3 chars < minNameMatch
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a literal shorter than minNameMatch is not evidence of identity, got %v", selectors(got.Blockers))
	}
}

// --- Scope: only mechanical a11y claims are touched --------------------------

func TestGroundLeavesSubjectiveFindingsAlone(t *testing.T) {
	d := civitaiDigest()
	pe := report.PageEvaluation{
		Blockers: []report.EvalFinding{
			blocker("The pricing is nowhere on this page, so I cannot decide", ".hero .cta"),
		},
		Frictions: []report.EvalFinding{
			blocker("Too much jargon above the fold", "h3:contains('SDXL Portrait')"),
		},
		TopFix: &report.EvalTopFix{Change: "Put the price above the fold", Selector: ".hero", Impact: "high"},
	}
	got := pipeline(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != ".hero .cta" {
		t.Errorf("subjective blocker must pass through untouched, got %v", selectors(got.Blockers))
	}
	if len(got.Frictions) != 1 || got.Frictions[0].Selector != "h3:contains('SDXL Portrait')" {
		t.Errorf("subjective friction must pass through untouched, got %v", selectors(got.Frictions))
	}
	if got.TopFix == nil || got.TopFix.Selector != ".hero" {
		t.Errorf("subjective top_fix must pass through untouched, got %+v", got.TopFix)
	}
}

func TestGroundDropsUnanchoredMechanicalClaim(t *testing.T) {
	d := civitaiDigest()
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("Several inputs are missing labels", ""),
		blocker("The copy is confusing", ""), // subjective, no selector → kept
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Issue != "The copy is confusing" {
		t.Errorf("an UNANCHORED mechanical a11y claim must drop while a subjective one stays, got %d: %v",
			len(got.Blockers), got.Blockers)
	}
}

// --- The truncation guard: a capped digest must never drop -------------------

func TestGroundDoesNotDropWhenDigestIsTruncated(t *testing.T) {
	d := &report.A11yDigest{}
	for i := 0; i < report.MaxA11yInteractive; i++ {
		d.Interactive = append(d.Interactive, report.A11yInteractive{
			Tag: "button", Selector: fmt.Sprintf("button#b%d", i), AccessibleName: fmt.Sprintf("Button %d", i), Focusable: true,
		})
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("This input has no label", "input.search"),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 {
		t.Error("at the interactive cap the digest is a PREFIX of the page — absence proves nothing, so nothing may drop")
	}
	// One under the cap → the digest is complete → the same claim drops.
	d.Interactive = d.Interactive[:report.MaxA11yInteractive-1]
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Error("below the cap the digest is complete and the ungrounded claim must drop")
	}
}

func TestGroundDoesNotDropWhenFormControlsAreCapped(t *testing.T) {
	d := &report.A11yDigest{}
	for i := 0; i < report.MaxA11yFormControls; i++ {
		d.FormControls = append(d.FormControls, report.A11yFormControl{
			Selector: fmt.Sprintf("input#c%d", i), AccessibleName: fmt.Sprintf("Control %d", i), HasLabel: true, LabelSource: "for",
		})
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{blocker("missing label here", "input.other")}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 {
		t.Error("a form-control-capped digest must not license a drop either")
	}
}

// --- Backward compatibility: no digest ⇒ byte-for-byte the old behaviour -----

func TestGroundIsNoOpWithoutDigest(t *testing.T) {
	pe := civitaiClaims()
	for name, d := range map[string]*report.A11yDigest{
		"nil":   nil,
		"empty": {},
	} {
		got := groundSelectors(pe, d)
		if len(got.Blockers) != 4 {
			t.Errorf("%s digest: grounding must be a no-op (pre-0060 / digest-less pushed runs), kept %d", name, len(got.Blockers))
		}
		for i := range got.Blockers {
			if got.Blockers[i].Selector != pe.Blockers[i].Selector {
				t.Errorf("%s digest: selector %d was rewritten", name, i)
			}
		}
	}
}

// --- top_fix carries a selector too ------------------------------------------

func TestGroundClearsUngroundedTopFix(t *testing.T) {
	d := civitaiDigest()
	pe := report.PageEvaluation{TopFix: &report.EvalTopFix{
		Change:   "Add an aria-label to this control so screen readers announce it",
		Selector: ".toolbar .icon-btn",
		Impact:   "high",
	}}
	if got := groundSelectors(pe, d); got.TopFix != nil {
		t.Errorf("an ungrounded mechanical top_fix must be cleared, got %+v", got.TopFix)
	}
	// A listed selector keeps it.
	pe.TopFix.Selector = "input#f-model"
	if got := groundSelectors(pe, d); got.TopFix == nil {
		t.Error("a top_fix citing a listed element must survive grounding")
	}
}

func TestGroundReanchorsTopFixWithoutMutatingInput(t *testing.T) {
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#promo", AccessibleName: "Promo code", HasLabel: false, LabelSource: "none"},
	}}
	pe := report.PageEvaluation{TopFix: &report.EvalTopFix{
		Change:   "Add a label to this field",
		Selector: `input[placeholder="Promo code"]`,
		Impact:   "medium",
	}}
	got := groundSelectors(pe, d)
	if got.TopFix == nil || got.TopFix.Selector != "input#promo" {
		t.Fatalf("top_fix should re-anchor onto input#promo, got %+v", got.TopFix)
	}
	// The caller's value must not be mutated through the shared pointer.
	if pe.TopFix.Selector != `input[placeholder="Promo code"]` {
		t.Errorf("groundSelectors mutated the input top_fix in place: %q", pe.TopFix.Selector)
	}
}

func TestGroundDoesNotMutateInputSlices(t *testing.T) {
	d := civitaiDigest()
	pe := civitaiClaims()
	before := selectors(pe.Blockers)
	_ = groundSelectors(pe, d)
	after := selectors(pe.Blockers)
	if strings.Join(before, "|") != strings.Join(after, "|") {
		t.Errorf("input blockers were mutated in place: %v → %v", before, after)
	}
	if len(pe.Blockers) != 4 {
		t.Errorf("input slice length changed: %d", len(pe.Blockers))
	}
}

// --- The empty-VOCABULARY case (a non-empty digest that lists no selectors) ---

// A digest can be non-empty (IsEmpty() == false) while carrying ONLY landmarks — an
// error page, a confirmation screen, a page whose element queries matched nothing, or
// a pushed digest with a single landmark (the push validator rejects only an ALL-empty
// one). Such a page asserts nothing about interactive elements, so it must not license
// a single drop. Without this guard the gate is a BLANKET DELETE there.
func TestGroundDoesNotDropAgainstAnEmptySelectorVocabulary(t *testing.T) {
	d := &report.A11yDigest{Landmarks: []report.A11yLandmark{{Tag: "h1", Text: "Something went wrong"}}}
	if d.IsEmpty() {
		t.Fatal("fixture bug: this digest must be NON-empty (that is the whole point)")
	}
	pe := report.PageEvaluation{
		Blockers: []report.EvalFinding{
			blocker("The search input has no label", "input#search"),
			blocker("This card is not keyboard-operable", ".card"),
		},
		TopFix: &report.EvalTopFix{Change: "Add a label to this field", Selector: "#q", Impact: "high"},
	}
	got := pipeline(pe, d)
	if len(got.Blockers) != 2 {
		t.Errorf("a landmarks-only digest must not drop ANY mechanical finding, kept %d of 2: %v",
			len(got.Blockers), selectors(got.Blockers))
	}
	if got.TopFix == nil {
		t.Error("a landmarks-only digest must not clear the top_fix either")
	}

	// Same conclusion by a different route: entries EXIST but carry no selector, so the
	// vocabulary is still empty and still testifies to nothing.
	d2 := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "", AccessibleName: "Continue", Focusable: true},
	}}
	if got := pipeline(pe, d2); len(got.Blockers) != 2 {
		t.Errorf("selector-less digest entries are not a vocabulary, kept %d of 2", len(got.Blockers))
	}
}

func TestGroundDoesNotDropWhenLandmarksAreCapped(t *testing.T) {
	// A landmark list at its cap means a LARGE page whose element lists are the likeliest
	// to be truncated in ways the counts alone do not show — err toward not dropping.
	d := &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "button", Selector: "button#go", AccessibleName: "Go", Focusable: true},
		},
		Landmarks: make([]report.A11yLandmark, report.MaxA11yLandmarks),
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{blocker("this input has no label", "input.search")}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 {
		t.Error("at the landmark cap the digest is treated as incomplete — nothing may drop")
	}
	d.Landmarks = d.Landmarks[:report.MaxA11yLandmarks-1]
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Error("below the landmark cap the same claim must drop")
	}
}

// --- Emitted anchors must be usable against the real DOM ---------------------

func TestReanchorPreservesSelectorCase(t *testing.T) {
	// id / [name=…] matching is CASE-SENSITIVE in the DOM: emitting `input#useremail`
	// for a real `input#userEmail` hands the reader an anchor that matches nothing.
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#userEmail", AccessibleName: "Email address", HasLabel: false, LabelSource: "none"},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this field has no label", `input[placeholder="Email address"]`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#userEmail" {
		t.Errorf("re-anchor must emit the digest's OWN selector verbatim, got %v", selectors(got.Blockers))
	}
}

func TestReanchorIgnoresIdentifierValuedAttributes(t *testing.T) {
	// `[type="submit"]` is an IDENTIFIER, not a name. Matching it against the name
	// index moved the finding onto an unrelated button whose accessible name happens
	// to be "Submit" — the ambiguity check cannot see this (only ONE digest element
	// carries that name; the problem is that the literal was never a name).
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#newsletter-submit", AccessibleName: "Submit", Focusable: true, LabelSource: "text-content"},
	}}
	for _, sel := range []string{
		`input[type="submit"]`,
		`div[class="Submit"]`,
		`a[href="/submit"]`,
		`input[name="submit"]`,
	} {
		pe := report.PageEvaluation{Blockers: []report.EvalFinding{
			blocker("this control has no accessible name", sel),
		}}
		if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
			t.Errorf("%s must NOT re-anchor off an identifier-valued attribute, got %q", sel, got.Blockers[0].Selector)
		}
	}
	// The name-bearing spelling of the same idea DOES re-anchor.
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this control has no accessible name", `button[aria-label="Submit"]`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 || got.Blockers[0].Selector != "button#newsletter-submit" {
		t.Errorf("a name-bearing attribute must still re-anchor, got %v", selectors(got.Blockers))
	}
}

func TestReanchorRejectsAShortPrefix(t *testing.T) {
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#promocode", AccessibleName: "Promotional discount code for returning buyers", HasLabel: false, LabelSource: "none"},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("missing label", `input[placeholder="Promo"]`), // a prefix of countless names
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a %d-rune prefix is not evidence of identity, re-anchored to %q", 5, got.Blockers[0].Selector)
	}
	// A genuinely truncated quote is long, and still re-anchors.
	pe.Blockers[0].Selector = `input[placeholder="Promotional discount code for ret..."]`
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#promocode" {
		t.Errorf("a long truncated quote must still re-anchor, got %v", selectors(got.Blockers))
	}
}

func TestReanchorRejectsAShortDIGESTName(t *testing.T) {
	// The mirror image of the short-literal case: the shared evidence is the SHORTER of
	// the two strings, so a digest element merely named "Save" must not absorb a finding
	// that quoted a long, specific label.
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#save-draft", AccessibleName: "Save", Focusable: true},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this control has no accessible name", `button[aria-label="Save these changes and publish now"]`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a 4-rune digest name is not evidence of identity, re-anchored to %q", got.Blockers[0].Selector)
	}
}

func TestPrefixBarSitsExactlyAtMinPrefixMatch(t *testing.T) {
	// Pin the CONSTANT, not just the idea: fixtures one rune either side of the bar, in
	// BOTH directions (short literal vs short digest name). Without a boundary fixture
	// the value 12 is asserted only in prose and any nearby value passes.
	name12, name11 := "abcdefghijkl", "abcdefghijk" // 12 and 11 runes
	if len([]rune(name12)) != minPrefixMatch || len([]rune(name11)) != minPrefixMatch-1 {
		t.Fatalf("fixture bug: fixtures must straddle minPrefixMatch=%d", minPrefixMatch)
	}
	// (a) digest name at the bar + a longer literal → re-anchors.
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#at-bar", AccessibleName: name12, HasLabel: false, LabelSource: "none"},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this field has no label", `input[placeholder="`+name12+`mnop"]`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#at-bar" {
		t.Errorf("a name exactly at minPrefixMatch must re-anchor, got %v", selectors(got.Blockers))
	}
	// (b) one rune under the bar → must not.
	d.FormControls[0].AccessibleName = name11
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a name one rune UNDER minPrefixMatch must not re-anchor, got %q", got.Blockers[0].Selector)
	}
	// (c) mirror image: the literal one rune under the bar, digest name long.
	d.FormControls[0].AccessibleName = name12 + "mnop"
	pe.Blockers[0].Selector = `input[placeholder="` + name11 + `"]`
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a literal one rune UNDER minPrefixMatch must not re-anchor, got %q", got.Blockers[0].Selector)
	}
	// (d) and the literal EXACTLY at the bar must re-anchor. Without this case the
	// literal side is pinned only from below, so `<` → `<=` on its guard passes the
	// whole suite — each side of the comparison needs a fixture sitting ON the bar,
	// not merely near it.
	pe.Blockers[0].Selector = `input[placeholder="` + name12 + `"]`
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#at-bar" {
		t.Errorf("a literal exactly at minPrefixMatch must re-anchor, got %v", selectors(got.Blockers))
	}
}

func TestReanchorMatchesInBothPrefixDirections(t *testing.T) {
	// The literal may be SHORTER than the digest name (the model truncated its quote)
	// or LONGER (the digest truncated the name at its own cap). Both directions are
	// live; a test that only exercises one cannot tell "the bar rejected it" from "that
	// direction was deleted".
	long := "Export the current report as a CSV file"
	short := "Export the current report" // 25 runes, a strict prefix of `long`
	cases := map[string]struct{ digestName, quoted string }{
		"model truncated its quote": {digestName: long, quoted: short},
		"digest truncated the name": {digestName: short, quoted: long},
	}
	for name, c := range cases {
		d := &report.A11yDigest{Interactive: []report.A11yInteractive{
			{Tag: "button", Selector: "button#export", AccessibleName: c.digestName, Focusable: true},
		}}
		pe := report.PageEvaluation{Blockers: []report.EvalFinding{
			blocker("this control has no accessible name", `button[aria-label="`+c.quoted+`"]`),
		}}
		got := groundSelectors(pe, d)
		if len(got.Blockers) != 1 || got.Blockers[0].Selector != "button#export" {
			t.Errorf("%s: expected a re-anchor to button#export, got %v", name, selectors(got.Blockers))
		}
	}
}

func TestReanchorAbortsWhenTwoDISTINCTNamesSharePrefix(t *testing.T) {
	// The other half of the prefix ambiguity abort: not two elements sharing ONE name
	// (that is marked ambiguous at index time), but two DIFFERENT names the same quoted
	// prefix matches. Nothing may be re-anchored — the quote does not identify either.
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#export-csv", AccessibleName: "Export the current report as CSV", Focusable: true},
		{Tag: "button", Selector: "button#export-pdf", AccessibleName: "Export the current report as PDF", Focusable: true},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this control has no accessible name", `button[title="Export the current report as..."]`),
	}}
	for i := 0; i < 50; i++ { // map order must not decide the outcome
		if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
			t.Fatalf("iteration %d: two distinct prefix matches must abort, re-anchored to %q",
				i, got.Blockers[0].Selector)
		}
	}
}

func TestShortAmbiguousNameDoesNotVetoALongPrefixMatch(t *testing.T) {
	// Deliberate consequence of the evidence bar: a name below it is not a candidate,
	// so it neither matches nor aborts. Before the bar applied to digest names, the
	// ambiguous "Save" vetoed this; a 30-rune quote cannot be a truncation of "Save".
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#a", AccessibleName: "Save", Focusable: true},
		{Tag: "button", Selector: "button#b", AccessibleName: "Save", Focusable: true},
		{Tag: "button", Selector: "button#c", AccessibleName: "Save these changes and publish", Focusable: true},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this control has no accessible name", `button[aria-label="Save these changes and pub"]`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != "button#c" {
		t.Errorf("the long unique prefix must win over a short ambiguous name, got %v", selectors(got.Blockers))
	}
}

func TestNameLengthBarCountsRunesNotBytes(t *testing.T) {
	// "日本語" is 3 runes / 9 bytes: below minNameMatch, so it must not re-anchor. A
	// byte-counted bar would wave it through.
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#lang", AccessibleName: "日本語", HasLabel: false, LabelSource: "none"},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this field has no label", `input[placeholder="日本語"]`),
	}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
		t.Errorf("a 3-rune name must not clear the identity bar, re-anchored to %q", got.Blockers[0].Selector)
	}
}

// The gate's licence to drop and the prompt's SELECTOR RULE must be the SAME predicate:
// telling the model "unlisted a11y claims are discarded" on a page where the gate then
// keeps them makes it self-censor for nothing.
func TestPromptRuleAndGateAgreeOnEveryDigestShape(t *testing.T) {
	full := func(n int, sel func(int) string) []report.A11yInteractive {
		out := make([]report.A11yInteractive, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, report.A11yInteractive{Tag: "button", Selector: sel(i), AccessibleName: fmt.Sprintf("B%d", i), Focusable: true})
		}
		return out
	}
	shapes := map[string]*report.A11yDigest{
		"landmarks only":     {Landmarks: []report.A11yLandmark{{Tag: "h1", Text: "Oops"}}},
		"one interactive":    {Interactive: full(1, func(i int) string { return "button#b" })},
		"interactive at cap": {Interactive: full(report.MaxA11yInteractive, func(i int) string { return fmt.Sprintf("button#b%d", i) })},
		"landmarks at cap": {
			Interactive: full(2, func(i int) string { return fmt.Sprintf("button#b%d", i) }),
			Landmarks:   make([]report.A11yLandmark, report.MaxA11yLandmarks),
		},
		"form controls at cap": {
			Interactive:  full(2, func(i int) string { return fmt.Sprintf("button#b%d", i) }),
			FormControls: make([]report.A11yFormControl, report.MaxA11yFormControls),
		},
		"selectorless entries": {Interactive: full(2, func(i int) string { return "" })},
	}
	p, _ := PersonaByID("accessibility-constrained")
	for name, d := range shapes {
		g := sampleGrounding()
		g.A11yDigest = d
		promptSaysRule := strings.Contains(g.GenUserPrompt(p), "SELECTOR RULE")
		gateMayDrop := buildGroundIndex(d).complete
		if promptSaysRule != gateMayDrop {
			t.Errorf("%s: prompt emits the rule = %v but the gate may drop = %v — they must be one predicate",
				name, promptSaysRule, gateMayDrop)
		}
	}
}

func TestReanchorAbortsOnAmbiguousPrefix(t *testing.T) {
	// The ambiguity abort must hold on the PREFIX path, not just the exact-match one.
	// The fixture needs BOTH an ambiguous name AND a distinct unique name that the same
	// quoted prefix matches — with only the ambiguous one present, a mutant that drops
	// the abort still refuses (its `hit` never leaves ""), so the case would not
	// discriminate. Here, dropping the abort re-anchors onto button#other whenever the
	// ambiguous entry is visited first; the correct code refuses in EITHER order.
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#save-top", AccessibleName: "Save these changes now", Focusable: true},
		{Tag: "button", Selector: "button#save-bottom", AccessibleName: "Save these changes now", Focusable: true},
		{Tag: "button", Selector: "button#other", AccessibleName: "Save these changes now please", Focusable: true},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this control has no accessible name", `button[title="Save these changes n..."]`),
	}}
	// Go randomises map iteration order per range, and the abort is exactly what makes
	// the outcome order-INDEPENDENT — so assert it over enough iterations that an
	// order-dependent implementation cannot pass by luck.
	for i := 0; i < 50; i++ {
		if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
			t.Fatalf("iteration %d: an ambiguous name must abort the PREFIX path, re-anchored to %q",
				i, got.Blockers[0].Selector)
		}
	}
}

func TestIsListedRequiresAWholeAnchorNotASubstring(t *testing.T) {
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#signup-submit", AccessibleName: "Sign up", Focusable: true},
	}}
	for _, sel := range []string{"#sign", "#signup-submit-extra", "button#signup"} {
		pe := report.PageEvaluation{Blockers: []report.EvalFinding{blocker("not keyboard operable", sel)}}
		if got := groundSelectors(pe, d); len(got.Blockers) != 0 {
			t.Errorf("%q is not the digest's anchor and must not count as listed", sel)
		}
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{blocker("not keyboard operable", "#signup-submit")}}
	if got := groundSelectors(pe, d); len(got.Blockers) != 1 {
		t.Error("the exact anchor must count as listed")
	}
}

// Re-anchoring is documented to run EVEN on a capped (incomplete) digest — only
// DROPPING is suspended there. A guard placed above the re-anchor step would silently
// change that, so pin it.
func TestReanchorStillRunsOnACappedDigest(t *testing.T) {
	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#promo", AccessibleName: "Promo code", HasLabel: false, LabelSource: "none"},
	}}
	for i := len(d.Interactive); i < report.MaxA11yInteractive; i++ {
		d.Interactive = append(d.Interactive, report.A11yInteractive{
			Tag: "button", Selector: fmt.Sprintf("button#b%d", i), AccessibleName: fmt.Sprintf("Button %d", i), Focusable: true,
		})
	}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this field has no label", `input[placeholder="Promo code"]`),
	}}
	got := groundSelectors(pe, d)
	if len(got.Blockers) != 1 || got.Blockers[0].Selector != "input#promo" {
		t.Errorf("re-anchoring must still run on a capped digest (only dropping is suspended), got %v",
			selectors(got.Blockers))
	}
}

// --- The observable: the metric is the whole point of the gate ---------------

func TestGateMetricRecordsEachAction(t *testing.T) {
	// Read the counter directly (no prometheus/testutil — it would pull a new module
	// into go.sum for one assertion).
	read := func(action string) float64 {
		var m dto.Metric
		if err := metrics.EvalFindingsGated.WithLabelValues(action).Write(&m); err != nil {
			t.Fatalf("read counter %q: %v", action, err)
		}
		return m.GetCounter().GetValue()
	}
	before := map[string]float64{"ungrounded": read("ungrounded"), "reanchored": read("reanchored"), "contradicted": read("contradicted")}

	d := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#promo", AccessibleName: "Promo code", HasLabel: false, LabelSource: "none"},
		{Selector: "input#email", AccessibleName: "Email address", HasLabel: true, LabelSource: "for"},
	}}
	pe := report.PageEvaluation{Blockers: []report.EvalFinding{
		blocker("this field has no label", `input[placeholder="Promo code"]`), // → reanchored (kept)
		blocker("this input has no label", "input.nowhere"),                   // → ungrounded (dropped)
		blocker("this field has no label", "input#email"),                     // → contradicted (dropped)
	}}
	if got := pipeline(pe, d); len(got.Blockers) != 1 {
		t.Fatalf("fixture: expected exactly one survivor, got %v", selectors(got.Blockers))
	}
	for action, want := range map[string]float64{"ungrounded": 1, "reanchored": 1, "contradicted": 1} {
		if delta := read(action) - before[action]; delta != want {
			t.Errorf("auditloop_eval_findings_gated_total{action=%q} moved by %v, want %v", action, delta, want)
		}
	}
}

// --- Helpers ------------------------------------------------------------------

func TestQuotedLiterals(t *testing.T) {
	cases := map[string][]string{
		`input[placeholder="Email address"]`:  {"Email address"},
		`input[placeholder='Search, tag...']`: {"Search, tag..."},
		`h3:contains('SDXL Portrait')`:        {"SDXL Portrait"},
		`button:has-text(Save changes)`:       {"Save changes"},
		`.main-content`:                       nil,
		`input[name=q]`:                       nil,
		// The match-operator forms are what make a substring/prefix attribute selector
		// re-anchor at all; without stripping the operator the attribute name is
		// "aria-label*" and never matches the allowlist.
		`button[aria-label*="Close dialog"]`: {"Close dialog"},
		`input[placeholder^='Search the']`:   {"Search the"},
		`a[title$="(opens in a new tab)"]`:   {"(opens in a new tab)"},
		// Multiple attributes: only the name-bearing one is harvested.
		`input[type="text"][placeholder="Full name"]`: {"Full name"},
	}
	for sel, want := range cases {
		got := quotedLiterals(sel)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("quotedLiterals(%q) = %v, want %v", sel, got, want)
		}
	}
}

func TestNormalizeSelEquatesQuoteStyles(t *testing.T) {
	if normalizeSel(`input[name='q']`) != normalizeSel(`INPUT[name="q"]`) {
		t.Error("quote style / case must not change a selector's identity")
	}
}

// --- The prompt half ----------------------------------------------------------

func TestSemanticBlockCarriesTheSelectorRule(t *testing.T) {
	g := sampleGrounding()
	g.A11yDigest = sampleDigest()
	p, _ := PersonaByID("accessibility-constrained")
	got := g.GenUserPrompt(p)
	for _, want := range []string{"SELECTOR RULE", "VERBATIM", "discarded automatically"} {
		if !strings.Contains(got, want) {
			t.Errorf("gen prompt missing %q from the selector rule:\n%s", want, got)
		}
	}
	if !strings.Contains(VerifyUserPrompt(g, p, `{"comprehension":"clear"}`), "SELECTOR RULE") {
		t.Error("verify prompt must carry the selector rule too (it re-emits the same block)")
	}
	// Digest-less runs are untouched.
	g.A11yDigest = nil
	if strings.Contains(g.GenUserPrompt(p), "SELECTOR RULE") {
		t.Error("no digest ⇒ no selector rule (backward compat)")
	}
	// A landmarks-only digest lists no selectors, so the rule ("the elements listed
	// above are the ONLY ones you may cite") would sit above an empty list — and the
	// gate treats that case as an incomplete vocabulary that drops nothing. The prompt
	// and the gate must agree.
	g.A11yDigest = &report.A11yDigest{Landmarks: []report.A11yLandmark{{Tag: "h1", Text: "Oops"}}}
	out := g.GenUserPrompt(p)
	if strings.Contains(out, "SELECTOR RULE") {
		t.Error("a landmarks-only digest must not carry the selector rule (there is no vocabulary)")
	}
	if !strings.Contains(out, "SEMANTIC STRUCTURE") {
		t.Error("the landmarks themselves should still be rendered as grounding")
	}
}
