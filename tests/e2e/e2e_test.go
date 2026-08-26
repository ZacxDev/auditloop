// Package e2e is a hermetic end-to-end test: it stands up a fixture multi-page
// site (with a known axe violation, a first-party console.error + network 404,
// and a private-IP link), runs the app in DEV_MODE with the filesystem storage
// backend + sqlite, triggers a run through the HTTP API, waits for it to reach
// 'done', and asserts pages were crawled, the a11y violation was found, the
// screenshots landed in storage, the SSRF guard refused the private-IP link, and
// the run view renders.
//
// It drives real headless Chromium via chromedp (resolved host-agnostically),
// so it needs a chromium/chrome binary. Set AUDITLOOP_CHROMIUM to override.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

func resolveChromium(t *testing.T) string {
	if p := os.Getenv("AUDITLOOP_CHROMIUM"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "chrome", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium/chrome on PATH and AUDITLOOP_CHROMIUM unset — skipping browser e2e")
	return ""
}

// fixtureSite serves a tiny multi-page static site with known signals.
func fixtureSite() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// A private-IP link (10.0.0.5) the SSRF guard must refuse; an <img> with
		// no alt (axe image-alt violation); and a first-party console.error.
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Home</title></head>
<body>
<h1>Fixture home</h1>
<img src="/pixel.png">
<a href="/about">About</a>
<a href="/broken">Broken</a>
<a href="http://10.0.0.5/internal">Internal (should be blocked)</a>
<script>console.error('e2e-first-party-console-error');</script>
</body></html>`)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>About</title></head><body><h1>About</h1><p>Hello.</p></body></html>`)
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
		// References a missing first-party script → network 404.
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Broken</title><script src="/missing.js"></script></head><body><h1>Broken</h1></body></html>`)
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, r *http.Request) {
		// 1x1 transparent PNG.
		png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
			0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	return httptest.NewServer(mux)
}

func TestEndToEndCrawl(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fixture := fixtureSite()
	defer fixture.Close()
	// fixture.URL is http://127.0.0.1:<port>. Host is loopback.
	fixtureHost := strings.TrimPrefix(fixture.URL, "http://")
	if i := strings.Index(fixtureHost, ":"); i >= 0 {
		fixtureHost = fixtureHost[:i]
	}

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port:               "0",
		Role:               config.RoleAll,
		DatabaseDriver:     "sqlite",
		DatabasePath:       filepath.Join(tmp, "e2e.db"),
		S3Local:            filepath.Join(tmp, "artifacts"),
		CrawlMaxPages:      10,
		CrawlMaxDepth:      2,
		CrawlAllowLoopback: true, // dev/test-only: reach the loopback fixture
		ChromiumPath:       chromium,
		DevMode:            true,
	}

	database, err := db.Open(cfg.DatabaseDriver, cfg.DatabasePath)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()

	store, err := handlers.OpenStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router, err := handlers.NewRouter(ctx, cfg, database, store)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	// Create the target directly so we can register BOTH the loopback fixture
	// host (allowed via AllowLoopback) AND 10.0.0.5 (in the allowlist, so the
	// private-IP link is enqueued and then refused by the IP guard — proving the
	// guard, not merely the allowlist, blocks it). Owned by the dev user.
	tgt, err := database.CreateTarget(auth.DefaultDevUser, "Fixture", fixture.URL, []string{fixtureHost, "10.0.0.5"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	// Trigger the run THROUGH the HTTP API.
	req, _ := http.NewRequest("POST", appSrv.URL+"/api/targets/"+tgt.ID+"/runs", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger run: %v", err)
	}
	runURL := resp.Header.Get("HX-Redirect")
	resp.Body.Close()
	if runURL == "" || !strings.HasPrefix(runURL, "/runs/") {
		t.Fatalf("expected HX-Redirect to /runs/…, got %q", runURL)
	}
	runID := strings.TrimPrefix(runURL, "/runs/")

	// Poll until the run reaches a terminal state.
	deadline := time.Now().Add(90 * time.Second)
	var run *db.Run
	for time.Now().Before(deadline) {
		run, err = database.GetRun(auth.DefaultDevUser, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status == db.RunDone || run.Status == db.RunFailed {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if run == nil || run.Status != db.RunDone {
		t.Fatalf("run did not complete: status=%v err=%q", statusOf(run), errOf(run))
	}

	// --- Assertions ---

	// Summary counts.
	var summary report.Summary
	if err := json.Unmarshal([]byte(run.SummaryJSON), &summary); err != nil {
		t.Fatalf("summary json: %v (%s)", err, run.SummaryJSON)
	}
	if summary.PagesCrawled < 2 {
		t.Errorf("expected >=2 pages crawled, got %d", summary.PagesCrawled)
	}
	if summary.URLsBlocked < 1 {
		t.Errorf("expected the private-IP link to be blocked (URLsBlocked>=1), got %d", summary.URLsBlocked)
	}
	if summary.A11yViolations < 1 {
		t.Errorf("expected >=1 a11y violation (image-alt), got %d", summary.A11yViolations)
	}

	// The known a11y violation (image-alt) is recorded as a finding.
	pages, _ := database.ListPages(runID)
	if len(pages) < 2 {
		t.Fatalf("expected >=2 page rows (multi-viewport), got %d", len(pages))
	}
	foundImageAlt := false
	viewports := map[string]bool{}
	for _, p := range pages {
		viewports[p.Viewport] = true
		finds, _ := database.ListFindings(p.ID)
		for _, f := range finds {
			if f.Type == db.FindingA11y && strings.Contains(f.Detail, "image-alt") {
				foundImageAlt = true
			}
		}
	}
	if !foundImageAlt {
		t.Error("expected an 'image-alt' a11y finding")
	}
	if !viewports["mobile"] || !viewports["desktop"] {
		t.Errorf("expected both mobile+desktop viewports captured, got %v", viewports)
	}

	// Screenshots landed in storage (list the "bucket").
	fs, ok := store.(*storage.FS)
	if !ok {
		t.Fatalf("expected FS store")
	}
	keys, err := fs.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list storage: %v", err)
	}
	var pngs, reports int
	for _, k := range keys {
		if strings.HasSuffix(k, ".png") {
			pngs++
		}
		if strings.HasSuffix(k, "/report.json") {
			reports++
		}
	}
	if pngs < 2 {
		t.Errorf("expected >=2 screenshots in storage, got %d (keys=%v)", pngs, keys)
	}
	if reports != 1 {
		t.Errorf("expected exactly one report.json in storage, got %d", reports)
	}

	// The run view renders.
	viewResp, err := http.Get(appSrv.URL + "/runs/" + runID)
	if err != nil {
		t.Fatalf("get run view: %v", err)
	}
	body, _ := io.ReadAll(viewResp.Body)
	viewResp.Body.Close()
	if viewResp.StatusCode != 200 {
		t.Fatalf("run view status = %d", viewResp.StatusCode)
	}
	if !strings.Contains(string(body), "Audit report") {
		t.Error("run view did not render expected content")
	}

	t.Logf("e2e OK: pages=%d blocked=%d a11y=%d screenshots=%d", summary.PagesCrawled, summary.URLsBlocked, summary.A11yViolations, pngs)
}

// mutableFixtureSite serves a site whose pages change when `mutated` flips, so
// the P2 diff exercises BOTH regression paths:
//   - "/" (home) is a FIXED-HEIGHT page whose background flips white→dark and copy
//     changes: same capture dimensions → a genuine SAME-SIZE visual regression.
//     It also gains a NEW unlabeled input (fresh axe "label" violation) and a link
//     to a NEW /added page (page-set add).
//   - "/about" GROWS a tall block of content only when mutated: the capture height
//     changes → a LAYOUT/SIZE change, which must NOT be counted as a visual
//     regression (the height-shift signal-quality fix).
func mutableFixtureSite(mutated *atomic.Bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Fixed body height in BOTH states so the capture dimensions stay constant
		// (isolating a pure same-size visual change from any height shift).
		if mutated.Load() {
			_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Home</title>
<style>html,body{margin:0}body{min-height:900px;background:#101020;color:#fff}</style></head>
<body>
<h1>Fixture home — REVISED</h1>
<p>The copy and colors changed substantially in this release.</p>
<img src="/pixel.png">
<form><input type="text" name="q"><button>Go</button></form>
<a href="/about">About</a>
<a href="/added">Newly added page</a>
</body></html>`)
			return
		}
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Home</title>
<style>html,body{margin:0}body{min-height:900px;background:#ffffff;color:#000}</style></head>
<body>
<h1>Fixture home</h1>
<img src="/pixel.png">
<a href="/about">About</a>
</body></html>`)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		// A proper viewport meta makes the FULL-PAGE capture track content height, so
		// the appended tall block below (only when mutated) genuinely grows the
		// capture height → a layout/size change (must NOT be a visual regression).
		if mutated.Load() {
			_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>About</title></head><body style="margin:0"><h1>About</h1><p>Hello.</p>
<div style="height:1600px;background:#eef">Lots of newly added tall content pushing the page height down.</div>
</body></html>`)
			return
		}
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>About</title></head><body style="margin:0"><h1>About</h1><p>Hello.</p></body></html>`)
	})
	mux.HandleFunc("/added", func(w http.ResponseWriter, r *http.Request) {
		if !mutated.Load() {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Added</title></head><body><h1>Newly added</h1></body></html>`)
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, r *http.Request) {
		png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
			0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	return httptest.NewServer(mux)
}

// TestEndToEndRegressionDiff (P2) crawls the fixture TWICE. Between runs it
// mutates the site (visual change + a new a11y violation + a new page) and
// asserts the second run's diff summary surfaces all three, and that a diff
// image landed in storage.
func TestEndToEndRegressionDiff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	var mutated atomic.Bool
	fixture := mutableFixtureSite(&mutated)
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-diff.db"), S3Local: filepath.Join(tmp, "artifacts"),
		CrawlMaxPages: 10, CrawlMaxDepth: 2, CrawlAllowLoopback: true,
		ChromiumPath: chromium, DevMode: true,
	}
	database, err := db.Open(cfg.DatabaseDriver, cfg.DatabasePath)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	store, err := handlers.OpenStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router, err := handlers.NewRouter(ctx, cfg, database, store)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "DiffFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	// --- Run 1 (baseline). ---
	run1 := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run1.Status != db.RunDone {
		t.Fatalf("run1 did not complete: %s / %s", run1.Status, run1.Error)
	}
	if run1.PrevRunID != "" {
		t.Errorf("run1 should have no baseline, got %q", run1.PrevRunID)
	}
	if run1.DiffJSON != "" {
		t.Errorf("run1 should have no diff, got %q", run1.DiffJSON)
	}

	// --- Mutate the site, then Run 2. ---
	mutated.Store(true)
	run2 := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run2.Status != db.RunDone {
		t.Fatalf("run2 did not complete: %s / %s", run2.Status, run2.Error)
	}
	if run2.PrevRunID != run1.ID {
		t.Fatalf("run2 baseline = %q, want %q", run2.PrevRunID, run1.ID)
	}
	if run2.DiffJSON == "" {
		t.Fatal("run2 must carry a diff summary")
	}

	var d report.Diff
	if err := json.Unmarshal([]byte(run2.DiffJSON), &d); err != nil {
		t.Fatalf("decode diff: %v (%s)", err, run2.DiffJSON)
	}

	// (1a) The home page changed at the SAME size → a genuine visual regression.
	if d.PagesChanged < 1 {
		t.Errorf("expected >=1 same-size visual regression (home), got pages_changed=%d", d.PagesChanged)
	}
	if len(d.ChangedPages) < 1 {
		t.Fatalf("expected >=1 changed page entry, got %d", len(d.ChangedPages))
	}
	if d.ChangedPages[0].DiffPct <= 0 {
		t.Errorf("top changed page diff_pct = %.3f, want > 0", d.ChangedPages[0].DiffPct)
	}
	// Find the home entries ("/"): same-size regressions, NOT size-changed.
	var homeRegressions int
	for _, cp := range d.ChangedPages {
		if !isHomeURL(cp.URL) {
			continue
		}
		if cp.SizeChanged {
			t.Errorf("home capture should be same-size (regression), got size_changed: %+v", cp)
		}
		if cp.IsRegression() {
			homeRegressions++
		}
	}
	if homeRegressions < 1 {
		t.Errorf("expected the home page to be a same-size visual regression, got %d", homeRegressions)
	}

	// (1b) The /about page GREW taller → a layout/size change, NOT a regression.
	if d.PagesSizeChanged < 1 {
		t.Errorf("expected >=1 layout/size change (/about grew taller), got pages_size_changed=%d", d.PagesSizeChanged)
	}
	var aboutSizeChanges int
	for _, cp := range d.ChangedPages {
		if strings.HasSuffix(cp.URL, "/about") {
			if !cp.SizeChanged {
				t.Errorf("/about should be size_changed (height grew): %+v", cp)
			}
			if cp.IsRegression() {
				t.Errorf("/about height change must NOT be a visual regression: %+v", cp)
			}
			if cp.DiffKey != "" {
				t.Errorf("/about (size changed) must not carry a diff image key, got %q", cp.DiffKey)
			}
			aboutSizeChanges++
		}
	}
	if aboutSizeChanges < 1 {
		t.Errorf("expected /about to appear as a size/layout change, found none")
	}

	// (2) The new a11y rule (an unlabeled input → "label") is flagged.
	if !contains(d.NewA11yRules, "label") {
		t.Errorf("new a11y rules = %v, want to include 'label'", d.NewA11yRules)
	}

	// (3) The newly-added page is detected.
	foundAdded := false
	for _, u := range d.PagesAdded {
		if strings.HasSuffix(u, "/added") {
			foundAdded = true
		}
	}
	if !foundAdded {
		t.Errorf("pages added = %v, want to include .../added", d.PagesAdded)
	}

	// (4) A diff image landed in storage under run2.
	fs, ok := store.(*storage.FS)
	if !ok {
		t.Fatalf("expected FS store")
	}
	keys, _ := fs.List(context.Background(), "")
	var diffs int
	for _, k := range keys {
		if strings.HasSuffix(k, ".diff.png") && strings.Contains(k, run2.ID) {
			diffs++
		}
	}
	if diffs < 1 {
		t.Errorf("expected >=1 diff image in storage for run2, got %d (keys=%v)", diffs, keys)
	}

	// (5) The run view renders the "Changes since" section.
	body := getBody(t, appSrv.URL+"/runs/"+run2.ID)
	if !strings.Contains(body, "Changes since") {
		t.Error("run view did not render the P2 changes section")
	}

	t.Logf("e2e diff OK: changed=%d added=%v newA11y=%v consoleΔ=%d diffImages=%d",
		d.PagesChanged, d.PagesAdded, d.NewA11yRules, d.ConsoleDelta, diffs)
}

// triggerAndWait enqueues a run via the HTTP API and polls until it is terminal.
func triggerAndWait(t *testing.T, appURL string, database *db.DB, targetID string) *db.Run {
	t.Helper()
	req, _ := http.NewRequest("POST", appURL+"/api/targets/"+targetID+"/runs", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger run: %v", err)
	}
	runURL := resp.Header.Get("HX-Redirect")
	resp.Body.Close()
	if !strings.HasPrefix(runURL, "/runs/") {
		t.Fatalf("expected HX-Redirect to /runs/…, got %q", runURL)
	}
	runID := strings.TrimPrefix(runURL, "/runs/")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(auth.DefaultDevUser, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status == db.RunDone || run.Status == db.RunFailed {
			return run
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("run %s did not complete in time", runID)
	return nil
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// isHomeURL reports whether a crawled URL is the site root ("/"): the path after
// scheme://host[:port] is empty or a bare "/".
func isHomeURL(raw string) bool {
	h := strings.TrimPrefix(raw, "http://")
	h = strings.TrimPrefix(h, "https://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		return h[i:] == "/" // exactly host + "/"
	}
	return true // host with no path
}

func hostOnly(raw string) string {
	h := strings.TrimPrefix(raw, "http://")
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// signalsFixtureSite serves a page engineered to trigger every deterministic
// signal: a MOBILE-hostile layout (no viewport meta, a wide element forcing
// horizontal overflow, a <44px tap target, <12px text, an <img> with no
// dimensions), a scripted post-load layout shift (CLS), and a first-party 404
// subresource (broken link). The perf columns (weight/req count) populate simply
// by loading.
// signalsHomeHTML is the signals fixture's home document. Kept as a named
// constant so the weight-accounting assertion can floor weight_bytes at the
// served document's own size — which only holds once the main navigation
// document's transferred bytes (reported via Network.dataReceived, not
// loadingFinished) are counted.
const signalsHomeHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Signals</title>
<script src="/missing.js"></script></head>
<body style="margin:0">
<div style="width:2000px;height:8px;background:#eee"></div>
<h1>Signals fixture</h1>
<p style="font-size:8px">This is deliberately tiny body text under twelve pixels.</p>
<a href="/about" style="display:inline-block;width:20px;height:20px;background:#ccc">x</a>
<a href="/hostile">Hostile</a>
<img src="/pixel.png">
<div id="hero" style="height:760px;background:#cde">A large block of visible content filling most of the viewport, so shifting it down produces a substantial Cumulative Layout Shift.</div>
<script>setTimeout(function(){var d=document.createElement('div');d.style.height='450px';d.style.background='#f0f0f0';d.textContent='inserted banner';document.body.insertBefore(d,document.body.firstChild);},150);</script>
</body></html>`

func signalsFixtureSite() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Deliberately NO <meta name=viewport>. A 2000px-wide block forces
		// horizontal overflow on the 390px mobile capture. A 20x20 link is a tiny
		// tap target. 8px text is sub-12px. The <img> has no width/height. A
		// missing.js 404s (first-party broken link). The inline script shifts the
		// layout ~150ms after load → a measurable CLS.
		_, _ = io.WriteString(w, signalsHomeHTML)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>About</title></head><body><h1>About</h1></body></html>`)
	})
	mux.HandleFunc("/hostile", func(w http.ResponseWriter, r *http.Request) {
		// A page that sabotages the DOM APIs the layout eval uses (getComputedStyle
		// throws). The layout eval must degrade to zero smells (return '{}') WITHOUT
		// dropping the whole capture — the screenshot must still be captured. This
		// verifies the try/catch hardening of layoutSmellScript.
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Hostile</title>
<script>window.getComputedStyle=function(){throw new Error('sabotage')};Element.prototype.getBoundingClientRect=function(){throw new Error('sabotage')};</script></head>
<body><h1>Hostile page</h1><p>The DOM measurement APIs throw here.</p></body></html>`)
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, r *http.Request) {
		png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
			0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	// /missing.js is intentionally NOT registered → 404 (first-party broken link).
	return httptest.NewServer(mux)
}

// TestEndToEndDeterministicSignals crawls the signals fixture and asserts the
// deterministic pass actually fires: perf columns populate, perf (CLS) + layout
// smells + a broken-link network finding all land in the DB and report.json.
func TestEndToEndDeterministicSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fixture := signalsFixtureSite()
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-signals.db"), S3Local: filepath.Join(tmp, "artifacts"),
		CrawlMaxPages: 5, CrawlMaxDepth: 1, CrawlAllowLoopback: true,
		ChromiumPath: chromium, DevMode: true,
	}
	database, err := db.Open(cfg.DatabaseDriver, cfg.DatabasePath)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	store, err := handlers.OpenStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router, err := handlers.NewRouter(ctx, cfg, database, store)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "Signals", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	run := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run.Status != db.RunDone {
		t.Fatalf("run did not complete: %s / %s", run.Status, run.Error)
	}

	pages, _ := database.ListPages(run.ID)
	if len(pages) == 0 {
		t.Fatal("no pages crawled")
	}

	// (1) Perf columns populated: the home capture recorded a request count and
	// transfer weight from the CDP network events.
	var homeMobile, homeDesktop *db.Page
	for _, p := range pages {
		if isHomeURL(p.URL) {
			if p.Viewport == "mobile" {
				homeMobile = p
			} else if p.Viewport == "desktop" {
				homeDesktop = p
			}
		}
	}
	if homeMobile == nil || homeDesktop == nil {
		t.Fatalf("expected home page at both viewports, got %d pages", len(pages))
	}
	if homeMobile.ReqCount < 1 || homeMobile.WeightBytes < 1 {
		t.Errorf("perf columns not populated: req=%d weight=%d", homeMobile.ReqCount, homeMobile.WeightBytes)
	}
	// The main navigation document's own bytes must be counted, not just
	// subresources. Chromium reports loadingFinished.encodedDataLength=0 for the
	// document (its real bytes arrive via Network.dataReceived), so before the
	// dataReceived fallback the weight would reflect only the tiny subresources
	// (a 1x1 PNG + a 404 body) and fall well below the document size. The fixture
	// is served uncompressed, so the document's transfer weight is >= its byte
	// length; flooring weight at len(signalsHomeHTML) proves the document is
	// included.
	docSize := int64(len(signalsHomeHTML))
	if homeMobile.WeightBytes < docSize {
		t.Errorf("weight_bytes (%d) < served document size (%d): main-document bytes not counted", homeMobile.WeightBytes, docSize)
	}
	// The DESKTOP viewport is the 2nd capture of the same URL in the shared browser
	// context. With SetCacheDisabled(true) it is a cold load too, so its document
	// bytes are counted just like mobile's; without cache-disable it would be served
	// from Chromium's HTTP cache and report weight 0. Flooring it at docSize proves
	// every viewport gets an accurate cold-load weight, not just the first.
	if homeDesktop.WeightBytes < docSize {
		t.Errorf("desktop weight_bytes (%d) < served document size (%d): 2nd-viewport capture served from cache (cache not disabled)", homeDesktop.WeightBytes, docSize)
	}

	// Collect findings on the home page (both viewports).
	layoutSmells := map[string]bool{}
	var perfMetrics []string
	var brokenLinkSerious bool
	collect := func(p *db.Page) {
		finds, _ := database.ListFindings(p.ID)
		for _, f := range finds {
			switch f.Type {
			case db.FindingLayout:
				var d struct {
					Smell string `json:"smell"`
				}
				if json.Unmarshal([]byte(f.Detail), &d) == nil {
					layoutSmells[d.Smell] = true
				}
			case db.FindingPerf:
				var d struct {
					Metric string `json:"metric"`
				}
				if json.Unmarshal([]byte(f.Detail), &d) == nil && d.Metric != "" {
					perfMetrics = append(perfMetrics, d.Metric)
				}
			case db.FindingNetwork:
				if strings.Contains(f.Detail, "missing.js") && f.Severity == "serious" {
					brokenLinkSerious = true
				}
			}
		}
	}
	collect(homeMobile)
	collect(homeDesktop)

	// (2) Layout smells: the mobile-only ones fire on mobile; viewport-agnostic ones fire too.
	for _, want := range []string{"missing-viewport-meta", "images-without-dimensions", "small-text", "horizontal-overflow", "small-tap-targets"} {
		if !layoutSmells[want] {
			t.Errorf("expected layout smell %q, got %v", want, keysOf(layoutSmells))
		}
	}

	// (3) Perf finding: the scripted post-load shift produces a CLS breach.
	sawCLS := false
	for _, m := range perfMetrics {
		if m == "CLS" {
			sawCLS = true
		}
	}
	if !sawCLS {
		t.Errorf("expected a CLS perf finding from the scripted layout shift, got metrics %v", perfMetrics)
	}

	// (4) Broken link: the first-party 404 (missing.js) is a serious network finding.
	if !brokenLinkSerious {
		t.Error("expected a serious first-party network finding for the /missing.js 404")
	}

	// (5) report.json carries the perf block and the layout/perf findings.
	rc, err := store.Get(context.Background(), storage.ReportKey(storage.Slug("Signals"), run.ID))
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	rep, err := report.Decode(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("decode report: %v", err)
	}
	var sawPerfBlock, sawLayoutFinding, sawPerfFinding bool
	for _, pg := range rep.Pages {
		if pg.Perf != nil && (pg.Perf.ReqCount > 0 || pg.Perf.WeightBytes > 0) {
			sawPerfBlock = true
		}
		for _, f := range pg.Findings {
			if f.Type == "layout" {
				sawLayoutFinding = true
			}
			if f.Type == "perf" {
				sawPerfFinding = true
			}
		}
	}
	if !sawPerfBlock {
		t.Error("report.json pages missing the perf block")
	}
	if !sawLayoutFinding {
		t.Error("report.json missing layout findings")
	}
	if !sawPerfFinding {
		t.Error("report.json missing perf findings")
	}

	// (6) The /hostile page (getComputedStyle/getBoundingClientRect throw) must
	// still be CAPTURED — the layout eval degrades to zero smells (return '{}')
	// instead of throwing and dropping the whole capture (screenshot + axe + perf).
	var hostileCaptured, hostileHasScreenshot bool
	hostileLayoutFindings := 0
	for _, p := range pages {
		if !strings.HasSuffix(p.URL, "/hostile") {
			continue
		}
		hostileCaptured = true
		if p.ScreenshotKey != "" {
			hostileHasScreenshot = true
		}
		finds, _ := database.ListFindings(p.ID)
		for _, f := range finds {
			if f.Type == db.FindingLayout {
				hostileLayoutFindings++
			}
		}
	}
	if !hostileCaptured {
		t.Error("hostile page (throwing DOM APIs) was not captured — the layout eval likely dropped the whole capture")
	}
	if hostileCaptured && !hostileHasScreenshot {
		t.Error("hostile page captured but has no screenshot — capture partially lost")
	}
	if hostileLayoutFindings != 0 {
		t.Errorf("hostile page should degrade to zero layout findings, got %d", hostileLayoutFindings)
	}

	t.Logf("e2e signals OK: layoutSmells=%v perfMetrics=%v brokenLink=%v req=%d weight=%d hostileCaptured=%v",
		keysOf(layoutSmells), perfMetrics, brokenLinkSerious, homeMobile.ReqCount, homeMobile.WeightBytes, hostileCaptured)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func statusOf(r *db.Run) string {
	if r == nil {
		return "nil"
	}
	return r.Status
}
func errOf(r *db.Run) string {
	if r == nil {
		return ""
	}
	return r.Error
}
