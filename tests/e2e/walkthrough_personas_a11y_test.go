// Package e2e — Phase 2 DRIVEN-PATH DOM/a11y grounding. This is the thing Phase 1
// (crawl-path grounding, #35) could NOT do: make the persona evaluator's deterministic
// dropContradicted gate fire on a DRIVEN walkthrough trace. It drives a fixture whose
// landing page carries an sr-only <label for>-associated input + an <a> card with the
// real chromedp Drive loop (a scripted planner), persists the trace WITH the per-step
// a11y digest, materializes the synthetic run via the evaluate-walkthrough route, then
// asserts: (a) the synthetic pages carry an a11y_digest_key (the digest was captured on
// the driven path and carried through), and (b) the seeded objective a11y FP is DROPPED
// on the page whose digest refutes it while KEPT on a page whose digest cannot speak to
// it, and the SUBJECTIVE finding survives on both — the non-vacuous proof that the driven
// grounding drives the drop rather than blanket-discarding findings.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

func TestEndToEndWalkthroughPersonasDOMGrounding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	// The landing page carries #signup-email (sr-only <label for>) + #client-card-1 (<a>
	// card); /about carries neither. Reused from the Phase-1 crawl-grounding e2e.
	fixture := a11yEvalFixture()
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	var sawSemantic atomic.Bool
	or := fakeOpenRouterEvalA11y(t, &sawSemantic)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-wpe-a11y.db"), S3Local: filepath.Join(tmp, "artifacts"),
		CrawlMaxPages: 10, CrawlMaxDepth: 2, CrawlAllowLoopback: true,
		ChromiumPath: chromium, DevMode: true,
		OpenRouterAPIKey:  "dummy-key",
		OpenRouterBaseURL: or.URL,
		LLMModels:         []string{"anthropic/claude-haiku-4.5"},
		LLMMaxTokens:      512,
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

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "A11yDrivenFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := database.SetDrivingConfig(auth.DefaultDevUser, tgt.ID, true, false); err != nil {
		t.Fatalf("driving config: %v", err)
	}
	if err := database.SetTargetAuditConfig(&db.TargetAuditConfig{
		TargetID: tgt.ID, PrimaryJob: "create a campaign", SuccessURLContains: "/about",
		SuccessTimeoutMs: 4000, Confirmed: true,
	}); err != nil {
		t.Fatalf("audit config: %v", err)
	}

	// Drive: land on "/" (digest captures the sr-only label + <a> card), navigate /about
	// (success). Real chromedp Drive → each step now carries A11yDigestJSON.
	planner := &scriptedPlanner{actions: []action.Action{
		{Type: action.Navigate, URL: fixture.URL + "/about", Reason: "go to about"},
	}}
	trace, err := crawler.Drive(context.Background(), crawler.DriveOptions{
		BaseURL:        fixture.URL + "/",
		AllowedHosts:   []string{fixtureHost},
		Goal:           "create a campaign",
		Success:        action.SuccessAssertion{URLContains: "/about", TimeoutMs: 4000},
		Planner:        planner,
		MaxActions:     6,
		ActionTimeout:  15 * time.Second,
		OverallTimeout: 60 * time.Second,
		DryRun:         true,
		AllowLoopback:  true,
		ChromiumPath:   chromium,
	})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if trace.Outcome != "success" || len(trace.Steps) < 2 {
		t.Fatalf("drive outcome=%q steps=%d, want success with >=2 steps", trace.Outcome, len(trace.Steps))
	}

	// At least one driven step must have captured a non-empty digest (the landing page).
	var sawStepDigest bool
	for _, s := range trace.Steps {
		if len(s.A11yDigestJSON) > 0 {
			sawStepDigest = true
		}
	}
	if !sawStepDigest {
		t.Fatal("no driven step captured an a11y digest — Phase-2 capture did not run")
	}

	// Persist the trace as a completed walkthrough, carrying the per-step digest.
	wk, err := database.CreateWalkthrough(tgt.ID, "", "create a campaign", true)
	if err != nil {
		t.Fatalf("create walkthrough: %v", err)
	}
	for _, s := range trace.Steps {
		shotKey := ""
		if len(s.ScreenshotPNG) > 0 {
			shotKey = storage.WalkthroughStepKey(tgt.ID, wk.ID, s.Idx)
			if err := store.Put(context.Background(), shotKey, "image/png", bytes.NewReader(s.ScreenshotPNG), int64(len(s.ScreenshotPNG))); err != nil {
				t.Fatalf("put shot: %v", err)
			}
		}
		aj, _ := json.Marshal(s.Action)
		if _, err := database.InsertWalkthroughStep(&db.WalkthroughStep{
			WalkthroughID: wk.ID, Idx: s.Idx, ActionJSON: string(aj),
			URL: s.URL, ScreenshotKey: shotKey, Outcome: s.Outcome, PlannerReason: s.PlannerReason,
			DigestJSON: string(s.A11yDigestJSON),
		}); err != nil {
			t.Fatalf("insert step: %v", err)
		}
	}
	if err := database.FinishWalkthrough(wk.ID, db.WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish walkthrough: %v", err)
	}

	// Evaluate the driven trace (materializes the synthetic run + carries the digest into
	// its pages' a11y_digest_key).
	form := url.Values{"personas": {"accessibility-constrained"}}
	req, _ := http.NewRequest("POST", appSrv.URL+"/api/targets/"+tgt.ID+"/walkthroughs/"+wk.ID+"/evaluate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("evaluate status = %d", resp.StatusCode)
	}

	got, _ := database.GetWalkthroughByID(wk.ID)
	if got.RunID == "" {
		t.Fatal("walkthrough not linked to a synthetic run")
	}
	runID := got.RunID

	// (a) The synthetic pages carry the driven-path a11y digest key.
	pages, _ := database.ListPages(runID)
	pageByID := map[string]*db.Page{}
	var landingPage, aboutPage *db.Page
	var anyDigestKey bool
	for _, p := range pages {
		pageByID[p.ID] = p
		if p.A11yDigestKey != "" {
			anyDigestKey = true
		}
		if strings.Contains(p.URL, "/about") {
			aboutPage = p
		} else {
			landingPage = p
		}
	}
	if !anyDigestKey {
		t.Fatal("no synthetic page carries an a11y_digest_key — the driven digest was not materialized")
	}
	if landingPage == nil || aboutPage == nil {
		t.Fatalf("expected a landing + an /about synthetic page, got %d pages", len(pages))
	}
	if landingPage.A11yDigestKey == "" {
		t.Error("the landing synthetic page (with the sr-only label) has no a11y_digest_key")
	}

	// Poll eval-status until done.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := database.GetRunByID(runID)
		if r.EvalStatus == db.EvalDone || r.EvalStatus == db.EvalFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	final, _ := database.GetRunByID(runID)
	if final.EvalStatus != db.EvalDone {
		t.Fatalf("eval status = %q, want done", final.EvalStatus)
	}

	// (b) NON-VACUOUS: the "no accessible label" FP is DROPPED on the landing page (whose
	// digest resolves #signup-email's sr-only <label for>) but KEPT on the /about page
	// (whose digest does not contain that selector). This is exactly what Phase 1 could
	// not do on the driven path.
	rows, _ := database.ListPageEvaluations(runID)
	evalByPage := map[string]*db.PageEvaluation{}
	for _, r := range rows {
		evalByPage[r.PageID] = r
	}
	landingEval := evalByPage[landingPage.ID]
	aboutEval := evalByPage[aboutPage.ID]
	if landingEval == nil || aboutEval == nil {
		t.Fatalf("missing evaluations: landing=%v about=%v", landingEval != nil, aboutEval != nil)
	}
	// Each assertion names its own finding in full — a bare "no accessible label"
	// substring also matches other seeded findings, so a partial key silently stops
	// testing the finding its message names.
	if strings.Contains(landingEval.FindingsJSON, "Email input has no accessible label") {
		t.Error("the 'no label' FP was NOT dropped on the DRIVEN landing page by the REFUTATION stage (digest refutes it)")
	}
	if strings.Contains(landingEval.FindingsJSON, "not keyboard-operable") {
		t.Error("the 'not keyboard-operable' FP was NOT dropped on the DRIVEN landing page (<a> card)")
	}
	// NON-VACUOUS: the same #signup-email claim is KEPT on /about. That page has no
	// interactive elements or form controls at all, so its digest carries ONLY landmarks
	// — an EMPTY selector vocabulary, which selector grounding treats as incomplete and
	// which therefore licenses no drop (a page that lists nothing cannot testify that an
	// element is absent). So the drop on the landing page is caused by that page's
	// digest, not by anything page-independent.
	if !strings.Contains(aboutEval.FindingsJSON, "Email input has no accessible label") {
		t.Error("non-vacuous check failed: the FP should be KEPT on the /about page, whose digest has no selector vocabulary")
	}
	for name, ev := range map[string]*db.PageEvaluation{"landing": landingEval, "about": aboutEval} {
		if !strings.Contains(ev.FindingsJSON, "value proposition is unclear") {
			t.Errorf("non-vacuous check failed: the SUBJECTIVE finding must survive on the %s page", name)
		}
	}
	// SELECTOR GROUNDING on the DRIVEN path: the claim citing a selector the landing
	// page's digest does not list is dropped there (real vocabulary) and KEPT on /about
	// (landmarks-only ⇒ empty vocabulary ⇒ the gate may not drop). The pair is what makes
	// this a test of the digest's content rather than of a blanket rule.
	if strings.Contains(landingEval.FindingsJSON, ungroundedA11yIssue) {
		t.Error("the invented-anchor a11y claim was NOT dropped on the driven landing page by the GROUNDING stage")
	}
	if !strings.Contains(aboutEval.FindingsJSON, ungroundedA11yIssue) {
		t.Error("the invented-anchor claim must be KEPT on /about, whose digest has no selector vocabulary")
	}

	t.Logf("e2e driven DOM-grounding OK: landingKey=%s", landingPage.A11yDigestKey)
}
