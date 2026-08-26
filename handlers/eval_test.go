package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
)

// fakeOREval is a stub OpenRouter that returns structured persona-walkthrough JSON
// appropriate to the pass (generation / verification / synthesis), keyed off the
// system prompt so the whole eval flow produces clean rows.
func fakeOREval(t *testing.T) *httptest.Server {
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
			if m.Role == "system" && len(m.Content) > 0 {
				system = m.Content[0].Text
			}
		}
		content := `{"comprehension":"unclear","blockers":[{"issue":"no CTA","selector":"#a","evidence":"missing"}],"frictions":[],"top_fix":{"selector":"#a","change":"add a CTA","impact":"high"}}`
		switch {
		case strings.Contains(system, "fact-checker"):
			content = `{"comprehension":"unclear","blockers":[{"issue":"no CTA","selector":"#a","evidence":"missing","verified":true}],"frictions":[],"top_fix":{"selector":"#a","change":"add a CTA","impact":"high"}}`
		case strings.Contains(system, "product lead synthesizing"):
			content = `{"improvements":[{"title":"Add a prominent CTA","impact":"high","affected_urls":["https://acme.test/"],"affected_personas":["skeptical-evaluator"]}]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

func waitEvalDone(t *testing.T, app *App, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := app.DB.GetRunByID(runID)
		if err == nil && (run.EvalStatus == db.EvalDone || run.EvalStatus == db.EvalFailed) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("eval job for %s did not finish", runID)
}

func TestGenerateEvalDisabled503(t *testing.T) {
	app, router := testApp(t) // no OpenRouter key
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	run, _ := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID)
	_ = app.DB.FinishRun(run.ID, db.RunDone, "{}", "")

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+run.ID+"/evaluate", url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when disabled, got %d", rw.Code)
	}
}

func TestGenerateEvalValidation(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)

	// Empty personas → 400.
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/evaluate", url.Values{}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("empty personas should 400, got %d", rw.Code)
	}
	// Unknown persona → 400.
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/evaluate", url.Values{"personas": {"evil/backdoor"}}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("unknown persona should 400, got %d", rw.Code)
	}
	// Valid → 200, job starts + fragment returned.
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/evaluate", url.Values{"personas": {"skeptical-evaluator"}, "job": {"sign up"}, "verify": {"1"}}))
	if rw.Code != http.StatusOK {
		t.Fatalf("valid request should 200, got %d (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "eval-section") {
		t.Error("expected the eval-section fragment back")
	}
	waitEvalDone(t, app, runID)
	rows, _ := app.DB.ListPageEvaluations(runID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 evaluation row after the pass, got %d", len(rows))
	}
	if rows[0].Error != "" || rows[0].Comprehension != "unclear" {
		t.Errorf("evaluation row not clean: %+v", rows[0])
	}
	// The job string was recorded on the run.
	run, _ := app.DB.GetRunByID(runID)
	if run.EvalJob != "sign up" {
		t.Errorf("eval job = %q, want 'sign up'", run.EvalJob)
	}
}

func TestGenerateEvalRunNotDone(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	run, _ := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID) // queued, not done

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+run.ID+"/evaluate", url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("evaluate on a non-done run should 400, got %d", rw.Code)
	}
}

func TestEvalStatusFragment(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/runs/"+runID+"/eval-status", nil))
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "eval-section") {
		t.Errorf("eval-status fragment bad: %d", rw.Code)
	}
}

// --- Read API: /evaluation ---

func TestReadAPIEvaluationOwnerScoped(t *testing.T) {
	app, router := testAppNonDev(t)
	_, runID, _, _ := seedRun(t, app, "user-A", "AcmeA")

	// Attach an evaluation + synthesis to user-A's run.
	pages, _ := app.DB.ListPages(runID)
	if len(pages) == 0 {
		t.Fatal("seedRun produced no pages")
	}
	pe := `{"comprehension":"blocked","blockers":[{"issue":"no pricing","selector":".p","evidence":"empty","verified":true}]}`
	if err := app.DB.SavePageEvaluation(pages[0].ID, runID, "skeptical-evaluator", pe, "blocked", "", 0.001, 100, 20); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.SetRunEvalSynthesis(runID, `[{"title":"Show pricing","impact":"high"}]`); err != nil {
		t.Fatal(err)
	}
	tokenA := mintKey(t, app, "user-A")

	// Own run → 200 with structured payload.
	rw := getWithKey(router, "/api/audit/runs/"+runID+"/evaluation", tokenA)
	if rw.Code != http.StatusOK {
		t.Fatalf("own evaluation = %d (%s)", rw.Code, rw.Body.String())
	}
	var out struct {
		RunID     string `json:"run_id"`
		Synthesis []struct {
			Title string `json:"title"`
		} `json:"synthesis"`
		Pages []struct {
			Persona    string `json:"persona"`
			Evaluation *struct {
				Comprehension string `json:"comprehension"`
			} `json:"evaluation"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rw.Body.String())
	}
	if out.RunID != runID || len(out.Pages) != 1 || out.Pages[0].Persona != "skeptical-evaluator" {
		t.Fatalf("unexpected payload: %s", rw.Body.String())
	}
	if out.Pages[0].Evaluation == nil || out.Pages[0].Evaluation.Comprehension != "blocked" {
		t.Errorf("structured evaluation not returned: %s", rw.Body.String())
	}
	if len(out.Synthesis) != 1 || out.Synthesis[0].Title != "Show pricing" {
		t.Errorf("synthesis not returned: %s", rw.Body.String())
	}

	// A foreign user's run → 404 (existence not leaked).
	_, runB, _, _ := seedRun(t, app, "user-B", "AcmeB")
	if rw := getWithKey(router, "/api/audit/runs/"+runB+"/evaluation", tokenA); rw.Code != http.StatusNotFound {
		t.Errorf("foreign run evaluation = %d, want 404", rw.Code)
	}
	// No key → 401.
	if rw := getWithKey(router, "/api/audit/runs/"+runID+"/evaluation", ""); rw.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rw.Code)
	}
}
