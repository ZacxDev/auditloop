// Package e2e: Phase-3 PR-B — feed a DRIVEN walkthrough trace into the Phase-1 persona
// evaluator via the synthetic-run reuse path. It drives the funnel fixture with the REAL
// chromedp crawler.Drive loop + a SCRIPTED fake Planner (deterministic, no planner LLM),
// persists the trace as a completed walkthrough, then POSTs the evaluate-walkthrough
// route against a FAKE OpenRouter and asserts: a synthetic run + one page per step
// (ordered) materialized, one persona-evaluation row per step × persona, the findings +
// synthesis render in the walkthrough UI, and the read-API serves the evaluation
// owner-scoped.
//
// Drives real headless Chromium (chromedp) for the trace capture — needs a binary.
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
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/apikey"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

func TestEndToEndWalkthroughPersonas(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fx := funnelFixture()
	defer fx.Close()
	host := hostOnly(fx.URL)

	or := fakeOpenRouterEval(t, new(int64))
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-wpe.db"), S3Local: filepath.Join(tmp, "artifacts"),
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

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "FunnelSite", fx.URL, []string{host})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	// Driving-enabled so the walkthrough section (with the embedded eval control) renders.
	if err := database.SetDrivingConfig(auth.DefaultDevUser, tgt.ID, true, false); err != nil {
		t.Fatalf("driving config: %v", err)
	}
	if err := database.SetTargetAuditConfig(&db.TargetAuditConfig{
		TargetID: tgt.ID, PrimaryJob: "create an account", SuccessURLContains: "/welcome",
		SuccessTimeoutMs: 4000, Confirmed: true,
	}); err != nil {
		t.Fatalf("audit config: %v", err)
	}

	// --- Drive the funnel with a SCRIPTED planner (deterministic, no planner LLM). ---
	planner := &scriptedPlanner{actions: []action.Action{
		{Type: action.Navigate, URL: fx.URL + "/form", Reason: "go to signup"},
		{Type: action.TypeText, Selector: "#email", Text: "alice@example.com", Reason: "enter email"},
		{Type: action.Click, Selector: "#submit", Reason: "submit"},
	}}
	trace, err := crawler.Drive(context.Background(), crawler.DriveOptions{
		BaseURL:        fx.URL + "/",
		AllowedHosts:   []string{host},
		Goal:           "create an account",
		Success:        action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 4000},
		Planner:        planner,
		MaxActions:     10,
		ActionTimeout:  15 * time.Second,
		OverallTimeout: 60 * time.Second,
		DryRun:         false,
		AllowLoopback:  true,
		ChromiumPath:   chromium,
	})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if trace.Outcome != "success" || len(trace.Steps) < 2 {
		t.Fatalf("drive outcome=%q steps=%d, want success with >=2 steps", trace.Outcome, len(trace.Steps))
	}

	// --- Persist the trace as a completed walkthrough (mirrors the driver generator). ---
	wk, err := database.CreateWalkthrough(tgt.ID, "", "create an account", true)
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
		}); err != nil {
			t.Fatalf("insert step: %v", err)
		}
	}
	if err := database.FinishWalkthrough(wk.ID, db.WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatalf("finish walkthrough: %v", err)
	}
	nSteps := len(trace.Steps)

	// --- Evaluate the driven trace with 2 personas via the HTTP route. ---
	personas := []string{"first-time-nontechnical", "skeptical-evaluator"}
	form := url.Values{"personas": personas}
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

	// The synthetic run was materialized + linked.
	got, _ := database.GetWalkthroughByID(wk.ID)
	if got.RunID == "" {
		t.Fatal("walkthrough not linked to a synthetic run")
	}
	runID := got.RunID
	run, err := database.GetRunByID(runID)
	if err != nil || run.Trigger != "walkthrough" || run.Status != db.RunDone {
		t.Fatalf("synthetic run wrong: %+v err=%v", run, err)
	}
	// One page per step, in order (labels sort by the zero-padded index).
	pgs, _ := database.ListPages(runID)
	if len(pgs) != nSteps {
		t.Fatalf("synthetic pages = %d, want %d (one per step)", len(pgs), nSteps)
	}
	if !strings.HasPrefix(pgs[0].URL, "01 · ") {
		t.Fatalf("first synthetic page label = %q, want a '01 · ' prefix", pgs[0].URL)
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

	// One evaluation row per step × persona.
	rows, _ := database.ListPageEvaluations(runID)
	want := nSteps * len(personas)
	if len(rows) != want {
		t.Fatalf("evaluation rows = %d, want %d (%d steps × %d personas)", len(rows), want, nSteps, len(personas))
	}
	if strings.TrimSpace(final.EvalSynthesisJSON) == "" {
		t.Error("synthesis story not persisted for the walkthrough evaluation")
	}

	// The walkthrough UI (target view) renders the embedded persona evaluation.
	body := getBody(t, appSrv.URL+"/targets/"+tgt.ID)
	if !strings.Contains(body, "Persona walkthrough") {
		t.Error("target view did not render the embedded persona-walkthrough section")
	}
	if !strings.Contains(body, "Top improvements across the flow") {
		t.Error("target view did not render the synthesis story")
	}
	if !strings.Contains(body, "unclear primary action") {
		t.Error("target view did not render the structured persona finding")
	}

	// Read-API: the synthetic run's evaluation, owner-scoped.
	token, hash, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAPIKey(auth.DefaultDevUser, "e2e", hash, db.ScopeRead); err != nil {
		t.Fatal(err)
	}
	apiReq, _ := http.NewRequest("GET", appSrv.URL+"/api/audit/runs/"+runID+"/evaluation", nil)
	apiReq.Header.Set("Authorization", "Bearer "+token)
	apiResp, err := http.DefaultClient.Do(apiReq)
	if err != nil {
		t.Fatalf("read-api: %v", err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("read-api evaluation status = %d", apiResp.StatusCode)
	}
	var payload struct {
		Pages []struct {
			Persona string `json:"persona"`
		} `json:"pages"`
	}
	if err := json.NewDecoder(apiResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode read-api: %v", err)
	}
	if len(payload.Pages) != want {
		t.Errorf("read-api returned %d page evaluations, want %d", len(payload.Pages), want)
	}

	t.Logf("e2e walkthrough-personas OK: steps=%d personas=%d rows=%d", nSteps, len(personas), len(rows))
}
