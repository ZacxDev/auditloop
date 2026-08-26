package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// seedDoneWalkthrough creates a target (for userID) + a completed walkthrough with the
// given (actionType, url) steps, each with a stored screenshot. Returns target + wk.
func seedDoneWalkthroughH(t *testing.T, app *App, userID string, steps [][2]string) (*db.Target, *db.Walkthrough) {
	t.Helper()
	tgt, err := app.DB.CreateTarget(userID, "Site", "https://site.test", []string{"site.test"})
	if err != nil {
		t.Fatal(err)
	}
	wk, err := app.DB.CreateWalkthrough(tgt.ID, "", "sign up and reach welcome", true)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range steps {
		key := storage.WalkthroughStepKey(tgt.ID, wk.ID, i)
		_ = app.Store.Put(context.Background(), key, "image/png", bytes.NewReader(tinyPNG), int64(len(tinyPNG)))
		if _, err := app.DB.InsertWalkthroughStep(&db.WalkthroughStep{
			WalkthroughID: wk.ID, Idx: i, ActionJSON: `{"type":"` + s[0] + `"}`,
			URL: s[1], ScreenshotKey: key, Outcome: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeSuccess, 0, "", false); err != nil {
		t.Fatal(err)
	}
	return tgt, wk
}

func evalWalkPath(tid, wid string) string {
	return "/api/targets/" + tid + "/walkthroughs/" + wid + "/evaluate"
}

func TestEvaluateWalkthroughDisabled503(t *testing.T) {
	app, router := testApp(t) // no OpenRouter key
	tgt, wk := seedDoneWalkthroughH(t, app, auth.DefaultDevUser, [][2]string{{"navigate", "https://site.test/"}})
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(tgt.ID, wk.ID), url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled = %d, want 503", rw.Code)
	}
}

func TestEvaluateWalkthroughNotDone409(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Site", "https://site.test", []string{"site.test"})
	wk, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true) // idle, not done
	_, _ = app.DB.InsertWalkthroughStep(&db.WalkthroughStep{WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://site.test/", Outcome: "ok"})

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(tgt.ID, wk.ID), url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusConflict {
		t.Fatalf("not-done = %d, want 409", rw.Code)
	}
}

func TestEvaluateWalkthroughNoSteps409(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Site", "https://site.test", []string{"site.test"})
	wk, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeFailed, 0, "driver error", false) // done/failed, zero steps

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(tgt.ID, wk.ID), url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusConflict {
		t.Fatalf("no-steps = %d, want 409", rw.Code)
	}
}

func TestEvaluateWalkthroughForeign404(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	// A walkthrough owned by ANOTHER user; the dev-user request must not resolve it.
	_, wkB := seedDoneWalkthroughH(t, app, "user-b", [][2]string{{"navigate", "https://site.test/"}})

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(wkB.TargetID, wkB.ID), url.Values{"personas": {"skeptical-evaluator"}}))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("foreign walkthrough = %d, want 404", rw.Code)
	}
	// A target/walkthrough mismatch (wrong target id in the path) is also 404.
	tgt, wk := seedDoneWalkthroughH(t, app, auth.DefaultDevUser, [][2]string{{"navigate", "https://site.test/"}})
	_ = tgt
	rw2 := httptest.NewRecorder()
	router.ServeHTTP(rw2, formPost(evalWalkPath("not-the-right-target", wk.ID), url.Values{"personas": {"skeptical-evaluator"}}))
	if rw2.Code != http.StatusNotFound {
		t.Fatalf("target mismatch = %d, want 404", rw2.Code)
	}
}

func TestEvaluateWalkthroughHappy(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, wk := seedDoneWalkthroughH(t, app, auth.DefaultDevUser, [][2]string{
		{"navigate", "https://site.test/form"},
		{"click", "https://site.test/welcome"},
	})

	personas := []string{"first-time-nontechnical", "skeptical-evaluator"}
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(tgt.ID, wk.ID), url.Values{"personas": personas}))
	if rw.Code != http.StatusOK {
		t.Fatalf("evaluate = %d (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "eval-section") {
		t.Fatalf("expected the eval-section fragment, got:\n%s", rw.Body.String())
	}

	// The synthetic run was materialized + stamped onto the walkthrough.
	got, _ := app.DB.GetWalkthroughByID(wk.ID)
	if got.RunID == "" {
		t.Fatal("walkthrough was not stamped with a synthetic run id")
	}
	runID := got.RunID
	run, err := app.DB.GetRunByID(runID)
	if err != nil || run.Trigger != "walkthrough" || run.Status != db.RunDone {
		t.Fatalf("synthetic run wrong: %+v err=%v", run, err)
	}
	// One page per step, in order.
	pgs, _ := app.DB.ListPages(runID)
	if len(pgs) != 2 {
		t.Fatalf("expected 2 synthetic pages, got %d", len(pgs))
	}

	// The eval pass ran: one page_evaluation row per step × persona.
	waitEvalDone(t, app, runID)
	rows, _ := app.DB.ListPageEvaluations(runID)
	want := len(pgs) * len(personas)
	if len(rows) != want {
		t.Fatalf("expected %d evaluation rows (%d pages × %d personas), got %d", want, len(pgs), len(personas), len(rows))
	}

	// Read-API surfaces the synthetic eval_run_id on the walkthrough.
	rw2 := httptest.NewRecorder()
	app.handleAuditWalkthrough(rw2, walkReq("GET", "/api/audit/walkthroughs/"+wk.ID, nil, map[string]string{"id": wk.ID}))
	var payload apiWalkthrough
	if err := json.Unmarshal(rw2.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.EvalRunID != runID {
		t.Fatalf("read-API eval_run_id = %q, want %q", payload.EvalRunID, runID)
	}
}

// TestEvaluateWalkthroughDefaultsPersonasFromConfig: omitting personas[] falls back to
// the target's confirmed audit-config personas.
func TestEvaluateWalkthroughDefaultsPersonasFromConfig(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, wk := seedDoneWalkthroughH(t, app, auth.DefaultDevUser, [][2]string{{"navigate", "https://site.test/"}})
	if err := app.DB.SetTargetAuditConfig(&db.TargetAuditConfig{
		TargetID: tgt.ID, PrimaryJob: "sign up", Personas: []string{"returning-power-user"}, Confirmed: true,
	}); err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost(evalWalkPath(tgt.ID, wk.ID), url.Values{})) // NO personas
	if rw.Code != http.StatusOK {
		t.Fatalf("evaluate = %d (%s)", rw.Code, rw.Body.String())
	}
	got, _ := app.DB.GetWalkthroughByID(wk.ID)
	waitEvalDone(t, app, got.RunID)
	rows, _ := app.DB.ListPageEvaluations(got.RunID)
	if len(rows) != 1 || rows[0].Persona != "returning-power-user" {
		t.Fatalf("expected 1 row for the config's default persona, got %d rows (%+v)", len(rows), rows)
	}
}
