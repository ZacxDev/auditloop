package worker

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

func setup(t *testing.T) (*db.DB, *storage.FS) {
	t.Helper()
	d, err := db.Open("sqlite", filepath.Join(t.TempDir(), "w.db"))
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

// fakeCrawl returns a canned result — one page at two viewports with a known
// axe violation, a first-party console error, and a third-party network error.
func fakeCrawl(_ context.Context, opts crawler.Options) (*crawler.Result, error) {
	axe := []byte(`{"violations":[{"id":"image-alt","impact":"serious","nodeCount":1,"nodes":[{"target":["img"]}]}]}`)
	mk := func(vp crawler.Viewport) crawler.PageResult {
		return crawler.PageResult{
			URL: opts.BaseURL, Viewport: vp,
			ScreenshotPNG: []byte("PNGDATA-" + vp.Name),
			AxeJSON:       axe, AxeViolations: 1, AxeNodes: 1,
			ConsoleErrors:  []crawler.ConsoleError{{Text: "boom", URL: opts.BaseURL + "/app.js", FirstPart: true}},
			NetworkErrors:  []crawler.NetworkError{{URL: "https://cdn.example.com/x.js", Status: 404, FirstPart: false}},
			NetworkLogJSON: []byte(`[{"url":"https://cdn.example.com/x.js","status":404}]`),
			LoadMS:         123,
		}
	}
	return &crawler.Result{
		Pages:          []crawler.PageResult{mk(crawler.DefaultViewports[0]), mk(crawler.DefaultViewports[1])},
		URLsDiscovered: 3,
		URLsBlocked:    1,
	}, nil
}

func TestProcessRun(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()

	w := New(d, st, "", 50, 3)
	w.Crawl = fakeCrawl
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Run finished done with a summary.
	got, _ := d.GetRun("u", run.ID)
	if got.Status != db.RunDone {
		t.Fatalf("run status = %q", got.Status)
	}

	// Two page rows (2 viewports).
	pages, _ := d.ListPages(run.ID)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	p := pages[0]
	if p.AxeViolationCount != 1 {
		t.Errorf("axe count = %d", p.AxeViolationCount)
	}
	if p.ConsoleFirstPartyCount != 1 || p.NetworkThirdPartyCount != 1 {
		t.Errorf("origin counts wrong: %+v", p)
	}

	// Findings: a11y + console + network, per page.
	finds, _ := d.ListFindings(p.ID)
	types := map[string]int{}
	for _, f := range finds {
		types[f.Type]++
	}
	if types["a11y"] != 1 || types["console"] != 1 || types["network"] != 1 {
		t.Errorf("finding types = %v", types)
	}

	// Screenshots landed in storage.
	keys, _ := st.List(context.Background(), storage.Slug("Acme")+"/"+run.ID+"/")
	var pngs, reports int
	for _, k := range keys {
		if filepath.Ext(k) == ".png" {
			pngs++
		}
		if filepath.Base(k) == "report.json" {
			reports++
		}
	}
	if pngs != 2 {
		t.Errorf("expected 2 screenshots in storage, got %d (keys=%v)", pngs, keys)
	}
	if reports != 1 {
		t.Errorf("expected report.json in storage, got %d", reports)
	}

	// report.json is valid and carries the rollups.
	rc, err := st.Get(context.Background(), storage.ReportKey(storage.Slug("Acme"), run.ID))
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	rep, err := report.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.Summary.PagesCrawled != 1 || rep.Summary.A11yViolations != 2 {
		t.Errorf("report summary wrong: %+v", rep.Summary)
	}
	if rep.Summary.URLsBlocked != 1 {
		t.Errorf("blocked not recorded: %d", rep.Summary.URLsBlocked)
	}
	if rep.Versions.AxeCore != crawler.AxeVersion {
		t.Errorf("axe version = %q", rep.Versions.AxeCore)
	}
}

// TestProcessRunFaviconCaptured asserts a crawl-captured favicon is uploaded under
// the run-scoped key and recorded on the run (best-effort persistence path).
func TestProcessRunFaviconCaptured(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()

	w := New(d, st, "", 50, 3)
	w.Crawl = func(ctx context.Context, opts crawler.Options) (*crawler.Result, error) {
		res, _ := fakeCrawl(ctx, opts)
		res.Favicon = []byte("ICODATA")
		res.FaviconExt = "ico"
		return res, nil
	}
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process: %v", err)
	}

	got, _ := d.GetRun("u", run.ID)
	wantKey := storage.FaviconKey(storage.Slug("Acme"), run.ID, "ico")
	if got.FaviconKey != wantKey {
		t.Fatalf("run favicon_key = %q, want %q", got.FaviconKey, wantKey)
	}
	rc, err := st.Get(context.Background(), wantKey)
	if err != nil {
		t.Fatalf("favicon not stored: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "ICODATA" {
		t.Errorf("stored favicon bytes = %q", body)
	}
}

// TestProcessRunNoFaviconDegrades asserts a run with no captured favicon leaves
// favicon_key empty and never fails (degrade path).
func TestProcessRunNoFaviconDegrades(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()

	w := New(d, st, "", 50, 3)
	w.Crawl = fakeCrawl // no Favicon set
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.Status != db.RunDone {
		t.Fatalf("run status = %q", got.Status)
	}
	if got.FaviconKey != "" {
		t.Errorf("expected empty favicon_key when none captured, got %q", got.FaviconKey)
	}
}

func TestProcessRunCrawlError(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()
	w := New(d, st, "", 10, 2)
	w.Crawl = func(context.Context, crawler.Options) (*crawler.Result, error) {
		return nil, io.ErrUnexpectedEOF
	}
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process should swallow crawl error: %v", err)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.Status != db.RunFailed || got.Error == "" {
		t.Errorf("expected failed run with error, got %+v", got)
	}
}
