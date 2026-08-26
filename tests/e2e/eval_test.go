package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/apikey"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
)

// fakeOpenRouterEval serves canned STRUCTURED persona-walkthrough JSON keyed off
// the system prompt (generation / verification / synthesis) and counts image parts
// (proving both viewports are sent to the vision model).
func fakeOpenRouterEval(t *testing.T, imageParts *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		system := ""
		for _, m := range req.Messages {
			for _, part := range m.Content {
				if part.Type == "image_url" {
					atomic.AddInt64(imageParts, 1)
				}
			}
			if m.Role == "system" && len(m.Content) > 0 {
				system = m.Content[0].Text
			}
		}
		content := `{"comprehension":"unclear","blockers":[{"issue":"unclear primary action","selector":"header","evidence":"no obvious CTA"}],"frictions":[{"issue":"dense copy","selector":"main","evidence":"a lot of text"}],"top_fix":{"selector":"header .cta","change":"add a prominent primary button","rationale":"orient the visitor","impact":"high"}}`
		switch {
		case strings.Contains(system, "fact-checker"):
			content = `{"comprehension":"unclear","blockers":[{"issue":"unclear primary action","selector":"header","evidence":"no obvious CTA","verified":true}],"frictions":[],"top_fix":{"selector":"header .cta","change":"add a prominent primary button","rationale":"orient the visitor","impact":"high"}}`
		case strings.Contains(system, "product lead synthesizing"):
			content = `{"improvements":[{"title":"Make the primary action obvious on every page","rationale":"visitors cannot tell what to do first","impact":"high","affected_personas":["first-time-nontechnical"]}]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

// TestEndToEndPersonaEvaluation (Phase 1) crawls the fixture, runs a two-persona
// walkthrough against a FAKE OpenRouter (no real key), and asserts: one row per
// (page,persona), both viewports sent, the structured findings + synthesis render
// in the UI, and the read-API /evaluation returns them owner-scoped.
func TestEndToEndPersonaEvaluation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	var mutated atomic.Bool
	fixture := mutableFixtureSite(&mutated)
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	var imageParts int64
	or := fakeOpenRouterEval(t, &imageParts)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-eval.db"), S3Local: filepath.Join(tmp, "artifacts"),
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

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "EvalFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	run := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run.Status != db.RunDone {
		t.Fatalf("run did not complete: %s / %s", run.Status, run.Error)
	}
	pages, _ := database.ListPages(run.ID)
	urlSet := map[string]bool{}
	for _, p := range pages {
		urlSet[p.URL] = true
	}
	numURLs := len(urlSet)
	if numURLs < 1 {
		t.Fatal("no pages crawled")
	}

	// Trigger the two-persona walkthrough THROUGH the HTTP API.
	personas := []string{"first-time-nontechnical", "skeptical-evaluator"}
	form := url.Values{"personas": personas, "job": {"sign up and reach the second page"}, "verify": {"1"}}
	req, _ := http.NewRequest("POST", appSrv.URL+"/api/runs/"+run.ID+"/evaluate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger eval: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eval trigger status = %d", resp.StatusCode)
	}

	// Poll eval-status until done.
	deadline := time.Now().Add(60 * time.Second)
	var got *db.Run
	for time.Now().Before(deadline) {
		got, _ = database.GetRunByID(run.ID)
		if got.EvalStatus == db.EvalDone || got.EvalStatus == db.EvalFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got == nil || got.EvalStatus != db.EvalDone {
		t.Fatalf("eval job did not complete: status=%v", statusOfEval(got))
	}

	// One row per (page, persona).
	rows, _ := database.ListPageEvaluations(run.ID)
	wantRows := numURLs * len(personas)
	if len(rows) != wantRows {
		t.Fatalf("expected %d evaluation rows (%d URLs × %d personas), got %d", wantRows, numURLs, len(personas), len(rows))
	}
	for _, r := range rows {
		if r.Error != "" || r.FindingsJSON == "" {
			t.Errorf("row %s/%s should be clean: err=%q", r.PageID, r.Persona, r.Error)
		}
	}
	// Both viewports sent per (gen + verify) call: >= 2 images each, 2 vision calls
	// per cell → >= 4 image parts per cell.
	if got := atomic.LoadInt64(&imageParts); got < int64(4*wantRows) {
		t.Errorf("image parts sent = %d, want >= %d (2 viewports × gen+verify per cell)", got, 4*wantRows)
	}

	// Synthesis persisted.
	if strings.TrimSpace(got.EvalSynthesisJSON) == "" {
		t.Error("synthesis story was not persisted")
	}

	// The run view renders the persona-walkthrough section + structured content.
	bodyStr := getBody(t, appSrv.URL+"/runs/"+run.ID)
	if !strings.Contains(bodyStr, "Persona walkthrough") {
		t.Error("run view did not render the persona-walkthrough section")
	}
	if !strings.Contains(bodyStr, "Top improvements across the flow") {
		t.Error("run view did not render the synthesis story")
	}
	if !strings.Contains(bodyStr, "unclear primary action") {
		t.Error("run view did not render the structured blocker")
	}

	// The read-API /evaluation returns the machine layer, owner-scoped.
	token, hash, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAPIKey(auth.DefaultDevUser, "e2e", hash, db.ScopeRead); err != nil {
		t.Fatal(err)
	}
	apiReq, _ := http.NewRequest("GET", appSrv.URL+"/api/audit/runs/"+run.ID+"/evaluation", nil)
	apiReq.Header.Set("Authorization", "Bearer "+token)
	apiResp, err := http.DefaultClient.Do(apiReq)
	if err != nil {
		t.Fatalf("read-api evaluation: %v", err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("read-api evaluation status = %d", apiResp.StatusCode)
	}
	var payload struct {
		Pages []struct {
			Persona    string `json:"persona"`
			Evaluation *struct {
				Comprehension string `json:"comprehension"`
			} `json:"evaluation"`
		} `json:"pages"`
		Synthesis []struct {
			Title string `json:"title"`
		} `json:"synthesis"`
	}
	if err := json.NewDecoder(apiResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode read-api evaluation: %v", err)
	}
	if len(payload.Pages) != wantRows {
		t.Errorf("read-api returned %d page evaluations, want %d", len(payload.Pages), wantRows)
	}
	if len(payload.Synthesis) == 0 {
		t.Error("read-api returned no synthesis")
	}

	t.Logf("e2e eval OK: urls=%d personas=%d rows=%d imageParts=%d", numURLs, len(personas), len(rows), atomic.LoadInt64(&imageParts))
}

func statusOfEval(r *db.Run) string {
	if r == nil {
		return "nil"
	}
	return r.EvalStatus
}

// fakeOpenRouterInfer serves a canned audit-config draft for the Phase-2 goal
// inference call (system prompt: "product analyst") and counts image parts (proving
// the landing screenshot is sent).
func fakeOpenRouterInfer(t *testing.T, imageParts *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			for _, part := range m.Content {
				if part.Type == "image_url" {
					atomic.AddInt64(imageParts, 1)
				}
			}
		}
		content := `{"product_summary":"A demo fixture site","primary_job":"reach the second page and sign up","primary_cta":"Sign up","audiences":["skeptical-evaluator","returning-power-user"]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

// TestEndToEndGoalInference (Phase 2) crawls the fixture, INFERS a draft audit
// config from the completed run against a FAKE OpenRouter, asserts the draft is
// stored + the card renders, CONFIRMS/saves it, and asserts the run-view evaluate
// form pre-fills its job + personas from the confirmed config.
func TestEndToEndGoalInference(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	var mutated atomic.Bool
	fixture := mutableFixtureSite(&mutated)
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	var imageParts int64
	or := fakeOpenRouterInfer(t, &imageParts)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-infer.db"), S3Local: filepath.Join(tmp, "artifacts"),
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

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "InferFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	run := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run.Status != db.RunDone {
		t.Fatalf("run did not complete: %s / %s", run.Status, run.Error)
	}

	// Infer the config from the completed run through the HTTP API.
	inferReq, _ := http.NewRequest("POST", appSrv.URL+"/api/targets/"+tgt.ID+"/audit-config/infer", nil)
	inferResp, err := http.DefaultClient.Do(inferReq)
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	inferBody, _ := io.ReadAll(inferResp.Body)
	inferResp.Body.Close()
	if inferResp.StatusCode != http.StatusOK {
		t.Fatalf("infer status = %d (%s)", inferResp.StatusCode, inferBody)
	}
	if !strings.Contains(string(inferBody), "reach the second page and sign up") {
		t.Error("inferred card did not pre-fill the drafted job")
	}
	if atomic.LoadInt64(&imageParts) < 1 {
		t.Error("inference did not send the landing screenshot")
	}

	cfgRow, found, err := database.GetTargetAuditConfig(auth.DefaultDevUser, tgt.ID)
	if err != nil || !found {
		t.Fatalf("inferred config not stored: found=%v err=%v", found, err)
	}
	if !cfgRow.Inferred || cfgRow.Confirmed {
		t.Errorf("draft flags: inferred=%v confirmed=%v", cfgRow.Inferred, cfgRow.Confirmed)
	}
	if len(cfgRow.Personas) != 2 {
		t.Errorf("inferred personas = %v, want 2 (allowlisted)", cfgRow.Personas)
	}

	// The target page renders the Audit configuration card.
	if body := getBody(t, appSrv.URL+"/targets/"+tgt.ID); !strings.Contains(body, "Audit configuration") {
		t.Error("target page did not render the Audit configuration card")
	}

	// Confirm/save the config (distinct job + a single non-default persona).
	saveForm := url.Values{
		"product_summary": {"A demo fixture site"},
		"primary_job":     {"complete signup on step two"},
		"primary_cta":     {"Sign up"},
		"personas":        {"returning-power-user"},
	}
	saveReq, _ := http.NewRequest("POST", appSrv.URL+"/api/targets/"+tgt.ID+"/audit-config", strings.NewReader(saveForm.Encode()))
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveResp, err := http.DefaultClient.Do(saveReq)
	if err != nil {
		t.Fatalf("save config: %v", err)
	}
	saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("save config status = %d", saveResp.StatusCode)
	}
	cfgRow, _, _ = database.GetTargetAuditConfig(auth.DefaultDevUser, tgt.ID)
	if !cfgRow.Confirmed || cfgRow.PrimaryJob != "complete signup on step two" {
		t.Fatalf("config not confirmed/saved: %+v", cfgRow)
	}

	// The run-view evaluate form pre-fills the confirmed job + persona.
	runBody := getBody(t, appSrv.URL+"/runs/"+run.ID)
	if !strings.Contains(runBody, "complete signup on step two") {
		t.Error("evaluate form did not pre-fill the job from the confirmed config")
	}

	t.Logf("e2e goal-inference OK: imageParts=%d personas=%v", atomic.LoadInt64(&imageParts), cfgRow.Personas)
}
