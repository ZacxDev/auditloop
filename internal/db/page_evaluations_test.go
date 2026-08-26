package db

import (
	"path/filepath"
	"sync"
	"testing"
)

func openEvalTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedEvalRun(t *testing.T, d *DB, user string) (runID, pageID string) {
	t.Helper()
	tgt, err := d.CreateTarget(user, "Acme", "https://acme.test", []string{"acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.CreateRun(user, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := d.InsertPage(&Page{RunID: run.ID, URL: "https://acme.test/", Viewport: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.FinishRun(run.ID, RunDone, "{}", "")
	return run.ID, pid
}

func TestPageEvaluationUpsertAndUnique(t *testing.T) {
	d := openEvalTestDB(t)
	runID, pageID := seedEvalRun(t, d, "u")

	if err := d.SavePageEvaluation(pageID, runID, "skeptical-evaluator", `{"comprehension":"unclear"}`, "unclear", "", 0.001, 100, 20); err != nil {
		t.Fatal(err)
	}
	// Re-save (same page,persona) REPLACES the row (upsert).
	if err := d.SavePageEvaluation(pageID, runID, "skeptical-evaluator", `{"comprehension":"clear"}`, "clear", "", 0.002, 200, 40); err != nil {
		t.Fatal(err)
	}
	rows, _ := d.ListPageEvaluations(runID)
	if len(rows) != 1 {
		t.Fatalf("upsert should keep ONE row per (page,persona), got %d", len(rows))
	}
	if rows[0].Comprehension != "clear" || rows[0].CostUSD != 0.002 {
		t.Errorf("upsert did not replace the row: %+v", rows[0])
	}
	// A different persona is a distinct row.
	if err := d.SavePageEvaluation(pageID, runID, "returning-power-user", `{"comprehension":"blocked"}`, "blocked", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	rows, _ = d.ListPageEvaluations(runID)
	if len(rows) != 2 {
		t.Fatalf("a second persona should add a row, got %d", len(rows))
	}

	got, err := d.GetPageEvaluation(pageID, "skeptical-evaluator")
	if err != nil || got.Comprehension != "clear" {
		t.Errorf("GetPageEvaluation: %+v (%v)", got, err)
	}
}

func TestListPageEvaluationsRunScoped(t *testing.T) {
	d := openEvalTestDB(t)
	run1, page1 := seedEvalRun(t, d, "u")
	run2, page2 := seedEvalRun(t, d, "u")
	_ = d.SavePageEvaluation(page1, run1, "skeptical-evaluator", `{"comprehension":"clear"}`, "clear", "", 0, 0, 0)
	_ = d.SavePageEvaluation(page2, run2, "skeptical-evaluator", `{"comprehension":"blocked"}`, "blocked", "", 0, 0, 0)

	r1, _ := d.ListPageEvaluations(run1)
	if len(r1) != 1 || r1[0].RunID != run1 {
		t.Errorf("run1 should only see its own evaluations: %+v", r1)
	}
	r2, _ := d.ListPageEvaluations(run2)
	if len(r2) != 1 || r2[0].RunID != run2 {
		t.Errorf("run2 should only see its own evaluations: %+v", r2)
	}
}

func TestClaimEvalJobAtomicityAndReset(t *testing.T) {
	d := openEvalTestDB(t)
	runID, _ := seedEvalRun(t, d, "u")

	// Pre-load some cost so we can prove the claim resets it.
	_ = d.AddEvalCost(runID, 0.05, 5000, 1000)

	won, err := d.ClaimEvalJob(runID, "sign up", 5)
	if err != nil || !won {
		t.Fatalf("first claim should win: won=%v err=%v", won, err)
	}
	run, _ := d.GetRunByID(runID)
	if run.EvalStatus != EvalGenerating || run.EvalTotal != 5 || run.EvalJob != "sign up" {
		t.Errorf("claim did not set job state: %+v", run)
	}
	if run.EvalCostUSD != 0 || run.EvalPromptTokens != 0 || run.EvalCompletionTokens != 0 {
		t.Errorf("claim should RESET cost accumulators, got %v/%d/%d", run.EvalCostUSD, run.EvalPromptTokens, run.EvalCompletionTokens)
	}

	// A second claim while generating loses (guards double-run).
	won2, _ := d.ClaimEvalJob(runID, "sign up", 5)
	if won2 {
		t.Error("second claim while generating should NOT win")
	}

	// Progress + finish.
	if err := d.UpdateEvalProgress(runID, 3); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishEvalJob(runID, EvalDone); err != nil {
		t.Fatal(err)
	}
	run, _ = d.GetRunByID(runID)
	if run.EvalDone != 3 || run.EvalStatus != EvalDone {
		t.Errorf("progress/finish not applied: %+v", run)
	}

	// After done, a fresh claim wins again.
	won3, _ := d.ClaimEvalJob(runID, "new job", 8)
	if !won3 {
		t.Error("a claim on a done job should win (re-run)")
	}
}

func TestAddEvalCostConcurrent(t *testing.T) {
	d := openEvalTestDB(t)
	runID, _ := seedEvalRun(t, d, "u")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.AddEvalCost(runID, 0.001, 100, 10)
		}()
	}
	wg.Wait()
	run, _ := d.GetRunByID(runID)
	if run.EvalCostUSD < 0.0199 || run.EvalCostUSD > 0.0201 {
		t.Errorf("concurrent AddEvalCost lost updates: cost=%v want ~0.02", run.EvalCostUSD)
	}
	if run.EvalPromptTokens != 2000 || run.EvalCompletionTokens != 200 {
		t.Errorf("concurrent token accumulation wrong: %d/%d", run.EvalPromptTokens, run.EvalCompletionTokens)
	}
}

func TestMarkGeneratingEvalFailed(t *testing.T) {
	d := openEvalTestDB(t)
	runID, _ := seedEvalRun(t, d, "u")
	_, _ = d.ClaimEvalJob(runID, "", 3) // → generating

	n, err := d.MarkGeneratingEvalFailed()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 orphaned eval job swept, got %d", n)
	}
	run, _ := d.GetRunByID(runID)
	if run.EvalStatus != EvalFailed {
		t.Errorf("swept job status = %q, want failed", run.EvalStatus)
	}
}

func TestSetRunEvalSynthesis(t *testing.T) {
	d := openEvalTestDB(t)
	runID, _ := seedEvalRun(t, d, "u")
	if err := d.SetRunEvalSynthesis(runID, `[{"title":"Fix the CTA"}]`); err != nil {
		t.Fatal(err)
	}
	run, _ := d.GetRunByID(runID)
	if run.EvalSynthesisJSON != `[{"title":"Fix the CTA"}]` {
		t.Errorf("synthesis not persisted: %q", run.EvalSynthesisJSON)
	}
}
