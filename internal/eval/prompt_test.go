package eval

import (
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/report"
)

func sampleGrounding() Grounding {
	return Grounding{
		URL:          "https://acme.test/pricing",
		Viewports:    []string{"desktop", "mobile"},
		Job:          "sign up for a paid plan",
		FlowPos:      2,
		FlowTotal:    3,
		PrevURL:      "https://acme.test/",
		NextURL:      "https://acme.test/checkout",
		A11yRuleIDs:  []string{"color-contrast", "image-alt"},
		A11yCount:    4,
		ConsoleFirst: 1,
		NetworkThird: 2,
		Perf:         &report.Perf{LCPMs: 5000, CLS: 0.3, TBTMs: 800, WeightBytes: 4 << 20, ReqCount: 120},
		LayoutSmells: []string{"small-tap-targets"},
	}
}

func TestGenUserPromptIncludesPersonaFlowJobGrounding(t *testing.T) {
	g := sampleGrounding()
	p, _ := PersonaByID("skeptical-evaluator")
	got := g.GenUserPrompt(p)

	// Persona text present.
	if !strings.Contains(got, "comparison-shopping evaluator") {
		t.Error("prompt missing the persona's 'cares' description")
	}
	// Flow position + job present.
	if !strings.Contains(got, "step 2 of 3") {
		t.Errorf("prompt missing flow position: %q", got)
	}
	if !strings.Contains(got, "sign up for a paid plan") {
		t.Error("prompt missing the job/task")
	}
	if !strings.Contains(got, "https://acme.test/") || !strings.Contains(got, "https://acme.test/checkout") {
		t.Error("prompt missing prev/next flow URLs")
	}
	// Deterministic grounding is INCLUDED (as grounding, not findings).
	if !strings.Contains(got, "color-contrast") || !strings.Contains(got, "4 axe violation") {
		t.Error("prompt missing a11y grounding")
	}
	if !strings.Contains(got, "LCP 5000ms") {
		t.Error("prompt missing perf grounding")
	}
	if !strings.Contains(got, "small-tap-targets") {
		t.Error("prompt missing layout-smell grounding")
	}
	// It must instruct NOT to re-emit the deterministic signals as findings.
	if !strings.Contains(GenSystemPrompt, "Do NOT re-list") {
		t.Error("system prompt should forbid re-listing deterministic signals as findings")
	}
}

func TestParseEvaluationValid(t *testing.T) {
	reply := "```json\n" + `{"comprehension":"unclear",
		"blockers":[{"issue":"No pricing shown","selector":".pricing","evidence":"empty section"}],
		"frictions":[{"issue":"Dense copy","selector":"p.lead","evidence":"wall of text"}],
		"top_fix":{"selector":".cta","change":"Add a clear price","rationale":"trust","impact":"HIGH"}}` + "\n```"
	pe, err := ParseEvaluation(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pe.Comprehension != "unclear" {
		t.Errorf("comprehension = %q", pe.Comprehension)
	}
	if len(pe.Blockers) != 1 || pe.Blockers[0].Issue != "No pricing shown" {
		t.Errorf("blockers not parsed: %+v", pe.Blockers)
	}
	if pe.TopFix == nil || pe.TopFix.Impact != "high" {
		t.Errorf("top_fix impact should be normalized to lower-case: %+v", pe.TopFix)
	}
}

func TestParseEvaluationMalformedIsErrorNotPanic(t *testing.T) {
	if _, err := ParseEvaluation("the model refused to answer"); err == nil {
		t.Error("expected an error for a non-JSON reply")
	}
	// Valid JSON but comprehension outside the closed set → error.
	if _, err := ParseEvaluation(`{"comprehension":"maybe"}`); err == nil {
		t.Error("expected an error for an invalid comprehension value")
	}
}

func TestApplyVerificationFiltersUnverified(t *testing.T) {
	draft := report.PageEvaluation{
		Comprehension: "blocked",
		Blockers: []report.EvalFinding{
			{Issue: "real blocker", Selector: "#a"},
			{Issue: "speculative blocker", Selector: "#b"},
		},
		Frictions: []report.EvalFinding{{Issue: "minor", Selector: "#c"}},
	}
	// The verify pass keeps only the substantiated blocker + no frictions.
	verifyReply := `{"comprehension":"blocked","blockers":[{"issue":"real blocker","selector":"#a","evidence":"the CTA is missing","verified":true}],"frictions":[]}`
	out := applyVerification(draft, verifyReply)
	if len(out.Blockers) != 1 || !out.Blockers[0].Verified {
		t.Errorf("verification should keep 1 verified blocker: %+v", out.Blockers)
	}
	if len(out.Frictions) != 0 {
		t.Errorf("unverified friction should be dropped: %+v", out.Frictions)
	}
}

func TestApplyVerificationDegradesOnBadReply(t *testing.T) {
	draft := report.PageEvaluation{Comprehension: "clear", Blockers: []report.EvalFinding{{Issue: "x"}}}
	out := applyVerification(draft, "garbage not json")
	if len(out.Blockers) != 1 {
		t.Error("a bad verify reply should degrade to the unchanged draft, not lose findings")
	}
}

func TestParseSynthesisCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"improvements":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"item","impact":"high"}`)
	}
	b.WriteString("]}")
	items, err := ParseSynthesis(b.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != MaxSynthItems {
		t.Errorf("synthesis should be capped at %d, got %d", MaxSynthItems, len(items))
	}
}

// TestParseSynthesisRealisticMultiItem asserts a full, rich multi-improvement body
// (the shape the larger token budget now lets the model finish) parses cleanly.
func TestParseSynthesisRealisticMultiItem(t *testing.T) {
	reply := "```json\n" + `{"improvements":[
		{"title":"Add a prominent primary CTA","rationale":"first-time visitors can't tell what to do next","impact":"high","affected_urls":["https://acme.test/","https://acme.test/pricing"],"affected_personas":["first-time-nontechnical","skeptical-evaluator"],"selector":".hero"},
		{"title":"Surface pricing before signup","rationale":"skeptical evaluators bounce without price clarity","impact":"high","affected_urls":["https://acme.test/pricing"],"affected_personas":["skeptical-evaluator"]},
		{"title":"Improve tap-target sizing on mobile","rationale":"controls are under 44px","impact":"medium","affected_urls":["https://acme.test/checkout"],"affected_personas":["accessibility-constrained"]}
	]}` + "\n```"
	items, err := ParseSynthesis(reply)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 improvements, got %d", len(items))
	}
	if items[0].Title != "Add a prominent primary CTA" || items[0].Impact != "high" {
		t.Errorf("item 0 not parsed: %+v", items[0])
	}
	if len(items[0].AffectedURLs) != 2 || len(items[0].AffectedPersonas) != 2 {
		t.Errorf("item 0 affected lists not parsed: %+v", items[0])
	}
}

// TestParseSynthesisTruncatedSalvagesPrefix simulates the LIVE bug: the completion
// is cut off mid-JSON (the failure was "unexpected end of JSON input"). The strict
// decode fails, but the parser salvages the COMPLETED prefix objects rather than
// losing the whole story — and never panics.
func TestParseSynthesisTruncatedSalvagesPrefix(t *testing.T) {
	// Two complete items, then a third truncated mid-string (no closing brace/array).
	truncated := `{"improvements":[` +
		`{"title":"Add a prominent CTA","rationale":"blocks signup","impact":"high","affected_urls":["https://acme.test/"]},` +
		`{"title":"Surface pricing","rationale":"evaluators bounce","impact":"high"},` +
		`{"title":"Improve tap targets","rationale":"controls under 44p`
	items, err := ParseSynthesis(truncated)
	if err != nil {
		t.Fatalf("truncated body should salvage cleanly, got error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 salvaged (completed) items, got %d: %+v", len(items), items)
	}
	if items[0].Title != "Add a prominent CTA" || items[1].Title != "Surface pricing" {
		t.Errorf("salvaged items wrong: %+v", items)
	}
}

// TestParseSynthesisUnsalvageableIsCleanError asserts that a body with NO completed
// item degrades to a clean error (so the caller logs it) — never a panic, never a
// silent fake success.
func TestParseSynthesisUnsalvageableIsCleanError(t *testing.T) {
	// The very first object is truncated → nothing completed to salvage.
	if _, err := ParseSynthesis(`{"improvements":[{"title":"Add a CTA`); err == nil {
		t.Error("an unsalvageable truncated body should return an error (logged, non-fatal)")
	}
	// Not JSON at all.
	if _, err := ParseSynthesis("the model refused"); err == nil {
		t.Error("a non-JSON reply should return an error")
	}
	// Empty improvements array → empty result, no error.
	items, err := ParseSynthesis(`{"improvements":[]}`)
	if err != nil || len(items) != 0 {
		t.Errorf("empty improvements → want (nil,nil), got (%v,%v)", items, err)
	}
}

func TestPersonaAllowlist(t *testing.T) {
	if len(Personas) != 4 {
		t.Fatalf("expected exactly 4 curated personas, got %d", len(Personas))
	}
	for _, id := range []string{"first-time-nontechnical", "returning-power-user", "skeptical-evaluator", "accessibility-constrained"} {
		if !PersonaAllowed(id) {
			t.Errorf("persona %q should be allowed", id)
		}
	}
	if PersonaAllowed("evil/backdoor") {
		t.Error("unknown persona must be rejected")
	}
}
