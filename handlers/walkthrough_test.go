package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
	"github.com/ZacxDev/auditloop/internal/walkthrough"

	"github.com/gorilla/mux"
)

// walkTestApp builds an App directly (bypassing NewRouter) so the walkthrough
// generator's Drive can be STUBBED — no chromedp in a hermetic handler test.
// withWalk toggles the OpenRouter gate + the (stubbed) driver.
func walkTestApp(t *testing.T, withWalk bool, drive walkthrough.DriveFunc) *App {
	t.Helper()
	tmp := t.TempDir()
	database, err := db.Open("sqlite", filepath.Join(tmp, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFS(filepath.Join(tmp, "art"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.AppConfig{DevMode: true, DatabaseDriver: "sqlite", S3Local: filepath.Join(tmp, "art")}
	app := &App{Cfg: cfg, DB: database, Store: store}
	if withWalk {
		app.Cfg.OpenRouterAPIKey = "test-key"
		app.Walk = &walkthrough.Generator{DB: database, Store: store, Model: "m", Drive: drive, MaxActions: 5}
	}
	return app
}

// walkReq builds a dev-user, mux-var'd request for direct handler invocation.
func walkReq(method, path string, vals url.Values, vars map[string]string) *http.Request {
	var body io.Reader
	if vals != nil {
		body = strings.NewReader(vals.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if vals != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: auth.DefaultDevUser}))
	return mux.SetURLVars(req, vars)
}

func seedDriveTarget(t *testing.T, app *App, driving bool, withConfig bool) *db.Target {
	t.Helper()
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Site", "https://site.test", []string{"site.test"})
	if driving {
		if err := app.DB.SetDrivingConfig(auth.DefaultDevUser, tgt.ID, true, false); err != nil {
			t.Fatal(err)
		}
		tgt, _ = app.DB.GetTarget(auth.DefaultDevUser, tgt.ID)
	}
	if withConfig {
		if err := app.DB.SetTargetAuditConfig(&db.TargetAuditConfig{
			TargetID: tgt.ID, PrimaryJob: "sign up", SuccessURLContains: "/welcome",
			SuccessTimeoutMs: 5000, Confirmed: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return tgt
}

func TestWalkthroughDisabled503(t *testing.T) {
	app := walkTestApp(t, false, nil) // no OpenRouter → Walk nil
	tgt := seedDriveTarget(t, app, true, true)
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled = %d, want 503", rw.Code)
	}
}

func TestWalkthroughNotDrivingEnabled403(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, false, true) // driving OFF
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusForbidden {
		t.Fatalf("driving-disabled = %d, want 403", rw.Code)
	}
}

func TestWalkthroughNoGoalOrSuccess409(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, true, false) // driving ON, no config
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusConflict {
		t.Fatalf("no goal/success = %d, want 409", rw.Code)
	}
}

func TestWalkthroughHappyClaimAndDrive(t *testing.T) {
	trace := &crawler.DriveTrace{
		Outcome: "success",
		Steps: []crawler.StepRecord{
			{Idx: 0, Action: action.Action{Type: action.Click, Selector: "#go"}, URL: "https://site.test/", Outcome: "ok"},
			{Idx: 1, Action: action.Action{Type: action.Finish}, URL: "https://site.test/welcome", Outcome: "success"},
		},
	}
	app := walkTestApp(t, true, stubDrive(trace))
	tgt := seedDriveTarget(t, app, true, true)

	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "walkthrough-section") {
		t.Fatalf("response is not the walkthrough fragment:\n%s", rw.Body.String())
	}

	// The async pass (stubbed Drive) persists the trace and finalizes success.
	wk := waitWalkthroughDone(t, app, tgt.ID)
	if wk.Outcome != db.WalkOutcomeSuccess {
		t.Fatalf("outcome = %q, want success", wk.Outcome)
	}
	steps, _ := app.DB.ListWalkthroughSteps(wk.ID)
	if len(steps) != 2 {
		t.Fatalf("persisted %d steps, want 2", len(steps))
	}
}

func TestWalkthroughReadAPIOwnerScoped(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, true, true)
	wk, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_, _ = app.DB.InsertWalkthroughStep(&db.WalkthroughStep{WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://site.test/", Outcome: "ok"})
	_ = app.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeStuck, 1, "budget", false)

	// Owner reads it.
	rw := httptest.NewRecorder()
	app.handleAuditWalkthrough(rw, walkReq("GET", "/api/audit/walkthroughs/"+wk.ID, nil, map[string]string{"id": wk.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("owner read = %d, want 200", rw.Code)
	}
	var got apiWalkthrough
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Outcome != "stuck" || len(got.Steps) != 1 || got.Steps[0].Outcome != "ok" {
		t.Fatalf("read payload wrong: %+v", got)
	}

	// A DIFFERENT user must get 404 (owner-scoped, no existence leak).
	rw2 := httptest.NewRecorder()
	foreign := walkReq("GET", "/api/audit/walkthroughs/"+wk.ID, nil, map[string]string{"id": wk.ID})
	foreign = foreign.WithContext(auth.WithClaims(foreign.Context(), &auth.Claims{UserID: "someone-else"}))
	app.handleAuditWalkthrough(rw2, foreign)
	if rw2.Code != http.StatusNotFound {
		t.Fatalf("foreign read = %d, want 404", rw2.Code)
	}
}

// TestWalkthroughReadAPINoKey401 exercises the apiKeyAuth middleware via the full
// router: a request with no bearer key is rejected before the handler.
func TestWalkthroughReadAPINoKey401(t *testing.T) {
	_, router := testApp(t) // full router with the apiKeyAuth subrouter
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/api/audit/walkthroughs/whatever", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Fatalf("no-key read = %d, want 401", rw.Code)
	}
}

// stubDrive returns a DriveFunc that yields the given trace (or a minimal stuck
// trace when nil) without touching chromedp.
func stubDrive(trace *crawler.DriveTrace) walkthrough.DriveFunc {
	return func(ctx context.Context, opts crawler.DriveOptions) (*crawler.DriveTrace, error) {
		if trace != nil {
			return trace, nil
		}
		return &crawler.DriveTrace{Outcome: "stuck", StuckStep: 0, Reason: "stub"}, nil
	}
}

func waitWalkthroughDone(t *testing.T, app *App, targetID string) *db.Walkthrough {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		wk, err := app.DB.LatestWalkthroughForTarget(auth.DefaultDevUser, targetID)
		if err == nil && wk != nil && (wk.Status == db.WalkDone || wk.Status == db.WalkFailed) {
			return wk
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("walkthrough did not finish in time")
	return nil
}
