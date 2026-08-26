package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/auditloop/internal/metrics"

	"github.com/chromedp/chromedp"
)

// ---------------------------------------------------------------------------
// Hard watchdog around chromedp (issue #41)
//
// Every chromedp call in this package is already run under a context deadline.
// That is NOT sufficient: chromedp's internal target bookkeeping
// (Target.ensureFrame and friends) serialises on a plain sync.RWMutex which is
// NOT context-aware. When Chromium stops making progress on a page — the
// observed trigger is a FONT-LESS environment, where Chromium floods
// `TextRunHarfBuzz ... font: ''` and the load never settles — a chromedp
// goroutine can park on that mutex forever. The surrounding
// context.WithTimeout fires, nobody observes it, and chromedp.Run never
// returns. A "bounded" chromedp.Navigate is then not bounded at all: the crawl
// hangs indefinitely instead of failing (measured: 360s+ with no fonts vs 5.2s
// with fonts).
//
// runBounded enforces the deadline from OUTSIDE the stuck call: it runs the
// work on its own goroutine and, if the wall-clock budget expires, kills the
// BROWSER (the exec allocator's cancel → the chromium process dies) and
// returns ErrBrowserStalled to the caller. Killing the process is what
// eventually unblocks the parked chromedp goroutine (its CDP reader loop hits
// EOF); until then the goroutine is leaked, which is acceptable: the browser is
// gone, the session is unusable, and the caller aborts with an actionable
// error rather than hanging forever.
// ---------------------------------------------------------------------------

// ErrBrowserStalled is returned when a chromedp call exceeded its HARD budget —
// i.e. it ignored its own context deadline (the non-context-aware-mutex hang).
// The browser has been killed; the session it belonged to is dead.
var ErrBrowserStalled = errors.New("browser stalled past its deadline (killed)")

// ErrBrowserProbe is returned when the startup render probe does not settle.
// The message is deliberately actionable: the overwhelmingly likely cause is a
// container with no fonts installed.
var ErrBrowserProbe = errors.New("browser failed to render a trivial page")

// DefaultStallGrace is added to a call's own context deadline to form the HARD
// wall-clock budget. It must be generous enough that a merely slow (but
// progressing) page is never mistaken for a stall — the watchdog is a
// last-resort backstop for the case where the inner deadline is ignored, not a
// second, tighter timeout.
const DefaultStallGrace = 20 * time.Second

// DefaultProbeTimeout bounds the startup render probe. The probe is one loopback
// HTTP navigation plus a layout read, measured at ~40-75ms on a healthy browser,
// so this is a large multiple of the expected cost and does not meaningfully
// slow a crawl.
const DefaultProbeTimeout = 15 * time.Second

// StallHint is the operator-facing explanation attached to every stall, on every
// path (crawl capture, crawl login, drive, login probe). Keeping it in ONE place
// is why all four report the same diagnosis instead of, say, blaming a login
// recipe's selectors for what is actually an infrastructure stall.
const StallHint = "the browser stopped making progress and was killed; a font-less environment is the " +
	"known trigger (install fontconfig + a font package, e.g. dejavu_fonts, and set FONTCONFIG_FILE)"

// stallError wraps ErrBrowserStalled with the shared hint plus a caller-supplied
// context (which page/phase). Callers use errors.Is(err, ErrBrowserStalled) to
// detect it, so the wrapping never hides the sentinel.
func stallError(what string, err error) error {
	if what == "" {
		return fmt.Errorf("%w — %s", err, StallHint)
	}
	return fmt.Errorf("%s: %w — %s", what, err, StallHint)
}

// runBounded runs fn, enforcing hard as a WALL-CLOCK deadline even when fn
// ignores context cancellation. On expiry it calls kill (which must terminate
// the browser process) and returns ErrBrowserStalled; fn's own eventual return
// value is discarded.
//
// phase labels the stall metric (probe|capture|login|drive|start).
// kill may be nil (tests / callers with nothing to kill).
func runBounded(phase string, hard time.Duration, kill func(), fn func() error) error {
	if hard <= 0 {
		return fn()
	}
	done := make(chan error, 1) // buffered: a stuck fn that later returns must not block forever
	go func() { done <- fn() }()

	timer := time.NewTimer(hard)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		log.Printf("crawler: WATCHDOG — chromedp call in phase %q exceeded its hard budget of %s and did not honour its context; killing the browser (see issue #41: a font-less environment is the known trigger)", phase, hard)
		metrics.BrowserStalls.WithLabelValues(phase).Inc()
		if kill != nil {
			kill()
		}
		return ErrBrowserStalled
	}
}

// taskRunner is the chromedp.Run signature the seam swaps.
type taskRunner func(ctx context.Context, actions ...chromedp.Action) error

// runTasksOverride is the test seam, held as an ATOMIC pointer rather than a
// plain package var. That is load-bearing, not stylistic: when the watchdog
// fires it ABANDONS the stuck goroutine, which may still be inside runTasks and
// may read the seam at any later moment — including while a test's t.Cleanup
// restores it. A plain var makes that an unsynchronised read/write pair (caught
// by `go test -race`, which `make test` does not pass). nil ⇒ real chromedp.
var runTasksOverride atomic.Pointer[taskRunner]

// runTasks is the chromedp.Run seam. It exists so tests can simulate the exact
// failure this file defends against — a call that NEVER returns and ignores its
// context — without needing a font-less machine. Production code must always
// route its task runs through it.
func runTasks(ctx context.Context, actions ...chromedp.Action) error {
	if fn := runTasksOverride.Load(); fn != nil {
		return (*fn)(ctx, actions...)
	}
	return chromedp.Run(ctx, actions...)
}

// setRunTasks installs (fn != nil) or clears (fn == nil) the seam. Test-only;
// safe to call while an abandoned goroutine is still reading it.
func setRunTasks(fn taskRunner) {
	if fn == nil {
		runTasksOverride.Store(nil)
		return
	}
	runTasksOverride.Store(&fn)
}

// runHard is runBounded around a chromedp.Run: ctx supplies the ordinary
// (cooperative) deadline, hard the wall-clock backstop.
func runHard(phase string, ctx context.Context, hard time.Duration, kill func(), actions ...chromedp.Action) error {
	return runBounded(phase, hard, kill, func() error { return runTasks(ctx, actions...) })
}

// probeRender is the pure core of the startup probe: it runs `run` under the
// hard watchdog and translates a stall (or any failure) into an ACTIONABLE
// error. Separated from the chromedp plumbing so both outcomes are unit
// testable without a browser.
func probeRender(hard time.Duration, kill func(), run func() error) error {
	err := runBounded("probe", hard, kill, run)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrBrowserStalled) {
		return fmt.Errorf("%w within %s — this environment most likely has NO USABLE FONTS "+
			"(Chromium then floods TextRunHarfBuzz \"font: ''\" and page loads never settle). "+
			"Install fontconfig plus a font package (e.g. dejavu_fonts) and set FONTCONFIG_FILE. "+
			"See issue #41: %v", ErrBrowserProbe, hard, err)
	}
	return fmt.Errorf("%w: %v", ErrBrowserProbe, err)
}

// probePageHTML is the document the startup probe renders. Its contents are
// EMPIRICALLY DERIVED, not decorative — measured in the font-less repro
// container (nixos/nix + chromium 146, no fontconfig), navigating to:
//
//	plain text/<h1>/<p>  → 23ms   (settles fine)
//	<button>             → 27ms   (settles fine)
//	<a href>             → 28ms   (settles fine)
//	TEXT <input>         → NEVER settles (load event never fires)
//
// A font-less Chromium can lay out ordinary text; it is a text FORM CONTROL
// that wedges it. So the probe MUST contain a text input — a probe page without
// one passes cleanly in exactly the environment the probe exists to catch (an
// earlier revision did, and was useless). Keep the input.
const probePageHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<title>auditloop render probe</title></head><body>` +
	`<h1 id="p">auditloop render probe</h1>` +
	`<p>The quick brown fox jumps over the lazy dog. 0123456789</p>` +
	`<form><input id="probe-input" name="probe" type="text" placeholder="render probe"></form>` +
	`</body></html>`

// probeBrowser asserts the browser can complete a REAL HTTP navigation and lay
// out text within hard. It is called ONCE per browser session (crawl / drive /
// login probe); on a healthy browser it costs a few hundred milliseconds.
//
// It serves the page from an EPHEMERAL LOOPBACK LISTENER rather than a data:
// URL, and that choice is load-bearing: measured in the font-less repro
// container, a data: page renders in ~320ms while every http(s) navigation
// never completes its load event. Probing over data: would therefore pass in
// exactly the environment the probe exists to catch. The listener is bound to
// 127.0.0.1 on a random port, serves one document, and is closed immediately —
// it is not reachable off-host and carries no SSRF surface (the probe runs
// BEFORE the interception guard is installed, so it also cannot be aborted by
// the target's host allowlist).
//
// If the loopback listener cannot be bound (a locked-down sandbox), the probe
// DEGRADES to a skip rather than failing the crawl: the watchdog is still the
// backstop.
func probeBrowser(tabCtx context.Context, hard time.Duration, kill func()) error {
	if hard <= 0 {
		hard = DefaultProbeTimeout
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("crawler: render probe skipped (no loopback listener: %v)", err)
		return nil
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, probePageHTML)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	probeURL := "http://" + ln.Addr().String() + "/"

	return probeRender(hard, kill, func() error {
		ctx, cancel := context.WithTimeout(tabCtx, hard)
		defer cancel()
		var width float64
		// Routed through the runTasks SEAM (not chromedp.Run directly) so the
		// probe's own path is covered by the hang seam like every other run.
		if err := runTasks(ctx,
			chromedp.Navigate(probeURL),
			chromedp.WaitReady("#probe-input", chromedp.ByQuery),
			chromedp.Evaluate(`document.getElementById('p').getBoundingClientRect().width`, &width),
		); err != nil {
			return err
		}
		// ENFORCED, not merely measured: a non-zero laid-out width proves text was
		// actually SHAPED, not just that the document parsed. Asserting this is the
		// point of evaluating it — an earlier revision computed `width` and never
		// compared it, so width == 0 passed.
		if width <= 0 {
			return fmt.Errorf("probe text laid out to zero width (font shaping produced nothing)")
		}
		return nil
	})
}
