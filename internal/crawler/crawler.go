// Package crawler drives headless Chromium (chromedp) to crawl a target
// same-origin (BFS, depth+page capped) and, per page/viewport, capture a
// full-page screenshot, an axe-core accessibility scan, and origin-classified
// console + network errors. Every URL passes the SSRF/abuse guard first.
package crawler

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/ZacxDev/auditloop/internal/report"
)

// Viewport is a device profile to capture.
type Viewport struct {
	Name   string
	Width  int
	Height int
	Mobile bool
}

// DefaultViewports captures a mobile (390px) and desktop (1440px) rendering.
var DefaultViewports = []Viewport{
	{Name: "mobile", Width: 390, Height: 844, Mobile: true},
	{Name: "desktop", Width: 1440, Height: 900, Mobile: false},
}

// Options configures a crawl.
type Options struct {
	BaseURL      string
	AllowedHosts []string
	MaxPages     int
	MaxDepth     int
	ChromiumPath string // ExecPath override; "" → resolve from PATH
	Viewports    []Viewport
	// AllowLoopback is the dev/test-only loopback escape hatch (see GuardConfig).
	AllowLoopback bool
	// InternalAllowHosts is the exact-match internal-host allowlist (see
	// GuardConfig.InternalAllowHosts) — empty in normal deployments.
	InternalAllowHosts []string
	// Resolve overrides DNS resolution for the SSRF guard (tests).
	Resolve func(host string) ([]net.IP, error)
	// NavTimeout bounds a single page navigation+capture.
	NavTimeout time.Duration
	// Login, when non-nil, runs a login recipe in the SAME browser context
	// BEFORE the BFS crawl so the authenticated session/cookies carry into the
	// crawl (P4). A failed login aborts the crawl with *ErrLoginFailed.
	Login *LoginConfig
	// StallGrace is added to a call's own deadline to form the HARD wall-clock
	// budget enforced by the watchdog (#41). 0 → DefaultStallGrace.
	StallGrace time.Duration
	// ProbeTimeout bounds the startup render probe. 0 → DefaultProbeTimeout.
	ProbeTimeout time.Duration
	// SkipRenderProbe disables the startup render probe (tests that stub the
	// browser, or a caller that has already probed this environment).
	SkipRenderProbe bool
}

// ConsoleError is one origin-attributed console/JS error.
type ConsoleError struct {
	Text      string `json:"text"`
	URL       string `json:"url"`
	FirstPart bool   `json:"first_party"`
}

// NetworkError is one failed/4xx-5xx request.
type NetworkError struct {
	URL       string `json:"url"`
	Status    int64  `json:"status"`
	Reason    string `json:"reason"`
	FirstPart bool   `json:"first_party"`
}

// LayoutSmells is the deterministic per-page+viewport DOM layout-smell heuristics
// captured by layoutSmellScript. Counts + a few example selectors each, so a
// finding can point at the offending elements. Tap-target and horizontal-overflow
// are primarily MOBILE concerns; the worker gates those findings on the viewport.
type LayoutSmells struct {
	// HorizontalOverflow is true when documentElement.scrollWidth exceeds the
	// viewport width (beyond a small tolerance) — the page scrolls sideways.
	HorizontalOverflow  bool `json:"horizontal_overflow"`
	ScrollWidth         int  `json:"scroll_width"`
	InnerWidth          int  `json:"inner_width"`
	SmallTapTargets     int  `json:"small_tap_targets"` // interactive els with a rect edge < 44px
	SmallText           int  `json:"small_text"`        // elements with direct text and computed font-size < 12px
	MissingViewportMeta bool `json:"missing_viewport_meta"`
	ImagesNoDims        int  `json:"images_no_dims"` // <img> missing BOTH width+height attrs (CLS risk)
	// Examples holds up to a few CSS-ish selectors per smell (untrusted-ish DOM
	// content — the worker stores them as escaped JSON in the finding detail).
	Examples map[string][]string `json:"examples,omitempty"`
}

// PageResult is one crawled URL at one viewport.
type PageResult struct {
	URL            string
	Viewport       Viewport
	ScreenshotPNG  []byte
	AxeJSON        []byte
	AxeViolations  int
	AxeNodes       int
	ConsoleErrors  []ConsoleError
	NetworkErrors  []NetworkError
	NetworkLogJSON []byte
	// A11yDigestJSON is the bounded DOM/accessibility digest (a11y-digest.js output)
	// for persona-evaluator grounding — nil when capture failed (best-effort/non-fatal).
	A11yDigestJSON []byte
	LoadMS         int64
	// Deterministic web-vitals / perf signals. LCP/TBT are milliseconds, CLS is
	// unitless. TBTMs is a headless LAB PROXY — there is no field input, so it is
	// an approximation of main-thread blocking, NOT a real field Total Blocking
	// Time. WeightBytes/ReqCount come from CDP network events (more accurate than
	// JS resource-timing for cross-origin resources).
	LCPMs       int64
	CLS         float64
	TBTMs       int64
	WeightBytes int64
	ReqCount    int
	// Layout is the deterministic DOM layout-smell heuristics for this viewport.
	Layout LayoutSmells
	// FaviconHref is the page-declared favicon URL (absolute), captured only on the
	// landing page's first viewport (when extractLinks is set); "" otherwise.
	FaviconHref string
}

// Result is the full crawl outcome.
type Result struct {
	Pages          []PageResult
	URLsDiscovered int
	URLsBlocked    int
	// Favicon holds the site favicon bytes captured (best-effort, SSRF-guarded)
	// from the landing page; nil when none was resolved/fetched. FaviconExt is the
	// storage extension for the raster type (png|jpg|gif|webp|ico).
	Favicon    []byte
	FaviconExt string
}

// Crawl runs a BFS same-origin crawl and returns per-page results.
func Crawl(ctx context.Context, opts Options) (*Result, error) {
	if len(opts.Viewports) == 0 {
		opts.Viewports = DefaultViewports
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = 50
	}
	if opts.MaxDepth < 0 {
		opts.MaxDepth = 3
	}
	if opts.NavTimeout <= 0 {
		opts.NavTimeout = 45 * time.Second
	}
	guard := GuardConfig{AllowedHosts: opts.AllowedHosts, Resolve: opts.Resolve, AllowLoopback: opts.AllowLoopback, InternalAllowHosts: InternalAllowSet(opts.InternalAllowHosts)}

	// Browser allocator (host-agnostic chromium via ExecPath).
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
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()
	// Use the browser's DEFAULT tab for the whole crawl. Do NOT open a second
	// tab via chromedp.NewContext: under chromium 149, a captureBeyondViewport
	// screenshot on a non-default tab intermittently hangs after a few captures
	// (reproduced deterministically — default tab is unaffected). One listener
	// routes events to the current page's collector.
	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()
	// killBrowser terminates the chromium process. It is the ONLY lever that can
	// unwind a chromedp call parked on its non-context-aware internal mutex
	// (issue #41). Killing the browser ends the crawl — see the Crawl-loop comment.
	//
	// SCOPE (honest): every chromedp.Run reached from THIS function is wrapped —
	// browser start, the interception enable, the render probe, the whole login
	// recipe, and each page capture. That covers the crawl's own call graph. It
	// does NOT retroactively bound chromedp calls made elsewhere in the process.
	killBrowser := func() { cancelTab(); cancelAlloc() }
	stallGrace := opts.StallGrace
	if stallGrace <= 0 {
		stallGrace = DefaultStallGrace
	}
	// SetCacheDisabled forces every navigation to fetch fresh (cold load). The
	// crawl reuses ONE browser context across all pages+viewports, so without this
	// the 2nd+ capture of a URL (e.g. the desktop viewport after mobile) is served
	// from Chromium's HTTP cache — which both undercounts page weight (a cached
	// response reports no dataReceived bytes and loadingFinished.encodedDataLength=0)
	// and makes web-vitals unrealistically fast. A cold load is also the correct
	// thing to audit: it is the first-time-visitor experience (what Lighthouse measures).
	if err := runHard("start", tabCtx, stallGrace, killBrowser, network.Enable(), network.SetCacheDisabled(true)); err != nil {
		return nil, fmt.Errorf("crawler: browser start: %w", err)
	}

	// Startup render probe (#41): assert the browser can render a trivial page
	// with TEXT within a small budget BEFORE crawling. A font-less environment
	// fails here in seconds with an actionable message instead of hanging for
	// the whole run. Cost on a healthy browser is a few hundred milliseconds.
	if !opts.SkipRenderProbe {
		if err := probeBrowser(tabCtx, opts.ProbeTimeout, killBrowser); err != nil {
			return nil, fmt.Errorf("crawler: %w", err)
		}
	}

	sess := &session{}
	chromedp.ListenTarget(tabCtx, sess.handle)

	// Runtime SSRF guard: IP-check every navigation (incl. redirect hops) so a
	// 3xx to an internal/metadata address is aborted mid-flight, in BOTH the
	// login phase and the crawl. The pre-navigation CheckURL below is retained as
	// defense in depth (it also enforces the host allowlist).
	if _, err := enableInterception(tabCtx, guard, false, stallGrace, killBrowser); err != nil {
		return nil, err
	}

	// P4: authenticate first (same tab → session/cookies carry into the crawl).
	if opts.Login != nil && len(opts.Login.Steps) > 0 {
		// Bounded by the same hard watchdog: a login step can park on the same
		// non-context-aware mutex as a page capture (#41).
		err := runBounded("login", loginHardBudget(opts.Login, stallGrace), killBrowser,
			func() error { return runLogin(tabCtx, opts.Login, guard) })
		if err != nil {
			// A STALL is an infrastructure failure, NOT a bad recipe — returning it
			// unwrapped would send the user to debug selectors/credentials for a
			// dead browser. Same diagnosis every other path reports.
			if errors.Is(err, ErrBrowserStalled) {
				return nil, stallError("crawler: login", err)
			}
			return nil, err
		}
	}

	res := &Result{}
	visited := map[string]bool{}
	type item struct {
		url   string
		depth int
	}
	start := canonicalURL(opts.BaseURL)
	queue := []item{{start, 0}}
	visited[start] = true
	res.URLsDiscovered = 1
	var faviconHref string

	for len(queue) > 0 && countCrawled(res) < opts.MaxPages {
		cur := queue[0]
		queue = queue[1:]

		if err := guard.CheckURL(cur.url); err != nil {
			res.URLsBlocked++
			log.Printf("crawler: blocked %s: %v", cur.url, err)
			continue
		}

		var discoveredLinks []string
		for i, vp := range opts.Viewports {
			pr, links, err := capturePage(tabCtx, sess, cur.url, vp, opts.NavTimeout, stallGrace, killBrowser, i == 0)
			if err != nil {
				// A STALL is terminal for the whole crawl, not just this page: the
				// watchdog has killed the browser, and the crawl deliberately shares
				// ONE default tab for every page (re-opening a tab would re-introduce
				// the chromium-149 captureBeyondViewport hang documented in CLAUDE.md).
				// So we abort with an actionable error rather than looping over a dead
				// browser — a failed run is an acceptable outcome, an indefinite hang
				// is not.
				if errors.Is(err, ErrBrowserStalled) {
					return nil, stallError(fmt.Sprintf("crawler: %s @ %s", cur.url, vp.Name), err)
				}
				log.Printf("crawler: capture %s @ %s: %v", cur.url, vp.Name, err)
				continue
			}
			res.Pages = append(res.Pages, pr)
			if i == 0 {
				discoveredLinks = links
				if cur.url == start && faviconHref == "" {
					faviconHref = pr.FaviconHref
				}
			}
		}

		if cur.depth < opts.MaxDepth {
			for _, l := range discoveredLinks {
				c := canonicalURL(l)
				if c == "" || visited[c] {
					continue
				}
				if !hostAllowedIn(c, opts.AllowedHosts) {
					continue
				}
				visited[c] = true
				res.URLsDiscovered++
				queue = append(queue, item{c, cur.depth + 1})
			}
		}
	}

	// Best-effort favicon capture (SSRF-guarded, IP-pinned, raster-only, size-capped).
	// A failure never affects the crawl — the card degrades to a name monogram.
	if favURL := FaviconURLFor(start, faviconHref); favURL != "" {
		if data, ext, err := guard.FetchFavicon(ctx, favURL); err == nil {
			res.Favicon = data
			res.FaviconExt = ext
		} else {
			log.Printf("crawler: favicon skipped (%s): %v", favURL, err)
		}
	}
	return res, nil
}

func countCrawled(res *Result) int {
	seen := map[string]bool{}
	for _, p := range res.Pages {
		seen[p.URL] = true
	}
	return len(seen)
}

// capturePage navigates the shared tab, injects axe, screenshots, and collects
// origin-classified console/network errors for THIS page (via the session's
// current collector). When extractLinks is true it also returns the page's
// absolute anchor hrefs.
func capturePage(tabCtx context.Context, sess *session, pageURL string, vp Viewport, timeout, stallGrace time.Duration, killBrowser func(), extractLinks bool) (PageResult, []string, error) {
	// Per-page timeout derived from the shared tab context: a hang on one page
	// cancels only that page's command run — the tab (and browser) survive.
	pageCtx, cancelTimeout := context.WithTimeout(tabCtx, timeout)
	defer cancelTimeout()

	col := &collector{
		reqURLs:   map[network.RequestID]string{},
		recvBytes: map[network.RequestID]int64{},
	}
	sess.set(col)
	defer sess.set(nil)

	pr := PageResult{URL: pageURL, Viewport: vp}
	var axeJSON string
	var links []string
	var perfJSON, layoutJSON string
	t0 := time.Now()

	tasks := chromedp.Tasks{
		// Set device metrics directly (NOT chromedp.EmulateViewport): combined
		// with chromedp.FullScreenshot the latter hangs on the 2nd+ tab under
		// chromium 149. We capture the full page manually below instead.
		emulation.SetDeviceMetricsOverride(int64(vp.Width), int64(vp.Height), 1, vp.Mobile),
		chromedp.Navigate(pageURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(700 * time.Millisecond), // settle network/render
	}
	var faviconHref string
	if extractLinks {
		tasks = append(tasks,
			chromedp.Evaluate(linkExtractScript, &links),
			chromedp.Evaluate(faviconHrefScript, &faviconHref),
		)
	}
	tasks = append(tasks,
		// Deterministic web-vitals (buffered PerformanceObserver) + DOM layout smells.
		// Run after the settle sleep above so buffered LCP/CLS/longtask entries exist.
		chromedp.Evaluate(perfCaptureScript, &perfJSON, awaitPromise),
		chromedp.Evaluate(layoutSmellScript, &layoutJSON),
		chromedp.Evaluate(axeSource, nil),
		chromedp.Evaluate(axeRunScript, &axeJSON, awaitPromise),
		// Bounded DOM/accessibility digest for persona-evaluator grounding. Wrapped so
		// a digest failure is NON-FATAL (best-effort, like favicon) — it must never
		// fail the page capture; the eval degrades to screenshot-only on an empty digest.
		chromedp.ActionFunc(func(ctx context.Context) error {
			var raw string
			if err := chromedp.Evaluate(a11yDigestSource, &raw).Do(ctx); err != nil {
				log.Printf("crawler: a11y digest %s @ %s: %v", pageURL, vp.Name, err)
				return nil
			}
			// The digest is UNTRUSTED (page-authored) and nothing else bounds it — a
			// hostile page could override JSON.stringify to return an oversized payload.
			// Over the cap ⇒ treat as EMPTY (degrade to screenshot-only), never store it.
			if raw != "" {
				if len(raw) > report.MaxA11yDigestBytes {
					log.Printf("crawler: a11y digest %s @ %s: %d bytes over cap %d — dropping", pageURL, vp.Name, len(raw), report.MaxA11yDigestBytes)
				} else {
					pr.A11yDigestJSON = []byte(raw)
				}
			}
			return nil
		}),
		fullPageScreenshot(&pr.ScreenshotPNG),
	)

	// HARD watchdog (#41): pageCtx's deadline is cooperative and chromedp can
	// ignore it (non-context-aware internal mutex). If the whole capture has not
	// returned within timeout+stallGrace we kill the browser and surface
	// ErrBrowserStalled, which the caller treats as terminal.
	if err := runHard("capture", pageCtx, timeout+stallGrace, killBrowser, tasks); err != nil {
		if errors.Is(err, ErrBrowserStalled) {
			// The stuck goroutine was ABANDONED and may still be writing into pr
			// (&pr.ScreenshotPNG, pr.A11yDigestJSON). Returning pr by value here
			// would race with those writes, so return a zero value — the caller
			// discards the result on a stall anyway. (Drive handles the identical
			// hazard the same way.)
			return PageResult{}, nil, err
		}
		return pr, nil, err
	}
	pr.LoadMS = time.Since(t0).Milliseconds()
	pr.FaviconHref = strings.TrimSpace(faviconHref)

	// Parse axe.
	if axeJSON != "" {
		pr.AxeJSON = []byte(axeJSON)
		var ar struct {
			Violations []struct {
				NodeCount int `json:"nodeCount"`
			} `json:"violations"`
		}
		if err := json.Unmarshal([]byte(axeJSON), &ar); err == nil {
			pr.AxeViolations = len(ar.Violations)
			for _, v := range ar.Violations {
				pr.AxeNodes += v.NodeCount
			}
		}
	}

	// Parse the perf metrics (best-effort: a missing/invalid payload leaves zeros).
	if perfJSON != "" {
		var pm struct {
			LCP float64 `json:"lcp"`
			CLS float64 `json:"cls"`
			TBT float64 `json:"tbt"`
		}
		if json.Unmarshal([]byte(perfJSON), &pm) == nil {
			pr.LCPMs = int64(pm.LCP)
			pr.CLS = pm.CLS
			pr.TBTMs = int64(pm.TBT)
		}
	}
	// Parse the layout smells (best-effort).
	if layoutJSON != "" {
		_ = json.Unmarshal([]byte(layoutJSON), &pr.Layout)
	}

	// Classify collected events by origin; pull page weight + request count.
	col.mu.Lock()
	for _, ce := range col.console {
		ce.FirstPart = SameOrigin(pageURL, orDefault(ce.URL, pageURL))
		pr.ConsoleErrors = append(pr.ConsoleErrors, ce)
	}
	for _, ne := range col.network {
		ne.FirstPart = SameOrigin(pageURL, ne.URL)
		pr.NetworkErrors = append(pr.NetworkErrors, ne)
	}
	pr.WeightBytes = col.weightBytes
	pr.ReqCount = col.reqCount
	col.mu.Unlock()
	pr.NetworkLogJSON, _ = json.Marshal(pr.NetworkErrors)

	return pr, links, nil
}

func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// fullPageScreenshot captures the whole page (beyond the viewport) as PNG. This
// is a reliable substitute for chromedp.FullScreenshot, which hangs on the 2nd+
// tab when combined with device-metrics emulation under chromium 149.
func fullPageScreenshot(out *[]byte) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		b, err := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(true).
			Do(ctx)
		if err != nil {
			return err
		}
		*out = b
		return nil
	})
}

// session routes CDP events from the single shared tab to the current page's
// collector. The listener runs on its own goroutine, so access is mutex-guarded.
type session struct {
	mu  sync.Mutex
	cur *collector
}

func (s *session) set(c *collector) {
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
}

func (s *session) handle(ev any) {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c != nil {
		c.handle(ev)
	}
}

// collector accumulates console + network errors from CDP events (thread-safe;
// the listener runs on its own goroutine).
type collector struct {
	mu      sync.Mutex
	console []ConsoleError
	network []NetworkError
	reqURLs map[network.RequestID]string
	// Page-weight accounting from CDP network events: reqCount counts requests
	// dispatched, weightBytes sums the transferred (encoded) bytes. More accurate
	// than JS resource-timing, which zeroes encodedBodySize for opaque cross-origin
	// responses without Timing-Allow-Origin.
	//
	// recvBytes accumulates per-request Network.dataReceived encodedDataLength
	// chunks. Chromium reports loadingFinished.encodedDataLength=0 for the MAIN
	// navigation document — its real transferred bytes only arrive as dataReceived
	// chunk events — while subresources carry an authoritative non-zero
	// loadingFinished total. On loadingFinished we take the authoritative total when
	// present and fall back to the summed dataReceived bytes when it is 0, never
	// both, so the document is counted without double-counting subresources.
	recvBytes   map[network.RequestID]int64
	reqCount    int
	weightBytes int64
}

func (c *collector) handle(ev any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		c.reqURLs[e.RequestID] = e.Request.URL
		// EventRequestWillBeSent fires again for each HTTP 3xx redirect hop on the
		// same RequestID (carrying RedirectResponse); count only the initial request
		// so a redirected resource isn't over-counted against the request budget.
		if e.RedirectResponse == nil {
			c.reqCount++
		}
	case *network.EventDataReceived:
		// Accumulate compressed wire bytes per chunk. This is the only place the
		// MAIN navigation document's transferred bytes surface — its
		// loadingFinished.encodedDataLength is reported as 0 (see below).
		c.recvBytes[e.RequestID] += int64(e.EncodedDataLength)
	case *network.EventLoadingFinished:
		// EncodedDataLength is the authoritative total bytes received over the wire
		// for the resource (headers + compressed body). Subresources report a
		// correct non-zero total here, but Chromium reports 0 for the main
		// navigation document — whose real bytes arrived only via dataReceived
		// chunks. Use the authoritative total when present and fall back to the
		// summed dataReceived bytes when it is 0, never both, so the document is
		// counted without double-counting subresources.
		total := int64(e.EncodedDataLength)
		if total == 0 {
			total = c.recvBytes[e.RequestID]
		}
		c.weightBytes += total
		delete(c.recvBytes, e.RequestID)
	case *network.EventResponseReceived:
		if e.Response.Status >= 400 {
			c.network = append(c.network, NetworkError{URL: e.Response.URL, Status: e.Response.Status, Reason: "http_status"})
		}
	case *network.EventLoadingFailed:
		u := c.reqURLs[e.RequestID]
		if u == "" {
			return
		}
		// Ignore benign aborts.
		if e.ErrorText == "net::ERR_ABORTED" {
			return
		}
		c.network = append(c.network, NetworkError{URL: u, Reason: e.ErrorText})
	case *runtime.EventConsoleAPICalled:
		if e.Type != "error" {
			return
		}
		c.console = append(c.console, ConsoleError{Text: consoleText(e), URL: stackURL(e.StackTrace)})
	case *runtime.EventExceptionThrown:
		det := e.ExceptionDetails
		u := det.URL
		if u == "" {
			u = stackURL(det.StackTrace)
		}
		c.console = append(c.console, ConsoleError{Text: det.Text, URL: u})
	}
}

func consoleText(e *runtime.EventConsoleAPICalled) string {
	parts := make([]string, 0, len(e.Args))
	for _, a := range e.Args {
		if a.Value != nil {
			parts = append(parts, strings.Trim(string(a.Value), `"`))
		} else if a.Description != "" {
			parts = append(parts, a.Description)
		}
	}
	return strings.Join(parts, " ")
}

func stackURL(st *runtime.StackTrace) string {
	if st == nil || len(st.CallFrames) == 0 {
		return ""
	}
	return st.CallFrames[0].URL
}

const linkExtractScript = `Array.from(document.querySelectorAll('a[href]')).map(a => a.href).filter(h => h.startsWith('http'))`

// perfCaptureScript + layoutSmellScript are the deterministic perf/layout capture
// evals, vendored as canonical .js files (like axe.min.js) so external push harnesses
// (external ux-audit harnesses) can copy ONE source of truth. The returned JSON shapes are the
// P5 push contract. See perf-capture.js / layout-smells.js for the full commentary
// (incl. the honest "tbt is a headless LAB PROXY" note).
//
//go:embed perf-capture.js
var perfCaptureScript string

//go:embed layout-smells.js
var layoutSmellScript string

// canonicalURL strips the fragment and normalizes an absolute URL. Returns ""
// for unparseable / non-http URLs.
func canonicalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

func hostAllowedIn(rawURL string, hosts []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	g := GuardConfig{AllowedHosts: hosts}
	return g.hostAllowed(strings.ToLower(u.Hostname()))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
