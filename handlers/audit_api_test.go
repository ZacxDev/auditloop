package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/apikey"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// seedRun creates a done run for userID with a stored report.json and one page
// screenshot, and returns (targetID, runID, targetSlug, screenshotKey).
func seedRun(t *testing.T, app *App, userID, name string) (string, string, string, string) {
	t.Helper()
	ctx := context.Background()
	tgt, err := app.DB.CreateTarget(userID, name, "https://"+strings.ToLower(name)+".test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.DB.CreateRun(userID, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DB.FinishRun(run.ID, db.RunDone, `{"pages_crawled":1}`, ""); err != nil {
		t.Fatal(err)
	}
	slug := storage.Slug(name)

	// Stored report.json.
	reportBody := []byte(`{"schema":1,"run_id":"` + run.ID + `","status":"done"}`)
	if err := app.Store.Put(ctx, storage.ReportKey(slug, run.ID), "application/json",
		bytes.NewReader(reportBody), int64(len(reportBody))); err != nil {
		t.Fatal(err)
	}

	// One page + a screenshot artifact.
	pageSlug := storage.PageSlug("https://" + strings.ToLower(name) + ".test/")
	shotKey := storage.ScreenshotKey(slug, run.ID, pageSlug, "mobile")
	shot := pngBytes(t, 4, 4, color.Black)
	_, _ = app.DB.InsertPage(&db.Page{RunID: run.ID, URL: "https://x/", Viewport: "mobile", ScreenshotKey: shotKey})
	if err := app.Store.Put(ctx, shotKey, "image/png", bytes.NewReader(shot), int64(len(shot))); err != nil {
		t.Fatal(err)
	}
	return tgt.ID, run.ID, slug, shotKey
}

// mintKey creates a read-only API key for userID and returns the plaintext token.
func mintKey(t *testing.T, app *App, userID string) string {
	t.Helper()
	token, hash, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.CreateAPIKey(userID, "k", hash, db.ScopeRead); err != nil {
		t.Fatal(err)
	}
	return token
}

func getWithKey(router http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	return rw
}

func TestReadAPIRequiresValidKey(t *testing.T) {
	app, router := testAppNonDev(t)
	tgtID, _, _, _ := seedRun(t, app, "user-A", "AcmeA")
	path := "/api/audit/targets/" + tgtID + "/runs"

	// No key → 401.
	if rw := getWithKey(router, path, ""); rw.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rw.Code)
	}
	// Bad key → 401.
	if rw := getWithKey(router, path, "not-a-real-key"); rw.Code != http.StatusUnauthorized {
		t.Errorf("bad key = %d, want 401", rw.Code)
	}
	// Revoked key → 401.
	token, hash, _ := apikey.Generate()
	id, _ := app.DB.CreateAPIKey("user-A", "revoke-me", hash, db.ScopeRead)
	if rw := getWithKey(router, path, token); rw.Code != http.StatusOK {
		t.Fatalf("fresh key = %d, want 200", rw.Code)
	}
	if err := app.DB.RevokeAPIKey("user-A", id); err != nil {
		t.Fatal(err)
	}
	if rw := getWithKey(router, path, token); rw.Code != http.StatusUnauthorized {
		t.Errorf("revoked key = %d, want 401", rw.Code)
	}
}

func TestReadAPIServesOwnData(t *testing.T) {
	app, router := testAppNonDev(t)
	tgtID, runID, _, shotKey := seedRun(t, app, "user-A", "AcmeA")
	token := mintKey(t, app, "user-A")
	const tgtName = "AcmeA" // the target's stable spec name (== push key)

	// Run list.
	rw := getWithKey(router, "/api/audit/targets/"+tgtID+"/runs", token)
	if rw.Code != http.StatusOK {
		t.Fatalf("run list = %d (%s)", rw.Code, rw.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0]["run_id"] != runID {
		t.Fatalf("run list body = %s", rw.Body.String())
	}
	if list[0]["pages"].(float64) != 1 {
		t.Errorf("expected pages=1 from summary, got %v", list[0]["pages"])
	}

	// Run report.json.
	rw = getWithKey(router, "/api/audit/runs/"+runID, token)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), `"run_id":"`+runID+`"`) {
		t.Fatalf("run report = %d body=%s", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("report content-type = %q", ct)
	}
	if rw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("report missing nosniff")
	}

	// Latest.
	rw = getWithKey(router, "/api/audit/targets/"+tgtID+"/runs/latest", token)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), runID) {
		t.Fatalf("latest = %d body=%s", rw.Code, rw.Body.String())
	}

	// Artifact bytes (screenshot).
	rw = getWithKey(router, "/api/audit/artifacts/"+shotKey, token)
	if rw.Code != http.StatusOK {
		t.Fatalf("artifact = %d", rw.Code)
	}
	if rw.Header().Get("Content-Type") != "image/png" || rw.Header().Get("Content-Disposition") != "inline" {
		t.Errorf("artifact headers wrong: ct=%q cd=%q", rw.Header().Get("Content-Type"), rw.Header().Get("Content-Disposition"))
	}
	if len(rw.Body.Bytes()) == 0 {
		t.Error("empty artifact body")
	}

	// The target-scoped endpoints ALSO accept the owner-scoped target NAME (the
	// same stable spec name the push side keys on) and return the same data as
	// by-UUID. The UUID path (asserted above) stays backward-compatible.
	rw = getWithKey(router, "/api/audit/targets/"+tgtName+"/runs", token)
	if rw.Code != http.StatusOK {
		t.Fatalf("run list by name = %d (%s)", rw.Code, rw.Body.String())
	}
	var byName []map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &byName); err != nil {
		t.Fatalf("decode by-name list: %v", err)
	}
	if len(byName) != 1 || byName[0]["run_id"] != runID {
		t.Fatalf("run list by name body = %s", rw.Body.String())
	}
	rw = getWithKey(router, "/api/audit/targets/"+tgtName+"/runs/latest", token)
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), runID) {
		t.Fatalf("latest by name = %d body=%s", rw.Code, rw.Body.String())
	}

	// An unknown name (no such id AND no such owned name) → 404.
	if rw := getWithKey(router, "/api/audit/targets/no-such-target/runs", token); rw.Code != http.StatusNotFound {
		t.Errorf("unknown name run list = %d, want 404", rw.Code)
	}
	if rw := getWithKey(router, "/api/audit/targets/no-such-target/runs/latest", token); rw.Code != http.StatusNotFound {
		t.Errorf("unknown name latest = %d, want 404", rw.Code)
	}
}

// TestReadAPICrossUserIsolation is the critical isolation test: user-A's key
// must NOT be able to read user-B's target, run, latest, or artifact — each 404s
// (existence not leaked).
func TestReadAPICrossUserIsolation(t *testing.T) {
	app, router := testAppNonDev(t)
	_, _, _, _ = seedRun(t, app, "user-A", "AcmeA")
	tgtB, runB, _, shotB := seedRun(t, app, "user-B", "AcmeB")
	tokenA := mintKey(t, app, "user-A")

	cases := []struct {
		name string
		path string
	}{
		{"foreign target run list", "/api/audit/targets/" + tgtB + "/runs"},
		{"foreign target latest", "/api/audit/targets/" + tgtB + "/runs/latest"},
		// By NAME must be just as isolated as by UUID: user-A's key resolving
		// user-B's target NAME must NOT cross the ownership boundary (the name
		// lookup is user_id-scoped) → 404, not user-B's data.
		{"foreign target run list by name", "/api/audit/targets/AcmeB/runs"},
		{"foreign target latest by name", "/api/audit/targets/AcmeB/runs/latest"},
		{"foreign run report", "/api/audit/runs/" + runB},
		{"foreign artifact", "/api/audit/artifacts/" + shotB},
	}
	for _, c := range cases {
		rw := getWithKey(router, c.path, tokenA)
		if rw.Code != http.StatusNotFound {
			t.Errorf("%s: user-A key on user-B resource = %d, want 404 (%s)", c.name, rw.Code, rw.Body.String())
		}
	}

	// Sanity: user-B's own key CAN read user-B's resources (proves the 404s above
	// are ownership, not a broken fixture).
	tokenB := mintKey(t, app, "user-B")
	if rw := getWithKey(router, "/api/audit/runs/"+runB, tokenB); rw.Code != http.StatusOK {
		t.Errorf("user-B key on own run = %d, want 200", rw.Code)
	}
	if rw := getWithKey(router, "/api/audit/artifacts/"+shotB, tokenB); rw.Code != http.StatusOK {
		t.Errorf("user-B key on own artifact = %d, want 200", rw.Code)
	}
}

// TestReadKeyRejectedOnPushRoute proves a read API key cannot be used to push
// (mutate): presenting it as a bearer on the plugin-push route → 401.
func TestReadKeyRejectedOnPushRoute(t *testing.T) {
	app, router := testAppNonDev(t)
	token := mintKey(t, app, "user-A")
	req := httptest.NewRequest("POST", "/api/plugins/runs", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("read key on push route = %d, want 401", rw.Code)
	}
}

// TestReadAPIRateLimit fires a burst well past the token-bucket capacity and
// expects at least one 429 (limiter active because DevMode is off).
func TestReadAPIRateLimit(t *testing.T) {
	app, router := testAppNonDev(t)
	tgtID, _, _, _ := seedRun(t, app, "user-A", "AcmeA")
	token := mintKey(t, app, "user-A")
	path := "/api/audit/targets/" + tgtID + "/runs"

	got429 := false
	for i := 0; i < 40; i++ {
		if rw := getWithKey(router, path, token); rw.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Error("expected a 429 after bursting past the per-key rate limit")
	}
}

// TestKeyManagementRequiresSupabaseAuth proves the management routes are gated:
// without a verified Supabase identity (DevMode off, no token) they 401.
func TestKeyManagementRequiresSupabaseAuth(t *testing.T) {
	_, router := testAppNonDev(t)
	for _, path := range []string{"/api/keys", "/api/keys/some-id/revoke"} {
		req := httptest.NewRequest("POST", path, nil)
		rw := httptest.NewRecorder()
		router.ServeHTTP(rw, req)
		if rw.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", path, rw.Code)
		}
	}
}

// TestKeyManagementCreateAndRevoke exercises the human-facing mint/reveal +
// ownership-scoped revoke via the router in DevMode (auth bypassed → dev user).
func TestKeyManagementCreateAndRevoke(t *testing.T) {
	app, router := testApp(t) // DevMode → fixed dev user

	// Create: returns the one-time reveal fragment containing the token.
	form := strings.NewReader("name=ci-agent")
	req := httptest.NewRequest("POST", "/api/keys", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("create key = %d (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "API key created") {
		t.Errorf("reveal fragment missing: %s", rw.Body.String())
	}

	keys, _ := app.DB.ListAPIKeys("00000000-0000-0000-0000-000000000001")
	if len(keys) != 1 {
		t.Fatalf("expected 1 key after create, got %d", len(keys))
	}

	// Revoke → HX-Refresh.
	revReq := httptest.NewRequest("POST", "/api/keys/"+keys[0].ID+"/revoke", nil)
	revRW := httptest.NewRecorder()
	router.ServeHTTP(revRW, revReq)
	if revRW.Code != http.StatusOK || revRW.Header().Get("HX-Refresh") != "true" {
		t.Errorf("revoke = %d hx=%q", revRW.Code, revRW.Header().Get("HX-Refresh"))
	}
	// Revoking an unknown id → 404.
	badReq := httptest.NewRequest("POST", "/api/keys/nonexistent/revoke", nil)
	badRW := httptest.NewRecorder()
	router.ServeHTTP(badRW, badReq)
	if badRW.Code != http.StatusNotFound {
		t.Errorf("revoke unknown = %d, want 404", badRW.Code)
	}
}
