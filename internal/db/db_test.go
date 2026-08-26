package db

import (
	"path/filepath"
	"sync"
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	// A shared file (not :memory:) so all connections see the same schema; the
	// pool is capped at 1 conn for sqlite anyway.
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestTargetCRUD(t *testing.T) {
	d := testDB(t)
	tgt, err := d.CreateTarget("user-1", "Acme", "https://acme.test", []string{"acme.test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := d.GetTarget("user-1", tgt.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Acme" || got.AuthMode != AuthNone || len(got.VerifiedDomains) != 1 {
		t.Errorf("target mismatch: %+v", got)
	}
	// Scoping: another user can't read it.
	if _, err := d.GetTarget("user-2", tgt.ID); err != ErrNotFound {
		t.Errorf("cross-user get should be ErrNotFound, got %v", err)
	}
	list, err := d.ListTargets("user-1")
	if err != nil || len(list) != 1 {
		t.Errorf("list: %v n=%d", err, len(list))
	}
}

func TestGetTargetByName(t *testing.T) {
	d := testDB(t)
	tgt, err := d.CreateTarget("user-1", "lms-audit", "https://lms.test", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Owner resolves by name.
	got, err := d.GetTargetByName("user-1", "lms-audit")
	if err != nil {
		t.Fatalf("by-name: %v", err)
	}
	if got.ID != tgt.ID {
		t.Errorf("by-name id = %q, want %q", got.ID, tgt.ID)
	}

	// Cross-user isolation (critical): a DIFFERENT user owning a same-named
	// target must NOT see user-1's target, and vice versa.
	otherTgt, err := d.CreateTarget("user-2", "lms-audit", "https://lms2.test", nil)
	if err != nil {
		t.Fatalf("create user-2: %v", err)
	}
	got2, err := d.GetTargetByName("user-2", "lms-audit")
	if err != nil {
		t.Fatalf("user-2 by-name: %v", err)
	}
	if got2.ID != otherTgt.ID {
		t.Errorf("user-2 by-name resolved to %q, want its own %q", got2.ID, otherTgt.ID)
	}
	// A name that exists only under ANOTHER user → not found for this user.
	if _, err := d.GetTargetByName("user-3", "lms-audit"); err != ErrNotFound {
		t.Errorf("foreign-only name should be ErrNotFound, got %v", err)
	}

	// Collision (same user, duplicate names): returns the MOST RECENTLY created.
	dup, err := d.CreateTarget("user-1", "lms-audit", "https://lms-newer.test", nil)
	if err != nil {
		t.Fatalf("create dup: %v", err)
	}
	got, err = d.GetTargetByName("user-1", "lms-audit")
	if err != nil {
		t.Fatalf("collision by-name: %v", err)
	}
	if got.ID != dup.ID {
		t.Errorf("collision resolved to %q, want most-recent %q", got.ID, dup.ID)
	}

	// Unknown name → not found.
	if _, err := d.GetTargetByName("user-1", "does-not-exist"); err != ErrNotFound {
		t.Errorf("unknown name should be ErrNotFound, got %v", err)
	}
}

func TestRunLifecycle(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, err := d.CreateRun("u", tgt.ID)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != RunQueued {
		t.Errorf("status = %q", run.Status)
	}
	claimed, err := d.ClaimNextQueuedRun()
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", err, claimed)
	}
	if claimed.Status != RunRunning || claimed.StartedAt == nil {
		t.Errorf("claimed run not running: %+v", claimed)
	}
	if err := d.FinishRun(claimed.ID, RunDone, `{"pages_crawled":3}`, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	final, _ := d.GetRun("u", run.ID)
	if final.Status != RunDone || final.FinishedAt == nil {
		t.Errorf("final run: %+v", final)
	}
}

func TestClaimIsAtomic(t *testing.T) {
	// Exactly one of N concurrent claims of a single queued run wins.
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	if _, err := d.CreateRun("u", tgt.ID); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := d.ClaimNextQueuedRun()
			if err != nil {
				return
			}
			if r != nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("expected exactly 1 winning claim, got %d", wins)
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	d := testDB(t)
	r, err := d.ClaimNextQueuedRun()
	if err != nil {
		t.Fatalf("claim empty: %v", err)
	}
	if r != nil {
		t.Errorf("expected nil on empty queue, got %+v", r)
	}
}

func TestRecoverStaleRuns(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)
	// Simulate a crash mid-run: claim it (→running) but never finish.
	if _, err := d.ClaimNextQueuedRun(); err != nil {
		t.Fatal(err)
	}
	n, err := d.RecoverStaleRuns()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.Status != RunFailed || got.Error == "" {
		t.Errorf("stale run not failed: %+v", got)
	}
}

func TestPagesAndFindings(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)
	pid, err := d.InsertPage(&Page{
		RunID: run.ID, URL: "https://t.test/", Viewport: "mobile",
		ScreenshotKey: "t/r/home/mobile.png", AxeViolationCount: 2,
		ConsoleFirstPartyCount: 1, NetworkThirdPartyCount: 3,
	})
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	if _, err := d.InsertFinding(&Finding{PageID: pid, Type: FindingA11y, Severity: "serious", Detail: `{"id":"image-alt"}`}); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	pages, err := d.ListPages(run.ID)
	if err != nil || len(pages) != 1 {
		t.Fatalf("list pages: %v n=%d", err, len(pages))
	}
	if pages[0].AxeViolationCount != 2 || pages[0].NetworkThirdPartyCount != 3 {
		t.Errorf("page counts wrong: %+v", pages[0])
	}
	finds, err := d.ListFindings(pid)
	if err != nil || len(finds) != 1 || finds[0].Type != FindingA11y {
		t.Errorf("findings: %v %+v", err, finds)
	}
}

func TestBaselineLinking(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	// First run for the target: no baseline.
	r1, _ := d.CreateRun("u", tgt.ID)
	if r1.PrevRunID != "" {
		t.Errorf("first run should have no baseline, got prev=%q", r1.PrevRunID)
	}
	// No completed run yet.
	if prev, err := d.LatestDoneRunForTarget(tgt.ID, ""); err != nil || prev != nil {
		t.Errorf("expected no baseline before any run is done, got %v err=%v", prev, err)
	}

	// Complete r1. Now a second run links to it.
	if err := d.FinishRun(r1.ID, RunDone, "{}", ""); err != nil {
		t.Fatal(err)
	}
	r2, _ := d.CreateRun("u", tgt.ID)
	if r2.PrevRunID != r1.ID {
		t.Errorf("second run baseline = %q, want %q", r2.PrevRunID, r1.ID)
	}
	// A round-tripped read preserves prev_run_id.
	got2, _ := d.GetRun("u", r2.ID)
	if got2.PrevRunID != r1.ID {
		t.Errorf("persisted prev_run_id = %q, want %q", got2.PrevRunID, r1.ID)
	}

	// A still-running (not done) run is not a baseline; complete r2, then r3
	// links to r2 (the most recent DONE run), excluding itself.
	if err := d.FinishRun(r2.ID, RunDone, "{}", ""); err != nil {
		t.Fatal(err)
	}
	r3, _ := d.CreateRun("u", tgt.ID)
	if r3.PrevRunID != r2.ID {
		t.Errorf("third run baseline = %q, want %q (latest done)", r3.PrevRunID, r2.ID)
	}
	// Excluding r2 falls back to r1.
	prev, err := d.LatestDoneRunForTarget(tgt.ID, r2.ID)
	if err != nil || prev == nil || prev.ID != r1.ID {
		t.Errorf("latest-done excluding r2 = %v err=%v, want r1", prev, err)
	}

	// A failed run is not a valid baseline.
	other, _ := d.CreateTarget("u", "T2", "https://t2.test", nil)
	fr, _ := d.CreateRun("u", other.ID)
	_ = d.FinishRun(fr.ID, RunFailed, "{}", "boom")
	nr, _ := d.CreateRun("u", other.ID)
	if nr.PrevRunID != "" {
		t.Errorf("failed run must not be a baseline, got prev=%q", nr.PrevRunID)
	}
}

func TestPageAndRunDiffPersistence(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)
	pid, err := d.InsertPage(&Page{RunID: run.ID, URL: "https://t.test/", Viewport: "mobile"})
	if err != nil {
		t.Fatalf("insert page: %v", err)
	}
	// Defaults: no diff yet.
	pgs, _ := d.ListPages(run.ID)
	if pgs[0].DiffPct != 0 || pgs[0].DiffKey != "" {
		t.Errorf("fresh page diff defaults wrong: %+v", pgs[0])
	}
	// Update diff.
	if err := d.UpdatePageDiff(pid, 12.5, "t/r/home/mobile.diff.png"); err != nil {
		t.Fatalf("update page diff: %v", err)
	}
	pgs, _ = d.ListPages(run.ID)
	if pgs[0].DiffPct != 12.5 || pgs[0].DiffKey != "t/r/home/mobile.diff.png" {
		t.Errorf("page diff not persisted: %+v", pgs[0])
	}

	// Run diff JSON.
	if err := d.SetRunDiff(run.ID, `{"prev_run_id":"x","pages_changed":1}`); err != nil {
		t.Fatalf("set run diff: %v", err)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.DiffJSON != `{"prev_run_id":"x","pages_changed":1}` {
		t.Errorf("run diff_json = %q", got.DiffJSON)
	}
}

func TestRebindPostgres(t *testing.T) {
	d := &DB{driver: "postgres"}
	got := d.rebind(`SELECT * FROM t WHERE a=? AND b=? AND c='literal ? keep'`)
	want := `SELECT * FROM t WHERE a=$1 AND b=$2 AND c='literal ? keep'`
	if got != want {
		t.Errorf("rebind:\n got %q\nwant %q", got, want)
	}
	// SQLite leaves it untouched.
	ds := &DB{driver: "sqlite"}
	if ds.rebind(`a=?`) != `a=?` {
		t.Error("sqlite rebind should be a no-op")
	}
}
