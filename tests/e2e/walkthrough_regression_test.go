// Package e2e: Phase-4 — walkthrough-vs-walkthrough regression diffing. It drives a
// MUTABLE funnel fixture twice with the REAL chromedp crawler.Drive loop + a SCRIPTED
// planner (deterministic, no LLM): walkthrough #1 reaches the goal (success); then the
// fixture is MUTATED so the goal becomes unreachable; walkthrough #2 gets stuck. #2 is
// baseline-linked to #1, and the deterministic diff (success→stuck) surfaces as a
// regression both in walkthroughs.diff_json and via the owner-scoped read-API
// `regression` block a CI gate keys on.
//
// Drives real headless Chromium (chromedp) — needs a chromium/chrome binary.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
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
	"github.com/ZacxDev/auditloop/internal/walkthrough"
)

// regressFunnel is a funnel whose success path can be BROKEN at runtime: while intact,
// POST /submit → 303 /welcome (the success marker); once broken, /submit bounces back to
// /form so the /welcome goal is never reached.
type regressFunnel struct {
	broken atomic.Bool
}

func (f *regressFunnel) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Home</title></head>
<body><h1>Home</h1><a href="/form">Get started</a></body></html>`))
	})
	mux.HandleFunc("/form", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sign up</title></head>
<body><h1>Sign up</h1>
<form action="/submit" method="post">
<input id="email" name="email" type="text" placeholder="Email">
<button id="submit" type="submit">Create account</button>
</form></body></html>`))
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/form", http.StatusFound)
			return
		}
		if f.broken.Load() {
			// Regression: the goal is no longer reachable — bounce back to the form.
			http.Redirect(w, r, "/form", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
	})
	mux.HandleFunc("/welcome", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Welcome</title></head>
<body><h1 id="goal-reached">Welcome — account created</h1></body></html>`))
	})
	return httptest.NewServer(mux)
}

func TestEndToEndWalkthroughRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fxState := &regressFunnel{}
	fx := fxState.server()
	defer fx.Close()
	host := hostOnly(fx.URL)

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-wreg.db"), S3Local: filepath.Join(tmp, "artifacts"),
		CrawlAllowLoopback: true, ChromiumPath: chromium, DevMode: true,
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

	driveOnce := func() *crawler.DriveTrace {
		t.Helper()
		planner := &scriptedPlanner{actions: []action.Action{
			{Type: action.Navigate, URL: fx.URL + "/form", Reason: "go to signup"},
			{Type: action.TypeText, Selector: "#email", Text: "alice@example.com", Reason: "enter email"},
			{Type: action.Click, Selector: "#submit", Reason: "submit"},
		}}
		tr, derr := crawler.Drive(context.Background(), crawler.DriveOptions{
			BaseURL:        fx.URL + "/",
			AllowedHosts:   []string{host},
			Goal:           "create an account",
			Success:        action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 4000},
			Planner:        planner,
			MaxActions:     8,
			ActionTimeout:  15 * time.Second,
			OverallTimeout: 60 * time.Second,
			DryRun:         false,
			AllowLoopback:  true,
			ChromiumPath:   chromium,
		})
		if derr != nil {
			t.Fatalf("drive: %v", derr)
		}
		return tr
	}

	// persist mirrors the driver generator: create (baseline-linked) → steps → finish →
	// RefreshWalkthroughDiff (the drive-end diff trigger).
	persist := func(tr *crawler.DriveTrace) *db.Walkthrough {
		t.Helper()
		wk, cerr := database.CreateWalkthrough(tgt.ID, "", "create an account", true)
		if cerr != nil {
			t.Fatalf("create walkthrough: %v", cerr)
		}
		for _, s := range tr.Steps {
			shotKey := ""
			if len(s.ScreenshotPNG) > 0 {
				shotKey = storage.WalkthroughStepKey(tgt.ID, wk.ID, s.Idx)
				_ = store.Put(context.Background(), shotKey, "image/png", bytes.NewReader(s.ScreenshotPNG), int64(len(s.ScreenshotPNG)))
			}
			aj, _ := json.Marshal(s.Action)
			if _, ierr := database.InsertWalkthroughStep(&db.WalkthroughStep{
				WalkthroughID: wk.ID, Idx: s.Idx, ActionJSON: string(aj),
				URL: s.URL, ScreenshotKey: shotKey, Outcome: s.Outcome, PlannerReason: s.PlannerReason,
			}); ierr != nil {
				t.Fatalf("insert step: %v", ierr)
			}
		}
		outcome := tr.Outcome
		if outcome == "" {
			outcome = db.WalkOutcomeStuck
		}
		// Mirrors generator.Run: the trace's own in-band infra signal (#45).
		if ferr := database.FinishWalkthrough(wk.ID, outcome, tr.StuckStep, tr.Reason, tr.InfraFailed); ferr != nil {
			t.Fatalf("finish walkthrough: %v", ferr)
		}
		if _, rerr := walkthrough.RefreshWalkthroughDiff(context.Background(), database, wk.ID); rerr != nil {
			t.Fatalf("refresh diff: %v", rerr)
		}
		got, _ := database.GetWalkthroughByID(wk.ID)
		return got
	}

	// --- Walkthrough #1: the goal is reachable → success (the baseline). ---
	tr1 := driveOnce()
	if tr1.Outcome != "success" {
		t.Fatalf("walkthrough #1 outcome = %q, want success", tr1.Outcome)
	}
	wk1 := persist(tr1)
	if wk1.PrevWalkthroughID != "" {
		t.Fatalf("first walkthrough should have no baseline, got %q", wk1.PrevWalkthroughID)
	}

	// --- MUTATE the fixture so the goal regresses. ---
	fxState.broken.Store(true)

	// --- Walkthrough #2: same drive, but the goal is now unreachable → stuck. ---
	tr2 := driveOnce()
	if tr2.Outcome == "success" {
		t.Fatal("walkthrough #2 must NOT reach the (broken) goal")
	}
	wk2 := persist(tr2)

	// Baseline-linked to #1.
	if wk2.PrevWalkthroughID != wk1.ID {
		t.Fatalf("walkthrough #2 baseline = %q, want #1 %q", wk2.PrevWalkthroughID, wk1.ID)
	}
	// The diff surfaces the outcome regression.
	if wk2.DiffJSON == "" {
		t.Fatal("walkthrough #2 has no diff_json")
	}
	var d struct {
		PrevOutcome  string `json:"prev_outcome"`
		Outcome      string `json:"outcome"`
		IsRegression bool   `json:"is_regression"`
	}
	if err := json.Unmarshal([]byte(wk2.DiffJSON), &d); err != nil {
		t.Fatalf("diff_json: %v", err)
	}
	if !d.IsRegression || d.PrevOutcome != "success" {
		t.Fatalf("diff did not surface the regression: %+v", d)
	}

	// --- Read-API: the `regression` block a CI gate keys on, owner-scoped. ---
	token, hash, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateAPIKey(auth.DefaultDevUser, "e2e", hash, db.ScopeRead); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", appSrv.URL+"/api/audit/walkthroughs/"+wk2.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read-api: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-api status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Regression *struct {
			PrevWalkthroughID string   `json:"prev_walkthrough_id"`
			PrevOutcome       string   `json:"prev_outcome"`
			Outcome           string   `json:"outcome"`
			IsRegression      bool     `json:"is_regression"`
			NewTaskBlockers   []string `json:"new_task_blockers"`
		} `json:"regression"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode read-api: %v", err)
	}
	if payload.Regression == nil {
		t.Fatal("read-api missing the regression block")
	}
	if !payload.Regression.IsRegression || payload.Regression.PrevOutcome != "success" {
		t.Fatalf("read-api regression wrong: %+v", payload.Regression)
	}
	if payload.Regression.PrevWalkthroughID != wk1.ID {
		t.Fatalf("read-api prev id = %q, want %q", payload.Regression.PrevWalkthroughID, wk1.ID)
	}
	// CI gate: is_regression || len(new_task_blockers) > 0 → must trip.
	if !(payload.Regression.IsRegression || len(payload.Regression.NewTaskBlockers) > 0) {
		t.Fatal("CI gate should fail on this walkthrough")
	}
}
