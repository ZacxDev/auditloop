package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// solidShot builds a valid opaque PNG so the diff phase can decode it.
func solidShot(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode shot: %v", err)
	}
	return buf.Bytes()
}

// solidShotWH builds a valid opaque PNG of the given dimensions.
func solidShotWH(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode shot: %v", err)
	}
	return buf.Bytes()
}

// crawlFor returns a CrawlFunc yielding the given pages at both default
// viewports (mobile+desktop), with a valid screenshot and an axe payload
// carrying the supplied rule ids.
func crawlFor(t *testing.T, pages []fakePage) CrawlFunc {
	t.Helper()
	return func(_ context.Context, opts crawler.Options) (*crawler.Result, error) {
		var out []crawler.PageResult
		for _, fp := range pages {
			url := opts.BaseURL + fp.path
			axe := axeJSON(fp.rules)
			for _, vp := range crawler.DefaultViewports {
				out = append(out, crawler.PageResult{
					URL: url, Viewport: vp,
					ScreenshotPNG: fp.shot,
					AxeJSON:       axe, AxeViolations: len(fp.rules), AxeNodes: len(fp.rules),
					ConsoleErrors: fp.console,
				})
			}
		}
		return &crawler.Result{Pages: out, URLsDiscovered: len(pages)}, nil
	}
}

type fakePage struct {
	path    string
	rules   []string
	shot    []byte
	console []crawler.ConsoleError
}

func axeJSON(rules []string) []byte {
	var vs []string
	for _, r := range rules {
		vs = append(vs, `{"id":"`+r+`","impact":"serious","nodeCount":1}`)
	}
	return []byte(`{"violations":[` + strings.Join(vs, ",") + `]}`)
}

func processOnce(t *testing.T, w *Worker, d *db.DB, crawl CrawlFunc) *db.Run {
	t.Helper()
	claimed, err := d.ClaimNextQueuedRun()
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", err, claimed)
	}
	w.Crawl = crawl
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process: %v", err)
	}
	return claimed
}

// TestDiffPhaseSizeChangeVsRegression verifies the signal-quality fix: a page
// whose capture HEIGHT changed between runs is reported as a layout/size change
// (PagesSizeChanged), NOT a visual regression (PagesChanged), and produces no
// diff image; a page with a genuine SAME-SIZE pixel change IS counted as a
// regression with a diff image.
func TestDiffPhaseSizeChangeVsRegression(t *testing.T) {
	d, st := setup(t)
	w := New(d, st, "", 50, 3)

	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	white := color.RGBA{255, 255, 255, 255}
	black := color.RGBA{0, 0, 0, 255}
	short := solidShotWH(t, 40, 30, white) // baseline height
	tall := solidShotWH(t, 40, 60, white)  // same width, GREW taller → SizeChanged
	blackShort := solidShotWH(t, 40, 30, black)

	// Run 1 (baseline): /grow (short, white) and /paint (short, white).
	r1, _ := d.CreateRun("u", tgt.ID)
	processOnce(t, w, d, crawlFor(t, []fakePage{
		{path: "/grow", rules: []string{"image-alt"}, shot: short},
		{path: "/paint", rules: []string{"image-alt"}, shot: short},
	}))

	// Run 2: /grow got TALLER (same content color, height change only) →
	// layout/size change; /paint inverted white→black at the SAME size → true
	// visual regression.
	r2, _ := d.CreateRun("u", tgt.ID)
	if r2.PrevRunID != r1.ID {
		t.Fatalf("run2 not linked to baseline: prev=%q", r2.PrevRunID)
	}
	processOnce(t, w, d, crawlFor(t, []fakePage{
		{path: "/grow", rules: []string{"image-alt"}, shot: tall},
		{path: "/paint", rules: []string{"image-alt"}, shot: blackShort},
	}))

	got2, _ := d.GetRun("u", r2.ID)
	var dj report.Diff
	if err := json.Unmarshal([]byte(got2.DiffJSON), &dj); err != nil {
		t.Fatalf("decode diff: %v", err)
	}

	// /paint changed at the same size on BOTH viewports → 2 regressions.
	if dj.PagesChanged != 2 {
		t.Errorf("pages_changed (regressions) = %d, want 2 (the same-size /paint × 2 viewports)", dj.PagesChanged)
	}
	// /grow changed height on BOTH viewports → 2 size/layout changes.
	if dj.PagesSizeChanged != 2 {
		t.Errorf("pages_size_changed = %d, want 2 (the taller /grow × 2 viewports)", dj.PagesSizeChanged)
	}

	// Classify the per-page entries.
	var growEntries, paintEntries int
	for _, cp := range dj.ChangedPages {
		switch {
		case strings.HasSuffix(cp.URL, "/grow"):
			growEntries++
			if !cp.SizeChanged {
				t.Errorf("/grow entry should be SizeChanged: %+v", cp)
			}
			if cp.IsRegression() {
				t.Errorf("/grow (height change) must NOT be a regression: %+v", cp)
			}
			if cp.DiffKey != "" {
				t.Errorf("/grow (size changed) must have no diff image, got key %q", cp.DiffKey)
			}
		case strings.HasSuffix(cp.URL, "/paint"):
			paintEntries++
			if cp.SizeChanged {
				t.Errorf("/paint entry should not be SizeChanged: %+v", cp)
			}
			if !cp.IsRegression() {
				t.Errorf("/paint (same-size change) should be a regression: %+v", cp)
			}
			if cp.DiffKey == "" {
				t.Errorf("/paint (regression) should have a diff image key")
			}
		}
	}
	if growEntries != 2 || paintEntries != 2 {
		t.Errorf("changed-page entries: grow=%d paint=%d, want 2/2", growEntries, paintEntries)
	}

	// No diff image was uploaded for the size-changed /grow page.
	keys, _ := st.List(context.Background(), storage.Slug("Acme")+"/"+r2.ID+"/")
	for _, k := range keys {
		if strings.Contains(k, "grow") && strings.HasSuffix(k, ".diff.png") {
			t.Errorf("size-changed /grow must not upload a diff image, found %q", k)
		}
	}
}

func TestDiffPhase(t *testing.T) {
	d, st := setup(t)
	w := New(d, st, "", 50, 3)

	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	whiteShot := solidShot(t, color.RGBA{255, 255, 255, 255})
	blackShot := solidShot(t, color.RGBA{0, 0, 0, 255})

	// --- Run 1 (baseline): one page, one a11y rule, a first-party console error. ---
	r1, _ := d.CreateRun("u", tgt.ID)
	run1 := processOnce(t, w, d, crawlFor(t, []fakePage{
		{path: "/", rules: []string{"image-alt"}, shot: whiteShot,
			console: []crawler.ConsoleError{{Text: "boom", URL: "https://acme.test/app.js", FirstPart: true}}},
	}))
	if run1.ID != r1.ID {
		t.Fatalf("claimed wrong run")
	}
	got1, _ := d.GetRun("u", r1.ID)
	if got1.DiffJSON != "" {
		t.Errorf("first run should have no diff, got %q", got1.DiffJSON)
	}

	// --- Run 2: base page screenshot CHANGED (white→black), a NEW a11y rule
	// (label), the console error resolved, AND a new page (/new). ---
	r2, _ := d.CreateRun("u", tgt.ID)
	if r2.PrevRunID != r1.ID {
		t.Fatalf("run2 not linked to baseline: prev=%q", r2.PrevRunID)
	}
	processOnce(t, w, d, crawlFor(t, []fakePage{
		{path: "/", rules: []string{"image-alt", "label"}, shot: blackShot}, // no console error now
		{path: "/new", rules: []string{"image-alt"}, shot: whiteShot},
	}))

	got2, _ := d.GetRun("u", r2.ID)
	if got2.DiffJSON == "" {
		t.Fatal("run2 should carry a diff summary")
	}
	var dj report.Diff
	if err := json.Unmarshal([]byte(got2.DiffJSON), &dj); err != nil {
		t.Fatalf("decode diff: %v", err)
	}

	// Page-set delta: /new added, nothing removed.
	if len(dj.PagesAdded) != 1 || !strings.HasSuffix(dj.PagesAdded[0], "/new") {
		t.Errorf("pages added = %v, want [.../new]", dj.PagesAdded)
	}
	if len(dj.PagesRemoved) != 0 {
		t.Errorf("pages removed = %v, want none", dj.PagesRemoved)
	}

	// Visual change: the base page (both viewports) changed 100%.
	if dj.PagesChanged < 2 {
		t.Errorf("pages changed = %d, want >=2 (base page × 2 viewports)", dj.PagesChanged)
	}
	if len(dj.ChangedPages) < 2 {
		t.Fatalf("changed pages = %d, want >=2", len(dj.ChangedPages))
	}
	for _, cp := range dj.ChangedPages {
		if cp.DiffPct <= 0 {
			t.Errorf("changed page has non-positive diff pct: %+v", cp)
		}
		if !cp.IsRegression() {
			t.Errorf("100%% change should be a regression: %+v", cp)
		}
	}

	// a11y rule delta: "label" is new.
	foundLabel := false
	for _, r := range dj.NewA11yRules {
		if r == "label" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Errorf("new a11y rules = %v, want to include 'label'", dj.NewA11yRules)
	}

	// Console delta: run1 had 2 first-party console errors (one per viewport),
	// run2 has 0 → delta -2.
	if dj.ConsoleDelta != -2 {
		t.Errorf("console delta = %d, want -2", dj.ConsoleDelta)
	}

	// The diff image landed in storage under run2.
	keys, _ := st.List(context.Background(), storage.Slug("Acme")+"/"+r2.ID+"/")
	var diffs int
	for _, k := range keys {
		if strings.HasSuffix(k, ".diff.png") {
			diffs++
		}
	}
	if diffs < 2 {
		t.Errorf("expected >=2 diff images in storage, got %d (keys=%v)", diffs, keys)
	}

	// pages.diff_pct persisted on the current run's base-page rows.
	pgs, _ := d.ListPages(r2.ID)
	var withDiff int
	for _, p := range pgs {
		if strings.HasSuffix(p.URL, "acme.test/") && p.DiffPct > 0 && p.DiffKey != "" {
			withDiff++
		}
	}
	if withDiff < 2 {
		t.Errorf("expected base page rows to carry diff_pct+diff_key, got %d", withDiff)
	}
}
