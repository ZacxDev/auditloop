package eval

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/report"
)

func sampleDigest() *report.A11yDigest {
	return &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "a", Selector: "a#client-1", AccessibleName: "Open client Acme", Focusable: true},
			{Tag: "button", Selector: "button#submit", AccessibleName: "Create account", Focusable: true},
		},
		FormControls: []report.A11yFormControl{
			{Selector: "input#signup-email", AccessibleName: "Email", HasLabel: true, LabelSource: "for"},
			{Selector: "input#promo", HasLabel: false, LabelSource: "none"},
		},
		Landmarks: []report.A11yLandmark{
			{Tag: "h1", Text: "Sign up"},
			{Tag: "nav", Role: "navigation"},
		},
	}
}

// --- Semantic block rendering (present with a digest, omitted when nil = compat) ---

func TestGenUserPromptIncludesSemanticBlockWithDigest(t *testing.T) {
	g := sampleGrounding()
	g.A11yDigest = sampleDigest()
	p, _ := PersonaByID("accessibility-constrained")
	got := g.GenUserPrompt(p)

	if !strings.Contains(got, "SEMANTIC STRUCTURE") {
		t.Fatalf("gen prompt missing the SEMANTIC STRUCTURE block:\n%s", got)
	}
	if !strings.Contains(got, "AUTHORITATIVE") {
		t.Error("gen prompt semantic block missing the AUTHORITATIVE framing")
	}
	// The resolved accessible name + label source surface (the facts a screenshot can't show).
	if !strings.Contains(got, `input#signup-email`) || !strings.Contains(got, "labelled") || !strings.Contains(got, "<label for>") {
		t.Errorf("gen prompt missing the resolved form-control label:\n%s", got)
	}
	// The <a> is reported as a focusable link (kills the "not keyboard-operable" FP).
	// role/selector are %q-quoted — they are page/producer-authored and must not be able
	// to forge a prompt line (see TestSemanticBlockQuotesUntrustedFields).
	if !strings.Contains(got, "a#client-1") || !strings.Contains(got, `role="link"`) || !strings.Contains(got, "focusable") {
		t.Errorf("gen prompt missing the anchor's role/focusable facts:\n%s", got)
	}
	// The unlabelled control is honestly reported as having no programmatic label.
	if !strings.Contains(got, "input#promo") || !strings.Contains(got, "NO programmatic label") {
		t.Errorf("gen prompt should report the unlabelled control as unlabelled:\n%s", got)
	}
}

func TestGenUserPromptOmitsSemanticBlockWithoutDigest(t *testing.T) {
	g := sampleGrounding() // no A11yDigest set (nil) → pre-0060 / pushed run
	p, _ := PersonaByID("skeptical-evaluator")
	got := g.GenUserPrompt(p)
	if strings.Contains(got, "SEMANTIC STRUCTURE") {
		t.Errorf("gen prompt must NOT include the semantic block when no digest (backward-compat):\n%s", got)
	}
	// Empty (non-nil) digest is also treated as absent.
	g.A11yDigest = &report.A11yDigest{}
	if strings.Contains(g.GenUserPrompt(p), "SEMANTIC STRUCTURE") {
		t.Error("an empty digest must be treated as absent (no semantic block)")
	}
}

func TestVerifyUserPromptIncludesSemanticBlockWithDigest(t *testing.T) {
	g := sampleGrounding()
	g.A11yDigest = sampleDigest()
	p, _ := PersonaByID("accessibility-constrained")
	got := VerifyUserPrompt(g, p, `{"comprehension":"clear"}`)
	if !strings.Contains(got, "SEMANTIC STRUCTURE") {
		t.Errorf("verify prompt missing the semantic block:\n%s", got)
	}

	// Omitted when there is no digest.
	g.A11yDigest = nil
	if strings.Contains(VerifyUserPrompt(g, p, `{}`), "SEMANTIC STRUCTURE") {
		t.Error("verify prompt must omit the semantic block without a digest")
	}
}

func TestGenSystemPromptForbidsPixelDerivedA11y(t *testing.T) {
	if !strings.Contains(GenSystemPrompt, "AUTHORITATIVE") {
		t.Error("gen system prompt must state the semantic block is authoritative")
	}
	if !strings.Contains(GenSystemPrompt, "MUST NOT") {
		t.Error("gen system prompt must forbid asserting a missing label/role/focus from pixels")
	}
	if !strings.Contains(VerifySystemPrompt, "AUTHORITATIVE") {
		t.Error("verify system prompt must carry the authoritative-digest rule")
	}
}

// axe node selectors are surfaced into the a11y grounding line (WHERE, not just which rule).
func TestGenUserPromptSurfacesAxeNodeSelectors(t *testing.T) {
	g := sampleGrounding()
	g.A11yRuleIDs = []string{"label", "color-contrast"}
	g.A11yRuleNodes = map[string][]string{"label": {"input#email"}, "color-contrast": {".btn", ".link"}}
	p, _ := PersonaByID("accessibility-constrained")
	got := g.GenUserPrompt(p)
	// The selectors are QUOTED (%q): on a pushed run they come from the producer's raw
	// axe object, so an embedded newline would otherwise forge its own instruction line
	// in the grounding block (audit 🟡-D). They must still be PRESENT and readable.
	if !strings.Contains(got, `label ("input#email")`) {
		t.Errorf("a11y line missing rule node selector:\n%s", got)
	}
	if !strings.Contains(got, `color-contrast (".btn, .link")`) {
		t.Errorf("a11y line missing multi-node selectors:\n%s", got)
	}
}

// --- Deterministic DOM-grounded verify (§5.2) ---

func mkEval(blockers ...report.EvalFinding) report.PageEvaluation {
	return report.PageEvaluation{Comprehension: "unclear", Blockers: blockers}
}

func hasIssue(pe report.PageEvaluation, issue string) bool {
	for _, f := range append(append([]report.EvalFinding{}, pe.Blockers...), pe.Frictions...) {
		if f.Issue == issue {
			return true
		}
	}
	return false
}

func TestDropContradictedDropsRefutedA11yFindings(t *testing.T) {
	d := sampleDigest()
	labelFP := report.EvalFinding{Issue: "Email input has no accessible label for screen readers", Selector: "#signup-email"}
	operableFP := report.EvalFinding{Issue: "Client cards are not keyboard-operable", Selector: "#client-1"}
	subjective := report.EvalFinding{Issue: "The hero copy is dense and hard to scan", Selector: "main"}
	legitLabel := report.EvalFinding{Issue: "Promo field has no label", Selector: "#promo"}

	pe := mkEval(labelFP, operableFP, subjective, legitLabel)
	out := dropContradicted(pe, d)

	if hasIssue(out, labelFP.Issue) {
		t.Error("a 'no label' finding whose selector HAS a resolved label must be dropped")
	}
	if hasIssue(out, operableFP.Issue) {
		t.Error("a 'not keyboard-operable' finding on a tag=a element must be dropped")
	}
	if !hasIssue(out, subjective.Issue) {
		t.Error("a subjective finding (no objective a11y claim) must NOT be dropped")
	}
	if !hasIssue(out, legitLabel.Issue) {
		t.Error("a 'no label' finding whose control is genuinely unlabelled must NOT be dropped (no over-drop)")
	}
}

func TestDropContradictedNoOpWithoutDigest(t *testing.T) {
	labelFP := report.EvalFinding{Issue: "Email input has no accessible label", Selector: "#signup-email"}
	pe := mkEval(labelFP)

	// nil digest (pre-0060 / pushed run) → NEVER drop (load-bearing compat guard).
	if out := dropContradicted(pe, nil); !hasIssue(out, labelFP.Issue) {
		t.Error("must not drop any finding when the digest is nil")
	}
	// empty (non-nil) digest → also never drop.
	if out := dropContradicted(pe, &report.A11yDigest{}); !hasIssue(out, labelFP.Issue) {
		t.Error("must not drop any finding when the digest is empty")
	}
}

// A concrete selector ABSENT from the digest must NOT be dropped. The digest is capped
// (≤40 interactive / ≤30 controls) and visibility-filtered, so absence cannot soundly
// prove the element doesn't exist (over-cap, hidden, below-fold, collapsed accordion) —
// dropping on absence over-fires and hides REAL findings. The gate drops ONLY on a
// positive refutation, never on absence-inference (audit 🟡#1).
func TestDropContradictedKeepsAbsentSelector(t *testing.T) {
	d := sampleDigest()
	// A concrete #id selector NOT present in the (capped/partial) digest → KEEP.
	ghost := report.EvalFinding{Issue: "input has no label", Selector: "#does-not-exist"}
	if out := dropContradicted(mkEval(ghost), d); !hasIssue(out, ghost.Issue) {
		t.Error("a concrete selector absent from a capped/partial digest must NOT be dropped (no absence-inference)")
	}
	// A non-concrete selector (bare tag) is likewise never a not-in-DOM drop.
	vague := report.EvalFinding{Issue: "form has no labels", Selector: "form"}
	if out := dropContradicted(mkEval(vague), d); !hasIssue(out, vague.Issue) {
		t.Error("a non-concrete selector must not trigger a not-in-DOM drop")
	}
}

// A VISIBLE-label complaint and a label-QUALITY complaint are legitimate even when the
// digest resolves a programmatic (sr-only) label — they must NOT be dropped (audit 🟡#2).
func TestDropContradictedKeepsVisibleAndQualityComplaints(t *testing.T) {
	d := sampleDigest() // #signup-email HAS a programmatic <label for>
	cases := []report.EvalFinding{
		{Issue: "There is no visible label on the email field", Selector: "#signup-email", Evidence: "only a placeholder is shown"},
		{Issue: "The label is not descriptive", Selector: "#signup-email", Evidence: "just says 'field'"},
		{Issue: "Add a clearer label to the email input", Selector: "#signup-email"},
		{Issue: "The email field label is confusing", Selector: "#signup-email"},
	}
	for _, f := range cases {
		if out := dropContradicted(mkEval(f), d); !hasIssue(out, f.Issue) {
			t.Errorf("a visible/quality label complaint must NOT be dropped: %q", f.Issue)
		}
	}
}

// A "not keyboard-operable" claim on a merely-focusable NON-native element
// (div[role=button][tabindex=0]) must NOT be dropped — focusable is not sufficient for
// operability, so it is not a positive refutation (audit 🟡#3a).
func TestDropContradictedKeepsNotOperableOnFocusableDiv(t *testing.T) {
	d := &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "div", Role: "button", Selector: "div#fancy-btn", AccessibleName: "Go", Focusable: true},
		},
	}
	f := report.EvalFinding{Issue: "This control is not keyboard-operable", Selector: "#fancy-btn"}
	if out := dropContradicted(mkEval(f), d); !hasIssue(out, f.Issue) {
		t.Error("a 'not keyboard-operable' claim on a focusable non-native <div role=button> must NOT be dropped")
	}
}

// An "icon button has no accessible label" claim on a textContent-only <button>★</button>
// must NOT be dropped: textContent ("★") is not a real accessible label, and the gate no
// longer refutes name-absence on non-form-control interactive elements (audit 🟡#3b).
func TestDropContradictedKeepsIconButtonNameClaim(t *testing.T) {
	d := &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			// The JS derives accessible_name from textContent ("★"), source "text".
			{Tag: "button", Selector: "button#star", AccessibleName: "★", Focusable: true},
		},
	}
	f := report.EvalFinding{Issue: "The icon button has no accessible label", Selector: "#star"}
	if out := dropContradicted(mkEval(f), d); !hasIssue(out, f.Issue) {
		t.Error("an icon-button name-absence claim (textContent-only) must NOT be dropped")
	}
}

// A label-absence claim refuted only by a PLACEHOLDER must NOT be dropped — a placeholder
// is not a programmatic label (has_label=false), so it is not a positive refutation (🟡#2a).
func TestDropContradictedKeepsPlaceholderOnlyControl(t *testing.T) {
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#search", AccessibleName: "Search", HasLabel: false, LabelSource: "placeholder"},
		},
	}
	f := report.EvalFinding{Issue: "Search field has no label", Selector: "#search"}
	if out := dropContradicted(mkEval(f), d); !hasIssue(out, f.Issue) {
		t.Error("a label-absence claim refuted only by a placeholder must NOT be dropped")
	}
}

// A control with a real PROGRAMMATIC accessible name → "add aria-label" IS a positive
// refutation → dropped (the feature must still kill the meta-audit FPs; audit 🟡#2/#3).
func TestDropContradictedDropsAddAriaLabelOnLabelledControl(t *testing.T) {
	d := &report.A11yDigest{
		FormControls: []report.A11yFormControl{
			{Selector: "input#email2", AccessibleName: "Email", HasLabel: true, LabelSource: "aria-label"},
		},
	}
	f := report.EvalFinding{Issue: "Add an aria-label to the email field", Selector: "#email2"}
	if out := dropContradicted(mkEval(f), d); hasIssue(out, f.Issue) {
		t.Error("an 'add aria-label' claim on a control that already has a programmatic name must be dropped")
	}
}

// A contradicted top_fix is cleared (nil), while a legitimate top_fix survives (audit 🟡#5).
func TestDropContradictedClearsRefutedTopFix(t *testing.T) {
	d := sampleDigest() // #signup-email has a programmatic label; a#client-1 is a link

	// (1) A top_fix that is itself a refuted a11y FP → cleared.
	fp := report.PageEvaluation{
		Comprehension: "unclear",
		TopFix:        &report.EvalTopFix{Selector: "#signup-email", Change: "Add a label to the email input", Impact: "high"},
	}
	if out := dropContradicted(fp, d); out.TopFix != nil {
		t.Errorf("a contradicted top_fix must be cleared, got %+v", out.TopFix)
	}

	// (2) A legitimate top_fix (not an objective a11y claim) survives.
	ok := report.PageEvaluation{
		Comprehension: "unclear",
		TopFix:        &report.EvalTopFix{Selector: "h1", Change: "Clarify the headline copy", Impact: "medium"},
	}
	if out := dropContradicted(ok, d); out.TopFix == nil {
		t.Error("a legitimate top_fix must NOT be cleared")
	}
}

// The class-only / shared-key merge must not drive a drop: two distinct <a>s under the
// same non-unique class selector are NOT indexed, so a class-anchored claim is never a
// positive refute (audit 🟢 buildDigestIndex + the "unique concrete keys only" rule).
func TestDropContradictedIgnoresClassOnlySelectors(t *testing.T) {
	d := &report.A11yDigest{
		Interactive: []report.A11yInteractive{
			{Tag: "a", Selector: "a.client-card", AccessibleName: "One", Focusable: true},
			{Tag: "a", Selector: "a.client-card", AccessibleName: "Two", Focusable: true},
		},
	}
	f := report.EvalFinding{Issue: "The client cards are not keyboard-operable", Selector: ".client-card"}
	if out := dropContradicted(mkEval(f), d); !hasIssue(out, f.Issue) {
		t.Error("a class-only selector (shared by distinct elements) must NOT drive a deterministic drop")
	}
}

// An oversized digest artifact is treated as EMPTY (no drop, non-fatal) on the read
// side — belt-and-suspenders for the capture-time cap (audit 🟡#4).
func TestLoadDigestRejectsOversized(t *testing.T) {
	d, st := setup(t)
	g := New(d, st, &fakeDrafter{}, "test-model")

	// A valid, in-cap digest loads.
	small := []byte(`{"interactive":[{"tag":"a","selector":"a#x","focusable":true}]}`)
	if err := st.Put(context.Background(), "ok.json", "application/json", bytes.NewReader(small), int64(len(small))); err != nil {
		t.Fatal(err)
	}
	if got := g.loadDigest(context.Background(), "ok.json"); got == nil {
		t.Fatal("a small valid digest should load")
	}

	// An over-cap payload (valid JSON, just too large) is ignored → nil.
	big := []byte(`{"interactive":[{"tag":"a","selector":"a#x","accessible_name":"` +
		strings.Repeat("x", report.MaxA11yDigestBytes+1024) + `"}]}`)
	if len(big) <= report.MaxA11yDigestBytes {
		t.Fatalf("test bug: payload %d not over cap %d", len(big), report.MaxA11yDigestBytes)
	}
	if err := st.Put(context.Background(), "big.json", "application/json", bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatal(err)
	}
	if got := g.loadDigest(context.Background(), "big.json"); got != nil {
		t.Error("an over-cap digest must be treated as absent (nil)")
	}
}
