package db

import "testing"

// TestRunFaviconKey covers migration 0059 (runs.favicon_key) end-to-end: a fresh run
// defaults to "" (additive/nullable), SetRunFavicon persists a key, and it survives
// the scanRun hot path (GetRun/LatestDoneRunForTarget).
func TestRunFaviconKey(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, err := d.CreateRun("u", tgt.ID)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.FaviconKey != "" {
		t.Fatalf("fresh run favicon_key = %q, want empty", run.FaviconKey)
	}

	// Persist a key and confirm it round-trips through scanRun.
	key := "t/" + run.ID + "/favicon.png"
	if err := d.SetRunFavicon(run.ID, key); err != nil {
		t.Fatalf("SetRunFavicon: %v", err)
	}
	got, err := d.GetRun("u", run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.FaviconKey != key {
		t.Fatalf("GetRun favicon_key = %q, want %q", got.FaviconKey, key)
	}

	// And through the dashboard's baseline query once the run is done.
	if err := d.FinishRun(run.ID, RunDone, "{}", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	latest, err := d.LatestDoneRunForTarget(tgt.ID, "")
	if err != nil || latest == nil {
		t.Fatalf("LatestDoneRunForTarget: %v (nil=%v)", err, latest == nil)
	}
	if latest.FaviconKey != key {
		t.Fatalf("LatestDoneRunForTarget favicon_key = %q, want %q", latest.FaviconKey, key)
	}
}
