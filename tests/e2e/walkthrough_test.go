// Package e2e: Phase-3 goal-directed walkthrough DRIVER end-to-end. It stands up a
// small funnel fixture (landing → form → /welcome success marker) over loopback and
// drives it with the REAL chromedp crawler.Drive loop, using a SCRIPTED fake
// Planner so the loop is deterministic (no LLM). It asserts the deterministic
// backbone: reaching success, getting stuck at the budget (no false success), the
// SSRF/off-domain refusal (no metadata screenshot exfil), the dry-run submit-guard
// (a POST-gated success is blocked in dry-run, succeeds with real submits), and the
// repeat/no-progress short-circuit.
//
// Drives real headless Chromium (chromedp) — needs a chromium/chrome binary.
package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/recipe"
)

// scriptedPlanner returns a fixed action sequence, repeating the last action once
// exhausted (so repeat/no-progress detection is exercised).
type scriptedPlanner struct {
	actions []action.Action
	i       int
}

func (p *scriptedPlanner) NextAction(ctx context.Context, st crawler.DriveState) (action.Action, error) {
	if len(p.actions) == 0 {
		return action.Action{Type: action.Finish}, nil
	}
	if p.i < len(p.actions) {
		a := p.actions[p.i]
		p.i++
		return a, nil
	}
	return p.actions[len(p.actions)-1], nil
}

// funnelFixture: a landing page, a form whose submit POSTs to /submit (302 →
// /welcome), the /welcome success marker, and a /trap that 302-redirects to the
// cloud-metadata address (SSRF).
func funnelFixture() *httptest.Server {
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
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
	})
	mux.HandleFunc("/welcome", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Welcome</title></head>
<body><h1 id="goal-reached">Welcome — account created</h1></body></html>`))
	})
	mux.HandleFunc("/trap", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	return httptest.NewServer(mux)
}

func TestEndToEndWalkthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)
	fx := funnelFixture()
	defer fx.Close()
	host := hostOnly(fx.URL)

	base := func(planner crawler.Planner, dryRun bool, maxActions int) crawler.DriveOptions {
		return crawler.DriveOptions{
			BaseURL:        fx.URL + "/",
			AllowedHosts:   []string{host},
			Goal:           "create an account",
			Success:        action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 4000},
			Planner:        planner,
			MaxActions:     maxActions,
			ActionTimeout:  15 * time.Second,
			OverallTimeout: 60 * time.Second,
			DryRun:         dryRun,
			AllowLoopback:  true,
			ChromiumPath:   chromium,
		}
	}

	// --- 1. Reaches success (real submissions) ---
	t.Run("success", func(t *testing.T) {
		p := &scriptedPlanner{actions: []action.Action{
			{Type: action.Navigate, URL: fx.URL + "/form", Reason: "go to signup"},
			{Type: action.TypeText, Selector: "#email", Text: "alice@example.com", Reason: "enter email"},
			{Type: action.Click, Selector: "#submit", Reason: "submit"},
		}}
		tr, err := crawler.Drive(context.Background(), base(p, false, 10))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if tr.Outcome != "success" {
			t.Fatalf("outcome = %q reason=%q, want success", tr.Outcome, tr.Reason)
		}
		if len(tr.Steps) == 0 {
			t.Fatal("expected recorded steps")
		}
		// At least one step carries a screenshot observation.
		var shots int
		for _, s := range tr.Steps {
			if len(s.ScreenshotPNG) > 0 {
				shots++
			}
		}
		if shots == 0 {
			t.Fatal("expected step screenshots in the trace")
		}
	})

	// --- 2. Stuck at the budget, NO false success ---
	t.Run("stuck-at-budget", func(t *testing.T) {
		p := &scriptedPlanner{actions: []action.Action{{Type: action.Scroll, Direction: "down", Reason: "look around"}}}
		tr, err := crawler.Drive(context.Background(), base(p, true, 3))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if tr.Outcome == "success" {
			t.Fatal("must NOT report success when the goal was never reached")
		}
		if tr.Outcome != "stuck" {
			t.Fatalf("outcome = %q, want stuck", tr.Outcome)
		}
		if tr.StuckStep == 0 {
			t.Fatal("stuck outcome must carry a StuckStep")
		}
	})

	// --- 3. SSRF: a navigate to /trap (302 → metadata) is refused, no exfil ---
	t.Run("ssrf-trap-refused", func(t *testing.T) {
		p := &scriptedPlanner{actions: []action.Action{{Type: action.Navigate, URL: fx.URL + "/trap", Reason: "explore"}}}
		tr, err := crawler.Drive(context.Background(), base(p, true, 5))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if tr.Outcome != "failed" {
			t.Fatalf("SSRF trap outcome = %q, want failed", tr.Outcome)
		}
		for _, s := range tr.Steps {
			if strings.Contains(s.URL, "169.254.169.254") || strings.Contains(s.URL, "meta-data") {
				t.Fatalf("a step captured a metadata URL (exfil): %+v", s)
			}
		}
	})

	// --- 4. Off-domain navigate is refused ---
	t.Run("off-domain-refused", func(t *testing.T) {
		p := &scriptedPlanner{actions: []action.Action{{Type: action.Navigate, URL: "http://evil.example.com/", Reason: "wander off"}}}
		tr, err := crawler.Drive(context.Background(), base(p, true, 5))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if tr.Outcome != "failed" {
			t.Fatalf("off-domain outcome = %q, want failed", tr.Outcome)
		}
	})

	// --- 6. Authenticated (auth_mode=login) dry-run drive: the LOGIN phase is
	// exempt from the mutation submit-guard (its form POST must authenticate), while
	// the post-login FUNNEL is still submit-guarded, and the SSRF guard stays active
	// during login. This is the safe default for driving login-gated funnels. ---
	authT := &authFunnel{user: "alice@example.com", pass: "hunter2"}
	afx := authT.server()
	defer afx.Close()
	ahost := hostOnly(afx.URL)
	authBase := func(login *crawler.LoginConfig, planner crawler.Planner, dryRun bool, maxActions int) crawler.DriveOptions {
		return crawler.DriveOptions{
			BaseURL:        afx.URL + "/app",
			AllowedHosts:   []string{ahost},
			Login:          login,
			Goal:           "complete the gated action",
			Success:        action.SuccessAssertion{URLContains: "/app/done", TimeoutMs: 4000},
			Planner:        planner,
			MaxActions:     maxActions,
			ActionTimeout:  15 * time.Second,
			OverallTimeout: 60 * time.Second,
			DryRun:         dryRun,
			AllowLoopback:  true,
			ChromiumPath:   chromium,
		}
	}

	t.Run("authed-dry-run-login-exempt-funnel-guarded", func(t *testing.T) {
		login := authT.loginConfig(afx.URL, authT.pass)
		// Post-login the browser is on /app; the planner drives the gated funnel: a
		// click that POSTs to /app/submit. In dry-run that POST must be submit-blocked.
		p := &scriptedPlanner{actions: []action.Action{
			{Type: action.Click, Selector: "#app-submit", Reason: "complete the gated action"},
		}}
		tr, err := crawler.Drive(context.Background(), authBase(login, p, true, 6))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		// The login POST must have SUCCEEDED (not been submit-blocked): a login failure
		// surfaces as Outcome=failed with a "login recipe failed" reason.
		if tr.Outcome == "failed" && strings.Contains(tr.Reason, "login recipe failed") {
			t.Fatalf("login was blocked in dry-run (should be exempt): %q", tr.Reason)
		}
		// The drive reached the GATED funnel (only reachable with the auth session).
		var reachedFunnel bool
		for _, s := range tr.Steps {
			if strings.Contains(s.URL, "/app") {
				reachedFunnel = true
			}
		}
		if !reachedFunnel {
			t.Fatalf("drive did not reach the gated /app funnel: %+v", tr.Steps)
		}
		// The funnel mutation IS still submit-blocked (dry-run guards the funnel), so a
		// POST-gated success is never reached → stuck, not success.
		if tr.Outcome == "success" {
			t.Fatal("dry-run must NOT reach the POST-gated funnel success")
		}
		var funnelBlocked bool
		for _, s := range tr.Steps {
			if strings.Contains(s.Outcome, "submit-blocked") {
				funnelBlocked = true
			}
		}
		if !funnelBlocked {
			t.Fatalf("expected the funnel POST to be submit-blocked in dry-run: %+v", tr.Steps)
		}
	})

	// --- 7. SSRF stays active during LOGIN: a login goto that 302s to the metadata
	// address is aborted by the runtime interception guard even though the mutation
	// submit-guard is exempt for login. The goto is an allowlisted-host loopback GET,
	// so ONLY the SSRF guard can refuse it. ---
	t.Run("ssrf-active-during-login", func(t *testing.T) {
		trapLogin := &crawler.LoginConfig{
			Steps: []recipe.Step{
				{Type: recipe.StepGoto, URL: afx.URL + "/trap"},
				{Type: recipe.StepWaitFor, URLContains: "/app", TimeoutMs: 4000},
			},
		}
		p := &scriptedPlanner{actions: []action.Action{{Type: action.Scroll, Direction: "down", Reason: "look"}}}
		tr, err := crawler.Drive(context.Background(), authBase(trapLogin, p, true, 4))
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if tr.Outcome != "failed" || !strings.Contains(tr.Reason, "login recipe failed") {
			t.Fatalf("SSRF login goto should fail the login: outcome=%q reason=%q", tr.Outcome, tr.Reason)
		}
		for _, s := range tr.Steps {
			if strings.Contains(s.URL, "169.254.169.254") || strings.Contains(s.URL, "meta-data") {
				t.Fatalf("a step captured a metadata URL during login (exfil): %+v", s)
			}
		}
	})

	// --- 5. Dry-run submit-guard: a POST-gated success is blocked in dry-run
	// (→ stuck, with a submit-blocked step + no-progress short-circuit), then
	// succeeds with real submits. Also exercises repeat/dedup. ---
	t.Run("dry-run-submit-guard", func(t *testing.T) {
		mkPlanner := func() *scriptedPlanner {
			return &scriptedPlanner{actions: []action.Action{
				{Type: action.Navigate, URL: fx.URL + "/form", Reason: "go to signup"},
				{Type: action.TypeText, Selector: "#email", Text: "bob@example.com", Reason: "email"},
				{Type: action.Click, Selector: "#submit", Reason: "submit"},
			}}
		}
		// Dry run: the submit POST is aborted → never reaches /welcome → stuck.
		trDry, err := crawler.Drive(context.Background(), base(mkPlanner(), true, 8))
		if err != nil {
			t.Fatalf("drive dry: %v", err)
		}
		if trDry.Outcome == "success" {
			t.Fatal("dry-run must NOT reach a POST-gated success")
		}
		var blocked bool
		for _, s := range trDry.Steps {
			if strings.Contains(s.Outcome, "submit-blocked") {
				blocked = true
			}
		}
		if !blocked {
			t.Fatalf("expected a submit-blocked (dry-run) step, got: %+v", trDry.Steps)
		}

		// Real submissions allowed → the POST goes through → success.
		trReal, err := crawler.Drive(context.Background(), base(mkPlanner(), false, 8))
		if err != nil {
			t.Fatalf("drive real: %v", err)
		}
		if trReal.Outcome != "success" {
			t.Fatalf("real-submit outcome = %q reason=%q, want success", trReal.Outcome, trReal.Reason)
		}
	})
}

// authFunnel is a login-GATED funnel fixture: /login (POST authenticates → cookie →
// 303 /app), /app (gated; a form that POSTs to /app/submit → 303 /app/done), the
// /app/done success marker, and /trap (302 → the cloud-metadata address, for the
// login-phase SSRF check).
type authFunnel struct {
	user, pass string
}

func (a *authFunnel) authed(r *http.Request) bool {
	c, err := r.Cookie("sess")
	return err == nil && c.Value == "ok"
}

func (a *authFunnel) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			if r.FormValue("email") == a.user && r.FormValue("password") == a.pass {
				http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok", Path: "/"})
				http.Redirect(w, r, "/app", http.StatusSeeOther)
				return
			}
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
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			http.NotFound(w, r)
			return
		}
		if !a.authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		// Gated funnel — a POST-gated action (only reachable with the session).
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>App</title></head>
<body><h1 id="app-marker">Gated app</h1>
<form action="/app/submit" method="post">
<button id="app-submit" type="submit">Complete action</button>
</form></body></html>`))
	})
	mux.HandleFunc("/app/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !a.authed(r) {
			http.Redirect(w, r, "/app", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/app/done", http.StatusSeeOther)
	})
	mux.HandleFunc("/app/done", func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Done</title></head>
<body><h1 id="goal-reached">Action complete</h1></body></html>`))
	})
	mux.HandleFunc("/trap", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	return httptest.NewServer(mux)
}

// loginConfig builds a canonical login recipe (goto /login → fill → click → waitFor
// /app) with decrypted credentials, exactly as the worker/generator would pass it.
func (a *authFunnel) loginConfig(base, password string) *crawler.LoginConfig {
	return &crawler.LoginConfig{
		Steps: []recipe.Step{
			{Type: recipe.StepGoto, URL: strings.TrimRight(base, "/") + "/login"},
			{Type: recipe.StepFill, Selector: "#email", ValueRef: recipe.RefUsername},
			{Type: recipe.StepFill, Selector: "#password", ValueRef: recipe.RefPassword},
			{Type: recipe.StepClick, Selector: "#submit"},
			{Type: recipe.StepWaitFor, URLContains: "/app", TimeoutMs: 6000},
		},
		Credentials: map[string]string{
			recipe.RefUsername: a.user,
			recipe.RefPassword: password,
		},
	}
}
