// Package e2e: P4 authenticated-crawl end-to-end. It stands up a login-GATED
// fixture site (a session cookie unlocks /dashboard and a deeper /dashboard/
// secret link), configures a target with auth_mode=login + a login recipe (via
// the real HTTP save endpoint, so credentials are AES-encrypted at rest), runs
// the crawl, and asserts the gated pages WERE crawled (the session carried into
// the crawl). It also asserts: without a recipe the deep gated page is
// unreachable; the login-test route returns success + a stored screenshot; and a
// wrong-password recipe fails the run with the login-failed message.
//
// Drives real headless Chromium (chromedp) — needs a chromium/chrome binary.
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// loginFixture serves a login-gated site. Correct creds set a session cookie;
// /dashboard and /dashboard/secret require it (else 302 → /login).
func loginFixture() *httptest.Server {
	const (
		user = "alice@example.com"
		pass = "hunter2"
	)
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("sess")
		return err == nil && c.Value == "ok"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Public landing page linking to the gated dashboard.
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Home</title></head>
<body><h1>Public home</h1><a href="/dashboard">Dashboard</a></body></html>`))
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			if r.FormValue("email") == user && r.FormValue("password") == pass {
				http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok", Path: "/"})
				http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
				return
			}
			// Wrong creds: re-render the login form (no cookie, stays on /login).
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Login</title></head>
<body><h1>Login</h1><p class="err">Invalid credentials</p></body></html>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Login</title></head>
<body><h1>Login</h1>
<form action="/login" method="post">
<input id="email" name="email" type="text">
<input id="password" name="password" type="password">
<button id="submit" type="submit">Sign in</button>
</form></body></html>`))
	})
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dashboard" {
			http.NotFound(w, r)
			return
		}
		if !authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		// The deep link only exists on the AUTHED dashboard, so it is discovered
		// (and crawled) ONLY when the session carried into the crawl.
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Dashboard</title></head>
<body><h1 id="dashboard-marker">Welcome back</h1><a href="/dashboard/secret">Secret area</a></body></html>`))
	})
	// /trap 302-redirects to the cloud-metadata address. A recipe may goto /trap
	// (same-domain loopback, passes the save-time guard), but the redirect hop to
	// 169.254.169.254 must be aborted by the RUNTIME request-interception guard —
	// link-local/metadata stays blocked even under the dev loopback exception.
	mux.HandleFunc("/trap", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/dashboard/secret", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Secret</title></head>
<body><h1>Secret area</h1></body></html>`))
	})
	return httptest.NewServer(mux)
}

// e2eEncKey is a deterministic test encryption key (hex, 32 bytes). NEVER a prod key.
const e2eEncKey = "1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f"

func TestEndToEndLoginRecipe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	fixture := loginFixture()
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	tmp := t.TempDir()
	database, err := db.Open("sqlite", filepath.Join(tmp, "e2e.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	store, err := storage.NewFS(filepath.Join(tmp, "art"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	cfg := config.AppConfig{
		Port:               "0",
		Role:               config.RoleAll,
		DatabaseDriver:     "sqlite",
		DatabasePath:       filepath.Join(tmp, "e2e.db"),
		S3Local:            filepath.Join(tmp, "art"),
		CrawlMaxPages:      10,
		CrawlMaxDepth:      3,
		CrawlAllowLoopback: true, // dev/test-only: reach + log into the loopback fixture
		ChromiumPath:       chromium,
		EncryptionKey:      e2eEncKey,
		DevMode:            true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router, err := handlers.NewRouter(ctx, cfg, database, store)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	// --- Target WITH a login recipe (correct creds) ---
	tgt, err := database.CreateTarget(auth.DefaultDevUser, "Gated", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	saveRecipe(t, appSrv.URL, tgt.ID, fixture.URL, "hunter2")

	// Confirm auth_mode flipped and the recipe persisted (encrypted).
	got, _ := database.GetTargetByID(tgt.ID)
	if got.AuthMode != db.AuthLogin {
		t.Fatalf("auth_mode = %q, want login", got.AuthMode)
	}
	lr, err := database.GetLoginRecipe(tgt.ID)
	if err != nil {
		t.Fatalf("GetLoginRecipe: %v", err)
	}
	if strings.Contains(lr.CredsEncrypted, "hunter2") {
		t.Fatal("stored credentials are not encrypted")
	}

	// --- login-test route: success + a stored screenshot ---
	ltBody := postAuthForm(t, appSrv.URL+"/api/targets/"+tgt.ID+"/login-test", url.Values{})
	if !strings.Contains(ltBody, "Login succeeded") {
		t.Fatalf("login-test did not report success:\n%s", ltBody)
	}
	if n := countKeys(t, store, "login-tests/", ".png"); n < 1 {
		t.Fatalf("expected a login-test screenshot stored, found %d", n)
	}

	// --- Run the authenticated crawl ---
	authedURLs := runAndCollectURLs(t, appSrv.URL, database, tgt.ID)
	if !containsURL(authedURLs, "/dashboard") {
		t.Errorf("authed run did not crawl /dashboard: %v", authedURLs)
	}
	if !containsURL(authedURLs, "/dashboard/secret") {
		t.Errorf("authed run did not reach the gated deep link /dashboard/secret (session did not carry): %v", authedURLs)
	}

	// --- Control: a second target WITHOUT a recipe cannot reach the gated deep link ---
	tgt2, _ := database.CreateTarget(auth.DefaultDevUser, "Public", fixture.URL, []string{fixtureHost})
	publicURLs := runAndCollectURLs(t, appSrv.URL, database, tgt2.ID)
	if containsURL(publicURLs, "/dashboard/secret") {
		t.Errorf("unauthenticated run should NOT reach /dashboard/secret (login wall): %v", publicURLs)
	}

	// --- SSRF: a recipe that redirects to the metadata address is BLOCKED at
	// runtime, and login-test returns NO screenshot (exfil primitive removed) ---
	tgtTrap, _ := database.CreateTarget(auth.DefaultDevUser, "Trap", fixture.URL, []string{fixtureHost})
	saveTrapRecipe(t, appSrv.URL, tgtTrap.ID, fixture.URL)
	shotsBefore := countKeys(t, store, "login-tests/", ".png")
	trapBody := postAuthForm(t, appSrv.URL+"/api/targets/"+tgtTrap.ID+"/login-test", url.Values{})
	if strings.Contains(trapBody, "Login succeeded") {
		t.Fatalf("SSRF trap login-test should NOT report success:\n%s", trapBody)
	}
	if shotsAfter := countKeys(t, store, "login-tests/", ".png"); shotsAfter != shotsBefore {
		t.Fatalf("a guard-blocked login-test must NOT store a screenshot (before=%d after=%d)", shotsBefore, shotsAfter)
	}

	// The same guard blocks the metadata redirect during a normal CRAWL run: a
	// target whose base URL redirects to the metadata address yields zero crawled
	// pages (the navigation is aborted; no internal-page screenshots captured).
	tgtCrawlTrap, _ := database.CreateTarget(auth.DefaultDevUser, "CrawlTrap", strings.TrimRight(fixture.URL, "/")+"/trap", []string{fixtureHost})
	crawlTrapURLs := runAndCollectURLs(t, appSrv.URL, database, tgtCrawlTrap.ID)
	for _, u := range crawlTrapURLs {
		if strings.Contains(u, "169.254.169.254") || strings.Contains(u, "meta-data") {
			t.Fatalf("crawl reached a metadata address (SSRF): %v", crawlTrapURLs)
		}
	}

	// --- Wrong password → run fails with the login-failed message ---
	tgt3, _ := database.CreateTarget(auth.DefaultDevUser, "BadCreds", fixture.URL, []string{fixtureHost})
	saveRecipe(t, appSrv.URL, tgt3.ID, fixture.URL, "WRONG-password")
	run3 := runToTerminal(t, appSrv.URL, database, tgt3.ID)
	if run3.Status != db.RunFailed {
		t.Fatalf("wrong-password run status = %q, want failed", run3.Status)
	}
	if !strings.Contains(run3.Error, "login recipe failed") {
		t.Fatalf("wrong-password run error = %q, want login-failed message", run3.Error)
	}
	if strings.Contains(run3.Error, "WRONG-password") {
		t.Fatal("credential value leaked into the run error")
	}
}

// --- helpers ---

func containsURL(urls []string, suffix string) bool {
	for _, u := range urls {
		if strings.HasSuffix(u, suffix) {
			return true
		}
	}
	return false
}

// saveRecipe posts a guided login recipe through the real save endpoint.
func saveRecipe(t *testing.T, appURL, targetID, loginBase, password string) {
	t.Helper()
	vals := url.Values{
		"auth_mode":            {"login"},
		"recipe_mode":          {"guided"},
		"login_url":            {strings.TrimRight(loginBase, "/") + "/login"},
		"username_selector":    {"#email"},
		"password_selector":    {"#password"},
		"submit_selector":      {"#submit"},
		"success_url_contains": {"/dashboard"},
		"success_timeout_ms":   {"15000"},
		"username":             {"alice@example.com"},
		"password":             {password},
	}
	body := postAuthForm(t, appURL+"/api/targets/"+targetID+"/auth", vals)
	_ = body
}

// saveTrapRecipe posts an advanced recipe whose only goto targets /trap (which
// 302-redirects to the metadata address). It passes the save-time guard (the
// goto host is the allowlisted loopback fixture) — the redirect is what must be
// blocked at runtime.
func saveTrapRecipe(t *testing.T, appURL, targetID, fixtureBase string) {
	t.Helper()
	steps := `[{"type":"goto","url":"` + strings.TrimRight(fixtureBase, "/") + `/trap"},` +
		`{"type":"waitFor","url_contains":"/meta-data","timeout_ms":6000}]`
	vals := url.Values{
		"auth_mode":   {"login"},
		"recipe_mode": {"advanced"},
		"steps_json":  {steps},
	}
	postAuthForm(t, appURL+"/api/targets/"+targetID+"/auth", vals)
}

func postAuthForm(t *testing.T, urlStr string, vals url.Values) string {
	t.Helper()
	req, _ := http.NewRequest("POST", urlStr, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", urlStr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("post %s = %d", urlStr, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func runToTerminal(t *testing.T, appURL string, database *db.DB, targetID string) *db.Run {
	t.Helper()
	req, _ := http.NewRequest("POST", appURL+"/api/targets/"+targetID+"/runs", nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger run: %v", err)
	}
	runURL := resp.Header.Get("HX-Redirect")
	resp.Body.Close()
	if !strings.HasPrefix(runURL, "/runs/") {
		t.Fatalf("expected HX-Redirect to /runs/…, got %q", runURL)
	}
	runID := strings.TrimPrefix(runURL, "/runs/")

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(auth.DefaultDevUser, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status == db.RunDone || run.Status == db.RunFailed {
			return run
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state", runID)
	return nil
}

func runAndCollectURLs(t *testing.T, appURL string, database *db.DB, targetID string) []string {
	t.Helper()
	run := runToTerminal(t, appURL, database, targetID)
	if run.Status != db.RunDone {
		t.Fatalf("run status = %q (err=%q), want done", run.Status, run.Error)
	}
	pages, _ := database.ListPages(run.ID)
	seen := map[string]bool{}
	var out []string
	for _, p := range pages {
		if !seen[p.URL] {
			seen[p.URL] = true
			out = append(out, p.URL)
		}
	}
	return out
}

func countKeys(t *testing.T, store storage.Store, contains, suffix string) int {
	t.Helper()
	keys, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list storage: %v", err)
	}
	n := 0
	for _, k := range keys {
		if strings.Contains(k, contains) && strings.HasSuffix(k, suffix) {
			n++
		}
	}
	return n
}
