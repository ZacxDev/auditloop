package walkthrough

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
)

// TestComputeDiffOutcomeTransitions is the core, non-vacuous classification table: it
// asserts the exact is_regression/resolved verdict AND the stuck-step delta for every
// outcome transition the CI gate keys on.
func TestComputeDiffOutcomeTransitions(t *testing.T) {
	cases := []struct {
		name                         string
		prevOutcome, curOutcome      string
		prevStuck, curStuck          int
		wantRegression, wantResolved bool
		wantChanged                  bool
		wantStuckDelta               int
	}{
		{"success→stuck is a regression", "success", "stuck", 0, 3, true, false, true, 0},
		{"success→failed is a regression", "success", "failed", 0, 0, true, false, true, 0},
		{"stuck→failed is a regression", "stuck", "failed", 2, 0, true, false, true, 0},
		{"stuck→success is resolved", "stuck", "success", 2, 0, false, true, true, 0},
		{"failed→success is resolved", "failed", "success", 0, 0, false, true, true, 0},
		{"stuck earlier is a regression", "stuck", "stuck", 5, 2, true, false, false, -3},
		{"stuck later is progress, not a regression", "stuck", "stuck", 2, 5, false, false, false, 3},
		{"same stuck step is stable", "stuck", "stuck", 3, 3, false, false, false, 0},
		{"success→success is stable", "success", "success", 0, 0, false, false, false, 0},
		{"failed→failed is stable", "failed", "failed", 0, 0, false, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := &db.Walkthrough{ID: "prev", Outcome: tc.prevOutcome, StuckStep: tc.prevStuck}
			cur := &db.Walkthrough{ID: "cur", Outcome: tc.curOutcome, StuckStep: tc.curStuck}
			d := ComputeDiff(prev, cur, nil, nil, false)
			if d.IsRegression != tc.wantRegression {
				t.Errorf("IsRegression = %v, want %v", d.IsRegression, tc.wantRegression)
			}
			if d.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", d.Resolved, tc.wantResolved)
			}
			if d.OutcomeChanged != tc.wantChanged {
				t.Errorf("OutcomeChanged = %v, want %v", d.OutcomeChanged, tc.wantChanged)
			}
			if d.StuckStepDelta != tc.wantStuckDelta {
				t.Errorf("StuckStepDelta = %d, want %d", d.StuckStepDelta, tc.wantStuckDelta)
			}
			if d.PrevWalkthroughID != "prev" {
				t.Errorf("PrevWalkthroughID = %q, want prev", d.PrevWalkthroughID)
			}
			// Every pre-existing (non-infra) case must report the outcome axis as
			// COMPARED — otherwise a consumer cannot tell "no regression" from
			// "not scored" (#45).
			if !d.OutcomeCompared {
				t.Errorf("OutcomeCompared = false, want true for a normally-driven pair")
			}
			if d.InfraFailed {
				t.Errorf("InfraFailed = true, want false for a normally-driven pair")
			}
		})
	}
}

// TestComputeDiffInfraFailedNotScored is the #45 regression test: a walkthrough that
// FAILED because the driver could not run (a watchdog-killed browser stall) must not be
// scored as a product regression. The DISCRIMINATING CONTROL is the identical
// success→failed transition WITHOUT the infra flag, which must still be a regression —
// without it this test would pass against a blanket "never regress" bug.
func TestComputeDiffInfraFailedNotScored(t *testing.T) {
	base := &db.Walkthrough{ID: "prev", Outcome: db.WalkOutcomeSuccess}

	// CONTROL: a genuine product-side failure is STILL a regression.
	product := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeFailed, Reason: "off-domain navigate refused"}
	ctl := ComputeDiff(base, product, nil, nil, false)
	if !ctl.IsRegression {
		t.Fatal("control: success→failed WITHOUT the infra flag must still be a regression")
	}
	if !ctl.OutcomeCompared || ctl.InfraFailed {
		t.Fatalf("control: OutcomeCompared=%v InfraFailed=%v, want true/false", ctl.OutcomeCompared, ctl.InfraFailed)
	}

	// THE FIX: the same transition, but the driver never ran.
	infra := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeFailed, Reason: "browser stalled", InfraFailed: true}
	d := ComputeDiff(base, infra, nil, nil, false)
	if d.IsRegression {
		t.Error("IsRegression = true for an INFRA-failed walkthrough — a pass that never ran cannot be a product regression (#45)")
	}
	if d.Resolved {
		t.Error("Resolved = true for an INFRA-failed walkthrough")
	}
	if d.OutcomeCompared {
		t.Error("OutcomeCompared = true for an INFRA-failed walkthrough — nothing was compared")
	}
	if !d.InfraFailed {
		t.Error("InfraFailed = false, want true so a CI gate can distinguish 'could not run' from 'no regression'")
	}
	// The descriptive fields still populate so the UI can show what happened.
	if d.PrevOutcome != db.WalkOutcomeSuccess || d.Outcome != db.WalkOutcomeFailed || !d.OutcomeChanged {
		t.Errorf("descriptive fields lost: prev=%q cur=%q changed=%v", d.PrevOutcome, d.Outcome, d.OutcomeChanged)
	}
}

// TestComputeDiffInfraFailedSuppressesStuckDelta pins the both-stuck axis: an
// infra-failed current walkthrough must not report stuck-step movement either (a
// negative delta is independently a regression signal).
func TestComputeDiffInfraFailedSuppressesStuckDelta(t *testing.T) {
	prev := &db.Walkthrough{ID: "prev", Outcome: db.WalkOutcomeStuck, StuckStep: 5}
	cur := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeStuck, StuckStep: 2, InfraFailed: true}
	d := ComputeDiff(prev, cur, nil, nil, false)
	if d.StuckStepDelta != 0 {
		t.Errorf("StuckStepDelta = %d, want 0 for an infra-failed walkthrough", d.StuckStepDelta)
	}
	if d.IsRegression {
		t.Error("IsRegression = true — a stuck-earlier delta must not be scored when the driver never ran")
	}
	// Control: the same shape WITHOUT the flag is a stuck-earlier regression.
	ok := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeStuck, StuckStep: 2}
	if c := ComputeDiff(prev, ok, nil, nil, false); c.StuckStepDelta != -3 || !c.IsRegression {
		t.Fatalf("control: delta=%d regression=%v, want -3/true", c.StuckStepDelta, c.IsRegression)
	}
}

// TestComputeDiffPrevInfraFailedNotCompared covers the DEFENSIVE baseline case: the
// baseline query already excludes infra-failed walkthroughs, but a pre-migration row
// reads back infra_failed=0 and could still be linked. Diffing against a
// non-observation is not a comparison.
func TestComputeDiffPrevInfraFailedNotCompared(t *testing.T) {
	prev := &db.Walkthrough{ID: "prev", Outcome: db.WalkOutcomeFailed, InfraFailed: true}
	cur := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeSuccess}
	d := ComputeDiff(prev, cur, nil, nil, false)
	if d.OutcomeCompared {
		t.Error("OutcomeCompared = true against an infra-failed BASELINE")
	}
	if d.Resolved || d.IsRegression {
		t.Errorf("scored against an infra-failed baseline: resolved=%v regression=%v", d.Resolved, d.IsRegression)
	}
	// InfraFailed describes the CURRENT walkthrough — which ran fine here.
	if d.InfraFailed {
		t.Error("InfraFailed should describe the CURRENT walkthrough, which ran normally")
	}
}

// TestComputeDiffInfraFailedSuppressesBlockers proves the blocker axis degrades too:
// even if a caller claimed blockersCompared, an infra-failed pass emits no blocker
// delta (the CI gate ORs new_task_blockers into --fail-on-regression).
func TestComputeDiffInfraFailedSuppressesBlockers(t *testing.T) {
	prev := &db.Walkthrough{ID: "prev", Outcome: db.WalkOutcomeSuccess}
	cur := &db.Walkthrough{ID: "cur", Outcome: db.WalkOutcomeFailed, InfraFailed: true}
	d := ComputeDiff(prev, cur, []string{"skeptic\x1f#a"}, []string{"skeptic\x1f#b"}, true)
	if d.BlockersCompared {
		t.Error("BlockersCompared = true for an infra-failed walkthrough")
	}
	if len(d.NewTaskBlockers) != 0 || len(d.ResolvedTaskBlockers) != 0 {
		t.Errorf("blocker delta emitted for an infra-failed walkthrough: new=%v resolved=%v", d.NewTaskBlockers, d.ResolvedTaskBlockers)
	}
	if d.NewTaskBlockers == nil {
		t.Error("NewTaskBlockers must stay a non-nil empty slice for stable JSON")
	}
}

// TestComputeDiffBlockerDelta proves new/resolved task-blockers come out of
// StringSetDelta, and that with blockersCompared=false the delta is suppressed
// (degrade) even when the key sets differ.
func TestComputeDiffBlockerDelta(t *testing.T) {
	prev := &db.Walkthrough{ID: "prev", Outcome: "success"}
	cur := &db.Walkthrough{ID: "cur", Outcome: "success"}
	prevKeys := []string{"skeptic\x1f#a", "skeptic\x1f#b"}
	curKeys := []string{"skeptic\x1f#b", "skeptic\x1f#c"} // #a resolved, #c new

	// Compared: #c is a NEW task-blocker (regression signal), #a is resolved.
	d := ComputeDiff(prev, cur, prevKeys, curKeys, true)
	if !reflect.DeepEqual(d.NewTaskBlockers, []string{"skeptic\x1f#c"}) {
		t.Fatalf("NewTaskBlockers = %v, want [skeptic\\x1f#c]", d.NewTaskBlockers)
	}
	if !reflect.DeepEqual(d.ResolvedTaskBlockers, []string{"skeptic\x1f#a"}) {
		t.Fatalf("ResolvedTaskBlockers = %v, want [skeptic\\x1f#a]", d.ResolvedTaskBlockers)
	}
	if !d.BlockersCompared {
		t.Fatal("BlockersCompared should be true")
	}

	// Not compared (either side lacks an eval): the delta is suppressed — NewTaskBlockers
	// is the empty slice, never a false "everything resolved".
	d2 := ComputeDiff(prev, cur, prevKeys, curKeys, false)
	if len(d2.NewTaskBlockers) != 0 || len(d2.ResolvedTaskBlockers) != 0 {
		t.Fatalf("uncompared blockers should be empty, got new=%v resolved=%v", d2.NewTaskBlockers, d2.ResolvedTaskBlockers)
	}
	if d2.NewTaskBlockers == nil {
		t.Fatal("NewTaskBlockers should be a non-nil empty slice for stable JSON")
	}
	if d2.BlockersCompared {
		t.Fatal("BlockersCompared should be false")
	}
}

// TestBlockerKeysFromEvaluations proves the stable identity key: persona + normalized
// selector (or issue when no selector), deduped, malformed cells skipped, and — the
// non-vacuous part — that ONLY VERIFIED blockers feed the key set (an unverified /
// verify-degraded blocker is excluded so it never trips the CI regression gate).
func TestBlockerKeysFromEvaluations(t *testing.T) {
	mk := func(persona string, pv report.PageEvaluation) *db.PageEvaluation {
		blob := mustJSON(t, pv)
		return &db.PageEvaluation{Persona: persona, FindingsJSON: blob}
	}
	evals := []*db.PageEvaluation{
		mk("skeptic", report.PageEvaluation{Blockers: []report.EvalFinding{
			{Issue: "no visible submit", Selector: "  #Submit  ", Verified: true}, // normalized → "#submit"
			{Issue: "Ambiguous  Label", Verified: true},                           // no selector → issue anchor
			// UNVERIFIED (verify-pass dropped it, or verify=off): must be EXCLUDED.
			// Without the !b.Verified guard this leaks "skeptic\x1f#unverified" and
			// the test fails on the extra key.
			{Issue: "maybe a problem", Selector: "#unverified", Verified: false},
		}}),
		// Same persona + same selector on another page → deduped to one key.
		mk("skeptic", report.PageEvaluation{Blockers: []report.EvalFinding{{Selector: "#submit", Verified: true}}}),
		// A DIFFERENT persona with the same selector is a DISTINCT key.
		mk("first-timer", report.PageEvaluation{Blockers: []report.EvalFinding{{Selector: "#submit", Verified: true}}}),
		{Persona: "skeptic", FindingsJSON: "{not json"}, // malformed → skipped, no panic
	}
	got := blockerKeysFromEvaluations(evals)
	want := map[string]bool{
		"skeptic\x1f#submit":         true,
		"skeptic\x1fambiguous label": true,
		"first-timer\x1f#submit":     true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys %v, want %d", len(got), got, len(want))
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected key %q (unverified blockers must be excluded)", k)
		}
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
