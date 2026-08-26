package db

import (
	"context"
	"testing"
)

// seedDoneRun creates a run for target tgt, inserts one page carrying the given
// per-type finding counts, and finishes it 'done'. Returns the run id.
func seedDoneRun(t *testing.T, d *DB, userID, targetID string, a11y, layout, console, network, perf int) string {
	t.Helper()
	run, err := d.CreateRun(userID, targetID)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	pid, err := d.InsertPage(&Page{RunID: run.ID, URL: "https://t.test/", Viewport: "mobile"})
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	add := func(typ string, n int) {
		for i := 0; i < n; i++ {
			if _, err := d.InsertFinding(&Finding{PageID: pid, Type: typ, Severity: "moderate", Detail: "{}"}); err != nil {
				t.Fatalf("insert finding: %v", err)
			}
		}
	}
	add(FindingA11y, a11y)
	add(FindingLayout, layout)
	add(FindingConsole, console)
	add(FindingNetwork, network)
	add(FindingPerf, perf)
	if err := d.FinishRun(run.ID, RunDone, "{}", ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return run.ID
}

func TestTargetFindingTrend(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// Three completed runs, oldest→newest, with rising a11y debt.
	r1 := seedDoneRun(t, d, "u", tgt.ID, 2, 0, 1, 0, 0)
	r2 := seedDoneRun(t, d, "u", tgt.ID, 5, 1, 2, 1, 0)
	r3 := seedDoneRun(t, d, "u", tgt.ID, 3, 0, 0, 0, 2)

	// A queued run and a failed run must NOT appear in the trend.
	if _, err := d.CreateRun("u", tgt.ID); err != nil {
		t.Fatal(err)
	}
	fr, _ := d.CreateRun("u", tgt.ID)
	_ = d.FinishRun(fr.ID, RunFailed, "{}", "boom")

	pts, err := d.TargetFindingTrend("u", tgt.ID)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("want 3 completed-run points, got %d", len(pts))
	}
	// Chronological order.
	if pts[0].RunID != r1 || pts[1].RunID != r2 || pts[2].RunID != r3 {
		t.Errorf("out of chronological order: %s %s %s", pts[0].RunID, pts[1].RunID, pts[2].RunID)
	}
	// Per-type counts.
	want := []TrendPoint{
		{A11y: 2, Layout: 0, Console: 1, Network: 0, Perf: 0},
		{A11y: 5, Layout: 1, Console: 2, Network: 1, Perf: 0},
		{A11y: 3, Layout: 0, Console: 0, Network: 0, Perf: 2},
	}
	for i, w := range want {
		g := pts[i]
		if g.A11y != w.A11y || g.Layout != w.Layout || g.Console != w.Console || g.Network != w.Network || g.Perf != w.Perf {
			t.Errorf("point %d counts = {a11y:%d layout:%d console:%d network:%d perf:%d}, want %+v",
				i, g.A11y, g.Layout, g.Console, g.Network, g.Perf, w)
		}
		if g.At.IsZero() {
			t.Errorf("point %d has zero finished_at", i)
		}
	}
}

// TestTargetFindingTrendExcludesWalkthrough proves a synthetic walkthrough run (done,
// zero findings) does NOT produce a trend point — otherwise it would inject a false
// zero as the newest point and read as "findings just dropped to zero".
func TestTargetFindingTrendExcludesWalkthrough(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// One normal completed run with real findings.
	r1 := seedDoneRun(t, d, "u", tgt.ID, 4, 0, 2, 0, 0)

	// A done walkthrough ON THE SAME target, materialized into a synthetic
	// trigger='walkthrough' run (pages, zero findings). It must NOT appear in the trend.
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("walkthrough: %v", err)
	}
	for i, url := range []string{"https://t.test/a", "https://t.test/b"} {
		if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
			WalkthroughID: wk.ID, Idx: i, ActionJSON: `{"type":"click"}`, URL: url, Outcome: "ok",
		}); err != nil {
			t.Fatalf("step: %v", err)
		}
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish walkthrough: %v", err)
	}
	synthID, err := d.MaterializeWalkthroughRun(context.Background(), nil, "u", wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	pts, err := d.TargetFindingTrend("u", tgt.ID)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("want 1 trend point (walkthrough run excluded), got %d", len(pts))
	}
	if pts[0].RunID != r1 {
		t.Fatalf("trend point = %q, want the normal run %q", pts[0].RunID, r1)
	}
	if pts[0].RunID == synthID {
		t.Fatalf("synthetic walkthrough run leaked into the trend: %q", synthID)
	}
	if pts[0].A11y != 4 || pts[0].Console != 2 {
		t.Fatalf("trend counts should reflect the normal run, got %+v", pts[0])
	}
}

func TestTargetFindingTrendOwnerScoped(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	seedDoneRun(t, d, "u", tgt.ID, 2, 0, 0, 0, 0)
	seedDoneRun(t, d, "u", tgt.ID, 3, 0, 0, 0, 0)

	// Another user must not see this target's trend.
	pts, err := d.TargetFindingTrend("u2", tgt.ID)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("cross-user trend should be empty, got %d points", len(pts))
	}
}

func TestTargetFindingTrendFewRuns(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// No completed runs → empty (nil-safe).
	pts, err := d.TargetFindingTrend("u", tgt.ID)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("no-run trend should be empty, got %d", len(pts))
	}

	// A single completed run with ZERO findings still returns one point (all 0).
	seedDoneRun(t, d, "u", tgt.ID, 0, 0, 0, 0, 0)
	pts, err = d.TargetFindingTrend("u", tgt.ID)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("single-run trend should have 1 point, got %d", len(pts))
	}
	if pts[0].A11y != 0 || pts[0].Console != 0 {
		t.Errorf("zero-finding run should have zero counts, got %+v", pts[0])
	}
}
