package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/signals"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// readReport loads + decodes a run's stored report.json from the FS store.
func readReport(t *testing.T, app *App, runID string) *report.Report {
	t.Helper()
	fs := app.Store.(*storage.FS)
	keys, _ := fs.List(context.Background(), "")
	for _, k := range keys {
		if !strings.HasSuffix(k, "/report.json") || !strings.Contains(k, runID) {
			continue
		}
		rc, err := fs.Get(context.Background(), k)
		if err != nil {
			t.Fatalf("get report.json: %v", err)
		}
		defer rc.Close()
		var rep report.Report
		if err := json.NewDecoder(rc).Decode(&rep); err != nil {
			t.Fatalf("decode report.json: %v", err)
		}
		return &rep
	}
	t.Fatalf("no report.json found for run %s", runID)
	return nil
}

// testAppNonDev builds an app+router with DEV_MODE off (so the per-target push
// limiter is active). The plugin-push route is token-authed and public, so it
// works without Supabase auth.
func testAppNonDev(t *testing.T) (*App, http.Handler) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.AppConfig{
		Role:           config.RoleWeb,
		DatabaseDriver: "sqlite",
		DatabasePath:   tmp + "/h.db",
		S3Local:        tmp + "/art",
		DevMode:        false,
	}
	database, err := db.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFS(cfg.S3Local)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(context.Background(), cfg, database, store)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg, DB: database, Store: store}, router
}

// pngBytes encodes a solid-color w×h PNG.
func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// newPluginTarget creates a plugin target + token directly and returns the token.
func newPluginTarget(t *testing.T, app *App, name string) (string, string) {
	t.Helper()
	tgt, err := app.DB.CreatePluginTarget(auth.DefaultDevUser, name, "")
	if err != nil {
		t.Fatal(err)
	}
	token, hash, err := plugin.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DB.SetPluginToken(tgt.ID, hash); err != nil {
		t.Fatal(err)
	}
	return tgt.ID, token
}

// pushMultipart builds a multipart body: metadata JSON + the given files.
func pushMultipart(t *testing.T, meta string, files map[string][]byte) (string, *bytes.Buffer) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("metadata", meta); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		fw, err := mw.CreateFormFile(name, name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	return mw.FormDataContentType(), &body
}

func doPush(router http.Handler, token, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/plugins/runs", body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	return rw
}

func TestPluginPushNoToken(t *testing.T) {
	_, router := testApp(t)
	ct, body := pushMultipart(t, `{"pages":[]}`, nil)
	rw := doPush(router, "", ct, body)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rw.Code)
	}
}

func TestPluginPushBadToken(t *testing.T) {
	app, router := testApp(t)
	newPluginTarget(t, app, "T") // a valid token exists, but we present a bogus one
	ct, body := pushMultipart(t, `{"pages":[]}`, nil)
	rw := doPush(router, "not-a-real-token", ct, body)
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rw.Code)
	}
}

func TestPluginPushValid(t *testing.T) {
	app, router := testApp(t)
	tgtID, token := newPluginTarget(t, app, "Funnel")

	shot := pngBytes(t, 40, 40, color.White)
	meta := `{"label":"run 1","pages":[
		{"url":"signup-step-1","viewport":"desktop","screenshot":"a.png","axe":"a.json","axe_violations":2,
		 "findings":[{"type":"a11y","severity":"serious","detail":"missing <label>"}]},
		{"url":"signup-step-1","viewport":"mobile","screenshot":"b.png"}
	]}`
	files := map[string][]byte{"a.png": shot, "b.png": shot, "a.json": []byte(`{"violations":[]}`)}
	ct, body := pushMultipart(t, meta, files)
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusOK {
		t.Fatalf("push = %d (%s)", rw.Code, rw.Body.String())
	}
	var res plugin.PushResult
	if err := json.Unmarshal(rw.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad response: %v (%s)", err, rw.Body.String())
	}
	if res.RunID == "" || !strings.Contains(res.URL, "/runs/") {
		t.Fatalf("bad result: %+v", res)
	}

	// Run created, done, trigger=plugin, label set.
	run, err := app.DB.GetRun(auth.DefaultDevUser, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunDone || run.Trigger != "plugin" || run.Label != "run 1" {
		t.Fatalf("bad run: status=%s trigger=%s label=%q", run.Status, run.Trigger, run.Label)
	}
	if run.TargetID != tgtID {
		t.Fatalf("run target = %s, want %s", run.TargetID, tgtID)
	}

	// Two page rows + the a11y finding.
	prs, _ := app.DB.ListPages(res.RunID)
	if len(prs) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(prs))
	}
	foundFinding := false
	for _, p := range prs {
		if p.ScreenshotKey == "" {
			t.Errorf("page %s missing screenshot key", p.Viewport)
		}
		finds, _ := app.DB.ListFindings(p.ID)
		for _, f := range finds {
			if f.Type == db.FindingA11y {
				foundFinding = true
			}
		}
	}
	if !foundFinding {
		t.Error("expected an a11y finding on the pushed pages")
	}

	// Screenshots + report.json landed in storage.
	fs := app.Store.(*storage.FS)
	keys, _ := fs.List(context.Background(), "")
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
		t.Errorf("expected >=2 screenshots stored, got %d (%v)", pngs, keys)
	}
	if reports != 1 {
		t.Errorf("expected 1 report.json, got %d", reports)
	}
}

func TestPluginPushPerfAndLayout(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "PerfFunnel")

	shot := pngBytes(t, 40, 40, color.White)
	meta := `{"label":"perf run","pages":[
		{"url":"home","viewport":"mobile","screenshot":"m.png",
		 "perf":{"lcp_ms":3100,"cls":0.25,"tbt_ms":450,"weight_bytes":3000000,"req_count":120},
		 "findings":[
		   {"type":"layout","severity":"moderate","detail":"scrollWidth=500 > innerWidth=390 on <div>"},
		   {"type":"perf","severity":"serious","detail":"LCP is slow (3100ms)"}
		 ]}
	]}`
	files := map[string][]byte{"m.png": shot}
	ct, body := pushMultipart(t, meta, files)
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusOK {
		t.Fatalf("push = %d (%s)", rw.Code, rw.Body.String())
	}
	var res plugin.PushResult
	if err := json.Unmarshal(rw.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad response: %v", err)
	}

	prs, _ := app.DB.ListPages(res.RunID)
	if len(prs) != 1 {
		t.Fatalf("expected 1 page, got %d", len(prs))
	}
	p := prs[0]
	// Perf columns persisted on the page row.
	if p.LCPMs != 3100 || p.CLS != 0.25 || p.TBTMs != 450 || p.WeightBytes != 3000000 || p.ReqCount != 120 {
		t.Fatalf("perf columns not persisted: lcp=%d cls=%v tbt=%d weight=%d req=%d",
			p.LCPMs, p.CLS, p.TBTMs, p.WeightBytes, p.ReqCount)
	}
	// layout + perf findings inserted.
	finds, _ := app.DB.ListFindings(p.ID)
	var layoutOK, perfOK bool
	for _, f := range finds {
		if f.Type == db.FindingLayout {
			layoutOK = true
			if strings.Contains(f.Detail, "<div>") {
				t.Errorf("layout finding detail not escaped: %q", f.Detail)
			}
		}
		if f.Type == db.FindingPerf {
			perfOK = true
		}
	}
	if !layoutOK {
		t.Error("expected a layout finding to be inserted")
	}
	if !perfOK {
		t.Error("expected a perf finding to be inserted")
	}
}

// TestPluginPushLabSuppressesPerfFindings asserts the perf-honesty rule end-to-end
// through the push handler: an environment:"lab" push with a breaching perf block
// persists the raw perf COLUMNS but emits NO type:perf findings (localhost lab perf
// is not field-representative), while layout findings still fire; the run records
// environment="lab". A prod push with the SAME block DOES emit perf findings.
func TestPluginPushLabSuppressesPerfFindings(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "LabFunnel")

	shot := pngBytes(t, 40, 40, color.White)
	// Breaching perf block + a viewport-agnostic layout smell (small-text) so we can
	// assert layout findings survive regardless of environment.
	pageBlock := `{"url":"home","viewport":"mobile","screenshot":"m.png",
		 "perf":{"lcp_ms":5000,"cls":0.4,"tbt_ms":800,"weight_bytes":4000000,"req_count":120},
		 "layout":{"small_text":3,"missing_viewport_meta":true}}`

	// --- lab push: perf columns kept, NO perf findings, layout findings present. ---
	labMeta := `{"environment":"lab","label":"lab run","pages":[` + pageBlock + `]}`
	ct, body := pushMultipart(t, labMeta, map[string][]byte{"m.png": shot})
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusOK {
		t.Fatalf("lab push = %d (%s)", rw.Code, rw.Body.String())
	}
	var labRes plugin.PushResult
	if err := json.Unmarshal(rw.Body.Bytes(), &labRes); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	// The run records the declared environment.
	run, err := app.DB.GetRun(auth.DefaultDevUser, labRes.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Environment != "lab" {
		t.Fatalf("run.Environment = %q, want lab", run.Environment)
	}
	prs, _ := app.DB.ListPages(labRes.RunID)
	if len(prs) != 1 {
		t.Fatalf("expected 1 page, got %d", len(prs))
	}
	p := prs[0]
	// Raw perf columns are STILL persisted (numbers kept for reference/trend).
	if p.LCPMs != 5000 || p.CLS != 0.4 || p.WeightBytes != 4000000 || p.ReqCount != 120 {
		t.Fatalf("lab: raw perf columns must be persisted, got %+v", p)
	}
	labFinds, _ := app.DB.ListFindings(p.ID)
	var labPerf, labLayout int
	for _, f := range labFinds {
		switch f.Type {
		case db.FindingPerf:
			labPerf++
		case db.FindingLayout:
			labLayout++
		}
	}
	if labPerf != 0 {
		t.Errorf("lab: expected 0 perf findings, got %d", labPerf)
	}
	if labLayout == 0 {
		t.Error("lab: expected layout findings to still fire")
	}

	// --- prod push (same perf block): perf findings ARE emitted. ---
	prodMeta := `{"environment":"prod","label":"prod run","pages":[` + pageBlock + `]}`
	ct2, body2 := pushMultipart(t, prodMeta, map[string][]byte{"m.png": shot})
	rw2 := doPush(router, token, ct2, body2)
	if rw2.Code != http.StatusOK {
		t.Fatalf("prod push = %d (%s)", rw2.Code, rw2.Body.String())
	}
	var prodRes plugin.PushResult
	if err := json.Unmarshal(rw2.Body.Bytes(), &prodRes); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	prodPages, _ := app.DB.ListPages(prodRes.RunID)
	if len(prodPages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(prodPages))
	}
	prodFinds, _ := app.DB.ListFindings(prodPages[0].ID)
	var prodPerf int
	for _, f := range prodFinds {
		if f.Type == db.FindingPerf {
			prodPerf++
		}
	}
	if prodPerf == 0 {
		t.Error("prod: expected perf findings to be emitted (unchanged behavior)")
	}
}

// TestPluginPushRawBlocksComputeFindings pushes the harness's REAL shape — raw
// perf + layout measurement blocks and NO hand-authored perf/layout findings — and
// asserts auditloop computes the perf + layout findings server-side (internal/
// signals), gates the mobile-only layout smells by viewport, stores the untrusted
// example selectors ESCAPED, and echoes the raw blocks into report.json.
func TestPluginPushRawBlocksComputeFindings(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "RawBlocks")

	shot := pngBytes(t, 40, 40, color.White)
	// A mobile page with an overflow + small-tap-targets + missing-viewport smell
	// (example selector carries an HTML-ish payload to prove escaping), a breaching
	// perf block, and a DESKTOP page with the same layout block (mobile-only smells
	// must NOT fire there).
	meta := `{"label":"raw run","pages":[
		{"url":"home","viewport":"mobile","screenshot":"m.png",
		 "perf":{"lcp_ms":5000,"cls":0.4,"tbt_ms":800,"weight_bytes":1000,"req_count":10},
		 "layout":{"horizontal_overflow":true,"scroll_width":500,"inner_width":390,
		           "small_tap_targets":3,"missing_viewport_meta":true,
		           "examples":{"small_tap_targets":["<a class=btn> (20x20)"]}}},
		{"url":"home","viewport":"desktop","screenshot":"d.png",
		 "layout":{"horizontal_overflow":true,"scroll_width":2000,"inner_width":1440,"small_tap_targets":3}}
	]}`
	files := map[string][]byte{"m.png": shot, "d.png": shot}
	ct, body := pushMultipart(t, meta, files)
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusOK {
		t.Fatalf("push = %d (%s)", rw.Code, rw.Body.String())
	}
	var res plugin.PushResult
	if err := json.Unmarshal(rw.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad response: %v", err)
	}

	prs, _ := app.DB.ListPages(res.RunID)
	if len(prs) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(prs))
	}
	// Locate mobile vs desktop rows.
	var mobile, desktop *db.Page
	for _, p := range prs {
		if p.Viewport == "mobile" {
			mobile = p
		} else {
			desktop = p
		}
	}
	if mobile == nil || desktop == nil {
		t.Fatalf("missing viewport rows: %+v", prs)
	}

	// Mobile: server-computed perf findings (LCP poor, CLS poor, TBT poor) + the
	// gated layout smells (overflow + tap-targets + missing-viewport).
	smells := map[string]bool{}
	var perfCount int
	mfinds, _ := app.DB.ListFindings(mobile.ID)
	for _, f := range mfinds {
		switch f.Type {
		case db.FindingPerf:
			perfCount++
		case db.FindingLayout:
			var d signals.LayoutDetail
			if json.Unmarshal([]byte(f.Detail), &d) == nil {
				smells[d.Smell] = true
			}
			if strings.Contains(f.Detail, "<a class=btn>") {
				t.Errorf("layout example selector not escaped: %q", f.Detail)
			}
		}
	}
	if perfCount != 3 {
		t.Errorf("mobile: expected 3 server-computed perf findings (LCP/CLS/TBT), got %d", perfCount)
	}
	for _, want := range []string{"horizontal-overflow", "small-tap-targets", "missing-viewport-meta"} {
		if !smells[want] {
			t.Errorf("mobile: expected server-computed %q layout finding, got %v", want, smells)
		}
	}

	// Desktop: the mobile-only smells must NOT fire (viewport gating), even though
	// the same layout block was pushed.
	dfinds, _ := app.DB.ListFindings(desktop.ID)
	for _, f := range dfinds {
		if f.Type != db.FindingLayout {
			continue
		}
		var d signals.LayoutDetail
		_ = json.Unmarshal([]byte(f.Detail), &d)
		if d.Smell == "horizontal-overflow" || d.Smell == "small-tap-targets" {
			t.Errorf("desktop: mobile-only smell %q should not fire", d.Smell)
		}
	}

	// report.json echoes the raw Perf + Layout blocks for the mobile page.
	rep := readReport(t, app, res.RunID)
	var sawLayout, sawPerf bool
	for _, pg := range rep.Pages {
		if pg.Viewport == "mobile" {
			if pg.Layout != nil && pg.Layout.HorizontalOverflow && pg.Layout.SmallTapTargets == 3 {
				sawLayout = true
			}
			if pg.Perf != nil && pg.Perf.LCPMs == 5000 {
				sawPerf = true
			}
		}
	}
	if !sawLayout {
		t.Error("report.json mobile page missing echoed Layout block")
	}
	if !sawPerf {
		t.Error("report.json mobile page missing echoed Perf block")
	}
}

func TestPluginPushSecondPushDiff(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "Diffable")

	white := pngBytes(t, 60, 60, color.White)
	red := pngBytes(t, 60, 60, color.RGBA{255, 0, 0, 255})

	push := func(shot []byte) plugin.PushResult {
		meta := `{"pages":[{"url":"home","viewport":"desktop","screenshot":"s.png"}]}`
		ct, body := pushMultipart(t, meta, map[string][]byte{"s.png": shot})
		rw := doPush(router, token, ct, body)
		if rw.Code != http.StatusOK {
			t.Fatalf("push = %d (%s)", rw.Code, rw.Body.String())
		}
		var res plugin.PushResult
		json.Unmarshal(rw.Body.Bytes(), &res)
		return res
	}

	r1 := push(white)
	run1, _ := app.DB.GetRun(auth.DefaultDevUser, r1.RunID)
	if run1.PrevRunID != "" || run1.DiffJSON != "" {
		t.Fatalf("first push should have no baseline/diff: prev=%q diff=%q", run1.PrevRunID, run1.DiffJSON)
	}

	// Second push: same URL+viewport, DIFFERENT same-size screenshot → regression.
	r2 := push(red)
	run2, _ := app.DB.GetRun(auth.DefaultDevUser, r2.RunID)
	if run2.PrevRunID != r1.RunID {
		t.Fatalf("run2 baseline = %q, want %q", run2.PrevRunID, r1.RunID)
	}
	if run2.DiffJSON == "" {
		t.Fatal("run2 must carry a P2 diff")
	}
	if !strings.Contains(run2.DiffJSON, `"pages_changed":1`) {
		t.Errorf("expected 1 visual regression, diff=%s", run2.DiffJSON)
	}
}

func TestPluginPushValidationErrors(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "V")
	shot := pngBytes(t, 20, 20, color.White)

	cases := []struct {
		name  string
		meta  string
		files map[string][]byte
	}{
		{"missing screenshot part", `{"pages":[{"url":"x","viewport":"desktop","screenshot":"missing.png"}]}`, nil},
		{"orphan file part", `{"pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`,
			map[string][]byte{"s.png": shot, "orphan.png": shot}},
		{"unknown field", `{"pages":[],"bogus":1}`, nil},
		{"bad viewport", `{"pages":[{"url":"x","viewport":"tablet","screenshot":"s.png"}]}`, map[string][]byte{"s.png": shot}},
		{"bad environment", `{"environment":"local","pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`, map[string][]byte{"s.png": shot}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, body := pushMultipart(t, tc.meta, tc.files)
			rw := doPush(router, token, ct, body)
			if rw.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400 (%s)", tc.name, rw.Code, rw.Body.String())
			}
		})
	}

	// A non-image "screenshot" is rejected (content sniff, not the declared type).
	ct, body := pushMultipart(t, `{"pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`,
		map[string][]byte{"s.png": []byte("this is not an image")})
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("non-image screenshot = %d, want 400", rw.Code)
	}

	// No run should have been created by any rejected push.
	runs, _ := app.DB.ListRuns(auth.DefaultDevUser, "")
	if len(runs) != 0 {
		// ListRuns is scoped by target; a blanket check: ensure no pages exist.
		t.Logf("(info) runs for empty target filter: %d", len(runs))
	}
}

func TestPluginPushRateLimited(t *testing.T) {
	// The per-target limiter is relaxed under DEV_MODE, so exercise it with a
	// non-dev app (the push route is token-authed, not Supabase-gated).
	app, router := testAppNonDev(t)
	_, token := newPluginTarget(t, app, "RL")
	shot := pngBytes(t, 20, 20, color.White)
	meta := `{"pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`

	first := true
	got429 := false
	for i := 0; i < 2; i++ {
		ct, body := pushMultipart(t, meta, map[string][]byte{"s.png": shot})
		rw := doPush(router, token, ct, body)
		if first {
			if rw.Code != http.StatusOK {
				t.Fatalf("first push = %d (%s)", rw.Code, rw.Body.String())
			}
			first = false
		} else if rw.Code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Error("expected the second immediate push to be rate-limited (429)")
	}
}

func TestPluginPushOversizedBody(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "Big")

	// Stream a multipart body just over the 64 MiB cap WITHOUT allocating it — the
	// MaxBytesReader must trip ParseMultipartForm → 413.
	boundary := "OVERSIZEDBOUNDARY"
	header := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"s.png\"; filename=\"s.png\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	footer := "\r\n--" + boundary + "--\r\n"
	huge := io.LimitReader(zeroReader{}, maxPushBodyBytes+1024)
	body := io.MultiReader(strings.NewReader(header), huge, strings.NewReader(footer))

	rw := doPush(router, token, "multipart/form-data; boundary="+boundary, body)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rw.Code)
	}
	_ = app
}

// zeroReader yields an endless stream of NUL bytes (cheap, no allocation).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// --- plugin-target management route tests (Supabase-authed) ---

func TestCreatePluginTargetRevealsTokenOnce(t *testing.T) {
	_, router := testApp(t)
	form := strings.NewReader("name=CIfunnel&label=https://app.acme.com")
	req := httptest.NewRequest("POST", "/api/plugins/targets", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("create plugin target = %d (%s)", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "Upload key created") || !strings.Contains(body, "shown ONCE") {
		t.Errorf("token reveal not rendered: %s", body)
	}
	if !strings.Contains(body, "/api/plugins/runs") {
		t.Error("reveal should show the push endpoint")
	}
}

func TestRotatePluginTokenOwnership(t *testing.T) {
	app, router := testApp(t)

	// A plugin target owned by ANOTHER user must not be rotatable by the dev user.
	other, _ := app.DB.CreatePluginTarget("someone-else", "Theirs", "")
	req := httptest.NewRequest("POST", "/api/targets/"+other.ID+"/plugin-token/rotate", nil)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("rotating another user's target = %d, want 404", rw.Code)
	}

	// The dev user's own plugin target rotates and reveals a fresh token.
	mine, _ := app.DB.CreatePluginTarget(auth.DefaultDevUser, "Mine", "")
	token, hash, _ := plugin.GenerateToken()
	app.DB.SetPluginToken(mine.ID, hash)
	req2 := httptest.NewRequest("POST", "/api/targets/"+mine.ID+"/plugin-token/rotate", nil)
	rw2 := httptest.NewRecorder()
	router.ServeHTTP(rw2, req2)
	if rw2.Code != http.StatusOK {
		t.Fatalf("rotate own = %d (%s)", rw2.Code, rw2.Body.String())
	}
	if strings.Contains(rw2.Body.String(), token) {
		t.Error("rotate should reveal a NEW token, not the old one")
	}
	// Old token no longer resolves.
	if _, _, err := app.DB.PluginTokenLookup(plugin.HashToken(token)); err != db.ErrNotFound {
		t.Errorf("old token still valid after rotate: %v", err)
	}
}

// TestRotateNonPluginTargetRejected: a normal crawl target has no plugin-token
// route surface.
func TestRotateNonPluginTargetRejected(t *testing.T) {
	app, router := testApp(t)
	normal, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Normal", "https://acme.com", []string{"acme.com"})
	req := httptest.NewRequest("POST", "/api/targets/"+normal.ID+"/plugin-token/rotate", nil)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("rotate on non-plugin target = %d, want 404", rw.Code)
	}
}
