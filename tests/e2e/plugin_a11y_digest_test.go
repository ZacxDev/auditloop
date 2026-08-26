// Plugin-push DOM-grounding e2e: pushes a run WITH an optional per-page a11y digest
// through the REAL cmd/auditloop-push CLI, runs the persona evaluation against a FAKE
// OpenRouter (never a real key, no chromium — nothing is crawled), and asserts the
// pushed run gets the SAME deterministic, no-LLM gate the crawl path gets: the DOM-
// refuted a11y false positives are dropped while the genuinely-true a11y finding and
// the subjective finding survive.
//
// It also pushes a SECOND run with NO digest as the backward-compat control: identical
// findings, identical fake model, nothing dropped.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"

	"image/color"
)

// pushedA11yDigest is what a producing harness (any external push producer) emits
// per view — the SAME shape internal/crawler/a11y-digest.js produces. It says:
//   - input#signup-email HAS a programmatic label (an sr-only <label for>) → a
//     "no label" finding on it is REFUTED,
//   - input#promo has only a placeholder → a "no label" finding on it is TRUE,
//   - a#client-card-1 is a real <a> → a "not keyboard-operable" finding is REFUTED.
const pushedA11yDigest = `{
  "interactive":[
    {"tag":"a","role":"link","selector":"a#client-card-1","accessible_name":"Open client Acme","focusable":true,"label_source":"text-content"},
    {"tag":"input","selector":"input#signup-email","accessible_name":"Email address","focusable":true,"label_source":"for"}
  ],
  "form_controls":[
    {"selector":"input#signup-email","accessible_name":"Email address","has_label":true,"label_source":"for"},
    {"selector":"input#promo","accessible_name":"","has_label":false,"label_source":"placeholder"}
  ],
  "landmarks":[{"tag":"h1","text":"Create a campaign"},{"tag":"nav","role":"navigation"}]
}`

// The four canned findings the fake model emits for every (page, persona) cell.
const (
	pushRefutedLabel    = "Email input has no accessible label for screen readers"
	pushRefutedOperable = "Client cards are not keyboard-operable"
	pushTrueLabel       = "The promo code field has no label"
	pushSubjective      = "The value proposition is unclear for a first-time visitor"
)

// fakeOpenRouterPushedA11y echoes all four findings back as VERIFIED from both the
// generation and the verification pass — i.e. the LLM verify pass does NOT drop the
// false positives (it only re-reads the same pixels). The DETERMINISTIC digest gate is
// what must drop them, which is precisely what this test measures.
func fakeOpenRouterPushedA11y(t *testing.T, sawSemantic *atomic.Bool) *httptest.Server {
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
		if strings.Contains(userText, "SEMANTIC STRUCTURE") && strings.Contains(userText, "input#signup-email") {
			sawSemantic.Store(true)
		}

		findings := `"blockers":[` +
			`{"issue":"` + pushRefutedLabel + `","selector":"#signup-email","evidence":"no associated label element"%s},` +
			`{"issue":"` + pushRefutedOperable + `","selector":"#client-card-1","evidence":"looks like a div"%s},` +
			`{"issue":"` + pushTrueLabel + `","selector":"#promo","evidence":"no associated label element"%s},` +
			`{"issue":"` + pushSubjective + `","selector":"main","evidence":"dense copy"%s}],"frictions":[]`

		content := `{"comprehension":"unclear",` + strings.ReplaceAll(findings, "%s", "") +
			`,"top_fix":{"selector":"h1","change":"clarify the headline","rationale":"orient the visitor","impact":"high"}}`
		switch {
		case strings.Contains(system, "fact-checker"):
			content = `{"comprehension":"unclear",` + strings.ReplaceAll(findings, "%s", `,"verified":true`) +
				`,"top_fix":{"selector":"h1","change":"clarify the headline","rationale":"orient the visitor","impact":"high"}}`
		case strings.Contains(system, "product lead synthesizing"):
			content = `{"improvements":[{"title":"Clarify the primary action","rationale":"first-time visitors are unsure","impact":"high","affected_personas":["accessibility-constrained"]}]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

func TestEndToEndPluginPushA11yDigestGrounding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short mode")
	}
	cli := buildPushCLI(t)

	var sawSemantic atomic.Bool
	or := fakeOpenRouterPushedA11y(t, &sawSemantic)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleWeb, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-push-a11y.db"), S3Local: filepath.Join(tmp, "artifacts"),
		DevMode:           true,
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

	tgt, err := database.CreatePluginTarget(auth.DefaultDevUser, "Digest funnel", "")
	if err != nil {
		t.Fatalf("create plugin target: %v", err)
	}
	token, hash, _ := plugin.GenerateToken()
	if err := database.SetPluginToken(tgt.ID, hash); err != nil {
		t.Fatalf("set token: %v", err)
	}

	// --- Push WITH a digest, via the real uploader CLI. ---
	dir := t.TempDir()
	white := solidPNG(t, 80, 80, color.White)
	mustWrite(t, filepath.Join(dir, "home-desktop.png"), white)
	mustWrite(t, filepath.Join(dir, "home-mobile.png"), white)
	mustWrite(t, filepath.Join(dir, "home-a11y.json"), []byte(pushedA11yDigest))
	meta := `{"label":"digest build","pages":[
		{"url":"home","viewport":"desktop","screenshot":"home-desktop.png","a11y_digest":"home-a11y.json"},
		{"url":"home","viewport":"mobile","screenshot":"home-mobile.png"}
	]}`
	mustWrite(t, filepath.Join(dir, "metadata.json"), []byte(meta))

	out := runPushCLI(t, cli, appSrv.URL, token, filepath.Join(dir, "metadata.json"), dir)
	if !strings.Contains(out, "/runs/") {
		t.Fatalf("CLI output missing run URL: %q", out)
	}
	runs, _ := database.ListRuns(auth.DefaultDevUser, tgt.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after push, got %d", len(runs))
	}
	runID := runs[0].ID

	// The digest landed under the standard key scheme + the page references it.
	pages, _ := database.ListPages(runID)
	var desktop *db.Page
	for _, p := range pages {
		if p.Viewport == "desktop" {
			desktop = p
		}
	}
	if desktop == nil || desktop.A11yDigestKey == "" {
		t.Fatalf("pushed desktop page carries no a11y_digest_key: %+v", desktop)
	}
	rc, err := store.Get(context.Background(), desktop.A11yDigestKey)
	if err != nil {
		t.Fatalf("pushed digest artifact missing from the store: %v", err)
	}
	digestBytes, _ := io.ReadAll(rc)
	rc.Close()
	var digest report.A11yDigest
	if err := json.Unmarshal(digestBytes, &digest); err != nil {
		t.Fatalf("stored pushed digest is not valid report.A11yDigest JSON: %v", err)
	}
	var sawLabelled, sawPlaceholderOnly bool
	for _, c := range digest.FormControls {
		if c.Selector == "input#signup-email" && c.HasLabel && c.LabelSource == "for" {
			sawLabelled = true
		}
		if c.Selector == "input#promo" && !c.HasLabel {
			sawPlaceholderOnly = true
		}
	}
	if !sawLabelled || !sawPlaceholderOnly {
		t.Fatalf("stored pushed digest lost its label facts: %+v", digest.FormControls)
	}

	// --- Evaluate the pushed run with a persona. ---
	evalFindings := evaluatePushedRun(t, appSrv.URL, database, runID, desktop.ID)

	if !sawSemantic.Load() {
		t.Error("the SEMANTIC STRUCTURE block did not reach the prompt for a PUSHED run")
	}
	if strings.Contains(evalFindings, pushRefutedLabel) {
		t.Error("the 'no label' FP (refuted by the pushed digest's sr-only <label for>) was NOT dropped")
	}
	if strings.Contains(evalFindings, pushRefutedOperable) {
		t.Error("the 'not keyboard-operable' FP (refuted by the pushed digest's <a>) was NOT dropped")
	}
	// The discriminating half — the gate must not be a blanket a11y suppressor.
	if !strings.Contains(evalFindings, pushTrueLabel) {
		t.Error("the TRUE missing-label finding (#promo is placeholder-only) must SURVIVE — the gate must discriminate")
	}
	if !strings.Contains(evalFindings, pushSubjective) {
		t.Error("the subjective finding must survive the deterministic gate")
	}

	// --- BACKWARD-COMPAT CONTROL: the same push WITHOUT a digest drops nothing. ---
	dir2 := t.TempDir()
	mustWrite(t, filepath.Join(dir2, "home-desktop.png"), solidPNG(t, 80, 80, color.Black))
	mustWrite(t, filepath.Join(dir2, "home-mobile.png"), solidPNG(t, 80, 80, color.Black))
	meta2 := `{"label":"legacy build","pages":[
		{"url":"home","viewport":"desktop","screenshot":"home-desktop.png"},
		{"url":"home","viewport":"mobile","screenshot":"home-mobile.png"}
	]}`
	mustWrite(t, filepath.Join(dir2, "metadata.json"), []byte(meta2))
	if out := runPushCLI(t, cli, appSrv.URL, token, filepath.Join(dir2, "metadata.json"), dir2); !strings.Contains(out, "/runs/") {
		t.Fatalf("legacy (digest-less) push failed: %q", out)
	}
	runs, _ = database.ListRuns(auth.DefaultDevUser, tgt.ID)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	var legacyRunID string
	for _, r := range runs {
		if r.ID != runID {
			legacyRunID = r.ID
		}
	}
	legacyPages, _ := database.ListPages(legacyRunID)
	var legacyDesktop *db.Page
	for _, p := range legacyPages {
		if p.Viewport == "desktop" {
			legacyDesktop = p
		}
		if p.A11yDigestKey != "" {
			t.Errorf("a digest-less push must set NO a11y_digest_key, got %q", p.A11yDigestKey)
		}
	}
	legacyFindings := evaluatePushedRun(t, appSrv.URL, database, legacyRunID, legacyDesktop.ID)
	for _, want := range []string{pushRefutedLabel, pushRefutedOperable, pushTrueLabel, pushSubjective} {
		if !strings.Contains(legacyFindings, want) {
			t.Errorf("without a digest NOTHING may be dropped (legacy behaviour + the control for the drops above); missing %q", want)
		}
	}

	t.Logf("e2e pushed DOM-grounding OK: digestKey=%s sawSemantic=%v", desktop.A11yDigestKey, sawSemantic.Load())
}

// evaluatePushedRun triggers the persona walkthrough on a pushed run, waits for the
// job, and returns the stored findings JSON for the given page.
func evaluatePushedRun(t *testing.T, baseURL string, database *db.DB, runID, pageID string) string {
	t.Helper()
	form := url.Values{"personas": {"accessibility-constrained"}, "job": {"create a campaign"}, "verify": {"1"}}
	req, _ := http.NewRequest("POST", baseURL+"/api/runs/"+runID+"/evaluate", strings.NewReader(form.Encode()))
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
	for time.Now().Before(deadline) {
		got, _ := database.GetRunByID(runID)
		if got != nil && (got.EvalStatus == db.EvalDone || got.EvalStatus == db.EvalFailed) {
			if got.EvalStatus != db.EvalDone {
				t.Fatalf("eval job failed for run %s", runID)
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	rows, _ := database.ListPageEvaluations(runID)
	for _, r := range rows {
		if r.PageID == pageID {
			if r.Error != "" {
				t.Fatalf("evaluation cell errored: %s", r.Error)
			}
			return r.FindingsJSON
		}
	}
	t.Fatalf("no evaluation stored for page %s of run %s", pageID, runID)
	return ""
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
