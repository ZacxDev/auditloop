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
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
)

// a11yEvalFixture serves a home page with the two semantic elements that produced the
// meta-audit's objective false positives: an sr-only <label for>-associated input
// (#signup-email — a real label invisible in a screenshot, FP #11) and an <a> styled
// as a card (#client-card-1 — a focusable link the model mistook for a non-operable
// div, FP #1). Both are anchored to an existing page so the crawl graph stays small.
func a11yEvalFixture() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Home</title>
<style>.sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0,0,0,0)}</style></head>
<body>
<h1>Create a campaign</h1>
<form>
  <label for="signup-email" class="sr-only">Email address</label>
  <input id="signup-email" name="email" type="email">
</form>
<a id="client-card-1" class="client-card" href="/about">Open client Acme</a>
</body></html>`)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>About</title></head><body><h1>About</h1><p>Hello.</p></body></html>`)
	})
	return httptest.NewServer(mux)
}

// fakeOpenRouterEvalA11y returns canned findings that INCLUDE two objective a11y
// false positives anchored to the fixture's real selectors — a "no label" blocker on
// #signup-email and a "not keyboard-operable" blocker on #client-card-1 — plus one
// subjective finding. The verification pass echoes them back verified (proving the
// LLM verify does NOT drop them, exactly like the meta-audit). It also records whether
// the generation prompt carried the authoritative SEMANTIC STRUCTURE block.
// ungroundedA11yIssue is a mechanical a11y claim anchored to a selector the captured
// digest does not list — the pixel-invented anchor class. Selector grounding
// (internal/eval/ground.go) must drop it wherever the digest has a real vocabulary,
// and must NOT drop it where the digest has none (a landmarks-only page).
//
// 🔴 Its wording must NOT contain, or be contained by, any other seeded finding's
// assertion substring. The first version read "Promo widget has no accessible label",
// which CONTAINS the "no accessible label" that the /about non-vacuity check below
// keys on — so that check passed on THIS finding's presence and stopped proving
// anything about the #signup-email one it names. Keep these strings disjoint.
const ungroundedA11yIssue = "Promo widget lacks a label for screen readers"

func fakeOpenRouterEvalA11y(t *testing.T, sawSemantic *atomic.Bool) *httptest.Server {
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
		system, userText := "", ""
		for _, m := range req.Messages {
			for _, part := range m.Content {
				if m.Role == "system" && part.Type == "text" {
					system = part.Text
				}
				if m.Role == "user" && part.Type == "text" {
					userText += part.Text
				}
			}
		}
		// The gen/verify prompts must carry the authoritative DOM digest for THIS page.
		if strings.Contains(userText, "SEMANTIC STRUCTURE") && strings.Contains(userText, "input#signup-email") {
			sawSemantic.Store(true)
		}

		// Two objective a11y FPs anchored to real selectors (the digest REFUTES these),
		// one anchored to a selector the digest does not list at all (the model inventing
		// an anchor from pixels — SELECTOR GROUNDING must drop it), and one subjective
		// finding that must survive every stage.
		const findings = `"blockers":[` +
			`{"issue":"Email input has no accessible label for screen readers","selector":"#signup-email","evidence":"no associated label element"%s},` +
			`{"issue":"Client cards are not keyboard-operable","selector":"#client-card-1","evidence":"looks like a div"%s},` +
			`{"issue":"` + ungroundedA11yIssue + `","selector":"div.promo-widget:contains('Get started')","evidence":"no label element"%s},` +
			`{"issue":"The value proposition is unclear for a first-time visitor","selector":"main","evidence":"dense copy"%s}],"frictions":[]`
		content := `{"comprehension":"unclear",` + strings.Replace(findings, "%s", "", -1) +
			`,"top_fix":{"selector":"h1","change":"clarify the headline","rationale":"orient the visitor","impact":"high"}}`
		switch {
		case strings.Contains(system, "fact-checker"):
			// Verify echoes ALL three back as verified — the LLM verify keeps the FPs
			// (re-reading pixels). The DETERMINISTIC gate is what must drop them.
			content = `{"comprehension":"unclear",` + strings.Replace(findings, "%s", `,"verified":true`, -1) +
				`,"top_fix":{"selector":"h1","change":"clarify the headline","rationale":"orient the visitor","impact":"high"}}`
		case strings.Contains(system, "product lead synthesizing"):
			content = `{"improvements":[{"title":"Clarify the primary action","rationale":"first-time visitors are unsure","impact":"high","affected_personas":["accessibility-constrained"]}]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

// TestEndToEndPersonaEvaluationDOMGrounding (Phase 1 DOM/a11y grounding) crawls a
// fixture with an sr-only-labelled input + an <a> card, runs the persona eval against
// a FAKE OpenRouter, and asserts the load-bearing plumbing: (a) the a11y digest is
// captured + stored and carries the resolved label, (b) the authoritative SEMANTIC
// STRUCTURE block reaches the model prompt, and (c) the deterministic verify drops the
// two objective a11y false positives while keeping the subjective finding.
func TestEndToEndPersonaEvaluationDOMGrounding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fixture := a11yEvalFixture()
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	var sawSemantic atomic.Bool
	or := fakeOpenRouterEvalA11y(t, &sawSemantic)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-eval-a11y.db"), S3Local: filepath.Join(tmp, "artifacts"),
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

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "A11yEvalFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	run := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run.Status != db.RunDone {
		t.Fatalf("run did not complete: %s / %s", run.Status, run.Error)
	}

	// (a) The a11y digest is captured + stored, and carries the resolved sr-only label.
	pages, _ := database.ListPages(run.ID)
	var homePage *db.Page
	for _, p := range pages {
		if p.Viewport == "desktop" && !strings.Contains(p.URL, "/about") {
			homePage = p
		}
	}
	if homePage == nil {
		t.Fatalf("home desktop page not found among %d pages", len(pages))
	}
	if homePage.A11yDigestKey == "" {
		t.Fatal("home page has no a11y_digest_key — digest was not captured/stored")
	}
	rc, err := store.Get(context.Background(), homePage.A11yDigestKey)
	if err != nil {
		t.Fatalf("get stored digest: %v", err)
	}
	digestBytes, _ := io.ReadAll(rc)
	rc.Close()
	var digest report.A11yDigest
	if err := json.Unmarshal(digestBytes, &digest); err != nil {
		t.Fatalf("stored digest is not valid JSON: %v", err)
	}
	var sawLabel bool
	for _, c := range digest.FormControls {
		if c.Selector == "input#signup-email" && c.HasLabel && c.LabelSource == "for" {
			sawLabel = true
		}
	}
	if !sawLabel {
		t.Errorf("stored digest missing the resolved sr-only label: %+v", digest.FormControls)
	}

	// Trigger the persona walkthrough (one persona is enough for the drop assertion).
	form := url.Values{"personas": {"accessibility-constrained"}, "job": {"create a campaign"}, "verify": {"1"}}
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

	// (b) The authoritative semantic block reached the model prompt.
	if !sawSemantic.Load() {
		t.Error("the SEMANTIC STRUCTURE block was not present in the generation prompt")
	}

	// (c) The deterministic verify dropped BOTH objective a11y FPs but kept the
	// subjective finding — for the home page's evaluation.
	rows, _ := database.ListPageEvaluations(run.ID)
	var homeEval *db.PageEvaluation
	for _, r := range rows {
		if r.PageID == homePage.ID {
			homeEval = r
		}
	}
	if homeEval == nil || homeEval.FindingsJSON == "" {
		t.Fatalf("no evaluation stored for the home page")
	}
	// Name the STAGE precisely in each message: with two deterministic stages behind one
	// gate, a message blaming the wrong one sends whoever hits it in CI to the wrong file.
	if strings.Contains(homeEval.FindingsJSON, "Email input has no accessible label") {
		t.Error("the 'no label' FP (refuted by the sr-only <label for>) was NOT dropped by the REFUTATION stage (dropContradicted)")
	}
	if strings.Contains(homeEval.FindingsJSON, "not keyboard-operable") {
		t.Error("the 'not keyboard-operable' FP (refuted by the <a> tag) was NOT dropped")
	}
	// SELECTOR GROUNDING: the claim anchored to an invented selector the digest does not
	// list is dropped as unverifiable — this is the only e2e exercise of that stage on
	// the crawl path (the two FPs above are dropped by the refutation stage instead).
	if strings.Contains(homeEval.FindingsJSON, ungroundedA11yIssue) {
		t.Error("the a11y claim citing a selector absent from the digest was NOT dropped by the GROUNDING stage (groundSelectors)")
	}
	if !strings.Contains(homeEval.FindingsJSON, "value proposition is unclear") {
		t.Error("the subjective finding must survive the deterministic verify")
	}

	t.Logf("e2e DOM-grounding OK: digestKey=%s sawSemantic=%v", homePage.A11yDigestKey, sawSemantic.Load())
}
