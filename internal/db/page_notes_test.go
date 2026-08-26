package db

import (
	"sync"
	"testing"
)

// seedDoneRunWithPages creates a target, a done run, and one page, returning the
// run id and page id.
func seedRunWithPage(t *testing.T, d *DB) (runID, pageID string) {
	t.Helper()
	tgt, err := d.CreateTarget("u", "T", "https://t.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.CreateRun("u", tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := d.InsertPage(&Page{RunID: run.ID, URL: "https://t.test/", Viewport: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID, pid
}

func TestPageNoteUpsertAndEdit(t *testing.T) {
	d := testDB(t)
	runID, pageID := seedRunWithPage(t, d)

	// Fresh draft with cost + tokens.
	if err := d.SavePageNoteDraft(pageID, runID, "m1", "first draft", "", 0.0021, 1200, 340); err != nil {
		t.Fatalf("draft: %v", err)
	}
	got, err := d.GetPageNote(pageID, "m1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Notes != "first draft" || got.Edited {
		t.Errorf("draft mismatch: %+v", got)
	}
	if got.CostUSD != 0.0021 || got.PromptTokens != 1200 || got.CompletionTokens != 340 {
		t.Errorf("cost/tokens not persisted: %+v", got)
	}

	// A human edit sets edited=true.
	if err := d.SavePageNoteEdit(pageID, "m1", "my edits"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _ = d.GetPageNote(pageID, "m1")
	if got.Notes != "my edits" || !got.Edited {
		t.Errorf("edit not persisted: %+v", got)
	}

	// A re-draft REPLACES the row (one row per (page,model)) and clears edited.
	if err := d.SavePageNoteDraft(pageID, runID, "m1", "second draft", "", 0.0005, 100, 50); err != nil {
		t.Fatalf("redraft: %v", err)
	}
	got, _ = d.GetPageNote(pageID, "m1")
	if got.Notes != "second draft" || got.Edited {
		t.Errorf("redraft should replace + clear edited: %+v", got)
	}
	if got.CostUSD != 0.0005 || got.PromptTokens != 100 {
		t.Errorf("redraft should overwrite cost/tokens: %+v", got)
	}
	notes, _ := d.ListPageNotes(runID)
	if len(notes) != 1 {
		t.Errorf("expected exactly 1 row per (page,model), got %d", len(notes))
	}
}

func TestPageNoteError(t *testing.T) {
	d := testDB(t)
	runID, pageID := seedRunWithPage(t, d)
	if err := d.SavePageNoteDraft(pageID, runID, "m1", "", "openrouter status 500", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetPageNote(pageID, "m1")
	if got.Error == "" {
		t.Errorf("expected error stored, got %+v", got)
	}
}

func TestPageNoteEditMissing(t *testing.T) {
	d := testDB(t)
	_, pageID := seedRunWithPage(t, d)
	if err := d.SavePageNoteEdit(pageID, "nope", "x"); err != ErrNotFound {
		t.Errorf("editing a missing note should be ErrNotFound, got %v", err)
	}
}

// TestAddNotesCostAccumulatesAtomically fires many concurrent AddNotesCost calls
// (as the pass's per-(page,model) writes do) and asserts none are lost. Run under
// -race, the DB-side SET x = x + ? increment must land every update.
func TestAddNotesCostAccumulatesAtomically(t *testing.T) {
	d := testDB(t)
	runID, _ := seedRunWithPage(t, d)

	// A fresh pass resets the accumulators to 0.
	if _, err := d.ClaimNotesJob(runID, 100); err != nil {
		t.Fatal(err)
	}

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.AddNotesCost(runID, 0.0001, 10, 3); err != nil {
				t.Errorf("add: %v", err)
			}
		}()
	}
	wg.Wait()

	run, err := d.GetRunByID(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := run.NotesPromptTokens; got != n*10 {
		t.Errorf("prompt tokens = %d, want %d (lost updates)", got, n*10)
	}
	if got := run.NotesCompletionTokens; got != n*3 {
		t.Errorf("completion tokens = %d, want %d", got, n*3)
	}
	// Float sum ~ n*0.0001 = 0.01 (allow tiny FP slack).
	if got := run.NotesCostUSD; got < 0.0099 || got > 0.0101 {
		t.Errorf("cost = %v, want ~0.01", got)
	}

	// Re-claiming (a Regenerate) resets the accumulators.
	if _, err := d.MarkGeneratingNotesFailed(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimNotesJob(runID, 4); err != nil {
		t.Fatal(err)
	}
	run, _ = d.GetRunByID(runID)
	if run.NotesCostUSD != 0 || run.NotesPromptTokens != 0 || run.NotesCompletionTokens != 0 {
		t.Errorf("claim should reset cost accumulators, got %+v", run)
	}
}

func TestNotesJobLifecycleAndSweep(t *testing.T) {
	d := testDB(t)
	runID, _ := seedRunWithPage(t, d)

	// Claim.
	won, err := d.ClaimNotesJob(runID, 6)
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	// A second claim while generating loses.
	won2, _ := d.ClaimNotesJob(runID, 6)
	if won2 {
		t.Error("second claim should lose while generating")
	}

	run, _ := d.GetRunByID(runID)
	if run.NotesStatus != NotesGenerating || run.NotesTotal != 6 {
		t.Errorf("claim state wrong: %+v", run)
	}

	if err := d.UpdateNotesProgress(runID, 3); err != nil {
		t.Fatal(err)
	}
	run, _ = d.GetRunByID(runID)
	if run.NotesDone != 3 {
		t.Errorf("progress = %d, want 3", run.NotesDone)
	}

	// Startup sweep flips a still-generating job to failed.
	n, err := d.MarkGeneratingNotesFailed()
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d err=%v, want 1", n, err)
	}
	run, _ = d.GetRunByID(runID)
	if run.NotesStatus != NotesFailed {
		t.Errorf("swept status = %q, want failed", run.NotesStatus)
	}

	// After failing, a fresh claim wins again.
	won3, _ := d.ClaimNotesJob(runID, 2)
	if !won3 {
		t.Error("claim after failure should win")
	}
	if err := d.FinishNotesJob(runID, NotesDone); err != nil {
		t.Fatal(err)
	}
	run, _ = d.GetRunByID(runID)
	if run.NotesStatus != NotesDone {
		t.Errorf("finish status = %q", run.NotesStatus)
	}
}
