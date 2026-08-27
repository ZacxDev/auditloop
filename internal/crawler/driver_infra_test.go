package crawler

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/metrics"

	"github.com/chromedp/chromedp"
	dto "github.com/prometheus/client_model/go"
)

// TestIsInfraFailure pins the STRUCTURAL infra-failure predicate (#45): a walkthrough
// only skips regression scoring when the driver says so through a sentinel — never
// through a substring match on the reason prose.
func TestIsInfraFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not an infra failure", nil, false},
		{"a stalled browser is infra", ErrBrowserStalled, true},
		{"a wrapped stalled browser is infra", fmt.Errorf("drive: %w", ErrBrowserStalled), true},
		{"the driver-infra sentinel is infra", ErrDriverInfra, true},
		{"a wrapped driver-infra sentinel is infra", fmt.Errorf("driver: browser start: %w", ErrDriverInfra), true},
		{"a plain error is NOT infra", errors.New("driver: no planner"), false},
		{"a login failure is NOT infra", errors.New("login recipe failed: selector not found"), false},
		// The failure mode this replaces: the stall HINT text alone must not be enough —
		// the predicate is structural, not a prose match.
		{"the stall hint text alone is NOT infra", errors.New(StallHint), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInfraFailure(tc.err); got != tc.want {
				t.Errorf("IsInfraFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDriveNoPlannerIsInfra covers the one out-of-band return that is not a browser
// failure: it still never ran the product, so it must satisfy the same predicate.
func TestDriveNoPlannerIsInfra(t *testing.T) {
	tr, err := Drive(context.Background(), DriveOptions{BaseURL: "https://x.test/"})
	if tr != nil {
		t.Fatalf("trace = %+v, want nil", tr)
	}
	if !IsInfraFailure(err) {
		t.Fatalf("a missing planner must be an infra failure, got %v", err)
	}
}

// TestMarkStalledSetsTheInfraSignal pins the ONE definition both stall sites use.
// Mutating any of the three assignments in markStalled fails here.
func TestMarkStalledSetsTheInfraSignal(t *testing.T) {
	tr := markStalled(&DriveTrace{})
	if tr.Outcome != "failed" {
		t.Errorf("Outcome = %q, want failed", tr.Outcome)
	}
	if tr.Reason != StallHint {
		t.Errorf("Reason = %q, want the shared StallHint", tr.Reason)
	}
	if !tr.InfraFailed {
		t.Error("InfraFailed = false — a killed browser observed nothing about the product (#45)")
	}
	// It must not invent steps: the stalled goroutine's trace is deliberately discarded.
	if len(tr.Steps) != 0 {
		t.Errorf("Steps = %d, want 0", len(tr.Steps))
	}
}

// TestApplyLoginOutcomeClassifiesStallVsRecipe is the LOGIN-PHASE half of the #45
// classification, on the real production function driveSession calls. The recipe-failure
// case is the DISCRIMINATING CONTROL: without it, a mutant that flags EVERY login
// failure as infra (silently suppressing a real product regression) would survive.
func TestApplyLoginOutcomeClassifiesStallVsRecipe(t *testing.T) {
	// nil → the drive continues, nothing recorded.
	cont := &DriveTrace{}
	if applyLoginOutcome(cont, nil) {
		t.Fatal("a successful login must not stop the drive")
	}
	if cont.Outcome != "" || cont.InfraFailed {
		t.Fatalf("a successful login must leave the trace untouched: %+v", cont)
	}

	// A STALL is infrastructure.
	stalled := &DriveTrace{}
	if !applyLoginOutcome(stalled, fmt.Errorf("login: %w", ErrBrowserStalled)) {
		t.Fatal("a stalled login must stop the drive")
	}
	if !stalled.InfraFailed {
		t.Error("a login-phase browser stall must set InfraFailed (#45)")
	}
	if stalled.Outcome != "failed" || stalled.Reason != StallHint {
		t.Errorf("stall trace = %+v, want failed + the StallHint", stalled)
	}

	// CONTROL: a real recipe/credential failure is PRODUCT-side and must NOT be
	// flagged infra — otherwise a broken login would silently stop being a regression.
	recipeFail := &DriveTrace{}
	if !applyLoginOutcome(recipeFail, &ErrLoginFailed{Reason: "step 2 (fill): selector not found"}) {
		t.Fatal("a failed login recipe must stop the drive")
	}
	if recipeFail.InfraFailed {
		t.Error("a login RECIPE failure must NOT be flagged infra — it is a real product-side failure")
	}
	if recipeFail.Outcome != "failed" {
		t.Errorf("Outcome = %q, want failed", recipeFail.Outcome)
	}
	if recipeFail.Reason == StallHint || recipeFail.Reason == "" {
		t.Errorf("Reason = %q, want the login-recipe diagnosis", recipeFail.Reason)
	}
}

// readStalls reads auditloop_browser_stalls_total{phase} directly (no
// prometheus/testutil — it would pull a new module into go.sum for one assertion).
// Process-global, so tests read a DELTA. It is what makes the "which watchdog fired"
// claims below MEASURED on every run instead of asserted in a comment.
func readStalls(t *testing.T, phase string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.BrowserStalls.WithLabelValues(phase).Write(&m); err != nil {
		t.Fatalf("read stall counter %q: %v", phase, err)
	}
	return m.GetCounter().GetValue()
}

// assertStalledTrace pins the shared expectation: a stall yields the deterministic
// failed/StallHint trace AND the #45 InfraFailed signal, with no invented steps.
func assertStalledTrace(t *testing.T, tr *DriveTrace, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Drive returned an error, want a deterministic trace: %v", err)
	}
	if tr == nil {
		t.Fatal("Drive returned a nil trace")
	}
	if !tr.InfraFailed {
		t.Fatalf("a stalled drive must be InfraFailed (#45) — got %+v", tr)
	}
	if tr.Outcome != "failed" || tr.Reason != StallHint {
		t.Errorf("stalled trace = %+v, want failed + StallHint", tr)
	}
	if len(tr.Steps) != 0 {
		t.Errorf("Steps = %d, want 0 — the abandoned goroutine's steps are discarded", len(tr.Steps))
	}
}

// TestDriveSessionWatchdogReturnsAnInfraFailedTrace exercises the REAL #41 SESSION
// watchdog: the injected planner parks forever ignoring its context (the
// non-context-aware hang shape), so no inner deadline can unwind it and only
// runBounded("drive", OverallTimeout+StallGrace) can end the session — firing its
// doKill lever and ABANDONING the drive goroutine. Everything else is real: a real
// browser, real interception, a real navigation.
//
// MEASURED, not inferred: the asserted deltas (phase=drive 1, phase=start 0) are what
// make the sentence above a checked claim rather than a comment that can rot.
func TestDriveSessionWatchdogReturnsAnInfraFailedTrace(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	beforeDrive, beforeStart := readStalls(t, "drive"), readStalls(t, "start")

	start := time.Now()
	tr, err := Drive(context.Background(), DriveOptions{
		BaseURL:      fx.URL + "/",
		AllowedHosts: []string{"127.0.0.1"},
		// The planner is the hang: the drive loop parks inside NextAction past any
		// context deadline, exactly as chromedp's internal mutex does in production.
		Planner: &parkingPlanner{},
		Success: action.SuccessAssertion{URLContains: "/never-reached", TimeoutMs: 200},
		// TIMING, and why this number (#50). This test cannot be faster than the whole
		// pre-planner path, because the SESSION watchdog it exercises only fires at
		// OverallTimeout+StallGrace, and OverallTimeout ALSO bounds everything before
		// the parking planner is reached (runCtx). The original 1s raced that path:
		// first the START watchdog pre-empted the session one, then — once startup got
		// its own budget — runCtx expired mid-dial ("browser start: context deadline
		// exceeded"). One number was standing in for three unrelated budgets.
		//
		// The budget must exceed TIME-TO-PLANNER, which is NOT the same as startup
		// time — a first estimate sized this against startup alone (~1.7s) and was
		// wrong by ~6x, because ~1.4s of the path is post-navigation work the planner
		// waits on (the success probe, safeShot, buildInteractiveDigest,
		// captureA11yDigest). MEASURED end-to-end by instrumenting driveSession:
		//
		//   idle (load ~19):      2.67s  3.06s  3.51s
		//   loaded (load 22-32):  3.34s … 4.91s  6.07s   <- worst observed
		//
		// 20s is ~3.3x the worst observed on a deliberately oversubscribed 8-core box.
		// The earlier 8s left only ~1.3x, which is a threshold moved rather than a
		// dependency removed — and this test exists precisely because a moved
		// threshold comes back. The cost is ~20s of suite time, paid for determinism.
		//
		// NOTE the adjacent guarantee is conditional: the parking planner makes the
		// watchdog fire deterministically ONLY once the planner is reached. If the
		// budget is ever cut below time-to-planner again, this test fails with a
		// message blaming the driver rather than the budget — which is what the
		// numbers above are here to prevent.
		OverallTimeout: 20 * time.Second,
		StallGrace:     500 * time.Millisecond,
		// StartBudget stays at its generous default ON PURPOSE: it must not be the
		// thing that fires here. The assertions below (drive 1, start 0) are exactly
		// the claim that was flaking.
		SkipRenderProbe: true,
		AllowLoopback:   true,
		ChromiumPath:    chromium,
	})
	assertStalledTrace(t, tr, err)

	if d := readStalls(t, "drive") - beforeDrive; d != 1 {
		t.Errorf("phase=drive stall delta = %v, want 1 — this test must exercise the SESSION watchdog", d)
	}
	if d := readStalls(t, "start") - beforeStart; d != 0 {
		t.Errorf("phase=start stall delta = %v, want 0 — an inner watchdog fired instead of the session one", d)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("watchdog took %s — not bounded", elapsed)
	}
}

// parkingPlanner blocks forever inside NextAction, ignoring ctx. Only a wall-clock
// watchdog outside the drive loop can end a session that reaches it.
type parkingPlanner struct{}

func (p *parkingPlanner) NextAction(ctx context.Context, st DriveState) (action.Action, error) {
	select {} // never returns, never observes ctx
}

// TestDriveInnerStallReturnsAnInfraFailedTrace covers the OTHER production shape: a
// stall inside one of the bounded STARTUP calls (here enableInterception's
// fetch.Enable, whose own runHard fires first), which returns an
// ErrBrowserStalled-wrapping ERROR up to Drive. Drive must classify that as a stall
// too — it reaches the same line via errors.Is on the RETURNED ERROR, not via the
// session timer.
//
// MEASURED: an earlier revision of this file claimed this test exercised the SESSION
// watchdog. An instrumented probe showed phase=drive 0, phase=start 1 — the claim was
// wrong. The asserted deltas below (start 1, drive 0) are the mirror image of the
// session test and exist so that claim can never silently drift again.
func TestDriveInnerStallReturnsAnInfraFailedTrace(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	beforeDrive, beforeStart := readStalls(t, "drive"), readStalls(t, "start")

	// The seam lets the FIRST chromedp call (browser start) through to a real browser,
	// then parks on an already-held, non-context-aware mutex — the #41 shape.
	park := stuckForever()
	var calls int32
	setRunTasks(func(ctx context.Context, actions ...chromedp.Action) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return chromedp.Run(ctx, actions...)
		}
		return park()
	})
	t.Cleanup(func() { setRunTasks(nil) })

	tr, err := Drive(context.Background(), DriveOptions{
		BaseURL:        fx.URL + "/",
		AllowedHosts:   []string{"127.0.0.1"},
		Planner:        &fixedPlanner{},
		Success:        action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 500},
		OverallTimeout: 20 * time.Second, // deliberately far from the inner budget
		// This test is the MIRROR of the session one: here the START budget is the
		// short one, because the stall being exercised is inside a startup call
		// (enableInterception's fetch.Enable) and its own runHard must be what fires.
		// Stating it explicitly is the point of #50 — which watchdog a test exercises
		// is now a declared property, not a side effect of one shared knob.
		//
		// StallGrace is deliberately LARGE and different from StartBudget, so that what
		// fires here can only be StartBudget. With both at 500ms the test could not
		// tell the two knobs apart at all.
		//
		// MEASURED, and the reason this test pins only the FIRST startup call site:
		// 500ms is SHORTER than a real chromium launch, so the watchdog fires while
		// call 1 (driver.go's network.Enable, which is what actually boots the browser)
		// is still in flight — enableInterception is never reached. That is deliberate
		// here; the sibling test below
		// (TestDriveEnableInterceptionHonoursItsOwnStartBudget) covers the second
		// startup call site by giving the browser room to boot first.
		StallGrace:      10 * time.Second,
		StartBudget:     500 * time.Millisecond,
		SkipRenderProbe: true,
		AllowLoopback:   true,
		ChromiumPath:    chromium,
	})
	assertStalledTrace(t, tr, err)

	if d := readStalls(t, "start") - beforeStart; d != 1 {
		t.Errorf("phase=start stall delta = %v, want 1 — this test must exercise an INNER stall", d)
	}
	if d := readStalls(t, "drive") - beforeDrive; d != 0 {
		t.Errorf("phase=drive stall delta = %v, want 0 — the session timer must not be what fired here", d)
	}
}

// TestDriveEnableInterceptionHonoursItsOwnStartBudget closes the coverage gap the test
// above declares. driveSession has TWO bounded startup call sites — driver.go's
// runHard("start", …, network.Enable, …) and, inside enableInterception, a second
// runHard("start", …, fetch.Enable()) — and #50 made BOTH take opts.StartBudget rather
// than the session StallGrace. Only the first was pinned: reverting enableInterception's
// argument to `grace` survived a fully green package (measured: 0.55s), because the
// sibling test's 500ms budget expires while the browser is still booting on call 1, so
// the second call site is never reached.
//
// The discriminator here is TIMING, not the phase label — both call sites report
// phase="start", so the counter alone cannot tell them apart. StartBudget is set well
// ABOVE a real chromium launch (so call 1 completes and call 2 happens at all) and well
// BELOW StallGrace. Correct code stalls on StartBudget; the `grace` mutant stalls on
// StallGrace instead, ~5x later, and blows the elapsed bound.
//
// The call counter is the POSITIVE CONTROL and is not optional: without it, a slow host
// whose browser launch exceeds StartBudget would fire the watchdog on call 1, satisfy
// every other assertion, and pass while testing nothing — the exact vacuity this test
// exists to remove. It must observe 2 calls: the browser start, then fetch.Enable.
//
// 🔴 COVERAGE LIMIT: this needs a REAL browser, so it SKIPS on a browser-less runner and
// auditloop has no pre-merge CI. There is no browser-free companion (unlike
// TestDriveClassifiesAStalledSessionWithoutABrowser below) because MEASURED: a stub that
// returns nil for call 1 and PARKS on call 2 reaches enableInterception fine (calls=2,
// watchdog fires on schedule) but then Drive NEVER RETURNS — killing an allocator that
// never allocated hangs, the same deadlock that test's comment records. Do not re-try
// that shape; it produces a permanently-hanging test, not a cheaper guard.
func TestDriveEnableInterceptionHonoursItsOwnStartBudget(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := digestFixture()
	defer fx.Close()
	beforeDrive, beforeStart := readStalls(t, "drive"), readStalls(t, "start")

	const (
		startBudget = 6 * time.Second  // > a real chromium launch (measured ~0.5-1s here)
		stallGrace  = 30 * time.Second // the mutant's budget: 5x startBudget
		// The session watchdog is runBounded("drive", OverallTimeout+StallGrace) = 150s,
		// far beyond BOTH, so it cannot be what fires in either the correct or the
		// mutant run — asserted below as a phase=drive delta of 0.
		overall = 120 * time.Second
	)

	// Call 1 goes to a real browser (it is what boots chromium). Call 2 — which can only
	// be enableInterception's fetch.Enable, since SkipRenderProbe removes the probe's run
	// and nothing else runs in between — parks on an already-held, non-context-aware
	// mutex: the #41 shape no context deadline can unwind.
	park := stuckForever()
	var calls atomic.Int32
	setRunTasks(func(ctx context.Context, actions ...chromedp.Action) error {
		if calls.Add(1) == 1 {
			return chromedp.Run(ctx, actions...)
		}
		return park()
	})
	t.Cleanup(func() { setRunTasks(nil) })

	start := time.Now()
	tr, err := Drive(context.Background(), DriveOptions{
		BaseURL:         fx.URL + "/",
		AllowedHosts:    []string{"127.0.0.1"},
		Planner:         &fixedPlanner{},
		Success:         action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 500},
		OverallTimeout:  overall,
		StallGrace:      stallGrace,
		StartBudget:     startBudget,
		SkipRenderProbe: true,
		AllowLoopback:   true,
		ChromiumPath:    chromium,
	})
	elapsed := time.Since(start)
	t.Logf("stalled after %s (StartBudget=%s, StallGrace=%s), runTasks calls=%d",
		elapsed.Round(time.Millisecond), startBudget, stallGrace, calls.Load())

	assertStalledTrace(t, tr, err)

	// POSITIVE CONTROL — see the doc comment. 1 means the watchdog fired on the browser
	// start and enableInterception was never reached, i.e. this test measured nothing.
	if n := calls.Load(); n != 2 {
		t.Fatalf("runTasks calls = %d, want 2 (browser start, then fetch.Enable) — "+
			"with 1 the stall fired before enableInterception and this test is vacuous; "+
			"if the host is simply slow to launch chromium, raise startBudget (and the "+
			"elapsed bound with it), never lower the assertion", n)
	}

	// THE MUTATION KILLER: passing `grace` instead of opts.StartBudget makes this
	// ~stallGrace rather than ~startBudget. The bound sits between the two, nearer the
	// correct value, so it tolerates a slow launch without tolerating the mutant.
	if max := startBudget + stallGrace/2; elapsed >= max {
		t.Errorf("Drive stalled after %s, want < %s — enableInterception must be bounded by "+
			"StartBudget (%s), not by StallGrace (%s)", elapsed.Round(time.Millisecond), max,
			startBudget, stallGrace)
	}

	if d := readStalls(t, "start") - beforeStart; d != 1 {
		t.Errorf("phase=start stall delta = %v, want 1 — the stall must be an INNER startup one", d)
	}
	if d := readStalls(t, "drive") - beforeDrive; d != 0 {
		t.Errorf("phase=drive stall delta = %v, want 0 — the session timer must not be what fired here", d)
	}
}

// TestDriveClassifiesAStalledSessionWithoutABrowser pins the SAME production branch
// with NO chromium at all. This is load-bearing, not a duplicate: both tests above
// call resolveChromiumT, which SKIPS when no browser is available, and auditloop has
// no pre-merge CI — so on a browser-less runner they vanish silently and the guard
// reverts to "the mutant survives the whole suite". This one always runs.
//
// MEASURED: an earlier comment in this file asserted a browser-less stub "cannot reach
// this line". That was inferred from one deadlock (a stub that PARKED, so the watchdog
// killed an allocator that had never allocated). A stub that RETURNS ErrBrowserStalled
// never triggers that kill — driveSession returns normally through its own defers — and
// the branch is reached in ~0ms. The false claim is retracted; this test is the proof.
func TestDriveClassifiesAStalledSessionWithoutABrowser(t *testing.T) {
	setRunTasks(func(ctx context.Context, _ ...chromedp.Action) error { return ErrBrowserStalled })
	t.Cleanup(func() { setRunTasks(nil) })

	done := make(chan struct{})
	var tr *DriveTrace
	var err error
	go func() {
		defer close(done)
		tr, err = Drive(context.Background(), DriveOptions{
			BaseURL:         "http://127.0.0.1:9/",
			AllowedHosts:    []string{"127.0.0.1"},
			Planner:         &fixedPlanner{},
			Success:         action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 200},
			OverallTimeout:  time.Second,
			StallGrace:      300 * time.Millisecond,
			SkipRenderProbe: true,
			AllowLoopback:   true,
		})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Drive hung on the browser-less path")
	}
	assertStalledTrace(t, tr, err)
}
