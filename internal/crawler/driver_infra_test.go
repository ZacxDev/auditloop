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
		Planner:         &parkingPlanner{},
		Success:         action.SuccessAssertion{URLContains: "/never-reached", TimeoutMs: 200},
		OverallTimeout:  time.Second,
		StallGrace:      500 * time.Millisecond,
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
		BaseURL:         fx.URL + "/",
		AllowedHosts:    []string{"127.0.0.1"},
		Planner:         &fixedPlanner{},
		Success:         action.SuccessAssertion{URLContains: "/welcome", TimeoutMs: 500},
		OverallTimeout:  20 * time.Second, // deliberately far from the inner budget
		StallGrace:      500 * time.Millisecond,
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
