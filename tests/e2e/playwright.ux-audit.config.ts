import { defineConfig, devices } from "@playwright/test";
import path from "node:path";
import { execSync } from "node:child_process";

/**
 * Dedicated config for auditloop's SELF-AUDIT loop (`make ux-audit`).
 *
 * auditloop audits ITSELF: the webServer below boots the auditloop Go binary in
 * DEV_MODE, and the curated walk (`ux-audit/auditloop-funnel.audit.ts`) drives
 * auditloop's OWN UI, full-page-screenshots every view, captures deterministic
 * findings (console errors / failed network requests / axe-core a11y) into a
 * timestamped run dir, emits a markdown findings doc, and — opt-in — PUSHES the
 * run back into auditloop via its own plugin API. See tests/e2e/ux-audit/README.md.
 *
 * SAFETY: this is LOCAL-ONLY.
 *  - Every paid / side-effecting key is EXPLICITLY UNSET (`env -u …`) so no real
 *    OpenRouter (LLM) call or S3/MinIO write can ever fire — the app falls back to
 *    the filesystem storage backend (S3_ENDPOINT unset) and sqlite.
 *  - The crawler self-crawls the LOCAL instance only (loopback), gated behind the
 *    dev-only `CRAWL_ALLOW_LOOPBACK` flag; the SSRF guard still blocks every other
 *    private/metadata range. Never point this at a remote/live server.
 *  - A DUMMY OpenRouter key + a DEAD loopback base URL are set on purpose (see
 *    below) so the key-gated P3 "Draft UX notes" CONTROL renders for a screenshot
 *    WITHOUT any external call being possible. Generation is never triggered by the
 *    walk, and the base URL points at a refused loopback port, so nothing reaches
 *    openrouter.ai. This mirrors a sibling app's dev force-render trick.
 */
const PROJECT_ROOT = path.resolve(__dirname, "..", "..");
const PORT = Number(process.env.UX_AUDIT_PORT ?? 8137);
const BASE_URL = `http://localhost:${PORT}`;

// Throwaway sqlite db + fs-storage dir, isolated from the Go e2e scratch. The
// webServer wipes both on boot so a re-run walks a clean flow (empty dashboard).
const DB_PATH = process.env.UX_AUDIT_DB ?? "/tmp/auditloop-ux-audit.sqlite";
const ARTIFACTS_DIR = process.env.UX_AUDIT_ARTIFACTS ?? "/tmp/auditloop-ux-audit-artifacts";

/**
 * nixos: Playwright's own downloaded browsers won't run (the dynamic linker
 * can't find their shared libraries). Drive a Chromium provided by Nix instead.
 * Host-agnostic resolution order:
 *   1. PLAYWRIGHT_CHROMIUM (explicit override)
 *   2. a Chromium/Chrome/Brave on PATH (e.g. `nix-shell -p chromium --run …`)
 * The SAME binary is handed to the auditloop Go app as AUDITLOOP_CHROMIUM so its
 * chromedp crawler can self-crawl.
 */
function resolveChromium(): string {
  if (process.env.PLAYWRIGHT_CHROMIUM) return process.env.PLAYWRIGHT_CHROMIUM;
  for (const bin of ["chromium", "chromium-browser", "google-chrome-stable", "chrome", "brave"]) {
    try {
      const p = execSync(`command -v ${bin}`, { stdio: ["ignore", "pipe", "ignore"] })
        .toString()
        .trim();
      if (p) return p;
    } catch {
      /* not on PATH — try the next */
    }
  }
  throw new Error(
    "No chromium/chrome found. Set PLAYWRIGHT_CHROMIUM or put one on PATH " +
      "(e.g. `nix-shell -p chromium --run 'make ux-audit'`). Never `playwright install` on nixos.",
  );
}
const NIX_CHROMIUM = resolveChromium();

/**
 * Keys STRIPPED from the child env via `env -u`. Belt-and-braces over merely
 * "not setting" them: even if the parent shell exported a real key (a sourced
 * .env), `-u` guarantees the child never sees them → no external side effect.
 */
const STRIP_KEYS = [
  "OPENROUTER_API_KEY", // real LLM key — re-added below as a DUMMY (dead base URL)
  "S3_ENDPOINT", // unset → filesystem storage backend (no MinIO/S3 write)
  "S3_ACCESS_KEY",
  "S3_SECRET_KEY",
  "DATABASE_URL", // unset → sqlite (never a real postgres)
  "SUPABASE_URL",
  "SUPABASE_JWT_SECRET",
]
  .map((k) => `-u ${k}`)
  .join(" ");

// A throwaway AES-256 key (64 hex chars) enables the P4 Authentication /
// login-recipe UI so the walk can screenshot it. Local-only, never leaves the box.
const THROWAWAY_ENCRYPTION_KEY =
  "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff";

const buildAndServe = [
  "./node_modules/.bin/tailwindcss -i static/input.css -o static/output.css --minify",
  "go build -o bin/auditloop .",
  `rm -f ${DB_PATH} ${DB_PATH}-shm ${DB_PATH}-wal`,
  `rm -rf ${ARTIFACTS_DIR}`,
  // DUMMY OpenRouter key + a DEAD loopback base URL: renders the key-gated "Draft
  // UX notes" control for the screenshot; any accidental generation would hit the
  // refused port 127.0.0.1:9 and degrade — it can NEVER reach openrouter.ai.
  `env ${STRIP_KEYS} ` +
    `DEV_MODE=true AUDITLOOP_ROLE=all CRAWL_ALLOW_LOOPBACK=true ` +
    `PORT=${PORT} DATABASE_DRIVER=sqlite DATABASE_PATH=${DB_PATH} S3_LOCAL_DIR=${ARTIFACTS_DIR} ` +
    `AUDITLOOP_CHROMIUM=${NIX_CHROMIUM} AUDITLOOP_ENCRYPTION_KEY=${THROWAWAY_ENCRYPTION_KEY} ` +
    `CRAWL_MAX_PAGES=3 CRAWL_MAX_DEPTH=1 ` +
    `OPENROUTER_API_KEY=dummy-local-only-not-a-real-key OPENROUTER_BASE_URL=http://127.0.0.1:9 ` +
    `./bin/auditloop`,
].join(" && ");

export default defineConfig({
  testDir: "./ux-audit",
  testMatch: /.*\.audit\.ts$/,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  // The walk seeds a real self-crawl (chromedp), so give it room.
  timeout: 240_000,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: BASE_URL,
    trace: "off",
    // auditloop is a PWA: its root-scoped service worker (`/sw.js`) otherwise
    // intercepts navigations and keeps the network non-idle, stalling goto/waits.
    // Block SW registration — we're auditing the pages, not the offline shell.
    serviceWorkers: "block",
    // The harness takes its own full-page screenshots per view; don't also
    // auto-capture on failure (keeps the run dir clean + deterministic).
    screenshot: "off",
    viewport: { width: 1280, height: 900 },
    launchOptions: {
      executablePath: NIX_CHROMIUM,
      args: ["--no-sandbox", "--disable-gpu"],
    },
  },
  projects: [
    {
      name: "ux-audit",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } },
    },
  ],
  webServer: {
    command: buildAndServe,
    cwd: PROJECT_ROOT,
    url: BASE_URL,
    // Reuse a server already listening (re-runs are cheap); the boot wipes the db.
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
