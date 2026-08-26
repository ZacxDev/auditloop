package crawler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// fixedPlanner returns a scripted action sequence (deterministic, no LLM), repeating
// the final action once exhausted so a slow success probe still terminates.
type fixedPlanner struct {
	actions []action.Action
	i       int
}

func (p *fixedPlanner) NextAction(ctx context.Context, st DriveState) (action.Action, error) {
	if p.i < len(p.actions) {
		a := p.actions[p.i]
		p.i++
		return a, nil
	}
	return action.Action{Type: action.Scroll, Direction: "down"}, nil
}

// TestDriveCapturesPerStepA11yDigest (chromium-gated, Phase 2) proves the drive loop
// captures + records the bounded a11y/DOM digest per step (the SAME a11y-digest.js shape
// the crawl path stores), so materialization can carry it into the driven eval. Every
// recorded step must carry a parseable, non-empty digest.
func TestDriveCapturesPerStepA11yDigest(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	host := mustHost(t, fx.URL)

	trace, err := Drive(context.Background(), DriveOptions{
		BaseURL:      fx.URL + "/",
		AllowedHosts: []string{host},
		Goal:         "reach welcome",
		Success:      action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 4000},
		Planner: &fixedPlanner{actions: []action.Action{
			{Type: action.Navigate, URL: fx.URL + "/welcome", Reason: "go to welcome"},
		}},
		MaxActions:     6,
		ActionTimeout:  15 * time.Second,
		OverallTimeout: 60 * time.Second,
		DryRun:         true,
		AllowLoopback:  true,
		ChromiumPath:   chromium,
	})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if trace.Outcome != "success" || len(trace.Steps) == 0 {
		t.Fatalf("drive outcome=%q steps=%d, want success with steps", trace.Outcome, len(trace.Steps))
	}
	for _, s := range trace.Steps {
		if len(s.A11yDigestJSON) == 0 {
			t.Fatalf("step %d has no captured a11y digest", s.Idx)
		}
		var d report.A11yDigest
		if err := json.Unmarshal(s.A11yDigestJSON, &d); err != nil {
			t.Fatalf("step %d digest not valid JSON: %v", s.Idx, err)
		}
		if d.IsEmpty() {
			t.Fatalf("step %d captured an EMPTY digest (should have interactive els or landmarks)", s.Idx)
		}
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

func TestIsMutatingMethod(t *testing.T) {
	for _, m := range []string{"POST", "post", "Put", "PATCH", "delete"} {
		if !isMutatingMethod(m) {
			t.Errorf("%q should be mutating", m)
		}
	}
	for _, m := range []string{"GET", "HEAD", "get", "OPTIONS", ""} {
		if isMutatingMethod(m) {
			t.Errorf("%q should NOT be mutating", m)
		}
	}
}

func resolveChromiumT(t *testing.T) string {
	if p := os.Getenv("AUDITLOOP_CHROMIUM"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "chrome", "google-chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium/chrome available — skipping browser-backed driver test")
	return ""
}

// digestFixture serves a small page with real interactive elements + a success
// marker on /welcome.
func digestFixture() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Form</title></head>
<body><h1>Sign up</h1>
<form><input id="email" name="email" type="text" placeholder="Your email">
<button id="submit" type="button">Create account</button></form>
<a id="skip" href="/welcome">Skip</a></body></html>`))
	})
	mux.HandleFunc("/welcome", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Welcome</title></head>
<body><h1 id="goal-reached">Welcome</h1></body></html>`))
	})
	return httptest.NewServer(mux)
}

// newDriverTab spins up a headless browser tab for the low-level driver helpers.
func newDriverTab(t *testing.T, chromium string) (context.Context, func()) {
	t.Helper()
	allocOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocOpts = append(allocOpts, chromedp.NoSandbox, chromedp.Headless, chromedp.DisableGPU,
		chromedp.Flag("disable-dev-shm-usage", true), chromedp.ExecPath(chromium))
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(tabCtx, network.Enable(), emulation.SetDeviceMetricsOverride(1440, 900, 1, false)); err != nil {
		cancelTab()
		cancelAlloc()
		t.Fatalf("browser start: %v", err)
	}
	return tabCtx, func() { cancelTab(); cancelAlloc() }
}

func TestBuildInteractiveDigest(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	tabCtx, cleanup := newDriverTab(t, chromium)
	defer cleanup()

	if err := navigate(tabCtx, fx.URL+"/", 20*time.Second); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	d := buildInteractiveDigest(tabCtx)
	if len(d.Elements) == 0 {
		t.Fatal("digest is empty — expected interactive elements")
	}
	var sawInput, sawButton bool
	for _, e := range d.Elements {
		if e.Selector == "input#email" {
			sawInput = true
		}
		if e.Selector == "button#submit" {
			sawButton = true
		}
	}
	if !sawInput || !sawButton {
		t.Fatalf("digest missing real selectors: %+v", d.Elements)
	}
}

func TestCheckAssertion(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	tabCtx, cleanup := newDriverTab(t, chromium)
	defer cleanup()

	if err := navigate(tabCtx, fx.URL+"/", 20*time.Second); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	// A visible selector holds; a missing one times out quickly (false).
	if !CheckAssertion(tabCtx, action.SuccessAssertion{Selector: "#submit", TimeoutMs: 4000}) {
		t.Error("expected #submit to be visible")
	}
	if CheckAssertion(tabCtx, action.SuccessAssertion{Selector: "#does-not-exist", TimeoutMs: 800}) {
		t.Error("missing selector must not satisfy the assertion")
	}
	// Navigate to the goal page; a url_contains assertion holds.
	if err := navigate(tabCtx, fx.URL+"/welcome", 20*time.Second); err != nil {
		t.Fatalf("navigate welcome: %v", err)
	}
	if !CheckAssertion(tabCtx, action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 4000}) {
		t.Error("expected url_contains /welcome to hold")
	}
	if !CheckAssertion(tabCtx, action.SuccessAssertion{Selector: "#goal-reached", TimeoutMs: 4000}) {
		t.Error("expected #goal-reached to be visible")
	}
}
