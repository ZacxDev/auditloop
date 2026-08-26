package pages

import (
	"strings"
	"testing"
)

// TestWalkChangesCardNoDiffYet (Fix 2) proves the rare no-diff-yet path — a baseline
// exists (HasBaseline) but the non-fatal drive-end refresh never produced a diff
// (HasDiff=false) — renders a NEUTRAL pending note, NOT a misleading "Stable" badge or
// an "Outcome unchanged ()" line over an empty outcome.
func TestWalkChangesCardNoDiffYet(t *testing.T) {
	out := renderNode(t, walkChangesCard(&WalkChangesVM{HasBaseline: true, HasDiff: false}))
	if strings.Contains(out, "Stable") {
		t.Errorf("no-diff-yet card must not render a Stable verdict: %s", out)
	}
	if strings.Contains(out, "unchanged") {
		t.Errorf("no-diff-yet card must not render an 'Outcome unchanged' line: %s", out)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("no-diff-yet card should render a pending note: %s", out)
	}
}

// TestWalkChangesCardNoBaseline keeps the first-walkthrough note distinct from the
// pending state.
func TestWalkChangesCardNoBaseline(t *testing.T) {
	out := renderNode(t, walkChangesCard(&WalkChangesVM{HasBaseline: false}))
	if !strings.Contains(out, "First walkthrough") {
		t.Errorf("no-baseline card should render the first-walkthrough note: %s", out)
	}
}

// TestWalkChangesCardRendersReason (Fix 3) proves a changed failure Reason is surfaced
// (escaped) in the card when the outcome is failed/stuck.
func TestWalkChangesCardRendersReason(t *testing.T) {
	vm := &WalkChangesVM{
		HasBaseline: true, HasDiff: true, OutcomeCompared: true,
		OutcomeChanged: true, PrevOutcome: "success", Outcome: "failed", IsRegression: true,
		ReasonChanged: true, Reason: "off-domain navigate refused",
	}
	out := renderNode(t, walkChangesCard(vm))
	if !strings.Contains(out, "off-domain navigate refused") {
		t.Errorf("card should render the changed reason: %s", out)
	}

	// A success outcome carries no failure reason to report — even if ReasonChanged.
	vm2 := &WalkChangesVM{
		HasBaseline: true, HasDiff: true, OutcomeCompared: true, Outcome: "success",
		ReasonChanged: true, Reason: "should-not-appear",
	}
	if strings.Contains(renderNode(t, walkChangesCard(vm2)), "should-not-appear") {
		t.Error("a success outcome should not render a failure reason")
	}
}

// TestWalkChangesCardInfraFailed is the #45 UI half: a walkthrough whose DRIVER could
// not run renders a neutral "could not run" state, never the red Regression badge that
// reads as a product regression. The CONTROL below is the same outcome transition with
// the infra flag off, which must still render Regression.
func TestWalkChangesCardInfraFailed(t *testing.T) {
	infra := &WalkChangesVM{
		HasBaseline: true, HasDiff: true,
		OutcomeChanged: true, PrevOutcome: "success", Outcome: "failed",
		OutcomeCompared: false, InfraFailed: true,
		Reason: "the browser stopped making progress and was killed",
	}
	out := renderNode(t, walkChangesCard(infra))
	if strings.Contains(out, "Regression") {
		t.Errorf("an infra-failed walkthrough must not render a Regression badge: %s", out)
	}
	if !strings.Contains(out, "Could not run") {
		t.Errorf("expected the neutral could-not-run state: %s", out)
	}
	if !strings.Contains(out, "badge-warning") {
		t.Errorf("expected the design-system amber badge class: %s", out)
	}
	if !strings.Contains(out, "the browser stopped making progress") {
		t.Errorf("the driver reason should still be surfaced (escaped): %s", out)
	}

	// CONTROL: the SAME transition, actually compared → still a red Regression badge.
	ctl := &WalkChangesVM{
		HasBaseline: true, HasDiff: true, OutcomeCompared: true,
		OutcomeChanged: true, PrevOutcome: "success", Outcome: "failed", IsRegression: true,
	}
	if got := renderNode(t, walkChangesCard(ctl)); !strings.Contains(got, "Regression") {
		t.Errorf("control: a genuinely compared success→failed must still render Regression: %s", got)
	}
}

// TestWalkChangesCardBaselineUnusable covers the OTHER not-compared subject: THIS
// walkthrough ran fine, the BASELINE is the unusable one (OutcomeCompared=false without
// InfraFailed). The card must not say "Could not run" — that names the wrong subject and
// directly contradicts its own explanatory note. It must also not leak this
// walkthrough's reason as if it were the failure.
func TestWalkChangesCardBaselineUnusable(t *testing.T) {
	vm := &WalkChangesVM{
		HasBaseline: true, HasDiff: true,
		OutcomeChanged: true, PrevOutcome: "failed", Outcome: "success",
		OutcomeCompared: false, InfraFailed: false,
		Reason: "reached the goal",
	}
	out := renderNode(t, walkChangesCard(vm))
	if strings.Contains(out, "Could not run") {
		t.Errorf("a walkthrough that RAN must not be badged 'Could not run' — the baseline is the unusable one: %s", out)
	}
	if !strings.Contains(out, "Not compared") {
		t.Errorf("expected a 'Not compared' badge naming the right subject: %s", out)
	}
	if strings.Contains(out, "Regression") || strings.Contains(out, "Resolved") {
		t.Errorf("nothing was compared, so no verdict may be rendered: %s", out)
	}
	if strings.Contains(out, "reached the goal") {
		t.Errorf("this walkthrough's own reason must not be shown as the failure reason: %s", out)
	}
}
