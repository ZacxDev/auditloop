package db

import "testing"

// TestPagePerfColumns verifies migration 0030-0034 apply on sqlite (Open runs all
// migrations) and that the new perf/web-vitals columns round-trip through
// InsertPage/ListPages. A pre-perf row (zero values) reads back as zeros.
func TestPagePerfColumns(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)

	id, err := d.InsertPage(&Page{
		RunID: run.ID, URL: "https://t.test/", Viewport: "mobile",
		LCPMs: 3200, CLS: 0.18, TBTMs: 450, WeightBytes: 4_500_000, ReqCount: 87,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A second row with zero perf (simulates a pre-migration/pushed row).
	_, _ = d.InsertPage(&Page{RunID: run.ID, URL: "https://t.test/x", Viewport: "desktop"})

	pages, err := d.ListPages(run.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *Page
	for _, p := range pages {
		if p.ID == id {
			got = p
		}
	}
	if got == nil {
		t.Fatal("inserted page not found")
	}
	if got.LCPMs != 3200 || got.TBTMs != 450 || got.WeightBytes != 4_500_000 || got.ReqCount != 87 {
		t.Errorf("perf ints did not round-trip: %+v", got)
	}
	if got.CLS < 0.17 || got.CLS > 0.19 {
		t.Errorf("CLS did not round-trip: %v", got.CLS)
	}

	// The migration ids are recorded (applied), proving dual-dialect DDL ran.
	for _, mid := range []string{"0030_pages_lcp_ms", "0031_pages_cls", "0032_pages_tbt_ms", "0033_pages_weight_bytes", "0034_pages_req_count"} {
		var n int
		if err := d.queryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, mid).Scan(&n); err != nil || n != 1 {
			t.Errorf("migration %s not recorded (n=%d err=%v)", mid, n, err)
		}
	}
}
