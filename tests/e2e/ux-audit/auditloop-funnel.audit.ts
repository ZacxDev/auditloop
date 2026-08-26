import { test, expect } from "@playwright/test";
import type { APIResponse } from "@playwright/test";
import { UxAudit } from "./_lib/audit";
import { pushRun } from "./_lib/push";

/**
 * auditloop SELF-AUDIT walk — auditloop auditing its OWN UI.
 *
 * Boots a local DEV_MODE auditloop (see playwright.ux-audit.config.ts: keys
 * stripped, sqlite + filesystem storage, loopback self-crawl allowed) and walks
 * its key funnel, `captureView`-ing each state (full-page screenshot + per-view
 * console/network/axe findings). It SEEDS a real self-crawl run against the local
 * instance so the run view has content, then (opt-in) pushes the whole walk back
 * into auditloop via its plugin API.
 *
 * Resilient by construction: an optional view that can't be reached is
 * record+skipped (recordError), never a hard failure — but the CORE views
 * (login, dashboard, a target, a completed run) MUST capture, asserted at the end.
 */

const audit = new UxAudit();
const SPEC = "auditloop-funnel";

// Distinct names so we can find each target's card/link deterministically.
const CRAWL_TARGET = "Self-crawl (auditloop itself)";
const PLUGIN_TARGET = "Self-audit push (CI)";

/** IDs discovered mid-walk (scraped from the rendered pages). */
let crawlTargetId = "";
let pluginTargetId = "";
/** The core views we require captured (asserted at end). */
const core = { login: false, dashboard: false, target: false, run: false };

/** Poll a run's status fragment until it reaches a terminal state (done|failed). */
async function waitForRun(baseURL: string, runId: string, timeoutMs = 150_000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    let res: APIResponse;
    try {
      res = await audit_request(baseURL, `/runs/${runId}/status`);
    } catch {
      await sleep(2000);
      continue;
    }
    const body = await res.text();
    // StatusPill renders "Done"/"Failed"/"Running"/"Queued".
    if (/>\s*Failed\s*</.test(body) || /Run failed/.test(body)) return "failed";
    if (/>\s*Done\s*</.test(body)) return "done";
    await sleep(2000);
  }
  return "timeout";
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * The dashboard now collapses the creation/setup forms (Add a target, plugin
 * push, API access) behind a primary "＋ New target" <details> disclosure (and a
 * nested "Advanced" one) so the Targets list is the primary content. Force every
 * <details> on the page open so the walk can reach the create-target / plugin /
 * API fields to interact with them (progressive disclosure is intentional UI;
 * the walk drives the underlying forms).
 */
async function expandDisclosures(page: import("@playwright/test").Page): Promise<void> {
  await page.evaluate(() => {
    document.querySelectorAll("details").forEach((d) => {
      (d as HTMLDetailsElement).open = true;
    });
  });
}

// Small helper so waitForRun can issue plain HTTP GETs without a Page.
let _apiCtx: import("@playwright/test").APIRequestContext | null = null;
async function audit_request(baseURL: string, pathname: string): Promise<APIResponse> {
  if (!_apiCtx) throw new Error("api context not ready");
  return _apiCtx.get(baseURL.replace(/\/+$/, "") + pathname);
}

test("auditloop self-audit walk", async ({ page, request }, testInfo) => {
  _apiCtx = request;
  const baseURL = testInfo.project.use.baseURL as string;
  audit.wire(page);

  // ---------------------------------------------------------------- 1. login --
  audit.resetBuffers();
  await page.goto("/login");
  await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
  await audit.captureView(page, "login", "/login", {
    title: "Login (DEV_MODE bypass)",
    note: "The sign-in page. In DEV_MODE auth is bypassed — the page offers an “Enter dashboard” shortcut.",
  });
  core.login = true;

  // ------------------------------------------------------------ 2. dashboard --
  audit.resetBuffers();
  await page.goto("/dashboard");
  await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
  await audit.captureView(page, "dashboard-empty", "/dashboard", {
    title: "Dashboard — empty state",
    note: "The target list is the primary content. Creation/setup is collapsed behind a single primary “＋ New target” disclosure (open by default for a brand-new user with no targets); plugin push + API access are demoted inside a nested “Advanced” disclosure.",
  });
  core.dashboard = true;

  // ------------------------------------------- 3. create a (self-crawl) target --
  // base_url points at THIS instance's /login (loopback, stable public page) so the
  // seeded run genuinely crawls auditloop itself.
  audit.resetBuffers();
  try {
    await expandDisclosures(page);
    await page.fill("#name", CRAWL_TARGET);
    await page.fill("#base_url", `${baseURL.replace(/\/+$/, "")}/login`);
    await page.getByRole("button", { name: "Add target" }).click();
    // handleCreateTarget returns HX-Refresh → full reload of /dashboard.
    await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
    await expect(page.getByText(CRAWL_TARGET)).toBeVisible({ timeout: 10_000 });
  } catch (e) {
    audit.recordError("create-target", "/dashboard", "Create target", (e as Error).message);
  }
  await audit.captureView(page, "dashboard-with-target", "/dashboard", {
    title: "Dashboard — after adding a target",
    note: "The new self-crawl target card appears with a “Run audit” action.",
  });

  // Discover the crawl target id from its card link.
  try {
    const href = await page
      .locator(`a:has-text("${CRAWL_TARGET}")`)
      .first()
      .getAttribute("href");
    if (href) crawlTargetId = href.replace(/^\/targets\//, "");
  } catch {
    /* handled below */
  }

  // --------------------------------------------------------- 4. target page ----
  if (crawlTargetId) {
    audit.resetBuffers();
    await page.goto(`/targets/${crawlTargetId}`);
    await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
    // Default (collapsed) state: the redesigned target page leads with an overview
    // header (identity + auth-mode badge + the primary "Run audit" CTA + a last-run
    // status strip), then the Runs list as the PRIMARY content. The Authentication /
    // Audit-configuration / Walkthrough controls are tucked behind a "Configuration"
    // native <details> accordion so they no longer compete for attention.
    await audit.captureView(page, "target", `/targets/${crawlTargetId}`, {
      title: "Target page — overview + runs-primary (config tucked away)",
      note: "The redesigned target page: an overview header (name, base URL, auth-mode badge, primary “Run audit” CTA + last-run status strip), then Runs as the primary content, then a “Configuration” accordion holding Authentication / Audit configuration / Walkthrough — collapsed by default so the page is no longer information overload.",
    });
    core.target = true;

    // Expand the Configuration accordion so the (previously always-visible) auth +
    // audit-config forms are reachable and axe scans them in their open state — the
    // accordion is native <details>/<summary> (keyboard + screen-reader accessible,
    // no ARIA needed), so opening it must add no a11y violation.
    audit.resetBuffers();
    await expandDisclosures(page);
    await page.waitForTimeout(300);
    await audit.captureView(page, "target-config-expanded", `/targets/${crawlTargetId}`, {
      title: "Target page — Configuration accordion expanded",
      note: "Expanding the Configuration accordion reveals the Authentication card (defaults to “No authentication”; the guided login-recipe form is progressively disclosed under the “Login recipe” radio) and the Audit-configuration card. Every control/route/gate is preserved — only the layout changed.",
    });
  } else {
    audit.recordError("target", "/targets/?", "Target page", "could not discover the crawl target id");
  }

  // -------------------------------------------- 5. plugin target create flow ---
  audit.resetBuffers();
  await page.goto("/dashboard");
  await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
  try {
    await expandDisclosures(page); // plugin form lives behind the "＋ New target" → "Advanced" disclosure
    await page.fill("#plugin_name", PLUGIN_TARGET);
    await page.getByRole("button", { name: "Create plugin target" }).click();
    // Swaps the one-time token reveal into #plugin-create-result (hx-boost=false).
    await expect(page.getByText(/Upload key created for/)).toBeVisible({ timeout: 10_000 });
  } catch (e) {
    audit.recordError("plugin-token-reveal", "/dashboard", "Plugin token reveal", (e as Error).message);
  }
  await audit.captureView(page, "plugin-token-reveal", "/dashboard", {
    title: "Plugin target — one-time push token reveal",
    note: "Creating a plugin (push-only) target mints a push token shown ONCE (this is the token a self-audit push would use).",
  });

  // Discover the plugin target id from the reveal panel's “Go to the plugin target” link.
  try {
    const href = await page
      .locator('#plugin-create-result a[href^="/targets/"]')
      .first()
      .getAttribute("href");
    if (href) pluginTargetId = href.replace(/^\/targets\//, "");
  } catch {
    /* optional */
  }

  // --------------------------------------------------- 6. plugin target page ---
  if (pluginTargetId) {
    audit.resetBuffers();
    await page.goto(`/targets/${pluginTargetId}`);
    await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
    await audit.captureView(page, "plugin-target", `/targets/${pluginTargetId}`, {
      title: "Plugin target page (push instructions + rotate token)",
      note: "Push-only: no “Run audit”. Shows the push endpoint, the reference-uploader example, and a Rotate-token control.",
    });
  } else {
    audit.recordError("plugin-target", "/targets/?", "Plugin target page", "could not discover the plugin target id");
  }

  // ------------------------------------------- 7. seed a crawl run + run view --
  if (crawlTargetId) {
    // Trigger the run deterministically via the API (reads HX-Redirect → run url).
    let runId = "";
    try {
      const res = await request.post(`${baseURL.replace(/\/+$/, "")}/api/targets/${crawlTargetId}/runs`, {
        headers: { "HX-Request": "true" },
      });
      const redirect = res.headers()["hx-redirect"] ?? "";
      runId = redirect.replace(/^\/runs\//, "");
    } catch (e) {
      audit.recordError("run", "/runs/?", "Seed run", (e as Error).message);
    }

    if (runId) {
      // Capture the in-progress run view first (queued/running) for the funnel.
      audit.resetBuffers();
      await page.goto(`/runs/${runId}`);
      await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
      await audit.captureView(page, "run-inprogress", `/runs/${runId}`, {
        title: "Run view — in progress",
        note: "Immediately after triggering: the run view shows the queued/running state and auto-polls.",
      });

      const status = await waitForRun(baseURL, runId);
      audit.resetBuffers();
      await page.goto(`/runs/${runId}`);
      await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
      // The redesigned run report shows per-page detail behind a native <details>
      // accordion (report-not-form-dump). Expand every disclosure so axe scans the
      // revealed screenshots/perf/findings detail too — a11y coverage of the detail
      // must hold (like the #31/#28 target/dashboard walks did).
      await expandDisclosures(page);
      await page.waitForTimeout(300);
      await audit.captureView(page, "run-done", `/runs/${runId}`, {
        title: `Run view — completed (${status})`,
        note:
          "The completed self-crawl: per-page screenshots, axe findings, origin-classified console/network errors, " +
          "the P2 “first run — no baseline” note, and the key-gated P3 “Draft UX notes” control " +
          "(force-rendered via a dummy local key; generation never fires — see the config).",
      });
      if (status === "done") core.run = true;
      else audit.recordError("run-done", `/runs/${runId}`, "Run completion", `run ended '${status}', expected 'done'`);

      // --------------------------- 8. second run → P2 “Changes since” (optional) --
      try {
        const res2 = await request.post(`${baseURL.replace(/\/+$/, "")}/api/targets/${crawlTargetId}/runs`, {
          headers: { "HX-Request": "true" },
        });
        const runId2 = (res2.headers()["hx-redirect"] ?? "").replace(/^\/runs\//, "");
        if (runId2) {
          const status2 = await waitForRun(baseURL, runId2);
          audit.resetBuffers();
          await page.goto(`/runs/${runId2}`);
          await page.waitForTimeout(400); // settle render (networkidle never idles: PWA service worker)
          await expandDisclosures(page); // expand the collapsible page cards so axe scans the detail
          await page.waitForTimeout(300);
          await audit.captureView(page, "run-diff", `/runs/${runId2}`, {
            title: `Run view — second run, P2 regression diff (${status2})`,
            note: "A second run against the same target surfaces the “Changes since previous run” diff card.",
          });
        }
      } catch (e) {
        audit.recordError("run-diff", "/runs/?", "Second run (P2 diff)", (e as Error).message);
      }

      // ---------------------- 9. dashboard PROJECT CARD populated (post-run) -----
      // Now that the self-crawl target has completed runs, the dashboard renders it
      // as a first-class PROJECT CARD: identity (captured favicon or a name monogram)
      // + a screenshot CAROUSEL (accessible, keyboard-focusable scroll region with
      // labeled prev/next controls when there is more than one thumbnail) + summary
      // stats. Re-scan /dashboard so axe covers the populated card + carousel controls.
      audit.resetBuffers();
      await page.goto("/dashboard");
      await page.waitForTimeout(500); // settle render (networkidle never idles: PWA service worker)
      await audit.captureView(page, "dashboard-populated", "/dashboard", {
        title: "Dashboard — project card populated (post-run)",
        note:
          "After runs complete, each target is a first-class project card: identity (site favicon captured " +
          "during the crawl, or a name monogram when none/SVG-only — auditloop's own icon is an SVG, so it " +
          "degrades to a monogram here), a keyboard-accessible screenshot carousel, a last-run status badge + " +
          "date, and cheap stats (pages / a11y / regressions).",
      });
    }
  }

  // ------------------------------------------------------------- finalize -----
  const missing = Object.entries(core)
    .filter(([, ok]) => !ok)
    .map(([k]) => k);
  const passed = missing.length === 0;
  const failMessage = passed ? undefined : `missing core view(s): ${missing.join(", ")}`;

  const md = audit.writeMarkdown({ passed, failMessage });
  // eslint-disable-next-line no-console
  console.log(`\nauditloop self-audit: ${audit.viewCount} views → ${md}`);

  // Opt-in, non-fatal push back into auditloop (auditloop audits itself).
  await pushRun(audit, SPEC);

  // Hard-fail only if a CORE view was missed.
  expect(passed, failMessage).toBe(true);
});
