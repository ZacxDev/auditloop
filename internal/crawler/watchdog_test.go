package crawler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/recipe"

	"github.com/chromedp/chromedp"
)

// stuckForever blocks on an ALREADY-HELD, non-context-aware sync.Mutex — the
// exact shape of the #41 hang (chromedp's Target.ensureFrame RWMutex). Nothing
// can unblock it: no context, no cancel, no timeout. Any code that merely wraps
// this in context.WithTimeout hangs forever, which is what makes this a
// non-vacuous test of the watchdog.
func stuckForever() func() error {
	var mu sync.Mutex
	mu.Lock() // never unlocked
	return func() error {
		mu.Lock() // parks here for the life of the process
		return nil
	}
}

// TestRunBoundedUnwindsAnUnbreakableWait is the core #41 regression test.
//
// MUTATION PROOF: replace runBounded's body with `return fn()` (i.e. drop the
// watchdog) and this test does not fail with a nice message — it HANGS until the
// go-test 10-minute panic, exactly reproducing the production symptom. With the
// watchdog it returns ErrBrowserStalled in ~100ms.
func TestRunBoundedUnwindsAnUnbreakableWait(t *testing.T) {
	var killed int32
	var mu sync.Mutex
	kill := func() { mu.Lock(); killed++; mu.Unlock() }

	start := time.Now()
	err := runBounded("test", 100*time.Millisecond, kill, stuckForever())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBrowserStalled) {
		t.Fatalf("want ErrBrowserStalled, got %v", err)
	}
	// The assertion that fires without the fix is the one above never being
	// reached; this one guards against a watchdog that is merely slow.
	if elapsed > 5*time.Second {
		t.Fatalf("watchdog took %s — the deadline was not enforced", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if killed != 1 {
		t.Fatalf("browser kill called %d times, want exactly 1", killed)
	}
}

func TestRunBoundedPassesThroughResults(t *testing.T) {
	sentinel := errors.New("boom")
	killed := false
	kill := func() { killed = true }

	if err := runBounded("test", time.Minute, kill, func() error { return nil }); err != nil {
		t.Fatalf("success case: %v", err)
	}
	if err := runBounded("test", time.Minute, kill, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error case: want sentinel, got %v", err)
	}
	if killed {
		t.Fatal("the browser must NOT be killed when the call returns in time")
	}
}

// A non-positive budget disables the watchdog (used by callers that opt out).
func TestRunBoundedZeroBudgetRunsInline(t *testing.T) {
	if err := runBounded("test", 0, nil, func() error { return nil }); err != nil {
		t.Fatalf("zero budget: %v", err)
	}
}

// --- startup probe -------------------------------------------------------

func TestProbeRenderHappyPath(t *testing.T) {
	killed := false
	if err := probeRender(time.Minute, func() { killed = true }, func() error { return nil }); err != nil {
		t.Fatalf("healthy browser must probe clean, got %v", err)
	}
	if killed {
		t.Fatal("a healthy probe must not kill the browser")
	}
}

// The font-less case: the probe never settles. It must fail FAST with an
// ACTIONABLE message (the whole point of #41's second half) — never hang.
func TestProbeRenderStallIsActionable(t *testing.T) {
	killed := false
	start := time.Now()
	err := probeRender(100*time.Millisecond, func() { killed = true }, stuckForever())
	if err == nil {
		t.Fatal("a stalled probe must fail")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the probe did not enforce its budget")
	}
	if !errors.Is(err, ErrBrowserProbe) {
		t.Fatalf("want ErrBrowserProbe, got %v", err)
	}
	if !killed {
		t.Fatal("a stalled probe must kill the browser")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"font", "fontconfig"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("probe error is not actionable — missing %q: %s", want, err)
		}
	}
}

// A probe that fails for an ORDINARY reason (browser start error) must not be
// mislabeled as a font problem, but must still be an ErrBrowserProbe.
func TestProbeRenderOrdinaryFailure(t *testing.T) {
	sentinel := errors.New("exec: chromium not found")
	err := probeRender(time.Minute, nil, func() error { return sentinel })
	if !errors.Is(err, ErrBrowserProbe) {
		t.Fatalf("want ErrBrowserProbe, got %v", err)
	}
	if strings.Contains(err.Error(), "FONTS") {
		t.Fatalf("an ordinary failure must not claim a font problem: %v", err)
	}
	if !strings.Contains(err.Error(), "chromium not found") {
		t.Fatalf("the underlying cause must be preserved: %v", err)
	}
}

// --- capturePage: the REAL crawl code path is bounded --------------------

// TestCapturePageIsBoundedWhenChromedpHangs drives the actual capturePage
// function with a task runner that never returns (the #41 shape), and asserts
// it surfaces ErrBrowserStalled quickly instead of hanging.
//
// MUTATION PROOF: revert capturePage's runHard(...) back to chromedp.Run(...)
// and this test hangs (the runTasks stub blocks forever), which is the exact
// production failure.
func TestCapturePageIsBoundedWhenChromedpHangs(t *testing.T) {
	stuck := stuckForever()
	// setRunTasks, not a plain assignment: the abandoned goroutine is still inside
	// runTasks when this cleanup runs, so an unsynchronised var swap is a data race
	// (go test -race). The seam is an atomic pointer for exactly this reason.
	setRunTasks(func(ctx context.Context, actions ...chromedp.Action) error { return stuck() })
	t.Cleanup(func() { setRunTasks(nil) })

	killed := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, _, err := capturePage(context.Background(), &session{}, "http://fixture.invalid/",
			Viewport{Name: "mobile", Width: 390, Height: 844, Mobile: true},
			50*time.Millisecond, 50*time.Millisecond,
			func() {
				select {
				case killed <- struct{}{}:
				default:
				}
			},
			false)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrBrowserStalled) {
			t.Fatalf("want ErrBrowserStalled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capturePage HUNG on an unbreakable chromedp wait — the #41 watchdog did not fire")
	}
	select {
	case <-killed:
	default:
		t.Fatal("capturePage must kill the browser when the watchdog fires")
	}
}

// loginHardBudget must scale with the recipe so a long, legitimate login is
// never killed early, while still being finite.
func TestLoginHardBudgetIsFiniteAndScales(t *testing.T) {
	if got := loginHardBudget(nil, time.Second); got <= 0 {
		t.Fatalf("nil recipe must still yield a positive budget, got %s", got)
	}
	small := loginHardBudget(&LoginConfig{Steps: dummySteps(1)}, time.Second)
	big := loginHardBudget(&LoginConfig{Steps: dummySteps(5)}, time.Second)
	if big <= small {
		t.Fatalf("budget must grow with step count: %s vs %s", small, big)
	}
}

// TestProbeBrowserAgainstRealChromium is the probe's REAL happy path: a healthy
// (font-equipped) chromium must render the probe page well inside the budget.
// Chromium-gated, like the other browser-backed tests in this package.
func TestProbeBrowserAgainstRealChromium(t *testing.T) {
	chromium := resolveChromiumT(t)
	tabCtx, cleanup := newDriverTab(t, chromium)
	defer cleanup()

	start := time.Now()
	if err := probeBrowser(tabCtx, DefaultProbeTimeout, func() { t.Error("healthy browser was killed") }); err != nil {
		t.Fatalf("probe failed on a healthy browser: %v", err)
	}
	if d := time.Since(start); d > DefaultProbeTimeout {
		t.Fatalf("probe took %s — it must be cheap enough not to slow a crawl", d)
	}
	t.Logf("render probe settled in %s", time.Since(start))
}

// The budget must have a CEILING, not merely be positive and monotonic. Without
// one it is a function of user-supplied numbers (step count × per-step waits),
// which degrades "a stall is bounded" to "a stall ends eventually" and pins the
// single-threaded crawl worker for the duration.
func TestLoginHardBudgetIsCapped(t *testing.T) {
	// The worst recipe the validator will accept: MaxSteps waitFors, each at the
	// maximum permitted timeout.
	worst := make([]recipe.Step, 0, recipe.MaxSteps)
	for i := 0; i < recipe.MaxSteps; i++ {
		worst = append(worst, recipe.Step{
			Type: recipe.StepWaitFor, Selector: "#ok", TimeoutMs: recipe.MaxWaitTimeoutMs,
		})
	}
	got := loginHardBudget(&LoginConfig{Steps: worst}, DefaultStallGrace)
	if got > MaxLoginHardBudget {
		t.Fatalf("budget %s exceeds the ceiling %s — a hostile/careless recipe can extend the watchdog without bound", got, MaxLoginHardBudget)
	}
	// Sanity: the uncapped computation really would have blown past the ceiling,
	// so this test is not vacuous.
	uncapped := time.Duration(recipe.MaxSteps) * (defaultStepTimeout + recipe.MaxWaitTimeoutMs*time.Millisecond)
	if uncapped <= MaxLoginHardBudget {
		t.Fatalf("test is vacuous: uncapped budget %s is already under the ceiling", uncapped)
	}
}

// The probe must ENFORCE the layout assertion it documents: a page that parses
// but shapes no text (zero laid-out width) is exactly the font-less failure, and
// must not pass.
func TestProbeRejectsZeroWidthLayout(t *testing.T) {
	setRunTasks(func(ctx context.Context, actions ...chromedp.Action) error { return nil }) // no-op: width stays 0
	t.Cleanup(func() { setRunTasks(nil) })

	err := probeBrowser(context.Background(), 2*time.Second, nil)
	if err == nil {
		t.Fatal("a probe whose text laid out to zero width must FAIL — that is the font-less signature")
	}
	if !errors.Is(err, ErrBrowserProbe) {
		t.Fatalf("want ErrBrowserProbe, got %v", err)
	}
	if !strings.Contains(err.Error(), "zero width") {
		t.Fatalf("error should name the unmet assertion: %v", err)
	}
}

// Every path must report the SAME diagnosis for a stall — an infra failure must
// never be dressed up as (say) a bad login recipe.
func TestStallErrorCarriesTheSharedHint(t *testing.T) {
	err := stallError("crawler: https://x/ @ mobile", ErrBrowserStalled)
	if !errors.Is(err, ErrBrowserStalled) {
		t.Fatal("stallError must preserve the sentinel for errors.Is")
	}
	for _, want := range []string{"crawler: https://x/ @ mobile", "fontconfig", "FONTCONFIG_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func dummySteps(n int) []recipe.Step {
	out := make([]recipe.Step, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, recipe.Step{Type: recipe.StepClick, Selector: "#go"})
	}
	return out
}
