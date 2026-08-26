package handlers

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/recipe"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// testKey is a deterministic 32-byte hex key for handler tests (NEVER a prod key).
const testKey = "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"

func testAppWithKey(t *testing.T) (*App, http.Handler) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.AppConfig{
		Role:           config.RoleWeb,
		DatabaseDriver: "sqlite",
		DatabasePath:   filepath.Join(tmp, "h.db"),
		S3Local:        filepath.Join(tmp, "art"),
		EncryptionKey:  testKey,
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
	if _, err := hex.DecodeString(testKey); err != nil {
		t.Fatal(err)
	}
	return app, router
}

// testHost is a public documentation-range IP literal (TEST-NET-3, RFC5737). As
// a literal IP the SSRF guard checks it directly WITHOUT a DNS lookup, so the
// save-validation happy path is hermetic (no network). It is never actually
// crawled in these handler tests.
const testHost = "203.0.113.10"

func guidedForm() url.Values {
	return url.Values{
		"auth_mode":          {"login"},
		"recipe_mode":        {"guided"},
		"login_url":          {"http://" + testHost + "/login"},
		"username_selector":  {"#email"},
		"password_selector":  {"#password"},
		"submit_selector":    {"button[type=submit]"},
		"success_selector":   {"nav.dashboard"},
		"success_timeout_ms": {"12000"},
		"username":           {"alice@acme.test"},
		"password":           {"SuperSecret123"},
	}
}

// loginTarget creates a target whose verified domain is the literal test host.
func loginTarget(t *testing.T, app *App) *db.Target {
	t.Helper()
	tgt, err := app.DB.CreateTarget(auth.DefaultDevUser, "Acme", "http://"+testHost, []string{testHost})
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func postForm(router http.Handler, path string, vals url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, req)
	return rw
}

func TestSaveAuthDisabledReturns503(t *testing.T) {
	// testApp has no encryption key → feature disabled.
	app, router := testApp(t)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Acme", "https://acme.test", []string{"acme.test"})
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", guidedForm())
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when feature disabled, got %d", rw.Code)
	}
}

func TestSaveGuidedRecipeEncryptsCreds(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)

	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", guidedForm())
	if rw.Code != http.StatusOK {
		t.Fatalf("save = %d (%s)", rw.Code, rw.Body.String())
	}
	if rw.Header().Get("HX-Refresh") != "true" {
		t.Error("expected HX-Refresh on save")
	}

	// auth_mode flipped, recipe stored.
	got, _ := app.DB.GetTargetByID(tgt.ID)
	if got.AuthMode != db.AuthLogin {
		t.Fatalf("auth_mode = %q", got.AuthMode)
	}
	lr, err := app.DB.GetLoginRecipe(tgt.ID)
	if err != nil {
		t.Fatalf("GetLoginRecipe: %v", err)
	}

	// Stored blob must not contain the plaintext credentials.
	if strings.Contains(lr.CredsEncrypted, "SuperSecret123") || strings.Contains(lr.CredsEncrypted, "alice@acme.test") {
		t.Fatal("encrypted blob leaks plaintext credentials")
	}
	// Steps JSON must carry placeholders, not values.
	if strings.Contains(lr.StepsJSON, "SuperSecret123") || strings.Contains(lr.StepsJSON, "alice@acme.test") {
		t.Fatal("steps_json leaks credential values")
	}
	if !strings.Contains(lr.StepsJSON, recipe.RefPassword) {
		t.Error("steps_json should reference the password placeholder")
	}
}

func TestSaveRejectsForeignDomain(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)

	vals := guidedForm()
	vals.Set("login_url", "https://evil.attacker.test/login") // off-domain
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", vals)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for foreign login domain, got %d (%s)", rw.Code, rw.Body.String())
	}
	if _, err := app.DB.GetLoginRecipe(tgt.ID); err != db.ErrNotFound {
		t.Fatal("a rejected recipe must not be persisted")
	}
}

func TestSaveRejectsPrivateIPLoginURL(t *testing.T) {
	app, router := testAppWithKey(t)
	// Register the private IP in the allowlist so the rejection is proven to come
	// from the SSRF IP guard, not merely the host allowlist.
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Acme", "https://acme.test", []string{"acme.test", "10.0.0.5"})

	vals := guidedForm()
	vals.Set("login_url", "http://10.0.0.5/login")
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", vals)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for private-IP login URL, got %d (%s)", rw.Code, rw.Body.String())
	}
}

func TestSaveRejectsUnknownStepTypeAdvanced(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)

	vals := url.Values{
		"auth_mode":   {"login"},
		"recipe_mode": {"advanced"},
		"steps_json":  {`[{"type":"goto","url":"https://acme.test/login"},{"type":"eval","selector":"x"}]`},
		"username":    {"a"}, "password": {"b"},
	}
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", vals)
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown step type, got %d (%s)", rw.Code, rw.Body.String())
	}
}

func TestClearAuthMode(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)
	if rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", guidedForm()); rw.Code != 200 {
		t.Fatalf("save = %d", rw.Code)
	}
	// Now clear it.
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", url.Values{"auth_mode": {"none"}})
	if rw.Code != 200 {
		t.Fatalf("clear = %d (%s)", rw.Code, rw.Body.String())
	}
	got, _ := app.DB.GetTargetByID(tgt.ID)
	if got.AuthMode != db.AuthNone {
		t.Fatalf("auth_mode after clear = %q", got.AuthMode)
	}
	if _, err := app.DB.GetLoginRecipe(tgt.ID); err != db.ErrNotFound {
		t.Fatal("recipe should be deleted on clear")
	}
}

// TestSaveAuthRejectsOversizedBody asserts the MaxBytesReader cap on the auth
// route: a body over 64KiB is rejected (413) before parsing.
func TestSaveAuthRejectsOversizedBody(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)

	vals := guidedForm()
	vals.Set("success_selector", strings.Repeat("a", 70<<10)) // ~70KiB → over the 64KiB cap
	rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", vals)
	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d (%s)", rw.Code, rw.Body.String())
	}
	if _, err := app.DB.GetLoginRecipe(tgt.ID); err != db.ErrNotFound {
		t.Fatal("an oversized/rejected save must not persist a recipe")
	}
}

// TestLoginTestRateLimited asserts the per-user throttle on /login-test. The
// first call (no recipe saved) returns 200 without spawning a browser but still
// consumes a token; the immediate second call is 429. Hermetic — no chromium.
func TestLoginTestRateLimited(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)

	first := postForm(router, "/api/targets/"+tgt.ID+"/login-test", url.Values{})
	if first.Code != http.StatusOK {
		t.Fatalf("first login-test = %d (%s)", first.Code, first.Body.String())
	}
	second := postForm(router, "/api/targets/"+tgt.ID+"/login-test", url.Values{})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second login-test should be rate-limited (429), got %d", second.Code)
	}
}

// TestTargetPageRedactsCredentials asserts the rendered target page NEVER
// contains stored credential values — only the write-only "set" affordance.
func TestTargetPageRedactsCredentials(t *testing.T) {
	app, router := testAppWithKey(t)
	tgt := loginTarget(t, app)
	if rw := postForm(router, "/api/targets/"+tgt.ID+"/auth", guidedForm()); rw.Code != 200 {
		t.Fatalf("save = %d (%s)", rw.Code, rw.Body.String())
	}

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/targets/"+tgt.ID, nil))
	if rw.Code != 200 {
		t.Fatalf("target view = %d", rw.Code)
	}
	body := rw.Body.String()
	if strings.Contains(body, "SuperSecret123") {
		t.Fatal("rendered target page leaked the password")
	}
	if strings.Contains(body, "alice@acme.test") {
		t.Fatal("rendered target page leaked the username")
	}
	// The write-only affordance + Authentication section render.
	if !strings.Contains(body, "Authentication") {
		t.Error("expected an Authentication section")
	}
	if !strings.Contains(body, "(set") {
		t.Error("expected the write-only '•••• (set)' affordance")
	}
}
