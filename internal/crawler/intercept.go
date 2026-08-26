package crawler

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// interceptor is the RUNTIME SSRF guard. The pre-navigation CheckURL calls only
// vet the URL a caller hands us; once chromedp.Navigate runs, Chromium follows
// HTTP 3xx redirects to arbitrary addresses with nothing else re-checking the
// resolved IP. That gap let a save-time-clean recipe (goto http://attacker/x →
// 302 → http://169.254.169.254/…) reach cloud metadata / internal hosts.
//
// enableInterception turns on Fetch request interception and guards every paused
// MAIN-FRAME DOCUMENT/navigation request (these carry the redirect chain AND every
// click-triggered navigation). It enforces BOTH the same-domain host-allowlist
// (hostAllowed) AND the IP-safety check (checkHostIP) — see checkNav. A request
// whose host is outside the target's AllowedHosts (a click that follows a public
// off-domain link, or an off-domain redirect hop), OR whose host is a literal
// private/metadata IP / resolves into a private/loopback/link-local/ULA/metadata
// range, is FailRequest'd (aborted) so Chromium never connects — no matter which
// phase (login or crawl) triggered it. Blocks are recorded so callers can surface
// a clear SSRF/containment failure (and suppress an exfil screenshot) rather than
// silently skipping.
//
// Scope: only DOCUMENT requests are guarded (bounded count; carries every
// navigation redirect + click-nav — the exploit surface for both the
// metadata-screenshot and off-domain-escape attacks). The host-allowlist check is
// DELIBERATELY Document-only: applying it to sub-resources would block third-party
// CDN/analytics/image assets and break page rendering. Sub-resource SSRF (an
// XHR/fetch to a private IP) is out of scope for this layer; it cannot become a
// page screenshot and a crawl of a public site should not be contacting private
// IPs for assets anyway.
//
// Residual risk: DNS rebinding remains a TOCTOU race — Chromium re-resolves the
// host when it connects, so a name that answers public here but private to the
// browser a moment later could slip one request. Blocking literal private IPs +
// resolve-and-check on each paused request closes the practical exploits; full
// closure needs connection-level IP pinning, which chromedp/CDP does not expose.
type interceptor struct {
	guard GuardConfig
	// dryRunAbort arms the Phase-3 walkthrough submit-guard: when set, any request
	// whose method is NOT GET/HEAD (POST/PUT/PATCH/DELETE) is aborted at the network
	// layer. It is a RUNTIME-TOGGLEABLE atomic (default OFF) — armMutationGuard flips
	// it on. The driver keeps it OFF during the P4 login phase (whose form POST must
	// go through to authenticate) and arms it BEFORE the post-login drive loop, all on
	// ONE Fetch listener. Deterministic (method-based), not a prose heuristic.
	//
	// What the armed guard COVERS: every TOP-FRAME HTTP(S) request of ANY resource
	// type Chromium routes through Fetch interception — a form POST, and every
	// XHR/fetch/sendBeacon POST/PUT/PATCH/DELETE issued by the page's own JS in the
	// top frame. Those cannot reach the network; a driver pass in dry-run therefore
	// does not write live data through them. The SSRF IP-guard stays active in BOTH
	// modes (armed or not).
	//
	// Residual risk (mutation vectors NOT covered — honest scoping): (1) WebSocket
	// frames — the WS handshake is an interceptable GET, but application MESSAGES sent
	// over an established socket are not HTTP requests and are not paused here; (2)
	// CROSS-ORIGIN (OOPIF) iframe requests — an out-of-process iframe is a SEPARATE CDP
	// target with its own Fetch domain, which this single-target interceptor does not
	// attach to; (3) SERVICE-WORKER-originated requests — a request dispatched from a
	// service-worker context can bypass the page-target Fetch interception. A target
	// that mutates state through any of these could still write in dry-run. Mitigation
	// / guidance: use allow_real_submit + point real driving at STAGING with a
	// DISPOSABLE account; full closure would need per-target Fetch attach + WS-frame
	// inspection (not exposed uniformly by chromedp/CDP — out of scope for this layer).
	dryRunAbort atomic.Bool

	mu        sync.Mutex
	blocked   []blockedNav
	mutations []blockedNav // non-GET/HEAD requests aborted while the guard is armed
}

type blockedNav struct {
	URL    string
	Reason string
}

// enableInterception installs the Fetch interception guard on ctx's browser
// target and enables the Fetch domain. Call it once per browser context, after
// network.Enable() and before any navigation. dryRun sets the INITIAL state of the
// mutation submit-guard; the driver passes false and arms it later (after login) via
// armMutationGuard, so ONE Fetch listener serves both phases. The SSRF guard (both
// the host-allowlist AND the IP-safety check — see checkNav) is always active on
// every paused Document navigation regardless of dryRun.
// hard/kill bound the fetch.Enable round-trip under the #41 watchdog (it is a
// chromedp.Run like any other and can park on the same non-context-aware mutex).
func enableInterception(ctx context.Context, guard GuardConfig, dryRun bool, hard time.Duration, kill func()) (*interceptor, error) {
	ic := &interceptor{guard: guard}
	ic.dryRunAbort.Store(dryRun)
	chromedp.ListenTarget(ctx, func(ev any) {
		if paused, ok := ev.(*fetch.EventRequestPaused); ok {
			// The listener callback must not block on a CDP round-trip, so the
			// continue/fail decision runs on its own goroutine.
			go ic.handlePaused(ctx, paused)
		}
	})
	if err := runHard("start", ctx, hard, kill, fetch.Enable()); err != nil {
		return nil, err
	}
	return ic, nil
}

// armMutationGuard turns ON the dry-run mutation submit-guard at runtime (aborting
// subsequent non-GET/HEAD requests). The driver calls it AFTER the P4 login phase
// (whose form POST must succeed) and BEFORE the drive loop, so the SSRF guard runs
// the whole time but mutations are only blocked during driving. Idempotent.
func (ic *interceptor) armMutationGuard() {
	ic.dryRunAbort.Store(true)
}

// handlePaused decides a single paused request: fail it (aborted) when a
// main-frame document/navigation request targets a host outside the allowlist or a
// blocked IP (see checkNav), else continue.
func (ic *interceptor) handlePaused(ctx context.Context, e *fetch.EventRequestPaused) {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Target == nil {
		return
	}
	exec := cdp.WithExecutor(ctx, c.Target)

	if e.ResourceType == network.ResourceTypeDocument && e.Request != nil {
		if reason := ic.checkNav(e.Request.URL); reason != "" {
			ic.record(e.Request.URL, reason)
			// Abort so Chromium never opens a connection to the blocked host.
			_ = fetch.FailRequest(e.RequestID, network.ErrorReasonAborted).Do(exec)
			return
		}
	}
	// Dry-run submit-guard: once armed, abort any mutating request
	// (POST/PUT/PATCH/DELETE) so a walkthrough driving pass does not write live data
	// through top-frame HTTP(S). Deterministic and at the network layer — not a prose
	// heuristic. GET/HEAD (and CONNECT/OPTIONS preflight, harmless) pass through. (See
	// the struct doc for the WS / cross-origin-iframe / service-worker residuals.)
	if ic.dryRunAbort.Load() && e.Request != nil && isMutatingMethod(e.Request.Method) {
		ic.recordMutation(e.Request.URL, e.Request.Method)
		_ = fetch.FailRequest(e.RequestID, network.ErrorReasonAborted).Do(exec)
		return
	}
	_ = fetch.ContinueRequest(e.RequestID).Do(exec)
}

// isMutatingMethod reports whether an HTTP method writes state (so the dry-run
// submit-guard aborts it).
func isMutatingMethod(m string) bool {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// checkNav returns a non-empty reason when a navigation URL must be blocked. It
// enforces BOTH halves of the guard, in the SAME order as CheckURL (ssrf.go): the
// same-domain host-allowlist (hostAllowed) THEN the IP-safety check (checkHostIP).
// Enforcing hostAllowed here — not just at the pre-navigation CheckURL — contains
// a CLICK that follows a *public* off-domain link (or a redirect hop to a public
// off-domain host): those never pass through CheckURL, so before this they were
// only IP-guarded and escaped same-origin containment. Non-http(s) schemes and
// empty hosts (about:blank, data:) are left for Chromium to handle — they are not
// network SSRF vectors. The InternalAllowHosts exact-host relaxation composes: an
// internal-allow host still must be in AllowedHosts to pass hostAllowed, then its
// private IP is tolerated by checkHostIP (the allowlist relaxes ONLY the IP half).
func (ic *interceptor) checkNav(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "" // malformed — let Chromium reject it
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	if !ic.guard.hostAllowed(host) {
		return "host not in allowlist: " + host
	}
	if err := ic.guard.checkHostIP(host); err != nil {
		if be, ok := err.(*ErrBlockedURL); ok {
			return be.Reason
		}
		return err.Error()
	}
	return ""
}

func (ic *interceptor) record(rawURL, reason string) {
	ic.mu.Lock()
	ic.blocked = append(ic.blocked, blockedNav{URL: rawURL, Reason: reason})
	ic.mu.Unlock()
}

func (ic *interceptor) recordMutation(rawURL, method string) {
	ic.mu.Lock()
	ic.mutations = append(ic.mutations, blockedNav{URL: rawURL, Reason: method})
	ic.mu.Unlock()
}

// mutationCount reports how many mutating requests the dry-run guard aborted.
func (ic *interceptor) mutationCount() int {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return len(ic.mutations)
}

// blockedCount reports how many navigations the guard aborted.
func (ic *interceptor) blockedCount() int {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return len(ic.blocked)
}

// lastBlocked returns the most recent blocked navigation, or (false) when none.
func (ic *interceptor) lastBlocked() (blockedNav, bool) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if len(ic.blocked) == 0 {
		return blockedNav{}, false
	}
	return ic.blocked[len(ic.blocked)-1], true
}
