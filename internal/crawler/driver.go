package crawler

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// Package driver (Phase 3): the DETERMINISTIC goal-directed walkthrough driver.
// It drives headless Chromium toward a goal using a CLOSED action set, with an
// injected Planner (the LLM lives OUTSIDE crawler, so tests pass a scripted fake
// and the loop is deterministic). The success/stuck signal is deterministic and
// OBSERVED (a SuccessAssertion), never taken on the planner's word. The SSRF/same-
// origin guard runs on every navigation + redirect (reused from the crawl/login
// paths, for the WHOLE pass incl. login); an optional dry-run submit-guard aborts
// mutating (non-GET/HEAD) top-frame HTTP(S) requests at the network layer — armed
// AFTER the login phase so a login POST still authenticates, then active for the
// drive loop. It covers form + XHR/fetch/beacon writes in the top frame; WS
// messages, cross-origin (OOPIF) iframes, and service-worker requests are NOT
// covered (see intercept.go) — point real-submit driving at staging.

// Planner produces the next action to execute given the current DriveState. It is
// injected so the crawler package has no LLM dependency and tests are hermetic.
type Planner interface {
	NextAction(ctx context.Context, st DriveState) (action.Action, error)
}

// DriveState is the per-turn context handed to the planner.
type DriveState struct {
	Goal          string
	StepIdx       int // 0-based index of the action being decided
	MaxActions    int
	URL           string
	ScreenshotPNG []byte
	Digest        InteractiveDigest
	History       []StepRecord
	LastOutcome   string // the previous step's outcome + any repeat/no-progress nudge
}

// StepRecord is one recorded action in the deterministic trace. ScreenshotPNG is
// the OBSERVATION the planner saw when it chose Action (captured BEFORE the action
// runs — so a guard-blocked navigation is never screenshotted, no exfil).
type StepRecord struct {
	Idx           int
	Action        action.Action
	URL           string
	ScreenshotPNG []byte
	Outcome       string
	PlannerReason string
	// A11yDigestJSON is the bounded DOM/accessibility digest (a11y-digest.js output)
	// captured on this step's page, BEFORE the action ran — the SAME shape the crawl
	// path stores (report.A11yDigest). Phase 2: it is persisted per step and carried
	// into the synthetic run's pages so the persona evaluator gets authoritative
	// semantic grounding on the DRIVEN path (not "no a11y measured"). Empty/nil when
	// capture failed or the page was hostile — the eval degrades to screenshot-only.
	A11yDigestJSON []byte
}

// DriveTrace is the deterministic outcome of a pass.
type DriveTrace struct {
	Outcome   string // success|stuck|failed
	StuckStep int
	Reason    string
	Steps     []StepRecord
	// InfraFailed marks a failure of the DRIVER/BROWSER rather than of the audited
	// product: the session watchdog killed a stalled browser, or the login phase
	// stalled. Such a pass produced NO OBSERVATION of the goal, so it must not be
	// scored as a walkthrough regression (issue #45). It is set at exactly the
	// in-band points where the driver already knows the cause is infrastructure —
	// never on a genuine product-side failure (a bad login recipe, an off-domain
	// navigation, a guard block, a planner refusal).
	InfraFailed bool
}

// ErrDriverInfra marks an out-of-band driver error (returned as `(nil, err)`) as an
// INFRASTRUCTURE failure — the browser could not be brought up, so the pass says
// nothing about the audited product. Wrapped around browser start, the startup
// render probe, and interception enablement.
var ErrDriverInfra = errors.New("driver infrastructure failure")

// IsInfraFailure reports whether a driver error is an infrastructure failure (a
// stalled/killed browser or a browser that could not be brought up) rather than a
// product-side one. Callers (walkthrough.Generator) use it to flag the persisted
// walkthrough so the Phase-4 regression diff does not score a non-observation.
func IsInfraFailure(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrBrowserStalled) || errors.Is(err, ErrDriverInfra)
}

// markStalled records a browser STALL on a trace — the ONE definition of what a
// stall means to the caller: a failed outcome, the shared operator hint, and the
// structural InfraFailed signal (#45) that keeps a non-observation out of the
// Phase-4 regression score.
//
// It is a function, not two literals, because BOTH stall sites (the session
// watchdog in Drive and the login phase in driveSession) must agree, and a
// duplicated literal is exactly how one of them silently loses the flag.
func markStalled(t *DriveTrace) *DriveTrace {
	t.Outcome = "failed"
	t.Reason = StallHint
	t.InfraFailed = true
	return t
}

// applyLoginOutcome translates the login phase's error into the trace's terminal
// state and reports whether the drive must stop. It is the login-phase half of the
// infra-vs-product classification (#45):
//
//   - a STALL is INFRASTRUCTURE (markStalled) — reporting it as "login recipe
//     failed" would send the user to debug selectors and credentials for what is
//     actually a dead browser, and would score a non-observation as a regression;
//   - any other login error is a REAL, PRODUCT-SIDE failure (a bad recipe, a wrong
//     credential, an off-domain SSO landing) and is deliberately NOT flagged infra.
func applyLoginOutcome(trace *DriveTrace, lerr error) bool {
	if lerr == nil {
		return false
	}
	if errors.Is(lerr, ErrBrowserStalled) {
		markStalled(trace)
		return true
	}
	trace.Outcome = "failed"
	trace.Reason = "login recipe failed: " + loginFailReason(lerr)
	return true
}

// DriveOptions configures a walkthrough driver pass.
type DriveOptions struct {
	BaseURL        string
	AllowedHosts   []string
	Login          *LoginConfig
	Goal           string
	Success        action.SuccessAssertion
	Planner        Planner
	MaxActions     int
	ActionTimeout  time.Duration
	OverallTimeout time.Duration
	// DryRun turns on the submit-guard (abort mutating requests). Default-safe: the
	// caller passes true unless the target has the loud allow_real_submit flag on.
	DryRun        bool
	AllowLoopback bool
	// InternalAllowHosts is the exact-match internal-host allowlist (see
	// GuardConfig.InternalAllowHosts) — empty in normal deployments.
	InternalAllowHosts []string
	Resolve            func(host string) ([]net.IP, error)
	ChromiumPath       string
	// StallGrace / ProbeTimeout / SkipRenderProbe configure the #41 hard
	// watchdog + startup render probe (see the crawler.Options equivalents).
	StallGrace      time.Duration
	ProbeTimeout    time.Duration
	SkipRenderProbe bool
}

const (
	defaultMaxActions     = 20
	defaultActionTimeout  = 20 * time.Second
	defaultOverallTimeout = 3 * time.Minute
	// defaultAssertionTimeout bounds a success/waitFor check when none is given.
	defaultAssertionTimeout = 15 * time.Second
	// loopProbeTimeout is the SHORT per-turn success probe (the full Success timeout
	// applies only to an explicit finish/waitFor).
	loopProbeTimeout = 1200 * time.Millisecond
	// repeatLimit short-circuits toward stuck after this many consecutive
	// no-progress repeats (the planner is spinning).
	repeatLimit = 3
	// settleDelay lets an async click-triggered navigation (and the interceptor's
	// off-goroutine abort/block decision) land before the outcome is judged.
	settleDelay = 700 * time.Millisecond
)

//go:embed interactive-digest.js
var interactiveDigestScript string

// InteractiveElement is one visible interactive element the planner can drive.
type InteractiveElement struct {
	Tag      string `json:"tag"`
	Role     string `json:"role"`
	Name     string `json:"name"`
	Selector string `json:"selector"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
}

// InteractiveDigest is the bounded snapshot of the page's interactive elements.
type InteractiveDigest struct {
	Elements []InteractiveElement `json:"elements"`
}

// Drive runs the goal-directed walkthrough. It NEVER returns a non-nil error for a
// normal outcome (success/stuck/failed are all in the trace); a non-nil error is
// reserved for infrastructure failures (browser launch). The whole pass runs on
// the browser's DEFAULT tab (the captureBeyondViewport gotcha) with the runtime
// SSRF interception guard active.
func Drive(ctx context.Context, opts DriveOptions) (*DriveTrace, error) {
	if opts.Planner == nil {
		// Every (nil, err) return from the driver is INFRA (#45): the drive never
		// ran, so it is not evidence about the audited product. Unreachable today
		// (the generator always injects a planner) but the rule holds without
		// exception, so a future caller cannot silently create a scored failure.
		return nil, fmt.Errorf("driver: no planner (%w)", ErrDriverInfra)
	}
	if opts.MaxActions <= 0 {
		opts.MaxActions = defaultMaxActions
	}
	if opts.ActionTimeout <= 0 {
		opts.ActionTimeout = defaultActionTimeout
	}
	if opts.OverallTimeout <= 0 {
		opts.OverallTimeout = defaultOverallTimeout
	}
	grace := opts.StallGrace
	if grace <= 0 {
		grace = DefaultStallGrace
	}

	// SESSION-LEVEL watchdog (#41). The drive loop makes many chromedp calls, each
	// under its own ActionTimeout — but ANY of them can park on chromedp's
	// non-context-aware internal mutex, which no inner deadline (nor runCtx's
	// OverallTimeout) can unwind. Rather than wrapping each of the ~10 call sites,
	// the WHOLE session gets one hard wall-clock bound: past OverallTimeout+grace
	// the browser is killed and the walkthrough reports a deterministic failure.
	// Coarser than the crawl's per-page watchdog, but the drive is already a single
	// bounded unit of work, so the granularity buys nothing here.
	var killMu sync.Mutex
	var killBrowser func()
	setKill := func(f func()) { killMu.Lock(); killBrowser = f; killMu.Unlock() }
	doKill := func() {
		killMu.Lock()
		f := killBrowser
		killMu.Unlock()
		if f != nil {
			f()
		}
	}

	var trace *DriveTrace
	var derr error
	if werr := runBounded("drive", opts.OverallTimeout+grace, doKill, func() error {
		trace, derr = driveSession(ctx, opts, grace, setKill)
		return derr
	}); errors.Is(werr, ErrBrowserStalled) {
		// The stalled goroutine is abandoned; do NOT read trace/derr (it may still
		// be writing them). Report a deterministic failure instead of hanging.
		// HONEST LIMIT: any steps the abandoned goroutine had already driven are
		// DISCARDED — reading its trace here would be a data race with a goroutine
		// that is still writing it. Do not "recover" them.
		// markStalled sets Outcome/Reason AND the InfraFailed signal: the browser was
		// killed by the watchdog, so this pass observed nothing about the product (#45).
		return markStalled(&DriveTrace{}), nil
	}
	return trace, derr
}

// driveSession is the real drive loop. setKill publishes the browser-kill func to
// the watchdog in Drive as soon as the browser exists.
func driveSession(ctx context.Context, opts DriveOptions, grace time.Duration, setKill func(func())) (*DriveTrace, error) {
	guard := GuardConfig{AllowedHosts: opts.AllowedHosts, Resolve: opts.Resolve, AllowLoopback: opts.AllowLoopback, InternalAllowHosts: InternalAllowSet(opts.InternalAllowHosts)}

	runCtx, cancelRun := context.WithTimeout(ctx, opts.OverallTimeout)
	defer cancelRun()

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
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(runCtx, allocOpts...)
	defer cancelAlloc()
	// DEFAULT tab only (the captureBeyondViewport-on-2nd-tab hang gotcha).
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	// Publish the kill lever to the session watchdog in Drive (#41).
	setKill(func() { cancelTab(); cancelAlloc(); cancelRun() })
	if err := runHard("start", tabCtx, grace, func() { cancelTab(); cancelAlloc() }, network.Enable(),
		emulation.SetDeviceMetricsOverride(1440, 900, 1, false)); err != nil {
		// Infra: the browser could not be brought up — no claim about the site (#45).
		return nil, fmt.Errorf("driver: browser start: %w (%w)", err, ErrDriverInfra)
	}
	// Startup render probe (#41): fail fast + loudly on a browser that cannot
	// render text (the font-less case) instead of burning the whole drive budget.
	if !opts.SkipRenderProbe {
		if err := probeBrowser(tabCtx, opts.ProbeTimeout, func() { cancelTab(); cancelAlloc() }); err != nil {
			// Infra: the render probe failed (font-less browser) — no claim about the site.
			return nil, fmt.Errorf("driver: %w (%w)", err, ErrDriverInfra)
		}
	}

	// Runtime SSRF guard. The IP/redirect guard is active for the ENTIRE pass (login
	// AND drive loop). The dry-run MUTATION submit-guard is deliberately NOT armed yet:
	// the optional P4 login POSTs its credentials, and arming the mutation-abort before
	// login would abort that POST → the login fails and the whole authenticated
	// walkthrough breaks in the safe default. We arm it AFTER login, before the drive
	// loop (below), so the funnel is still fully submit-guarded — only the login phase
	// is exempt. One Fetch listener serves both phases.
	ic, err := enableInterception(tabCtx, guard, false, grace, func() { cancelTab(); cancelAlloc() })
	if err != nil {
		// Infra: the browser could not be instrumented — no claim about the site.
		return nil, fmt.Errorf("driver: enable interception: %w (%w)", err, ErrDriverInfra)
	}

	trace := &DriveTrace{}

	// Optional P4 login FIRST (same tab → session carries into the drive). The SSRF
	// guard is live here (a login goto that 302s to a private/metadata IP is still
	// aborted); only the mutation submit-guard is exempt so the login POST goes through.
	if opts.Login != nil && len(opts.Login.Steps) > 0 {
		// The session watchdog in Drive already bounds this; the inner budget keeps
		// a stuck login from consuming the whole drive budget before it fires.
		lerr := runBounded("login", loginHardBudget(opts.Login, grace), func() { cancelTab(); cancelAlloc() },
			func() error { return runLogin(tabCtx, opts.Login, guard) })
		// applyLoginOutcome owns the stall-vs-recipe-failure classification (and the
		// #45 InfraFailed flag) so it is one tested definition, not a branch inlined
		// here where only an end-to-end browser stall could reach it.
		if applyLoginOutcome(trace, lerr) {
			return trace, nil
		}
	}

	// Now that login (if any) has authenticated, ARM the dry-run mutation submit-guard
	// so every subsequent funnel action is submit-blocked in dry-run. For a
	// non-login pass this arms before the very first action (login is skipped). The
	// caller passes DryRun=!allow_real_submit, so the safe default (dry-run) blocks.
	if opts.DryRun {
		ic.armMutationGuard()
	}

	// Navigate to the base URL (pre-navigation guard + the runtime interception).
	if gerr := guard.CheckURL(opts.BaseURL); gerr != nil {
		trace.Outcome = "failed"
		trace.Reason = "start url refused by the guard: " + gerr.Error()
		return trace, nil
	}
	if nerr := navigate(tabCtx, opts.BaseURL, opts.ActionTimeout); nerr != nil {
		if ic.blockedCount() > 0 {
			trace.Outcome = "failed"
			trace.Reason = "start navigation blocked by the SSRF guard"
			return trace, nil
		}
		trace.Outcome = "failed"
		trace.Reason = "could not load the start url: " + truncateErr(nerr)
		return trace, nil
	}

	seen := map[string]int{}
	noProgress := 0
	lastOutcome := ""
	prevURL := ""

	for stepIdx := 0; stepIdx < opts.MaxActions; stepIdx++ {
		if runCtx.Err() != nil {
			break // overall timeout → stuck (handled after the loop)
		}

		curURL := currentURL(tabCtx)

		// DETERMINISTIC success check (observed) BEFORE asking the planner — a short
		// per-turn probe (the full Success timeout applies only to explicit
		// waitFor/finish). If already at the goal, we're done.
		if !opts.Success.IsZero() && CheckAssertion(tabCtx, probeAssertion(opts.Success)) {
			shot := safeShot(tabCtx)
			trace.Steps = append(trace.Steps, StepRecord{
				Idx: stepIdx, Action: action.Action{Type: action.Finish}, URL: curURL,
				ScreenshotPNG: shot, Outcome: "success", A11yDigestJSON: captureA11yDigest(tabCtx),
			})
			trace.Outcome = "success"
			return trace, nil
		}

		shot := safeShot(tabCtx)
		digest := buildInteractiveDigest(tabCtx)
		// Phase 2: capture the fuller a11y/DOM digest (a11y-digest.js) for evaluator
		// grounding on this step's page. One extra Evaluate on the SAME default tab
		// (no second tab — respects the chromium-149 capture gotcha), best-effort.
		a11yDigest := captureA11yDigest(tabCtx)

		st := DriveState{
			Goal: opts.Goal, StepIdx: stepIdx, MaxActions: opts.MaxActions,
			URL: curURL, ScreenshotPNG: shot, Digest: digest,
			History: trace.Steps, LastOutcome: lastOutcome,
		}
		act, perr := opts.Planner.NextAction(runCtx, st)
		if perr != nil {
			trace.Outcome = "failed"
			trace.Reason = "planner error: " + truncateErr(perr)
			trace.StuckStep = stepIdx
			return trace, nil
		}

		rec := StepRecord{Idx: stepIdx, Action: act, URL: curURL, ScreenshotPNG: shot, PlannerReason: act.Reason, A11yDigestJSON: a11yDigest}

		if verr := action.Validate(act); verr != nil {
			rec.Outcome = "rejected: " + verr.Error()
			trace.Steps = append(trace.Steps, rec)
			lastOutcome = rec.Outcome
			continue // count toward the budget; the planner may correct next turn
		}

		// finish is ADVISORY: only the assertion firing makes it a success.
		if act.Type == action.Finish {
			rec.Outcome = "finish requested"
			if !opts.Success.IsZero() && CheckAssertion(tabCtx, opts.Success) {
				rec.Outcome = "success"
				trace.Steps = append(trace.Steps, rec)
				trace.Outcome = "success"
				return trace, nil
			}
			// Planner gave up (or claimed done without the goal met) → stuck here.
			trace.Steps = append(trace.Steps, rec)
			trace.Outcome = "stuck"
			trace.StuckStep = len(trace.Steps)
			trace.Reason = "planner finished without the success condition being met"
			return trace, nil
		}

		// Repeat / no-progress bookkeeping (before executing, keyed on the URL the
		// action was chosen at).
		key := act.DedupKey(curURL)
		repeat := seen[key] > 0
		seen[key]++

		// Execute the action, tracking any dry-run mutation abort or SSRF block.
		blockedBefore := ic.blockedCount()
		mutBefore := ic.mutationCount()
		aerr := execAction(tabCtx, act, guard, opts.ActionTimeout)
		// Let async navigations settle: a click that submits a form issues its
		// request AFTER the click returns, and the interceptor's abort/block decision
		// runs on its own goroutine — so we must settle BEFORE judging the outcome or
		// a dry-run submit-block / SSRF-block would be missed.
		_ = chromedp.Run(tabCtx, chromedp.Sleep(settleDelay))

		if ic.blockedCount() > blockedBefore {
			rec.Outcome = "ssrf-blocked"
			trace.Steps = append(trace.Steps, rec)
			trace.Outcome = "failed"
			trace.StuckStep = len(trace.Steps)
			trace.Reason = "a navigation was blocked by the SSRF guard (private/internal address)"
			return trace, nil
		}
		if act.Type == action.Navigate && aerr != nil {
			// A navigate that failed the pre-navigation host/SSRF guard (off-domain).
			var be *ErrBlockedURL
			if asBlocked(aerr, &be) {
				rec.Outcome = "refused: " + be.Reason
				trace.Steps = append(trace.Steps, rec)
				trace.Outcome = "failed"
				trace.StuckStep = len(trace.Steps)
				trace.Reason = "navigation refused by the same-origin/SSRF guard: " + be.Reason
				return trace, nil
			}
		}

		switch {
		case ic.mutationCount() > mutBefore:
			rec.Outcome = "submit-blocked (dry-run)"
		case aerr != nil:
			rec.Outcome = "error: " + truncateErr(aerr)
		default:
			rec.Outcome = "ok"
		}
		trace.Steps = append(trace.Steps, rec)
		lastOutcome = rec.Outcome

		// No-progress detection: a repeated action at a URL that did not change.
		newURL := currentURL(tabCtx)
		if repeat && newURL == curURL {
			noProgress++
			lastOutcome = rec.Outcome + " — you already tried this action here and it did not change the page; try something DIFFERENT or finish."
		} else {
			noProgress = 0
		}
		if noProgress >= repeatLimit {
			trace.Outcome = "stuck"
			trace.StuckStep = len(trace.Steps)
			trace.Reason = fmt.Sprintf("no progress after %d repeated actions", noProgress)
			return trace, nil
		}
		_ = prevURL
		prevURL = curURL
	}

	// Budget or overall timeout exhausted without reaching the goal.
	if !opts.Success.IsZero() && CheckAssertion(tabCtx, probeAssertion(opts.Success)) {
		trace.Outcome = "success"
		return trace, nil
	}
	trace.Outcome = "stuck"
	trace.StuckStep = len(trace.Steps)
	if trace.Reason == "" {
		trace.Reason = "reached the action budget without meeting the success condition"
	}
	return trace, nil
}

// CheckAssertion reports whether a success condition currently holds: Selector
// becomes visible OR the URL contains URLContains, within TimeoutMs. This is the
// ONE shared deterministic success check reused by the P4 login flow
// (waitForSuccess) and the Phase-3 driver.
func CheckAssertion(tabCtx context.Context, sa action.SuccessAssertion) bool {
	to := time.Duration(sa.TimeoutMs) * time.Millisecond
	if to <= 0 {
		to = defaultAssertionTimeout
	}
	ctx, cancel := context.WithTimeout(tabCtx, to)
	defer cancel()
	return assertionHolds(ctx, sa.Selector, sa.URLContains)
}

// probeAssertion returns a copy of sa with a SHORT timeout for the per-turn loop
// probe (so each turn doesn't stall for the full success timeout).
func probeAssertion(sa action.SuccessAssertion) action.SuccessAssertion {
	sa.TimeoutMs = int(loopProbeTimeout / time.Millisecond)
	return sa
}

// assertionHolds races the configured checks: selector-visible and/or url-contains.
// Returns true as soon as one is satisfied, false when ctx expires with neither.
func assertionHolds(ctx context.Context, selector, urlContains string) bool {
	selector = strings.TrimSpace(selector)
	urlContains = strings.TrimSpace(urlContains)
	done := make(chan bool, 2)
	n := 0
	if selector != "" {
		n++
		go func() { done <- chromedp.Run(ctx, chromedp.WaitVisible(selector, chromedp.ByQuery)) == nil }()
	}
	if urlContains != "" {
		n++
		go func() { done <- pollURLContains(ctx, urlContains) == nil }()
	}
	if n == 0 {
		return false
	}
	for i := 0; i < n; i++ {
		if <-done {
			return true
		}
	}
	return false
}

// buildInteractiveDigest evaluates the vendored digest script and returns the
// bounded list of visible interactive elements. Its OUTPUT is untrusted — the
// selectors round-trip ONLY through chromedp's selector engine, never eval. On any
// error it returns an empty digest (the planner degrades to the screenshot).
func buildInteractiveDigest(tabCtx context.Context) InteractiveDigest {
	ctx, cancel := context.WithTimeout(tabCtx, 8*time.Second)
	defer cancel()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(interactiveDigestScript, &raw)); err != nil || raw == "" {
		return InteractiveDigest{}
	}
	var d InteractiveDigest
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return InteractiveDigest{}
	}
	return d
}

// captureA11yDigest evaluates the vendored a11y/DOM digest script (a11y-digest.js —
// the SAME one the crawl path uses) and returns its bounded JSON bytes for
// persona-evaluator grounding on the DRIVEN path (Phase 2). It runs on the SAME
// default tab as everything else (one extra Evaluate — NO second tab, respecting the
// chromium-149 capture gotcha) and is best-effort/non-fatal: on any error, an empty
// payload, or an over-cap payload (the output is UNTRUSTED/page-authored) it returns
// nil and the eval degrades to screenshot-only for that step.
func captureA11yDigest(tabCtx context.Context) []byte {
	ctx, cancel := context.WithTimeout(tabCtx, 8*time.Second)
	defer cancel()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(a11yDigestSource, &raw)); err != nil || raw == "" {
		return nil
	}
	if len(raw) > report.MaxA11yDigestBytes {
		return nil
	}
	return []byte(raw)
}

// execAction executes ONE validated action by SELECTOR under an action timeout.
// It never evals planner-authored text: selectors go through chromedp's selector
// engine; scrolling uses a FIXED (non-model) script; a press uses a mapped, closed
// key set.
func execAction(tabCtx context.Context, a action.Action, guard GuardConfig, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(tabCtx, timeout)
	defer cancel()
	switch a.Type {
	case action.Click:
		return chromedp.Run(ctx, chromedp.Click(a.Selector, chromedp.ByQuery))
	case action.TypeText:
		return chromedp.Run(ctx,
			chromedp.WaitVisible(a.Selector, chromedp.ByQuery),
			chromedp.Click(a.Selector, chromedp.ByQuery),
			chromedp.SendKeys(a.Selector, a.Text, chromedp.ByQuery),
		)
	case action.Press:
		key, ok := mappedKey(a.Key)
		if !ok {
			return fmt.Errorf("unsupported key %q", a.Key)
		}
		return chromedp.Run(ctx, chromedp.KeyEvent(key))
	case action.Select:
		return chromedp.Run(ctx, chromedp.SetValue(a.Selector, a.Value, chromedp.ByQuery))
	case action.Scroll:
		// FIXED script (our constant, not model input) — scroll one viewport.
		script := "window.scrollBy(0, window.innerHeight*0.85)"
		if a.Direction == "up" {
			script = "window.scrollBy(0, -window.innerHeight*0.85)"
		}
		return chromedp.Run(ctx, chromedp.Evaluate(script, nil))
	case action.WaitFor:
		if CheckAssertion(ctx, action.SuccessAssertion{Selector: a.Selector, URLContains: a.URLContains, TimeoutMs: a.TimeoutMs}) {
			return nil
		}
		return fmt.Errorf("waitFor condition not met in time")
	case action.Navigate:
		if err := guard.CheckURL(a.URL); err != nil {
			return err // *ErrBlockedURL — same-origin/SSRF refusal
		}
		return navigate(ctx, a.URL, timeout)
	default:
		return fmt.Errorf("unsupported action %q", a.Type)
	}
}

// navigate loads a URL and waits for the body (under its own bounded context).
func navigate(tabCtx context.Context, rawURL string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(tabCtx, timeout)
	defer cancel()
	return chromedp.Run(ctx, chromedp.Navigate(rawURL), chromedp.WaitReady("body", chromedp.ByQuery))
}

func currentURL(tabCtx context.Context) string {
	ctx, cancel := context.WithTimeout(tabCtx, 5*time.Second)
	defer cancel()
	var loc string
	_ = chromedp.Run(ctx, chromedp.Location(&loc))
	return loc
}

// safeShot captures the visible viewport (best effort; nil on failure).
func safeShot(tabCtx context.Context) []byte {
	// hard=0 ⇒ no per-call watchdog here BY DESIGN: safeShot runs inside the drive
	// loop, which Drive already bounds at the SESSION level (OverallTimeout+grace).
	// A second watchdog would only duplicate the kill.
	b, err := screenshotEndState(tabCtx, 0, nil)
	if err != nil {
		return nil
	}
	return b
}

// mappedKey maps an allowlisted key name to its chromedp key sequence.
func mappedKey(name string) (string, bool) {
	switch name {
	case "Enter":
		return kb.Enter, true
	case "Tab":
		return kb.Tab, true
	case "Escape":
		return kb.Escape, true
	case "ArrowUp":
		return kb.ArrowUp, true
	case "ArrowDown":
		return kb.ArrowDown, true
	default:
		return "", false
	}
}

func loginFailReason(err error) string {
	if lf, ok := err.(*ErrLoginFailed); ok {
		return lf.Reason
	}
	return err.Error()
}

func asBlocked(err error, target **ErrBlockedURL) bool {
	if be, ok := err.(*ErrBlockedURL); ok {
		*target = be
		return true
	}
	return false
}

func truncateErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
