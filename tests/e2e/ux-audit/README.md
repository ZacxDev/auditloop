# auditloop self-audit harness (`make ux-audit`)

A curated Playwright UX-audit walk of **auditloop's OWN UI** — auditloop auditing
itself. It boots a local DEV_MODE auditloop, walks its key funnel, captures a
full-page screenshot + deterministic findings (console errors / failed network
requests / axe-core a11y) per view, writes a markdown findings doc, and — opt-in —
**pushes the whole run back into auditloop via its own plugin API** (P5), where it
becomes a run that flows into the same dashboard and P2 regression diffing.

It is the direct port of an external ux-audit harness (auditloop was spun
out of that lineage); `_lib/audit.ts` (the `UxAudit` capture lib) and `_lib/push.ts`
(the push shim) port ~verbatim, and the push schema matches auditloop's own
`internal/plugin/schema.go`.

## What the walk captures

The funnel (`auditloop-funnel.audit.ts`), each state `captureView`'d:

1. `/login` — the sign-in page (DEV_MODE shows an “Enter dashboard” bypass).
2. `/dashboard` — the target list **empty state** + the add-target / add-plugin forms.
3. **Create a target** (self-crawl: `base_url` = this instance's `/login`) → the
   dashboard with the new target card.
4. The **target page** — “Run audit”, the **P4 Authentication / login-recipe card**
   (enabled via a throwaway local AES key), and the runs list.
5. **Plugin target create flow** — the one-time **push-token reveal** screen.
6. The **plugin target page** — push instructions, uploader example, Rotate token.
7. **Seed a real self-crawl run** against the local instance → the **run view**
   in-progress, then **completed** (per-page screenshots, axe findings,
   origin-classified console/network errors, and the key-gated P3 “Draft UX notes”
   control).
8. A **second run** → the **P2 “Changes since previous run”** diff card.

Core views (login, dashboard, a target, a completed run) are asserted; any optional
view that can't be reached is record+skipped (never a hard fail).

Run dirs land under `tests/e2e/ux-audit-runs/<timestamp>/` (gitignored):
`NN-<slug>.png` per view + a `findings.md`.

## Run it

```bash
make ux-audit               # boot local DEV_MODE auditloop + walk (auto-teardown)
make ux-audit-push-test     # unit-test the push shim (node:test, no browser)
```

The Playwright config (`tests/e2e/playwright.ux-audit.config.ts`) boots the Go
binary itself as its `webServer`:

- `DEV_MODE=true` (auth bypassed → dev user), `AUDITLOOP_ROLE=all`.
- `DATABASE_DRIVER=sqlite` + a throwaway `DATABASE_PATH` (wiped on boot).
- **filesystem storage backend** (`S3_ENDPOINT` unset; `S3_LOCAL_DIR` → a temp dir) —
  no MinIO/S3 write.
- `CRAWL_ALLOW_LOOPBACK=true` so the self-crawl of the local instance isn't
  SSRF-blocked (every other private/metadata range stays blocked; **never set in prod**).
- `CRAWL_MAX_PAGES=3 CRAWL_MAX_DEPTH=1` — a fast, bounded self-crawl.
- **All paid/side-effecting keys stripped** via `env -u` (OpenRouter, S3, postgres,
  Supabase). A **dummy** `OPENROUTER_API_KEY` + a **dead** `OPENROUTER_BASE_URL`
  (`http://127.0.0.1:9`) are set on purpose so the key-gated “Draft UX notes”
  control renders for the screenshot **without any external call being possible**
  (the walk never triggers generation; the dead port refuses anyway). Mirrors
  a sibling app's dev force-render.
- `AUDITLOOP_ENCRYPTION_KEY` = a throwaway local hex key, to render the P4 auth UI.

**No external side effects**: local sqlite + filesystem only, nothing reaches
openrouter.ai / S3 / prod.

## Enable the self-push (auditloop → auditloop)

The push is **opt-in and non-fatal**. Without config, the walk still captures
locally and the push is skipped.

1. In the live/dev auditloop app, create a **plugin target** (dashboard → “Add a
   plugin target (push-only)”). Copy the **push token** shown once.
2. Point the walk at it:

   ```bash
   AUDITLOOP_PUSH_URL="https://auditloop.example.com" \
   AUDITLOOP_PUSH_TOKENS='{"auditloop-funnel":"<PUSH_TOKEN>"}' \
   make ux-audit
   ```

The shim maps each view → the plugin schema (`url` = the view slug, the stable P2
diff identity) and POSTs a multipart run to `POST {URL}/api/plugins/runs` with
`Authorization: Bearer <token>`. Re-runs diff against the previous push.

## nixos / chromium notes

- **Playwright browsers won't download/run on nixos** — do NOT `playwright install`.
  The config drives a **nix-provided Chromium** via `executablePath`, resolved
  host-agnostically: `PLAYWRIGHT_CHROMIUM` → a `chromium`/`chrome`/`brave` on PATH.
  Easiest on a fresh host: `nix-shell -p chromium --run "make ux-audit"`.
- The **same** chromium is handed to the auditloop Go app as `AUDITLOOP_CHROMIUM`
  so its chromedp crawler can seed the self-crawl run.
- `npx` is broken here — call `./node_modules/.bin/playwright` directly (the Makefile
  targets do).
