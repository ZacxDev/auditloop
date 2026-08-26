package db

import (
	"path/filepath"
	"testing"
)

// A PLUGIN-PUSHED page's a11y digest key rides the EXISTING pages.a11y_digest_key
// column (migration 0060) — no parallel column, no new migration. This asserts the
// key round-trips on a pushed run and that a pushed run's pages stay owner-scoped:
// page ownership runs through run→target→user, so a foreign user cannot resolve the
// run and therefore can never reach the digest artifact key.
func TestPushedRunA11yDigestKeyRoundTripAndScoping(t *testing.T) {
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "pushdigest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	tgt, err := d.CreatePluginTarget("owner", "Funnel", "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.CreatePushedRun("owner", tgt.ID, "push #1", "")
	if err != nil {
		t.Fatal(err)
	}

	const digestKey = "funnel/run/step-1/a11y.json"
	withID, err := d.InsertPage(&Page{
		RunID: run.ID, URL: "step-1", Viewport: "desktop",
		ScreenshotKey: "funnel/run/step-1/desktop.png", A11yDigestKey: digestKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A page from the SAME push that carried no digest must read back "" (a producer
	// may send the digest for some views only).
	withoutID, err := d.InsertPage(&Page{
		RunID: run.ID, URL: "step-2", Viewport: "desktop",
		ScreenshotKey: "funnel/run/step-2/desktop.png",
	})
	if err != nil {
		t.Fatal(err)
	}

	pages, err := d.ListPages(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Page{}
	for _, p := range pages {
		byID[p.ID] = p
	}
	if byID[withID] == nil || byID[withID].A11yDigestKey != digestKey {
		t.Errorf("pushed a11y_digest_key did not round-trip via ListPages: %+v", byID[withID])
	}
	if byID[withoutID] == nil || byID[withoutID].A11yDigestKey != "" {
		t.Errorf("digest-less pushed page must read back \"\", got %+v", byID[withoutID])
	}

	// Owner-scoping: the owner resolves the run; another user does not (so the pushed
	// digest artifact is unreachable cross-user).
	if _, err := d.GetRun("owner", run.ID); err != nil {
		t.Fatalf("owner must resolve its own pushed run: %v", err)
	}
	if _, err := d.GetRun("someone-else", run.ID); err == nil {
		t.Fatal("a foreign user must NOT resolve a pushed run carrying an a11y digest")
	}
}
