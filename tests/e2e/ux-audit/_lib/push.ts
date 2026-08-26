/**
 * Opt-in shim that pushes a completed ux-audit run to the auditloop
 * plugin-ingestion API (POST {AUDITLOOP_PUSH_URL}/api/plugins/runs).
 *
 * It is OPT-IN and NON-FATAL by construction:
 *   - no AUDITLOOP_PUSH_URL, or no token for this spec → skip silently (the walk
 *     is unaffected — the push env is only read here);
 *   - any HTTP/network/timeout error → log a warning and RETURN normally, so a
 *     push failure can NEVER fail the ux-audit run.
 *
 * The metadata JSON mirrors auditloop's `internal/plugin/schema.go` PushPayload
 * EXACTLY (the server decodes with DisallowUnknownFields, so an extra key is
 * rejected — we emit only the documented fields). Each file part's multipart
 * field NAME equals the referenced screenshot filename (the server maps
 * `MultipartForm.File[name]` → the metadata's `screenshot` ref).
 */
import * as fs from "node:fs";
import * as path from "node:path";
import type { ViewFinding } from "./audit";

/** One normalized issue on a pushed page (mirrors plugin.PushFinding). */
export interface PushFinding {
  type: "a11y" | "console" | "network" | "perf" | "other";
  severity: string;
  detail: string;
}

/** One captured view (mirrors plugin.PushPage). `url` is the STABLE diff identity. */
export interface PushPage {
  url: string;
  viewport: "desktop" | "mobile";
  screenshot: string;
  axe?: string;
  network?: string;
  axe_violations: number;
  console_first_party: number;
  console_third_party: number;
  network_first_party: number;
  network_third_party: number;
  findings: PushFinding[];
}

/** The top-level push body (mirrors plugin.PushPayload). */
export interface PushMetadata {
  label: string;
  pages: PushPage[];
}

/** The minimal audit surface pushRun needs (so tests can pass a plain object). */
export interface Auditish {
  findings: ViewFinding[];
  runDir: string;
  stamp: string;
}

/** Per-finding detail cap (keep the payload bounded; server renders escaped). */
const MAX_DETAIL = 500;
/** Skip a push whose screenshots exceed this (server caps the body at 64 MiB). */
const MAX_BODY_BYTES = 60 * 1024 * 1024;
/** Network timeout for the push POST. */
const PUSH_TIMEOUT_MS = 60_000;

function trunc(s: string): string {
  const one = s.replace(/\s+/g, " ").trim();
  return one.length > MAX_DETAIL ? one.slice(0, MAX_DETAIL - 1) + "…" : one;
}

/**
 * Map the harness's ViewFinding[] → the auditloop push schema. Pure + deterministic.
 *
 * Conventions:
 *   - `url` = view.slug — the harness's STABLE, unique-per-spec view identity
 *     (what the P2 diff engine matches on run-to-run).
 *   - `viewport` = "desktop" — the harness captures one full-page desktop viewport.
 *   - `axe_violations` = SUM of axe `.nodes` across the view's rule findings
 *     (matches the harness's own `axeCount()` node semantics — a rule that
 *     regresses on N elements counts N, not 1).
 *   - `console_first_party` / `network_first_party` = the view's console / network
 *     finding counts; third-party counts are always 0 (the walk is keys-stripped
 *     and same-origin, so every signal is first-party).
 *   - A view with no screenshot file recorded is skipped (defensive — the server
 *     requires a screenshot per page).
 */
export function viewsToPushMetadata(views: ViewFinding[], label: string): PushMetadata {
  const pages: PushPage[] = [];
  for (const v of views) {
    if (!v.screenshot) continue; // no screenshot recorded → nothing to push for this view
    const findings: PushFinding[] = [];
    for (const a of v.axe) {
      findings.push({ type: "a11y", severity: a.impact ?? "", detail: trunc(`${a.id} — ${a.help}`) });
    }
    for (const c of v.console) {
      const detail = c.location ? `${c.text} (${c.location})` : c.text;
      findings.push({ type: "console", severity: c.severity, detail: trunc(detail) });
    }
    for (const n of v.network) {
      findings.push({ type: "network", severity: "", detail: trunc(`${n.status} ${n.method} ${n.url}`) });
    }
    pages.push({
      url: v.slug,
      viewport: "desktop",
      screenshot: v.screenshot,
      axe_violations: v.axe.reduce((sum, a) => sum + a.nodes, 0),
      console_first_party: v.console.length,
      console_third_party: 0,
      network_first_party: v.network.length,
      network_third_party: 0,
      findings,
    });
  }
  return { label, pages };
}

/** Resolve this spec's push token from the AUDITLOOP_PUSH_TOKENS JSON env. */
function specToken(spec: string): string | undefined {
  const raw = process.env.AUDITLOOP_PUSH_TOKENS;
  if (!raw || !raw.trim()) return undefined;
  let map: Record<string, unknown>;
  try {
    map = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    // eslint-disable-next-line no-console
    console.warn("auditloop push: AUDITLOOP_PUSH_TOKENS is not valid JSON — skipping");
    return undefined;
  }
  const t = map?.[spec];
  return typeof t === "string" && t.trim() ? t.trim() : undefined;
}

// Guards against a double-push if pushRun is ever reached twice for one run.
const pushed = new WeakSet<object>();

/**
 * Push a completed audit run to auditloop. Opt-in + NON-FATAL: see the module
 * doc. `spec` is the funnel name ("sales-audit" | "outreach" | "core-pages" |
 * "autodiscovery"); its token is looked up in AUDITLOOP_PUSH_TOKENS.
 */
export async function pushRun(audit: Auditish, spec: string): Promise<void> {
  if (pushed.has(audit)) return;

  const base = process.env.AUDITLOOP_PUSH_URL?.trim();
  const token = specToken(spec);
  if (!base || !token) {
    // eslint-disable-next-line no-console
    console.log(`auditloop push: not configured for ${spec}, skipping`);
    return;
  }
  pushed.add(audit);

  try {
    const meta = viewsToPushMetadata(audit.findings, `${spec} ${audit.stamp}`);
    if (meta.pages.length === 0) {
      // eslint-disable-next-line no-console
      console.log(`auditloop push: no captured pages for ${spec}, skipping`);
      return;
    }

    const form = new FormData();
    form.append("metadata", JSON.stringify(meta));
    let total = 0;
    const kept: PushPage[] = [];
    for (const pg of meta.pages) {
      const abs = path.join(audit.runDir, pg.screenshot);
      let buf: Buffer;
      try {
        buf = fs.readFileSync(abs);
      } catch {
        continue; // screenshot file vanished — drop the page (defensive)
      }
      total += buf.length;
      // Field name === the referenced filename (server maps parts by field name).
      form.append(pg.screenshot, new Blob([buf], { type: "image/png" }), pg.screenshot);
      kept.push(pg);
    }

    // If some screenshots were unreadable, re-emit metadata for only the kept
    // pages so the server's strict refs↔parts integrity check still passes.
    if (kept.length !== meta.pages.length) {
      if (kept.length === 0) {
        // eslint-disable-next-line no-console
        console.log(`auditloop push: no readable screenshots for ${spec}, skipping`);
        return;
      }
      form.set("metadata", JSON.stringify({ label: meta.label, pages: kept }));
    }
    if (total > MAX_BODY_BYTES) {
      // eslint-disable-next-line no-console
      console.warn(`auditloop push: ${spec} payload ${total} bytes exceeds ${MAX_BODY_BYTES} cap, skipping`);
      return;
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), PUSH_TIMEOUT_MS);
    try {
      const res = await fetch(`${base.replace(/\/+$/, "")}/api/plugins/runs`, {
        method: "POST",
        headers: { Authorization: `Bearer ${token}` },
        body: form,
        signal: controller.signal,
      });
      if (!res.ok) {
        const body = await res.text().catch(() => "");
        // eslint-disable-next-line no-console
        console.warn(`auditloop push: ${spec} failed — HTTP ${res.status} ${trunc(body)}`);
        return;
      }
      const result = (await res.json().catch(() => null)) as { run_id?: string; url?: string } | null;
      // eslint-disable-next-line no-console
      console.log(`auditloop push: ${spec} pushed ${kept.length} page(s) → ${result?.url ?? result?.run_id ?? "(ok)"}`);
    } finally {
      clearTimeout(timer);
    }
  } catch (e) {
    // NON-FATAL: a push failure must never fail the ux-audit run.
    // eslint-disable-next-line no-console
    console.warn(`auditloop push: ${spec} error — ${(e as Error).message}`);
  }
}
