package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

func testApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.AppConfig{
		Role:           config.RoleWeb, // no worker goroutine in handler tests
		DatabaseDriver: "sqlite",
		DatabasePath:   filepath.Join(tmp, "h.db"),
		S3Local:        filepath.Join(tmp, "art"),
		DevMode:        true,
	}
	database, err := db.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFS(cfg.S3Local)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(context.Background(), cfg, database, store)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, DB: database, Store: store}
	return app, router
}

func TestHealthEndpoints(t *testing.T) {
	_, router := testApp(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rw := httptest.NewRecorder()
		router.ServeHTTP(rw, httptest.NewRequest("GET", path, nil))
		if rw.Code != 200 {
			t.Errorf("%s = %d", path, rw.Code)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, router := testApp(t)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/metrics", nil))
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "go_goroutines") {
		t.Errorf("metrics endpoint bad: %d", rw.Code)
	}
}

func TestDashboardRendersInDevMode(t *testing.T) {
	_, router := testApp(t)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/dashboard", nil))
	if rw.Code != 200 {
		t.Fatalf("dashboard = %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "Add a target") {
		t.Error("dashboard missing add-target form")
	}
}

func TestCreateTargetAndRunFlow(t *testing.T) {
	_, router := testApp(t)

	// Create a target.
	form := strings.NewReader("name=Acme&base_url=https://acme.test")
	req := httptest.NewRequest("POST", "/api/targets", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("create target = %d (%s)", rw.Code, rw.Body.String())
	}
	if rw.Header().Get("HX-Refresh") != "true" {
		t.Error("expected HX-Refresh on target create")
	}

	// Bad URL is rejected.
	badReq := httptest.NewRequest("POST", "/api/targets", strings.NewReader("name=x&base_url=not-a-url"))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badRW := httptest.NewRecorder()
	router.ServeHTTP(badRW, badReq)
	if badRW.Code != http.StatusBadRequest {
		t.Errorf("bad url should 400, got %d", badRW.Code)
	}
}

func TestManifestAndServiceWorker(t *testing.T) {
	_, router := testApp(t)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/manifest.webmanifest", nil))
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "auditloop") {
		t.Errorf("manifest bad: %d", rw.Code)
	}
	swRW := httptest.NewRecorder()
	router.ServeHTTP(swRW, httptest.NewRequest("GET", "/sw.js", nil))
	if swRW.Code != 200 || swRW.Header().Get("Service-Worker-Allowed") != "/" {
		t.Errorf("sw bad: %d hdr=%q", swRW.Code, swRW.Header().Get("Service-Worker-Allowed"))
	}
}

func TestAuthSyncSetsCookie(t *testing.T) {
	app, _ := testApp(t)
	// Simulate a verified request carrying a bearer token; the sync handler must
	// mirror it into the HttpOnly session cookie (the login-loop fix).
	req := httptest.NewRequest("POST", "/api/auth/sync", nil)
	req.Header.Set("Authorization", "Bearer the-access-token")
	req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: "u1", Email: "a@b.com"}))
	rw := httptest.NewRecorder()
	app.handleAuthSync(rw, req)
	if rw.Code != 200 {
		t.Fatalf("sync = %d", rw.Code)
	}
	var found bool
	for _, ck := range rw.Result().Cookies() {
		if ck.Name == auth.SessionCookie && ck.Value == "the-access-token" && ck.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Error("expected HttpOnly session cookie mirroring the bearer token")
	}
}

func TestLoginPagePublic(t *testing.T) {
	_, router := testApp(t)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/login", nil))
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "Sign in") {
		t.Errorf("login page bad: %d", rw.Code)
	}
}
