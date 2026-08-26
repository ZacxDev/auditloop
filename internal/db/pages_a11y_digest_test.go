package db

import "testing"

// TestPageA11yDigestKeyColumn verifies migration 0060 applies on sqlite and that the
// new pages.a11y_digest_key column round-trips through InsertPage/ListPages/
// GetPageByID. A pre-0060/pushed row (unset) reads back as "" (backward-compat).
func TestPageA11yDigestKeyColumn(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)

	id, err := d.InsertPage(&Page{
		RunID: run.ID, URL: "https://t.test/", Viewport: "desktop",
		AxeKey: "t/run/home/axe.json", A11yDigestKey: "t/run/home/a11y.json",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A second row with no digest key (simulates a pushed / pre-0060 row).
	unsetID, _ := d.InsertPage(&Page{RunID: run.ID, URL: "https://t.test/x", Viewport: "mobile"})

	pages, err := d.ListPages(run.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]*Page{}
	for _, p := range pages {
		got[p.ID] = p
	}
	if got[id] == nil || got[id].A11yDigestKey != "t/run/home/a11y.json" {
		t.Errorf("a11y_digest_key did not round-trip via ListPages: %+v", got[id])
	}
	if got[unsetID] == nil || got[unsetID].A11yDigestKey != "" {
		t.Errorf("unset a11y_digest_key should read back \"\", got %q", got[unsetID].A11yDigestKey)
	}

	// GetPageByID also carries the column.
	p, err := d.GetPageByID(id)
	if err != nil {
		t.Fatalf("GetPageByID: %v", err)
	}
	if p.A11yDigestKey != "t/run/home/a11y.json" {
		t.Errorf("GetPageByID a11y_digest_key = %q", p.A11yDigestKey)
	}

	// The migration id is recorded (dual-dialect DDL ran).
	var n int
	if err := d.queryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, "0060_pages_a11y_digest_key").Scan(&n); err != nil || n != 1 {
		t.Errorf("migration 0060 not recorded (n=%d err=%v)", n, err)
	}
}
