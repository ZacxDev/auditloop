package db

import "testing"

// TestWalkthroughBaselineLinking proves CreateWalkthrough stamps prev_walkthrough_id
// with the target's NEWEST TERMINAL walkthrough, "" for the first, and never a
// non-terminal one (mirrors runs.prev_run_id baseline linking).
func TestWalkthroughBaselineLinking(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// First walkthrough → no baseline.
	wk1, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("create wk1: %v", err)
	}
	if wk1.PrevWalkthroughID != "" {
		t.Fatalf("first walkthrough should have no baseline, got %q", wk1.PrevWalkthroughID)
	}

	// wk1 still idle (non-terminal): the next walkthrough must NOT baseline-link to it.
	wkMid, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wkMid.PrevWalkthroughID != "" {
		t.Fatalf("non-terminal wk1 must not be a baseline, got %q", wkMid.PrevWalkthroughID)
	}

	// Finish wk1 (terminal) → the next walkthrough links to it.
	if err := d.FinishWalkthrough(wk1.ID, WalkOutcomeSuccess, 0, "reached", false); err != nil {
		t.Fatal(err)
	}
	wk2, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk2.PrevWalkthroughID != wk1.ID {
		t.Fatalf("wk2 baseline = %q, want wk1 %q", wk2.PrevWalkthroughID, wk1.ID)
	}

	// Finish wk2 → wk3 links to the NEWEST terminal (wk2), not wk1.
	if err := d.FinishWalkthrough(wk2.ID, WalkOutcomeStuck, 3, "budget", false); err != nil {
		t.Fatal(err)
	}
	wk3, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk3.PrevWalkthroughID != wk2.ID {
		t.Fatalf("wk3 baseline = %q, want newest terminal wk2 %q", wk3.PrevWalkthroughID, wk2.ID)
	}

	// A DIFFERENT target's walkthroughs never cross into the baseline.
	other, _ := d.CreateTarget("u", "O", "https://o.test", nil)
	wkOther, _ := d.CreateWalkthrough(other.ID, "", "sign up", true)
	if wkOther.PrevWalkthroughID != "" {
		t.Fatalf("other target's first walkthrough should have no baseline, got %q", wkOther.PrevWalkthroughID)
	}
}

// TestSetWalkthroughDiffRoundTrips proves diff_json persists + reads back, defaults ""
// (migration applied on sqlite), and is owner-scoped through GetWalkthrough.
func TestSetWalkthroughDiffRoundTrips(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("owner", "T", "https://t.test", nil)
	wk, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)

	// Fresh row: diff_json defaults to "" (additive migration applied).
	if wk.DiffJSON != "" {
		t.Fatalf("fresh diff_json = %q, want empty", wk.DiffJSON)
	}

	const blob = `{"prev_walkthrough_id":"prev","is_regression":true,"new_task_blockers":["skeptic#submit"]}`
	if err := d.SetWalkthroughDiff(wk.ID, blob); err != nil {
		t.Fatalf("set diff: %v", err)
	}
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.DiffJSON != blob {
		t.Fatalf("diff_json = %q, want %q", got.DiffJSON, blob)
	}
	// Owner-scoped read carries the diff too.
	scoped, err := d.GetWalkthrough("owner", wk.ID)
	if err != nil || scoped.DiffJSON != blob {
		t.Fatalf("owner-scoped diff = %q err=%v", scoped.DiffJSON, err)
	}
	// A foreign user cannot read it at all.
	if _, err := d.GetWalkthrough("intruder", wk.ID); err != ErrNotFound {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
}

// TestWalkthroughInfraFailedRoundTrips proves migration 0064 applied on sqlite, that
// infra_failed round-trips through FinishWalkthrough + the shared scan (both scoped and
// unscoped reads), and that the startup sweep marks a restart-orphaned walkthrough as
// an INFRA failure (#45).
func TestWalkthroughInfraFailedRoundTrips(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("owner", "T", "https://t.test", nil)

	// Fresh row: infra_failed defaults to false (additive migration applied).
	wk, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk.InfraFailed {
		t.Fatal("fresh walkthrough should not be infra_failed")
	}

	// A normal product-side failure is NOT infra.
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeFailed, 0, "off-domain", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetWalkthroughByID(wk.ID); got.InfraFailed {
		t.Fatal("product-side failure must not be flagged infra_failed")
	}

	// An infra failure round-trips through BOTH read paths (the shared scan helper).
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeFailed, 0, "browser stalled", true); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetWalkthroughByID(wk.ID)
	if err != nil || !got.InfraFailed {
		t.Fatalf("unscoped read infra_failed = %v err=%v, want true", got.InfraFailed, err)
	}
	scoped, err := d.GetWalkthrough("owner", wk.ID)
	if err != nil || !scoped.InfraFailed {
		t.Fatalf("owner-scoped read infra_failed = %v err=%v, want true", scoped.InfraFailed, err)
	}

	// A fresh claim clears the stale flag (a re-run has not failed on infra yet).
	if ok, err := d.ClaimWalkthroughJob(wk.ID, 5); err != nil || !ok {
		t.Fatalf("claim = %v err=%v", ok, err)
	}
	if again, _ := d.GetWalkthroughByID(wk.ID); again.InfraFailed {
		t.Fatal("ClaimWalkthroughJob must reset infra_failed for the new pass")
	}

	// The startup sweep settles a driving walkthrough as an INFRA failure — a restart
	// orphaned it, so it produced no observation of the goal.
	if n, err := d.MarkDrivingWalkthroughsFailed(); err != nil || n != 1 {
		t.Fatalf("sweep swept %d err=%v, want 1", n, err)
	}
	swept, _ := d.GetWalkthroughByID(wk.ID)
	if !swept.InfraFailed {
		t.Fatal("MarkDrivingWalkthroughsFailed must set infra_failed=1")
	}
}

// TestBaselineSkipsInfraFailedWalkthrough is the #45 baseline-exclusion test: an
// infra-failed walkthrough is never a regression baseline, so the next real walkthrough
// links to the last one that ACTUALLY RAN. The CONTROL (a non-infra `failed`) proves
// the query still accepts a genuine product-side failure — without it this would pass
// against a bug that excluded every `failed` walkthrough.
func TestBaselineSkipsInfraFailedWalkthrough(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// wk1: a real pass that reached the goal.
	wk1, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err := d.FinishWalkthrough(wk1.ID, WalkOutcomeSuccess, 0, "reached", false); err != nil {
		t.Fatal(err)
	}
	// wk2: NEWER, but the driver never ran (a killed browser stall).
	wk2, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err := d.FinishWalkthrough(wk2.ID, WalkOutcomeFailed, 0, "browser stalled", true); err != nil {
		t.Fatal(err)
	}
	// wk3 must baseline against wk1 (the last real observation), NOT the infra failure.
	wk3, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk3.PrevWalkthroughID != wk1.ID {
		t.Fatalf("baseline = %q, want the last non-infra walkthrough wk1 %q (an infra failure is not a baseline)",
			wk3.PrevWalkthroughID, wk1.ID)
	}

	// CONTROL: a genuine PRODUCT-side failure IS still a valid baseline.
	if err := d.FinishWalkthrough(wk3.ID, WalkOutcomeFailed, 0, "off-domain navigate refused", false); err != nil {
		t.Fatal(err)
	}
	wk4, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk4.PrevWalkthroughID != wk3.ID {
		t.Fatalf("control: baseline = %q, want the non-infra failed wk3 %q", wk4.PrevWalkthroughID, wk3.ID)
	}
}
