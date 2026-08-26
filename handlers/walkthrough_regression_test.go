package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/walkthrough"
)

// TestWalkthroughGeneratorPersistsDiff proves the DRIVE-END trigger: running a
// walkthrough through the generator (via the start handler, stubbed Drive) when a
// terminal baseline exists persists walkthroughs.diff_json with the outcome regression
// — no manual RefreshWalkthroughDiff call. Exercises generator.Run's diff wiring.
func TestWalkthroughGeneratorPersistsDiff(t *testing.T) {
	trace := &crawler.DriveTrace{
		Outcome:   "stuck",
		StuckStep: 1,
		Reason:    "budget exhausted",
		Steps: []crawler.StepRecord{
			{Idx: 0, Action: action.Action{Type: action.Click, Selector: "#go"}, URL: "https://site.test/", Outcome: "ok"},
		},
	}
	app := walkTestApp(t, true, stubDrive(trace))
	tgt := seedDriveTarget(t, app, true, true)

	// A prior SUCCESSFUL walkthrough is the baseline.
	base, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(base.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	// Drive a new one (stubbed → stuck). The generator finalizes it AND diffs it.
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rw.Code)
	}
	wk := waitWalkthroughDone(t, app, tgt.ID)
	if wk.ID == base.ID || wk.PrevWalkthroughID != base.ID {
		t.Fatalf("new walkthrough not baseline-linked: id=%s prev=%s base=%s", wk.ID, wk.PrevWalkthroughID, base.ID)
	}
	if wk.DiffJSON == "" {
		t.Fatal("generator did not persist diff_json at drive end")
	}
	var d report.WalkthroughDiff
	if err := json.Unmarshal([]byte(wk.DiffJSON), &d); err != nil {
		t.Fatalf("diff_json: %v", err)
	}
	if !d.IsRegression || d.PrevOutcome != "success" || d.Outcome != "stuck" {
		t.Fatalf("generator diff wrong: %+v", d)
	}
}

// TestWalkthroughReadAPIRegressionBlock proves the CI-gate shape: a walkthrough
// baseline-linked to a prior SUCCESS that itself STUCK surfaces a `regression` block
// with is_regression=true and a present (array) new_task_blockers via the read-API.
func TestWalkthroughReadAPIRegressionBlock(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, true, true)

	// Baseline: a SUCCESSFUL walkthrough.
	wk1, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(wk1.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	// Current: baseline-linked, then it STUCK — an outcome regression.
	wk2, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if wk2.PrevWalkthroughID != wk1.ID {
		t.Fatalf("wk2 not baseline-linked: %q", wk2.PrevWalkthroughID)
	}
	_, _ = app.DB.InsertWalkthroughStep(&db.WalkthroughStep{WalkthroughID: wk2.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://site.test/", Outcome: "ok"})
	_ = app.DB.FinishWalkthrough(wk2.ID, db.WalkOutcomeStuck, 1, "budget exhausted", false)

	// Compute + persist the diff (drive-end trigger).
	d, err := walkthrough.RefreshWalkthroughDiff(context.Background(), app.DB, wk2.ID)
	if err != nil || d == nil {
		t.Fatalf("refresh diff: d=%v err=%v", d, err)
	}
	if !d.IsRegression {
		t.Fatal("success→stuck should be an outcome regression")
	}

	// Read-API returns the regression block owner-scoped.
	rw := httptest.NewRecorder()
	app.handleAuditWalkthrough(rw, walkReq("GET", "/api/audit/walkthroughs/"+wk2.ID, nil, map[string]string{"id": wk2.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("read = %d, want 200", rw.Code)
	}
	var got apiWalkthrough
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Regression == nil {
		t.Fatal("regression block missing")
	}
	if !got.Regression.IsRegression {
		t.Fatalf("is_regression = false, want true: %+v", got.Regression)
	}
	if got.Regression.PrevOutcome != "success" || got.Regression.Outcome != "stuck" {
		t.Fatalf("transition = %s→%s, want success→stuck", got.Regression.PrevOutcome, got.Regression.Outcome)
	}
	if got.Regression.PrevWalkthroughID != wk1.ID {
		t.Fatalf("prev id = %q, want %q", got.Regression.PrevWalkthroughID, wk1.ID)
	}
	// Fix 3: the changed failure reason is real CI signal — it must surface in the
	// read-API regression block ("reached" → "budget exhausted").
	if !got.Regression.ReasonChanged {
		t.Fatalf("reason_changed = false, want true (reached→budget exhausted): %+v", got.Regression)
	}
	if got.Regression.Reason != "budget exhausted" {
		t.Fatalf("reason = %q, want %q", got.Regression.Reason, "budget exhausted")
	}
	// No eval on either side yet → blockers not compared, but the field is a present array.
	if got.Regression.BlockersCompared {
		t.Fatal("blockers should not be compared without evaluations")
	}
	// The CI gate reads new_task_blockers as an array (present, possibly empty).
	if !bytesContainsKey(rw.Body.Bytes(), "new_task_blockers") {
		t.Fatal("new_task_blockers key must be present for the CI gate")
	}
}

// TestWalkthroughReadAPINewTaskBlockers proves NewTaskBlockers is populated end-to-end
// through the read-API when BOTH walkthroughs have completed persona evaluations and a
// new blocker appeared in the current one.
func TestWalkthroughReadAPINewTaskBlockers(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, true, true)

	// Baseline walkthrough evaluated with ONE blocker (#a).
	wk1 := evaluatedWalkthrough(t, app, tgt.ID, `{"comprehension":"unclear","blockers":[{"issue":"x","selector":"#a","verified":true}]}`)
	// Current walkthrough, baseline-linked, evaluated with a NEW blocker (#b) — same
	// deterministic outcome (success), so the ONLY regression signal is the new blocker.
	wk2 := evaluatedWalkthrough(t, app, tgt.ID, `{"comprehension":"blocked","blockers":[{"issue":"y","selector":"#b","verified":true}]}`)
	if wk2.PrevWalkthroughID != wk1.ID {
		t.Fatalf("wk2 baseline = %q, want %q", wk2.PrevWalkthroughID, wk1.ID)
	}

	if _, err := walkthrough.RefreshWalkthroughDiff(context.Background(), app.DB, wk2.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rw := httptest.NewRecorder()
	app.handleAuditWalkthrough(rw, walkReq("GET", "/api/audit/walkthroughs/"+wk2.ID, nil, map[string]string{"id": wk2.ID}))
	var got apiWalkthrough
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if got.Regression == nil || !got.Regression.BlockersCompared {
		t.Fatalf("blockers should be compared: %+v", got.Regression)
	}
	if len(got.Regression.NewTaskBlockers) != 1 {
		t.Fatalf("new_task_blockers = %v, want exactly the new #b blocker", got.Regression.NewTaskBlockers)
	}
	// Outcome-axis is stable (success→success) — the gate fires purely on the new blocker.
	if got.Regression.IsRegression {
		t.Fatal("outcome axis should be stable (success→success)")
	}
	// CI gate: is_regression || len(new_task_blockers) > 0  → must trip here.
	if !(got.Regression.IsRegression || len(got.Regression.NewTaskBlockers) > 0) {
		t.Fatal("CI gate should fail on the new task-blocker")
	}
}

// evaluatedWalkthrough creates a walkthrough with one step, finishes it SUCCESS,
// materializes its synthetic run, stores one persona evaluation with the given
// findings JSON, and marks the eval job done — so RefreshWalkthroughDiff sees a
// completed evaluation for it.
func evaluatedWalkthrough(t *testing.T, app *App, targetID, findingsJSON string) *db.Walkthrough {
	t.Helper()
	wk, _ := app.DB.CreateWalkthrough(targetID, "", "sign up", true)
	_, _ = app.DB.InsertWalkthroughStep(&db.WalkthroughStep{WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://site.test/", Outcome: "ok"})
	_ = app.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	runID, err := app.DB.MaterializeWalkthroughRun(context.Background(), app.Store, auth.DefaultDevUser, wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	pgs, err := app.DB.ListPages(runID)
	if err != nil || len(pgs) == 0 {
		t.Fatalf("list pages: %v (%d)", err, len(pgs))
	}
	if err := app.DB.SavePageEvaluation(pgs[0].ID, runID, "skeptical-evaluator", findingsJSON, "blocked", "", 0, 0, 0); err != nil {
		t.Fatalf("save eval: %v", err)
	}
	if err := app.DB.FinishEvalJob(runID, db.EvalDone); err != nil {
		t.Fatalf("finish eval: %v", err)
	}
	// Reload so the caller sees the stamped run_id + baseline.
	got, _ := app.DB.GetWalkthroughByID(wk.ID)
	return got
}

func bytesContainsKey(b []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	reg, ok := m["regression"]
	if !ok {
		return false
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(reg, &inner); err != nil {
		return false
	}
	_, ok = inner[key]
	return ok
}
