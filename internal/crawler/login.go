package crawler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/recipe"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// LoginConfig carries a resolved login recipe for a crawl: the canonical steps
// plus the DECRYPTED credential values keyed by placeholder ref (username|
// password). The worker builds this just before crawling; it is never persisted
// or logged. When set on Options, Crawl runs the login in the SAME browser
// context before the BFS crawl, so the authenticated session/cookies carry over.
type LoginConfig struct {
	Steps       []recipe.Step
	Credentials map[string]string // ref → decrypted value (never logged)
}

// ErrLoginFailed is returned when a login recipe does not reach its success
// condition (bad selectors/credentials), when a step targets a blocked/off-
// domain URL, or when the post-login page lands on a foreign domain (SSO). The
// message is safe to surface — it NEVER contains a credential value.
type ErrLoginFailed struct {
	Reason string
}

func (e *ErrLoginFailed) Error() string { return "login recipe failed: " + e.Reason }

// defaultStepTimeout bounds a single fill/click/goto action.
const defaultStepTimeout = 20 * time.Second

// MaxLoginHardBudget is the CEILING on the login watchdog budget, regardless of
// what the recipe asks for. Without it the budget is a function of
// USER-SUPPLIED numbers (step count × per-step waits), so the #41 guarantee
// would degrade from "a stall is bounded" to "a stall ends eventually" on the
// one path where the user picks the numbers — and, because the crawl worker
// loop is single-threaded with no run-level deadline, a stalled login would pin
// the WHOLE worker for that budget. recipe.MaxWaitTimeoutMs caps the input;
// this caps the computed total as defence in depth. 5 minutes is far beyond any
// legitimate form login.
const MaxLoginHardBudget = 5 * time.Minute

// loginHardBudget is the WALL-CLOCK budget the #41 watchdog enforces around a
// whole login recipe: the worst case each step can legitimately take, plus the
// landing-URL check, plus grace — CLAMPED to MaxLoginHardBudget. Every login
// step is a chromedp.Run and can therefore park on the same non-context-aware
// mutex as a page capture, so runLogin needs the same backstop as capturePage.
func loginHardBudget(lc *LoginConfig, grace time.Duration) time.Duration {
	steps := 0
	worstWait := time.Duration(0)
	if lc != nil {
		steps = len(lc.Steps)
		for _, s := range lc.Steps {
			if s.Type == recipe.StepWaitFor {
				ms := s.TimeoutMs
				if ms <= 0 {
					ms = recipe.DefaultWaitTimeoutMs
				}
				if w := time.Duration(ms) * time.Millisecond; w > worstWait {
					worstWait = w
				}
			}
		}
	}
	if grace <= 0 {
		grace = DefaultStallGrace
	}
	// landingCheckTimeout mirrors the 10s chromedp.Location bound in runLogin.
	const landingCheckTimeout = 10 * time.Second
	budget := time.Duration(steps)*(defaultStepTimeout+worstWait) + landingCheckTimeout + grace
	if budget > MaxLoginHardBudget {
		return MaxLoginHardBudget
	}
	return budget
}

// runLogin executes the login steps on the shared tab context, enforcing the
// SSRF/same-domain guard on every goto and on the final landing URL. It returns
// *ErrLoginFailed on any auth/selector/guard failure so callers can distinguish
// a login wall from an infra error.
func runLogin(tabCtx context.Context, lc *LoginConfig, guard GuardConfig) error {
	if lc == nil || len(lc.Steps) == 0 {
		return nil
	}
	// Preflight: refuse a goto to a blocked / off-domain URL BEFORE opening it
	// (same guard the crawl uses — same-domain MVP, SSRF ranges, DNS-rebind).
	for _, raw := range recipe.GotoURLs(lc.Steps) {
		if err := guard.CheckURL(raw); err != nil {
			return &ErrLoginFailed{Reason: "login step url refused (" + err.Error() + ")"}
		}
	}

	for i, s := range lc.Steps {
		if err := runLoginStep(tabCtx, s, lc.Credentials); err != nil {
			return &ErrLoginFailed{Reason: fmt.Sprintf("step %d (%s): %s", i+1, s.Type, err.Error())}
		}
	}

	// After the recipe, the browser must be on an allowed domain. A login that
	// redirected to a third-party IdP (SSO) lands off-domain → unsupported.
	var landing string
	lctx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
	defer cancel()
	if err := chromedp.Run(lctx, chromedp.Location(&landing)); err == nil && landing != "" {
		if !hostAllowedIn(landing, guard.AllowedHosts) {
			return &ErrLoginFailed{Reason: "post-login page is on a different domain (third-party SSO is not supported)"}
		}
	}
	return nil
}

// runLoginStep executes one canonical step under a per-step timeout.
func runLoginStep(tabCtx context.Context, s recipe.Step, creds map[string]string) error {
	switch s.Type {
	case recipe.StepGoto:
		ctx, cancel := context.WithTimeout(tabCtx, defaultStepTimeout)
		defer cancel()
		return chromedp.Run(ctx,
			chromedp.Navigate(s.URL),
			chromedp.WaitReady("body", chromedp.ByQuery),
		)
	case recipe.StepFill:
		val, ok := creds[s.ValueRef]
		if !ok {
			// Never echo the value; ref names are non-secret.
			return fmt.Errorf("no credential supplied for %q", s.ValueRef)
		}
		ctx, cancel := context.WithTimeout(tabCtx, defaultStepTimeout)
		defer cancel()
		return chromedp.Run(ctx,
			chromedp.WaitVisible(s.Selector, chromedp.ByQuery),
			chromedp.Click(s.Selector, chromedp.ByQuery), // focus the field
			chromedp.SendKeys(s.Selector, val, chromedp.ByQuery),
		)
	case recipe.StepClick:
		ctx, cancel := context.WithTimeout(tabCtx, defaultStepTimeout)
		defer cancel()
		return chromedp.Run(ctx, chromedp.Click(s.Selector, chromedp.ByQuery))
	case recipe.StepWaitFor:
		return waitForSuccess(tabCtx, s)
	default:
		return fmt.Errorf("unknown step type %q", s.Type)
	}
}

// waitForSuccess blocks until the login recipe's success condition holds or the
// step timeout elapses. It delegates to the shared CheckAssertion (selector-
// visible OR url-contains within the timeout) — ONE implementation reused by the
// P4 login flow and the Phase-3 walkthrough success check.
func waitForSuccess(tabCtx context.Context, s recipe.Step) error {
	to := s.TimeoutMs
	if to <= 0 {
		to = recipe.DefaultWaitTimeoutMs
	}
	if CheckAssertion(tabCtx, action.SuccessAssertion{Selector: s.Selector, URLContains: s.URLContains, TimeoutMs: to}) {
		return nil
	}
	return fmt.Errorf("success condition not met (selector %q / url %q)", strings.TrimSpace(s.Selector), strings.TrimSpace(s.URLContains))
}

func pollURLContains(ctx context.Context, needle string) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		var loc string
		if err := chromedp.Run(ctx, chromedp.Location(&loc)); err == nil && strings.Contains(loc, needle) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("url did not contain %q in time", needle)
		case <-tick.C:
		}
	}
}

// LoginProbeOptions configures a standalone login test (login-test route): it
// runs ONLY the login steps in a fresh headless browser and captures a
// screenshot of the end state — no crawl.
type LoginProbeOptions struct {
	Login         *LoginConfig
	AllowedHosts  []string
	ChromiumPath  string
	AllowLoopback bool
	// InternalAllowHosts is the exact-match internal-host allowlist (see
	// GuardConfig.InternalAllowHosts) — empty in normal deployments. Login
	// redirects honor the same allowlist.
	InternalAllowHosts []string
	Resolve            func(host string) ([]net.IP, error)
	Timeout            time.Duration
	// StallGrace / ProbeTimeout / SkipRenderProbe configure the #41 hard
	// watchdog + startup render probe (see the crawler.Options equivalents).
	StallGrace      time.Duration
	ProbeTimeout    time.Duration
	SkipRenderProbe bool
}

// LoginProbeResult is the outcome of a login test. OK is true when the recipe
// reached its success condition. Screenshot is a PNG of the final page (captured
// whether or not login succeeded, so the user can see where it landed).
// FailReason is a credential-free explanation when OK is false.
type LoginProbeResult struct {
	OK         bool
	EndURL     string
	Screenshot []byte
	FailReason string
}

// RunLoginProbe runs the login steps in an isolated headless browser and returns
// pass/fail plus an end-state screenshot. It only returns a non-nil error for
// infrastructure failures (browser launch, screenshot) — a failed LOGIN is a
// normal result (OK=false, FailReason set).
func RunLoginProbe(ctx context.Context, opts LoginProbeOptions) (*LoginProbeResult, error) {
	if opts.Login == nil || len(opts.Login.Steps) == 0 {
		return nil, fmt.Errorf("no login recipe to test")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 90 * time.Second
	}
	guard := GuardConfig{AllowedHosts: opts.AllowedHosts, Resolve: opts.Resolve, AllowLoopback: opts.AllowLoopback, InternalAllowHosts: InternalAllowSet(opts.InternalAllowHosts)}

	allocOpts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocOpts = append(allocOpts,
		chromedp.NoSandbox,
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	if opts.ChromiumPath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(opts.ChromiumPath))
	}
	runCtx, cancelRun := context.WithTimeout(ctx, opts.Timeout)
	defer cancelRun()
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(runCtx, allocOpts...)
	defer cancelAlloc()
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	killBrowser := func() { cancelTab(); cancelAlloc() }
	grace := opts.StallGrace
	if grace <= 0 {
		grace = DefaultStallGrace
	}
	if err := runHard("start", tabCtx, grace, killBrowser); err != nil {
		return nil, fmt.Errorf("browser start: %w", err)
	}
	// Startup render probe (#41) — a font-less environment fails here in seconds
	// with an actionable message instead of hanging the login test.
	if !opts.SkipRenderProbe {
		if err := probeBrowser(tabCtx, opts.ProbeTimeout, killBrowser); err != nil {
			return nil, err
		}
	}

	// Runtime SSRF guard: abort any navigation (incl. a redirect hop) to an
	// internal/metadata address. This is what stops a save-time-clean recipe from
	// pivoting to 169.254.169.254 / 10.x via a 302 during the probe.
	ic, err := enableInterception(tabCtx, guard, false, grace, killBrowser)
	if err != nil {
		return nil, fmt.Errorf("enable request interception: %w", err)
	}

	res := &LoginProbeResult{OK: true}
	// Bounded by the #41 hard watchdog: a login step can park on chromedp's
	// non-context-aware mutex, which runCtx's deadline cannot unwind.
	err = runBounded("login", loginHardBudget(opts.Login, grace), killBrowser,
		func() error { return runLogin(tabCtx, opts.Login, guard) })
	if err != nil {
		res.OK = false
		var lf *ErrLoginFailed
		switch {
		case errors.Is(err, ErrBrowserStalled):
			res.FailReason = StallHint
			return res, nil // browser is dead: no end URL, no screenshot
		case errors.As(err, &lf):
			res.FailReason = lf.Reason
		default:
			res.FailReason = err.Error()
		}
	}

	// Record the end URL (best-effort) regardless of outcome. Watchdog-bounded:
	// this runs AFTER the login budget, so an un-wrapped Run here would be a
	// fresh unbounded hang on an otherwise-covered path.
	_ = runHard("login", tabCtx, grace, killBrowser, chromedp.Location(&res.EndURL))

	// SCREENSHOT SUPPRESSION (SSRF exfil defense): only ever capture/return a
	// screenshot for a LEGITIMATE outcome on an allowed, IP-clean page — i.e. a
	// genuine selector/credential failure that still landed on an allowlisted
	// host with no guard block. If the runtime guard aborted a navigation, or the
	// page ended off-domain (third-party SSO / redirect pivot), we NEVER capture
	// the end state — otherwise an attacker could exfil a screenshot of internal
	// pages or cloud metadata even when the login "failed".
	if bn, blocked := ic.lastBlocked(); blocked {
		res.OK = false
		res.FailReason = "navigation blocked by the SSRF guard (redirect to a private/internal address: " + bn.Reason + ")"
		return res, nil // no screenshot
	}
	if !hostAllowedIn(res.EndURL, opts.AllowedHosts) {
		// Landed off the allowed domain (or nowhere) — do not screenshot.
		if res.OK {
			// Should not happen (runLogin rejects off-domain landings), but be safe.
			res.OK = false
			if res.FailReason == "" {
				res.FailReason = "login ended on a non-allowlisted page; screenshot withheld"
			}
		}
		return res, nil
	}

	shot, err := screenshotEndState(tabCtx, grace, killBrowser)
	if err != nil {
		// A screenshot failure shouldn't mask the login result; report it only if
		// the login itself was fine.
		if res.OK {
			return res, fmt.Errorf("end-state screenshot: %w", err)
		}
	}
	res.Screenshot = shot
	return res, nil
}

func screenshotEndState(tabCtx context.Context, grace time.Duration, kill func()) ([]byte, error) {
	const shotTimeout = 15 * time.Second
	ctx, cancel := context.WithTimeout(tabCtx, shotTimeout)
	defer cancel()
	var out []byte
	// Watchdog-bounded (#41): a capture can park on the same non-context-aware
	// mutex as any other chromedp call.
	err := runHard("capture", ctx, shotTimeout+grace, kill, chromedp.ActionFunc(func(c context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(c)
		if err != nil {
			return err
		}
		out = b
		return nil
	}))
	return out, err
}
