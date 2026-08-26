package notes

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// fakeDrafter records calls and can be told to fail for one model. It returns a
// fixed per-call Usage (cost + tokens) so cost accumulation can be asserted.
type fakeDrafter struct {
	mu        sync.Mutex
	failModel string
	cost      float64 // per-successful-call cost; 0 → default 0.001
	calls     []fakeCall
}

type fakeCall struct {
	Model      string
	UserPrompt string
	NumImages  int
}

func (f *fakeDrafter) Draft(ctx context.Context, model, system, user string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Model: model, UserPrompt: user, NumImages: len(images)})
	f.mu.Unlock()
	if model == f.failModel {
		return "", llm.Usage{}, fmt.Errorf("simulated failure for %s", model)
	}
	cost := f.cost
	if cost == 0 {
		cost = 0.001
	}
	return "## UX notes for " + model + "\n- something actionable",
		llm.Usage{CostUSD: cost, PromptTokens: 1000, CompletionTokens: 200}, nil
}

func (f *fakeDrafter) callsFor(model string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Model == model {
			out = append(out, c)
		}
	}
	return out
}

func setup(t *testing.T) (*db.DB, *storage.FS) {
	t.Helper()
	d, err := db.Open("sqlite", filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d, st
}

// tinyPNG is a 1x1 transparent PNG (valid image bytes for the store).
var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

// seedTwoPageRun creates a done run with 2 URLs × 2 viewports (4 page rows) with
// screenshots in the store, and one axe finding on the home page.
func seedTwoPageRun(t *testing.T, d *db.DB, st *storage.FS) string {
	t.Helper()
	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	urls := []string{"https://acme.test/", "https://acme.test/about"}
	for _, u := range urls {
		for i, vp := range []string{"desktop", "mobile"} {
			key := storage.ScreenshotKey("acme", run.ID, storage.PageSlug(u), vp)
			if err := st.Put(context.Background(), key, "image/png", bytes.NewReader(tinyPNG), int64(len(tinyPNG))); err != nil {
				t.Fatal(err)
			}
			pid, _ := d.InsertPage(&db.Page{
				RunID: run.ID, URL: u, Viewport: vp, ScreenshotKey: key,
				AxeViolationCount: 1, ConsoleFirstPartyCount: i,
			})
			// One a11y finding on the home page so grounding has a rule.
			if u == urls[0] {
				_, _ = d.InsertFinding(&db.Finding{PageID: pid, Type: db.FindingA11y, Severity: "serious", Detail: `{"id":"image-alt"}`})
			}
		}
	}
	_ = d.FinishRun(run.ID, db.RunDone, "{}", "")
	return run.ID
}

func TestMultiModelPass(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &fakeDrafter{}
	g := New(d, st, fake)

	models := []string{"m1", "m2"}
	total, err := g.CountUnits(runID, models)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("CountUnits = %d, want 4 (2 pages × 2 models)", total)
	}

	if err := g.Run(context.Background(), runID, models); err != nil {
		t.Fatalf("run: %v", err)
	}

	notes, _ := d.ListPageNotes(runID)
	if len(notes) != 4 {
		t.Fatalf("expected 4 page_notes rows (2 pages × 2 models), got %d", len(notes))
	}
	// Each note is a non-empty draft with no error, carrying its per-call cost.
	for _, n := range notes {
		if n.Error != "" || n.Notes == "" {
			t.Errorf("note %+v should be a clean draft", n)
		}
		if n.CostUSD != 0.001 || n.PromptTokens != 1000 || n.CompletionTokens != 200 {
			t.Errorf("note cost/tokens not persisted per-cell: %+v", n)
		}
	}

	// Run totals = sum of the 4 calls (2 pages × 2 models × 0.001).
	run0, _ := d.GetRunByID(runID)
	if run0.NotesCostUSD < 0.0039 || run0.NotesCostUSD > 0.0041 {
		t.Errorf("run notes_cost_usd = %v, want ~0.004", run0.NotesCostUSD)
	}
	if run0.NotesPromptTokens != 4000 || run0.NotesCompletionTokens != 800 {
		t.Errorf("run token totals = %d/%d, want 4000/800", run0.NotesPromptTokens, run0.NotesCompletionTokens)
	}

	// Each model saw both pages, each with 2 images (desktop + mobile).
	for _, m := range models {
		calls := fake.callsFor(m)
		if len(calls) != 2 {
			t.Errorf("model %s made %d calls, want 2", m, len(calls))
		}
		for _, c := range calls {
			if c.NumImages != 2 {
				t.Errorf("model %s call had %d images, want 2 (desktop+mobile)", m, c.NumImages)
			}
		}
	}
	// The prompt is scoped to SUBJECTIVE VISUAL critique: the deterministic signals
	// (axe rules, console/network counts) are shown separately by the crawler and
	// must NOT be narrated in the prompt. Assert they don't leak in.
	for _, c := range fake.callsFor("m1") {
		if bytes.Contains([]byte(c.UserPrompt), []byte("image-alt")) {
			t.Error("prompt should NOT enumerate axe rules (deterministic signal captured separately)")
		}
		if bytes.Contains([]byte(c.UserPrompt), []byte("console error")) {
			t.Error("prompt should NOT narrate console error counts")
		}
		if !bytes.Contains([]byte(c.UserPrompt), []byte("Page URL:")) {
			t.Errorf("prompt missing the page URL grounding: %q", c.UserPrompt)
		}
	}

	// Job finalized done, progress complete.
	run, _ := d.GetRunByID(runID)
	if run.NotesStatus != db.NotesDone {
		t.Errorf("notes status = %q, want done", run.NotesStatus)
	}
	if run.NotesDone != 4 {
		t.Errorf("notes done = %d, want 4", run.NotesDone)
	}
}

func TestPassDegradesOnPerModelFailure(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &fakeDrafter{failModel: "m2"} // m2 always errors
	g := New(d, st, fake)

	if err := g.Run(context.Background(), runID, []string{"m1", "m2"}); err != nil {
		t.Fatalf("run should not fail wholesale: %v", err)
	}

	notes, _ := d.ListPageNotes(runID)
	if len(notes) != 4 {
		t.Fatalf("still expect 4 rows, got %d", len(notes))
	}
	var m1ok, m2err int
	for _, n := range notes {
		switch n.Model {
		case "m1":
			if n.Error == "" && n.Notes != "" {
				m1ok++
			}
		case "m2":
			if n.Error != "" {
				m2err++
			}
		}
	}
	if m1ok != 2 {
		t.Errorf("m1 clean drafts = %d, want 2", m1ok)
	}
	if m2err != 2 {
		t.Errorf("m2 errored rows = %d, want 2", m2err)
	}
	// Failed cells carry cost 0; the run total counts only m1's 2 successes.
	for _, n := range notes {
		if n.Model == "m2" && n.CostUSD != 0 {
			t.Errorf("failed m2 cell should have cost 0, got %v", n.CostUSD)
		}
	}
	if run0, _ := d.GetRunByID(runID); run0.NotesCostUSD < 0.0019 || run0.NotesCostUSD > 0.0021 {
		t.Errorf("run cost should count only m1 successes (~0.002), got %v", run0.NotesCostUSD)
	}
	// The pass still completes.
	run, _ := d.GetRunByID(runID)
	if run.NotesStatus != db.NotesDone {
		t.Errorf("status = %q, want done (degraded, not failed)", run.NotesStatus)
	}
}
