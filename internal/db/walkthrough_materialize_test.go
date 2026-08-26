package db

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/storage"
)

// putSpyStore is a minimal storage.Store that records every Put key (for the
// no-orphan / ordering assertions) and delegates reads to an inner FS store so
// the success-path artifact-content check still works.
type putSpyStore struct {
	inner   storage.Store
	mu      sync.Mutex
	putKeys []string
}

func newPutSpyStore(t *testing.T) *putSpyStore {
	t.Helper()
	fs, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("fs store: %v", err)
	}
	return &putSpyStore{inner: fs}
}

func (s *putSpyStore) Put(ctx context.Context, key, ct string, r io.Reader, size int64) error {
	s.mu.Lock()
	s.putKeys = append(s.putKeys, key)
	s.mu.Unlock()
	return s.inner.Put(ctx, key, ct, r, size)
}
func (s *putSpyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.inner.Get(ctx, key)
}
func (s *putSpyStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.inner.PresignGet(ctx, key, ttl)
}
func (s *putSpyStore) List(ctx context.Context, prefix string) ([]string, error) {
	return s.inner.List(ctx, prefix)
}
func (s *putSpyStore) Backend() string { return s.inner.Backend() }
func (s *putSpyStore) puts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.putKeys))
	copy(out, s.putKeys)
	return out
}

// seedDoneWalkthrough creates a target + a done walkthrough with the given ordered
// (actionType, url, screenshotKey) steps, and returns the target + walkthrough.
func seedDoneWalkthrough(t *testing.T, d *DB, userID string, steps [][3]string) (*Target, *Walkthrough) {
	t.Helper()
	tgt, err := d.CreateTarget(userID, "T", "https://t.test", nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("walkthrough: %v", err)
	}
	for i, s := range steps {
		if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
			WalkthroughID: wk.ID,
			Idx:           i,
			ActionJSON:    `{"type":"` + s[0] + `"}`,
			URL:           s[1],
			ScreenshotKey: s[2],
			Outcome:       "ok",
		}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return tgt, wk
}

func TestMaterializeWalkthroughRun(t *testing.T) {
	d := testDB(t)
	tgt, wk := seedDoneWalkthrough(t, d, "user-a", [][3]string{
		{"navigate", "https://t.test/form", "u/w/step-0.png"},
		{"type", "https://t.test/form", "u/w/step-1.png"}, // SAME url as step 0
		{"click", "https://t.test/welcome", ""},           // no screenshot
	})

	runID, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Synthetic run: done + trigger=walkthrough.
	run, err := d.GetRunByID(runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != RunDone || run.Trigger != "walkthrough" {
		t.Fatalf("synthetic run: status=%q trigger=%q", run.Status, run.Trigger)
	}
	if run.TargetID != tgt.ID {
		t.Fatalf("run target = %q want %q", run.TargetID, tgt.ID)
	}
	if run.PrevRunID != "" {
		t.Fatalf("synthetic run must NOT be baseline-linked, got prev=%q", run.PrevRunID)
	}

	// One page per step, IN ORDER, distinct even when the URL repeats.
	pgs, err := d.ListPages(runID)
	if err != nil {
		t.Fatalf("list pages: %v", err)
	}
	if len(pgs) != 3 {
		t.Fatalf("expected 3 pages (one per step), got %d", len(pgs))
	}
	// ListPages orders by url ASC; the zero-padded index prefix keeps step order.
	wantPrefix := []string{"01 · navigate", "02 · type", "03 · click"}
	for i, p := range pgs {
		if !strings.HasPrefix(p.URL, wantPrefix[i]) {
			t.Fatalf("page %d label = %q, want prefix %q", i, p.URL, wantPrefix[i])
		}
		if p.Viewport != "desktop" {
			t.Fatalf("page %d viewport = %q, want desktop", i, p.Viewport)
		}
	}
	// The two same-URL steps produced DISTINCT page labels.
	if pgs[0].URL == pgs[1].URL {
		t.Fatalf("same-URL steps collapsed into one label: %q", pgs[0].URL)
	}
	// Step screenshot_keys are REUSED; a step with none → empty.
	if pgs[0].ScreenshotKey != "u/w/step-0.png" || pgs[1].ScreenshotKey != "u/w/step-1.png" {
		t.Fatalf("screenshot keys not reused: %q %q", pgs[0].ScreenshotKey, pgs[1].ScreenshotKey)
	}
	if pgs[2].ScreenshotKey != "" {
		t.Fatalf("shotless step should have empty screenshot key, got %q", pgs[2].ScreenshotKey)
	}

	// walkthroughs.run_id is stamped.
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.RunID != runID {
		t.Fatalf("walkthrough run_id = %q, want %q", got.RunID, runID)
	}
}

func TestMaterializeWalkthroughRunIdempotent(t *testing.T) {
	d := testDB(t)
	_, wk := seedDoneWalkthrough(t, d, "user-a", [][3]string{
		{"navigate", "https://t.test/a", "k0"},
		{"click", "https://t.test/b", "k1"},
	})

	first, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("re-materialize returned a new run: %q vs %q", first, second)
	}
	// No duplicate pages.
	pgs, _ := d.ListPages(first)
	if len(pgs) != 2 {
		t.Fatalf("idempotent materialize duplicated pages: got %d, want 2", len(pgs))
	}
}

func TestMaterializeWalkthroughRunOwnerScoped(t *testing.T) {
	d := testDB(t)
	_, wk := seedDoneWalkthrough(t, d, "user-a", [][3]string{{"navigate", "https://t.test/a", "k0"}})

	// A foreign user cannot materialize (owner-scoped GetWalkthrough → ErrNotFound).
	if _, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-b", wk.ID); err != ErrNotFound {
		t.Fatalf("foreign materialize = %v, want ErrNotFound", err)
	}
	// And nothing was stamped / created.
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.RunID != "" {
		t.Fatalf("foreign materialize leaked a run_id: %q", got.RunID)
	}
}

// TestMaterializeWalkthroughRunConcurrent simulates a double-submit (two concurrent
// evaluate POSTs): both call MaterializeWalkthroughRun on the same walkthrough. The
// CAS claim must yield a SINGLE synthetic run id (both callers agree) and NO duplicate
// pages — so the downstream eval pass runs at most once.
func TestMaterializeWalkthroughRunConcurrent(t *testing.T) {
	d := testDB(t)
	_, wk := seedDoneWalkthrough(t, d, "user-a", [][3]string{
		{"navigate", "https://t.test/a", "k0"},
		{"type", "https://t.test/a", "k1"},
		{"click", "https://t.test/b", "k2"},
	})

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together
			ids[i], errs[i] = d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
		}(i)
	}
	close(start)
	wg.Wait()

	// Every caller succeeded and returned the SAME run id.
	first := ids[0]
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != first {
			t.Fatalf("caller %d got run %q, want the single shared run %q", i, ids[i], first)
		}
	}
	if first == "" {
		t.Fatal("materialize returned an empty run id")
	}

	// Exactly one synthetic run exists for the target (no leaked orphan run).
	runs, err := d.query(`SELECT id FROM runs WHERE target_id=? AND trigger='walkthrough'`, wk.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	var runIDs []string
	for runs.Next() {
		var id string
		if err := runs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, id)
	}
	if len(runIDs) != 1 || runIDs[0] != first {
		t.Fatalf("expected exactly one synthetic run %q, got %v", first, runIDs)
	}

	// No duplicate pages (3 steps → 3 pages, once).
	pgs, _ := d.ListPages(first)
	if len(pgs) != 3 {
		t.Fatalf("concurrent materialize duplicated pages: got %d, want 3", len(pgs))
	}
}

// TestListRunsExcludesWalkthrough proves the synthetic walkthrough run is hidden from
// the target's crawl/push run list (UI + read-API) while normal runs remain.
func TestListRunsExcludesWalkthrough(t *testing.T) {
	d := testDB(t)
	tgt, err := d.CreateTarget("user-a", "T", "https://t.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A normal crawl run.
	normal, err := d.CreateRun("user-a", tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A done walkthrough on the same target → synthetic trigger='walkthrough' run.
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
		WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://t.test/a", Outcome: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatal(err)
	}
	synthID, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := d.ListRuns("user-a", tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns should return only the normal run, got %d", len(runs))
	}
	if runs[0].ID != normal.ID {
		t.Fatalf("ListRuns returned %q, want the normal run %q", runs[0].ID, normal.ID)
	}
	for _, r := range runs {
		if r.ID == synthID || r.Trigger == "walkthrough" {
			t.Fatalf("synthetic walkthrough run leaked into ListRuns: %q", r.ID)
		}
	}
	// It is still reachable directly (the eval vessel isn't deleted).
	if _, err := d.GetRun("user-a", synthID); err != nil {
		t.Fatalf("synthetic run should still be fetchable directly: %v", err)
	}
}

// TestMaterializeWalkthroughRunNotABaseline proves a synthetic walkthrough run never
// becomes the P2 baseline for a subsequent crawl/push run of the same target.
func TestMaterializeWalkthroughRunNotABaseline(t *testing.T) {
	d := testDB(t)
	_, wk := seedDoneWalkthrough(t, d, "user-a", [][3]string{{"navigate", "https://t.test/a", "k0"}})
	tgtID := wk.TargetID

	synthID, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The baseline query must skip the synthetic run.
	if base, _ := d.LatestDoneRunForTarget(tgtID, ""); base != nil {
		t.Fatalf("synthetic run became a baseline: %q", base.ID)
	}
	if base, _ := d.LatestDoneRunForTargetOwned("user-a", tgtID); base != nil {
		t.Fatalf("synthetic run became an owned baseline: %q", base.ID)
	}
	_ = synthID
}

// TestMaterializeWalkthroughRunA11yDigest (Phase 2) proves the driven-path digest is
// carried through materialization: a step WITH a captured DOM/a11y digest yields a
// synthetic page whose a11y_digest_key points at a stored a11y.json artifact holding
// that exact digest (so the existing eval read path + dropContradicted work unchanged
// on the driven path); a step WITHOUT a digest yields a page with NO key (degrade —
// the load-bearing backward-compat path).
func TestMaterializeWalkthroughRunA11yDigest(t *testing.T) {
	d := testDB(t)
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	tgt, err := d.CreateTarget("user-a", "Digest T", "https://t.test", nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("walkthrough: %v", err)
	}
	const digest = `{"interactive":[{"tag":"a","selector":"a#card","label_source":"text-content","focusable":true}],` +
		`"form_controls":[{"selector":"input#signup-email","accessible_name":"Email","has_label":true,"label_source":"for"}]}`
	// Step 0 carries a digest; step 1 does not.
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
		WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"navigate"}`, URL: "https://t.test/form",
		ScreenshotKey: "k0", Outcome: "ok", DigestJSON: digest,
	}); err != nil {
		t.Fatalf("step 0: %v", err)
	}
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
		WalkthroughID: wk.ID, Idx: 1, ActionJSON: `{"type":"click"}`, URL: "https://t.test/done",
		ScreenshotKey: "k1", Outcome: "ok", // DigestJSON empty → no key
	}); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// digest_json round-trips through the step store.
	steps, _ := d.ListWalkthroughSteps(wk.ID)
	if len(steps) != 2 || steps[0].DigestJSON != digest || steps[1].DigestJSON != "" {
		t.Fatalf("digest_json did not round-trip: %+v", steps)
	}

	runID, err := d.MaterializeWalkthroughRun(context.Background(), store, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	pgs, _ := d.ListPages(runID)
	if len(pgs) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(pgs))
	}
	// Page order follows the zero-padded label; step 0 = "01 · navigate …".
	if pgs[0].A11yDigestKey == "" {
		t.Fatal("step-with-digest page got no a11y_digest_key")
	}
	if pgs[1].A11yDigestKey != "" {
		t.Fatalf("step-without-digest page must have NO a11y_digest_key, got %q", pgs[1].A11yDigestKey)
	}
	// The stored artifact is the exact digest the step captured.
	rc, err := store.Get(context.Background(), pgs[0].A11yDigestKey)
	if err != nil {
		t.Fatalf("get artifact: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != digest {
		t.Fatalf("stored a11y digest = %q, want %q", string(got), digest)
	}
}

// TestMaterializeWalkthroughRunNoOrphanOnDBFailure proves the storage-leak fix: the
// a11y.json digest blobs are Put ONLY after the tx commits, so a DB failure mid-
// materialize (rolling back the run/pages) leaves ZERO orphaned artifacts in the store.
//
// Non-vacuous: the failure is forced BEFORE any commit (the pages INSERT fails because
// the table is dropped), and step 0 carries a digest. Under the OLD ordering (Put inside
// the loop, before each page INSERT) step 0's digest would already have been Put to the
// store before the insert failed → the spy would record 1 key and this test would fail.
// Under the fixed ordering (buffer in-loop, Put after commit) the function returns on the
// insert error before reaching the post-commit Put loop → zero Puts.
func TestMaterializeWalkthroughRunNoOrphanOnDBFailure(t *testing.T) {
	d := testDB(t)
	spy := newPutSpyStore(t)
	tgt, err := d.CreateTarget("user-a", "T", "https://t.test", nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("walkthrough: %v", err)
	}
	// Step 0 carries a digest (would be Put first under the old ordering); step 1 too.
	for i, url := range []string{"https://t.test/a", "https://t.test/b"} {
		if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
			WalkthroughID: wk.ID, Idx: i, ActionJSON: `{"type":"navigate"}`, URL: url,
			ScreenshotKey: "k", Outcome: "ok", DigestJSON: `{"interactive":[{"selector":"a#x"}]}`,
		}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// Force a deterministic mid-materialize DB failure: drop the pages table so the
	// per-step `INSERT INTO pages …` fails after the run row is inserted (within the tx),
	// triggering the deferred rollback. (The runs/walkthroughs/steps tables are untouched,
	// so everything up to the pages INSERT proceeds normally.)
	if _, err := d.exec(`DROP TABLE pages`); err != nil {
		t.Fatalf("drop pages: %v", err)
	}

	if _, err := d.MaterializeWalkthroughRun(context.Background(), spy, "user-a", wk.ID); err == nil {
		t.Fatal("expected materialize to fail after dropping pages, got nil error")
	}

	// The tx rolled back → no synthetic run persisted and run_id never stamped.
	if got, _ := d.GetWalkthroughByID(wk.ID); got.RunID != "" {
		t.Fatalf("failed materialize stamped a run_id: %q", got.RunID)
	}
	// THE GUARD: nothing was Put → zero orphaned a11y.json blobs on the error path.
	if puts := spy.puts(); len(puts) != 0 {
		t.Fatalf("DB failure orphaned %d artifact(s) in the store: %v", len(puts), puts)
	}
}

// TestMaterializeWalkthroughRunPutsAfterCommit proves the success-path invariant: on a
// clean materialize the digest artifacts ARE Put (exactly one per digest-carrying step)
// and each stored blob is reachable by the persisted pages.a11y_digest_key. Combined with
// the no-orphan test above, this pins the "Put only after a successful commit" ordering.
func TestMaterializeWalkthroughRunPutsAfterCommit(t *testing.T) {
	d := testDB(t)
	spy := newPutSpyStore(t)
	tgt, err := d.CreateTarget("user-a", "T", "https://t.test", nil)
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("walkthrough: %v", err)
	}
	const digest = `{"interactive":[{"selector":"a#x"}]}`
	// Two digest-carrying steps + one digest-less step (which must NOT produce a Put).
	steps := []struct {
		url, dg string
	}{
		{"https://t.test/a", digest},
		{"https://t.test/b", digest},
		{"https://t.test/c", ""},
	}
	for i, s := range steps {
		if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
			WalkthroughID: wk.ID, Idx: i, ActionJSON: `{"type":"navigate"}`, URL: s.url,
			ScreenshotKey: "k", Outcome: "ok", DigestJSON: s.dg,
		}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish: %v", err)
	}

	runID, err := d.MaterializeWalkthroughRun(context.Background(), spy, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Exactly one Put per digest-carrying step.
	if puts := spy.puts(); len(puts) != 2 {
		t.Fatalf("expected 2 digest Puts, got %d: %v", len(puts), puts)
	}
	// Each digest page's a11y_digest_key resolves to the exact stored digest.
	pgs, _ := d.ListPages(runID)
	if len(pgs) != 3 {
		t.Fatalf("expected 3 pages, got %d", len(pgs))
	}
	for i := 0; i < 2; i++ {
		if pgs[i].A11yDigestKey == "" {
			t.Fatalf("digest page %d got no a11y_digest_key", i)
		}
		rc, err := spy.Get(context.Background(), pgs[i].A11yDigestKey)
		if err != nil {
			t.Fatalf("get artifact for page %d: %v", i, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != digest {
			t.Fatalf("page %d stored digest = %q, want %q", i, string(got), digest)
		}
	}
	if pgs[2].A11yDigestKey != "" {
		t.Fatalf("digest-less step page must have no a11y_digest_key, got %q", pgs[2].A11yDigestKey)
	}
}

// TestMaterializeWalkthroughRunNilStoreDegrades proves materialization still succeeds
// (no key set) when no store is available — a digest-carrying step must never fail the
// synthetic run over a missing artifact backend.
func TestMaterializeWalkthroughRunNilStoreDegrades(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("user-a", "T", "https://t.test", nil)
	wk, _ := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{
		WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"navigate"}`, URL: "https://t.test/", DigestJSON: `{"interactive":[]}`,
	}); err != nil {
		t.Fatalf("step: %v", err)
	}
	_ = d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 0, "", false)
	runID, err := d.MaterializeWalkthroughRun(context.Background(), nil, "user-a", wk.ID)
	if err != nil {
		t.Fatalf("materialize with nil store: %v", err)
	}
	pgs, _ := d.ListPages(runID)
	if len(pgs) != 1 || pgs[0].A11yDigestKey != "" {
		t.Fatalf("nil-store materialize should set no a11y_digest_key: %+v", pgs)
	}
}
