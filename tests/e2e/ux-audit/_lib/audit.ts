import { AxeBuilder } from "@axe-core/playwright";
import type { ConsoleMessage, Page, Request, Response } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";

/**
 * UX-audit harness library.
 *
 * Drives a deterministic "capture a view" primitive: full-page screenshot +
 * the objective regression signals (browser console errors/warnings, failed
 * network requests, axe-core a11y violations) collected for THAT view, plus a
 * blank UX-notes scaffold for a human to fill. At the end it renders one
 * markdown findings doc that can be handed straight to an implementing Claude.
 *
 * The per-view console/network capture is scoped by clearing the running
 * buffers immediately before navigating to the view, so each view's findings
 * reflect only what happened while loading + interacting with it.
 */

export type Severity = "error" | "warning";

export interface ConsoleFinding {
  severity: Severity;
  text: string;
  location?: string;
}

export interface NetworkFinding {
  status: number;
  method: string;
  url: string;
}

/** One failing node of a color-contrast violation, with the measured data. */
export interface ContrastNode {
  target: string;
  fgColor?: string;
  bgColor?: string;
  contrastRatio?: number;
  expected?: number;
  fontSize?: string;
  fontWeight?: string;
}

/** One failing node of a heading-order violation, with its target + snippet. */
export interface HeadingNode {
  target: string;
  /** The offending heading element's HTML snippet (level + text). */
  html?: string;
}

export interface AxeFinding {
  id: string;
  impact: string | null;
  help: string;
  nodes: number;
  /** For `color-contrast` only: per-node target + measured fg/bg/ratio. */
  contrastNodes?: ContrastNode[];
  /** For `heading-order` only: per-node target + the heading's HTML snippet. */
  headingNodes?: HeadingNode[];
}

export interface ViewFinding {
  /** Stable slug used for the screenshot filename + markdown anchor. */
  slug: string;
  /** Human title for the section heading. */
  title: string;
  /** The route walked (relative path). */
  route: string;
  /** Optional note about what this view is / how it was reached. */
  note?: string;
  screenshot: string; // filename within the run dir
  console: ConsoleFinding[];
  network: NetworkFinding[];
  axe: AxeFinding[];
  /** A step-level failure captured but tolerated (see captureView opts). */
  error?: string;
}

export interface CaptureOptions {
  title: string;
  /** Optional markdown note describing the view / how it was reached. */
  note?: string;
  /**
   * Run axe-core on this view (default true). A few views (downloads, raw
   * fragments) aren't meaningful to a11y-scan; pass false to skip.
   */
  axe?: boolean;
}

const ROOT = path.resolve(__dirname, "..", "..", "..", "..");
const RUNS_DIR = path.join(ROOT, "tests", "e2e", "ux-audit-runs");
/** Committed baselines (route set + per-view control inventories). NOT gitignored. */
export const BASELINES_DIR = path.join(ROOT, "tests", "e2e", "ux-audit", "baselines");

/** True when the run should (re)write baselines instead of flagging drift. */
export function updateBaselines(): boolean {
  return process.env.UX_AUDIT_UPDATE_BASELINE === "1";
}

/** One interactive control on a page: its ARIA role + accessible name. */
export interface Control {
  role: string;
  name: string;
}

/**
 * The "route this gap to a human" recommendation appended to every control-drift
 * finding. Control-drift is on an ALREADY-covered page, so the question is
 * narrower than for a brand-new route: does the new/changed control need its own
 * assertion in the curated walk? Blessing is the deliberate second choice.
 */
const CONTROL_DRIFT_ACTION =
  "**Recommended action:** Bring this to the USER and decide together whether " +
  "the changed control needs a curated assertion in the multi-step walk — this " +
  "control-drift check only detects that the control set moved, it cannot verify " +
  "the flow still behaves. Only bless the baseline (UX_AUDIT_UPDATE_BASELINE=1) " +
  "AFTER you've decided no new assertion is warranted.";

/** Roles inventoried by captureControls (control-drift tripwire). */
const CONTROL_ROLES = new Set([
  "button",
  "tab",
  "link",
  "textbox",
  "combobox",
  "checkbox",
]);

/** A run-timestamp like 2026-06-29T15-14-22 (filesystem-safe ISO). */
export function runStamp(d = new Date()): string {
  return d.toISOString().replace(/:/g, "-").replace(/\..*$/, "");
}

export class UxAudit {
  readonly runDir: string;
  readonly stamp: string;
  private readonly views: ViewFinding[] = [];
  private consoleBuf: ConsoleFinding[] = [];
  private networkBuf: NetworkFinding[] = [];
  private wired = false;
  // Count of views whose control set drifted vs baseline (missing baseline
  // counts too). Read via `controlDrift` to hard-fail the run — control-drift is
  // ENFORCED like the route tripwire, not merely advisory. Zero under
  // UX_AUDIT_UPDATE_BASELINE=1 (captureControls rewrites the baseline instead).
  private controlDriftCount = 0;

  constructor(stamp = runStamp()) {
    this.stamp = stamp;
    this.runDir = path.join(RUNS_DIR, stamp);
    fs.mkdirSync(this.runDir, { recursive: true });
  }

  /**
   * Attach console + network listeners to the page. Idempotent. The buffers
   * accumulate until cleared by the next captureView's pre-nav reset.
   */
  wire(page: Page): void {
    if (this.wired) return;
    this.wired = true;

    page.on("console", (msg: ConsoleMessage) => {
      const type = msg.type();
      if (type !== "error" && type !== "warning") return;
      const loc = msg.location();
      this.consoleBuf.push({
        severity: type === "error" ? "error" : "warning",
        text: msg.text(),
        location: loc?.url ? `${loc.url}:${loc.lineNumber}` : undefined,
      });
    });

    // Page-level JS exceptions surface as console errors for the report.
    page.on("pageerror", (err: Error) => {
      this.consoleBuf.push({ severity: "error", text: `[pageerror] ${err.message}` });
    });

    page.on("response", (res: Response) => {
      const status = res.status();
      if (status < 400) return;
      const req: Request = res.request();
      this.networkBuf.push({ status, method: req.method(), url: res.url() });
    });

    // Outright request failures (DNS, connection refused, aborts) also count.
    page.on("requestfailed", (req: Request) => {
      this.networkBuf.push({
        status: 0,
        method: req.method(),
        url: `${req.url()} (${req.failure()?.errorText ?? "failed"})`,
      });
    });
  }

  /** Reset the per-view console/network buffers (call right before nav). */
  resetBuffers(): void {
    this.consoleBuf = [];
    this.networkBuf = [];
  }

  /**
   * Screenshot the current page (full-page) + snapshot the per-view buffers +
   * run axe, recording a ViewFinding. Caller is responsible for navigating /
   * interacting BEFORE calling this; call resetBuffers() before the nav so the
   * buffers are scoped to this view.
   */
  async captureView(
    page: Page,
    slug: string,
    route: string,
    opts: CaptureOptions,
  ): Promise<void> {
    const file = `${String(this.views.length + 1).padStart(2, "0")}-${slug}.png`;
    let error: string | undefined;

    try {
      await page.screenshot({ path: path.join(this.runDir, file), fullPage: true });
    } catch (e) {
      error = `screenshot failed: ${(e as Error).message}`;
    }

    let axe: AxeFinding[] = [];
    if (opts.axe !== false) {
      try {
        const results = await new AxeBuilder({ page }).analyze();
        axe = results.violations.map((v) => {
          const finding: AxeFinding = {
            id: v.id,
            impact: v.impact ?? null,
            help: v.help,
            nodes: v.nodes.length,
          };
          // For color-contrast, capture per-node target + measured colors/ratio
          // so a fix can target the EXACT failing element (don't guess).
          if (v.id === "color-contrast") {
            finding.contrastNodes = v.nodes.map((n) => {
              const check = (n.any ?? []).find((c) => c.id === "color-contrast");
              const d = (check?.data ?? {}) as Record<string, unknown>;
              return {
                target: n.target.join(" "),
                fgColor: d.fgColor as string | undefined,
                bgColor: d.bgColor as string | undefined,
                contrastRatio: d.contrastRatio as number | undefined,
                expected: d.expectedContrastRatio
                  ? Number(String(d.expectedContrastRatio).replace(/[^0-9.]/g, ""))
                  : undefined,
                fontSize: d.fontSize as string | undefined,
                fontWeight: d.fontWeight as string | undefined,
              };
            });
          }
          // For heading-order, capture per-node target + the heading's HTML
          // snippet (level + text) so a fix can target the EXACT skipping
          // heading element without guessing.
          if (v.id === "heading-order") {
            finding.headingNodes = v.nodes.map((n) => ({
              target: n.target.join(" "),
              html: oneLine(n.html ?? "").slice(0, 200),
            }));
          }
          return finding;
        });
      } catch (e) {
        // axe failing shouldn't sink the run; record it as a finding instead.
        axe = [{ id: "axe-error", impact: "n/a", help: (e as Error).message, nodes: 0 }];
      }
    }

    this.views.push({
      slug,
      title: opts.title,
      route,
      note: opts.note,
      screenshot: file,
      console: [...this.consoleBuf],
      network: [...this.networkBuf],
      axe,
      error,
    });
  }

  /** Record a HARD step failure (regression-smoke) so the report shows it even
   * though we then re-throw to fail the run. */
  recordError(slug: string, route: string, title: string, message: string): void {
    this.views.push({
      slug,
      title,
      route,
      screenshot: "",
      console: [...this.consoleBuf],
      network: [...this.networkBuf],
      axe: [],
      error: message,
    });
  }

  /**
   * Record a soft finding (not a hard failure) into the report. Used by the
   * autodiscovery + control-drift tripwires to surface drift without sinking the
   * run — it renders as a "Step issue" block in the markdown.
   */
  recordFinding(slug: string, route: string, title: string, message: string): void {
    this.views.push({
      slug,
      title,
      route,
      screenshot: "",
      console: [],
      network: [],
      axe: [],
      error: message,
    });
  }

  /**
   * Inventory the page's interactive controls ({role, name} for buttons, tabs,
   * links, textboxes, comboboxes, checkboxes) and diff against a checked-in
   * baseline at baselines/controls/<slug>.json. On drift, record a finding
   * listing the added/removed controls AND bump `controlDriftCount` so the walk
   * HARD-FAILS at end-of-run (drift is enforced like the route tripwire, not
   * advisory); with UX_AUDIT_UPDATE_BASELINE=1, (re)write the baseline instead
   * (no finding, no drift). Deterministic (deduped + sorted) so the diff is
   * stable across runs.
   *
   * The inventory is derived from the page's ARIA snapshot (role + accessible
   * name), which is exactly what an assistive-tech user perceives — so a control
   * silently losing its label, or a new/removed control, is caught.
   */
  async captureControls(page: Page, slug: string): Promise<void> {
    const controls = await inventoryControls(page);
    const rel = path.join("controls", `${slug}.json`);
    const abs = path.join(BASELINES_DIR, rel);

    if (updateBaselines()) {
      fs.mkdirSync(path.dirname(abs), { recursive: true });
      fs.writeFileSync(abs, JSON.stringify(controls, null, 2) + "\n", "utf8");
      return;
    }

    if (!fs.existsSync(abs)) {
      this.controlDriftCount++;
      this.recordFinding(
        `controls-${slug}`,
        slug,
        `Control inventory — ${slug}`,
        `🆕 no control baseline yet (${rel}). ${CONTROL_DRIFT_ACTION}`,
      );
      return;
    }

    const baseline: Control[] = JSON.parse(fs.readFileSync(abs, "utf8"));
    const key = (c: Control) => `${c.role} ${c.name}`;
    const baseKeys = new Set(baseline.map(key));
    const nowKeys = new Set(controls.map(key));
    const added = controls.filter((c) => !baseKeys.has(key(c)));
    const removed = baseline.filter((c) => !nowKeys.has(key(c)));

    if (added.length === 0 && removed.length === 0) return;

    this.controlDriftCount++;
    const fmt = (cs: Control[]) =>
      cs.map((c) => `\`${c.role}\` "${c.name}"`).join(", ");
    const parts: string[] = [];
    if (added.length) parts.push(`**added:** ${fmt(added)}`);
    if (removed.length) parts.push(`**removed:** ${fmt(removed)}`);
    this.recordFinding(
      `controls-${slug}`,
      slug,
      `Control drift — ${slug}`,
      `Interactive controls changed vs baseline (${rel}). ${parts.join(" · ")}.\n\n` +
        CONTROL_DRIFT_ACTION,
    );
  }

  /**
   * Number of views whose interactive-control set drifted from its baseline
   * (a missing baseline counts). The caller asserts this is 0 at end-of-walk to
   * hard-fail on drift — mirroring the route tripwire. Always 0 under
   * UX_AUDIT_UPDATE_BASELINE=1 (drift is blessed into the baseline instead).
   */
  get controlDrift(): number {
    return this.controlDriftCount;
  }

  get viewCount(): number {
    return this.views.length;
  }

  /**
   * The captured views (read-only). Exposed so the opt-in auditloop push shim
   * (`_lib/push.ts`) can map them to the plugin-ingestion schema. Returns a copy
   * so callers can't mutate the internal buffer.
   */
  get findings(): ViewFinding[] {
    return [...this.views];
  }

  /**
   * Total axe violation NODES across all walked views, optionally filtered to a
   * single rule id (e.g. "color-contrast"). Used as a scoped regression guard so
   * a fixed a11y-debt class can't silently return — assert `axeCount("color-contrast")`
   * is 0 at end-of-walk. Counts nodes (not violations) so a rule that regresses on
   * multiple elements is fully reflected.
   */
  axeCount(ruleId?: string): number {
    return this.views.reduce(
      (n, v) =>
        n +
        v.axe
          .filter((a) => !ruleId || a.id === ruleId)
          .reduce((m, a) => m + a.nodes, 0),
      0,
    );
  }

  /** Render + write the markdown findings doc. Returns its absolute path. */
  writeMarkdown(meta: { passed: boolean; failMessage?: string }): string {
    const md = this.renderMarkdown(meta);
    const out = path.join(this.runDir, "findings.md");
    fs.writeFileSync(out, md, "utf8");
    return out;
  }

  private renderMarkdown(meta: { passed: boolean; failMessage?: string }): string {
    const totalConsoleErrors = this.views.reduce(
      (n, v) => n + v.console.filter((c) => c.severity === "error").length,
      0,
    );
    const totalConsoleWarnings = this.views.reduce(
      (n, v) => n + v.console.filter((c) => c.severity === "warning").length,
      0,
    );
    const totalNetwork = this.views.reduce((n, v) => n + v.network.length, 0);
    const totalAxe = this.views.reduce((n, v) => n + v.axe.length, 0);

    const lines: string[] = [];
    lines.push(`# auditloop self-audit — ${this.stamp}`);
    lines.push("");
    lines.push(
      "Re-runnable walk of auditloop's OWN UI (`make ux-audit`) — auditloop auditing " +
        "itself. Local DEV_MODE server only; all paid/side-effecting keys unset. Each " +
        "section is one view: a full-page screenshot, the deterministic regression " +
        "signals (console/network/axe), and a blank **UX notes** scaffold for a human " +
        "to fill — then hand the whole doc to an implementing Claude.",
    );
    lines.push("");
    lines.push("## Summary");
    lines.push("");
    lines.push(`- **Result:** ${meta.passed ? "✅ PASS" : "❌ FAIL"}`);
    if (!meta.passed && meta.failMessage) {
      lines.push(`- **Failure:** ${meta.failMessage}`);
    }
    lines.push(`- **Views walked:** ${this.views.length}`);
    lines.push(`- **Console errors:** ${totalConsoleErrors}`);
    lines.push(`- **Console warnings:** ${totalConsoleWarnings}`);
    lines.push(`- **Failed network requests (4xx/5xx/failed):** ${totalNetwork}`);
    lines.push(`- **axe-core a11y violations (total nodes across views):** ${totalAxe}`);
    lines.push("");
    lines.push("| # | View | Route | Console err/warn | Net fails | a11y |");
    lines.push("|---|------|-------|------------------|-----------|------|");
    this.views.forEach((v, i) => {
      const e = v.console.filter((c) => c.severity === "error").length;
      const w = v.console.filter((c) => c.severity === "warning").length;
      lines.push(
        `| ${i + 1} | ${v.title} | \`${v.route}\` | ${e}/${w} | ${v.network.length} | ${v.axe.length} |`,
      );
    });
    lines.push("");
    lines.push("---");
    lines.push("");

    for (const [i, v] of this.views.entries()) {
      lines.push(`## ${i + 1}. ${v.title}`);
      lines.push("");
      lines.push(`**Route:** \`${v.route}\``);
      lines.push("");
      if (v.note) {
        lines.push(v.note);
        lines.push("");
      }
      if (v.error) {
        lines.push(`> ⚠️ **Step issue:** ${v.error}`);
        lines.push("");
      }
      if (v.screenshot) {
        lines.push(`![${v.title}](./${v.screenshot})`);
        lines.push("");
      }

      lines.push("**Deterministic findings:**");
      lines.push("");
      // Console
      if (v.console.length === 0) {
        lines.push("- Console: clean (no errors/warnings)");
      } else {
        lines.push("- Console:");
        for (const c of v.console) {
          const loc = c.location ? ` _(${c.location})_` : "";
          lines.push(`  - **${c.severity}**: ${oneLine(c.text)}${loc}`);
        }
      }
      // Network
      if (v.network.length === 0) {
        lines.push("- Network: no failed requests");
      } else {
        lines.push("- Network failures:");
        for (const n of v.network) {
          lines.push(`  - \`${n.status} ${n.method}\` ${n.url}`);
        }
      }
      // Axe
      if (v.axe.length === 0) {
        lines.push("- Accessibility (axe-core): no violations");
      } else {
        lines.push("- Accessibility (axe-core) violations:");
        for (const a of v.axe) {
          lines.push(`  - \`${a.id}\` (${a.impact ?? "n/a"}, ${a.nodes} node(s)): ${a.help}`);
          for (const cn of a.contrastNodes ?? []) {
            const ratio = cn.contrastRatio != null ? `${cn.contrastRatio}:1` : "?";
            const exp = cn.expected != null ? ` (needs ≥${cn.expected}:1)` : "";
            const font =
              cn.fontSize || cn.fontWeight
                ? ` — ${cn.fontSize ?? ""} ${cn.fontWeight ?? ""}`.trimEnd()
                : "";
            lines.push(
              `    - \`${cn.target}\` — fg \`${cn.fgColor ?? "?"}\` on bg \`${cn.bgColor ?? "?"}\` = ${ratio}${exp}${font}`,
            );
          }
          for (const hn of a.headingNodes ?? []) {
            lines.push(`    - \`${hn.target}\` — ${oneLine(hn.html ?? "")}`);
          }
        }
      }
      lines.push("");
      lines.push("**UX notes:** _(fill in — what feels broken/confusing/over-complex; what to change)_");
      lines.push("");
      lines.push("");
      lines.push("---");
      lines.push("");
    }

    return lines.join("\n");
  }
}

function oneLine(s: string): string {
  return s.replace(/\s+/g, " ").trim().slice(0, 500);
}

/**
 * Inventory the page's interactive controls from its ARIA snapshot. The snapshot
 * yields YAML lines like `- button "Save"` / `- textbox "Email"`; we parse the
 * role + accessible name for the roles we care about, dedupe, and sort. Reading
 * from the ARIA tree (rather than raw DOM) means the "name" is the accessible
 * name — what a screen-reader user hears — so a control losing its label reads as
 * a change.
 */
async function inventoryControls(page: Page): Promise<Control[]> {
  let yaml = "";
  try {
    yaml = await page.locator("body").ariaSnapshot();
  } catch {
    return [];
  }
  const seen = new Set<string>();
  const out: Control[] = [];
  for (const line of yaml.split("\n")) {
    // `  - button "Save":`  or  `- textbox "Email"`  or  `- link`
    const m = line.match(/^\s*-\s+([a-z]+)(?:\s+"((?:[^"\\]|\\.)*)")?/);
    if (!m) continue;
    const role = m[1];
    if (!CONTROL_ROLES.has(role)) continue;
    const name = (m[2] ?? "").replace(/\\"/g, '"').trim();
    const k = `${role} ${name}`;
    if (seen.has(k)) continue;
    seen.add(k);
    out.push({ role, name });
  }
  out.sort((a, b) =>
    a.role !== b.role ? a.role.localeCompare(b.role) : a.name.localeCompare(b.name),
  );
  return out;
}
