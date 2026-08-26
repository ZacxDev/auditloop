package eval

import (
	"testing"

	"github.com/ZacxDev/auditloop/internal/report"
)

// Issue #36 — the STRUCTURAL label_source gate. These pin the boundaries the keyword
// heuristic could not: refute an accessible-name-absence finding ONLY when the cited
// element's label_source is a PROGRAMMATIC source (for/aria-label/aria-labelledby/
// wrapping-label), on INTERACTIVE elements too (not just form controls); keep
// enhancement/quality complaints; keep absence claims whose only source is placeholder/
// text-content; recover the "visible"-phrased coverage. Each test proves NON-VACUOUSNESS
// by flipping a single fact (the label_source, or the finding phrasing) and showing the
// drop flips with it.

// #36 core: a name-absence claim (INCLUDING one phrased with "visible") on an
// interactive button whose label_source is PROGRAMMATIC is dropped — and the SAME
// finding is KEPT when the only source is text-content (the source drives the drop, not
// the word "visible"). This is the coverage Phase-1's keyword heuristic could not give:
// an interactive element's textContent could not be told from a real programmatic label.
func TestDropContradictedRefutesInteractiveProgrammaticLabel(t *testing.T) {
	// A claim mentioning both an accessible-name absence AND "visible" (the exact class
	// the fragile "visible"-anywhere bail used to keep as a false positive).
	finding := report.EvalFinding{
		Issue:    "The save button has no accessible name and no visible label",
		Selector: "#save",
	}

	// Programmatic aria-label on the interactive button → the accessible name provably
	// exists → DROP (regardless of the word "visible").
	prog := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#save", AccessibleName: "Save", Focusable: true, LabelSource: "aria-label"},
	}}
	if out := dropContradicted(mkEval(finding), prog); hasIssue(out, finding.Issue) {
		t.Error("a name-absence claim on a button with a PROGRAMMATIC aria-label must be dropped (incl. 'visible' phrasing)")
	}

	// NON-VACUOUS: the SAME finding, but the button's only source is text-content (an
	// icon glyph) → NOT a programmatic label → KEEP. Flipping just label_source flips the
	// drop, proving the gate keys on the structural source, not the finding text.
	txt := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#save", AccessibleName: "★", Focusable: true, LabelSource: "text-content"},
	}}
	if out := dropContradicted(mkEval(finding), txt); !hasIssue(out, finding.Issue) {
		t.Error("the SAME finding must be KEPT when the only source is text-content (non-vacuous: source flips the drop)")
	}
}

// #36: the "visible"-phrased coverage is recovered on a FORM control too — a programmatic
// (sr-only) label + a "no accessible name" claim is safe to drop even though it also says
// "visible". Non-vacuous: with only a placeholder source, the same claim is KEPT.
func TestDropContradictedRecoversVisiblePhrasedAbsence(t *testing.T) {
	finding := report.EvalFinding{
		Issue:    "The email field has no accessible name — there is no visible label",
		Selector: "#email",
	}
	prog := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#email", AccessibleName: "Email", HasLabel: true, LabelSource: "for"},
	}}
	if out := dropContradicted(mkEval(finding), prog); hasIssue(out, finding.Issue) {
		t.Error("a 'no accessible name … no visible label' claim on a programmatically (sr-only) labelled control must be dropped")
	}
	// NON-VACUOUS: only a placeholder → not programmatic → KEEP.
	ph := &report.A11yDigest{FormControls: []report.A11yFormControl{
		{Selector: "input#email", AccessibleName: "Email", HasLabel: false, LabelSource: "placeholder"},
	}}
	if out := dropContradicted(mkEval(finding), ph); !hasIssue(out, finding.Issue) {
		t.Error("the SAME claim must be KEPT when the only source is a placeholder (non-vacuous)")
	}
}

// #36: an ENHANCEMENT ("provide a label that is more descriptive") on a programmatically-
// labelled control is KEPT (the only-keep quality guard), while a bare name-absence claim
// on the SAME element is dropped — proving the enhancement guard, not blanket keep/drop.
func TestDropContradictedKeepsEnhancementOnProgrammaticInteractive(t *testing.T) {
	d := &report.A11yDigest{Interactive: []report.A11yInteractive{
		{Tag: "button", Selector: "button#save", AccessibleName: "Save", Focusable: true, LabelSource: "aria-label"},
	}}
	// This phrasing SUBSTRING-matches an absence phrase ("provide a label") AND a quality
	// word ("descriptive"). Because claimsMissingLabel bails on the quality word FIRST, the
	// finding is NOT classified as a missing-label claim → KEPT. This genuinely pins the
	// quality guard: remove the "descriptive"/"clearer"/… bail in claimsMissingLabel and the
	// "provide a label" match would classify it as an absence claim → dropContradicted drops
	// it → this assertion fails. (The earlier "Add a clearer label…" phrasing was weakly
	// non-vacuous — it also failed the absence gate, so it passed even without the bail.)
	enh := report.EvalFinding{Issue: "Provide a label that is more descriptive for the save button", Selector: "#save"}
	if out := dropContradicted(mkEval(enh), d); !hasIssue(out, enh.Issue) {
		t.Error("a label-QUALITY enhancement ('provide a label that is more descriptive') on a programmatically-labelled control must be KEPT by the quality bail")
	}
	// NON-VACUOUS: a bare name-absence claim on the SAME element IS dropped.
	abs := report.EvalFinding{Issue: "The save button has no accessible name", Selector: "#save"}
	if out := dropContradicted(mkEval(abs), d); hasIssue(out, abs.Issue) {
		t.Error("a bare name-absence claim on the same programmatic control must be dropped (non-vacuous)")
	}
}

// isProgrammaticLabelSource is the read-side mirror of the JS producer's programmatic set.
func TestIsProgrammaticLabelSource(t *testing.T) {
	for _, s := range []string{"for", "aria-label", "aria-labelledby", "wrapping-label", "FOR", " aria-label "} {
		if !isProgrammaticLabelSource(s) {
			t.Errorf("%q should be a programmatic label source", s)
		}
	}
	for _, s := range []string{"placeholder", "text-content", "value", "none", "", "text"} {
		if isProgrammaticLabelSource(s) {
			t.Errorf("%q must NOT be a programmatic label source", s)
		}
	}
}
