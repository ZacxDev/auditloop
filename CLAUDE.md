# CLAUDE.md — auditloop

Guidance for Claude Code when working in this repository.

## What this is

**auditloop** is a generic, crawler-based **UX auditor**. A user registers a
**target** (a site they own), triggers an on-demand **run**, and a background
worker crawls the target same-origin (BFS, depth+page capped). For each
page/URL at **multiple viewports** (mobile 390px + desktop 1440px) it captures:

- a **full-page screenshot** (chromedp `captureBeyondViewport`),
- an **axe-core accessibility scan** (vendored `axe.min.js`, injected per page),
- **origin-classified console + network errors** — first-party (the page's own
  origin, real UX signal) vs third-party (analytics/CDNs, bucketed separately,
  never dropped), split via `new URL().origin`,
- basic per-page load timing.

Artifacts (screenshots, `axe.json`, `network.json`, and a run-level
`report.json`) go to **S3/MinIO**; metadata goes to **Postgres/SQLite**. The web
UI is a server-rendered **PWA** to browse runs → pages → findings, with
screenshots served via **presigned URLs** (S3) or an authed proxy (filesystem).

## Roadmap (P0–P5)

| Phase | Scope | Status |
|-------|-------|--------|
| **P0** | Scaffold: single binary, config, auth, DB, storage, PWA shell, health/metrics | **done** |
| **P1** | Generic public crawl end-to-end (BFS, multi-viewport, axe, origin-classified errors, SSRF guard, artifacts, report.json) | **done** |
| **P2** | Regression diffing between runs: visual (pixel) + a11y-rule + console/network deltas vs the previous completed run | **done** |
| **P3** | Multi-model vision-LLM per-view UX notes (opt-in, paid; server-side OpenRouter key) | **done** |
| **P4** | Login recipes / authenticated crawls (`targets.auth_mode='login'`): encrypted-at-rest credentials + a canonical login-step recipe run before the crawl in the same browser context | **done** |
| **P5** | Plugin push API — a push-only customer app / harness POSTs a completed run's artifacts (`auth_mode='plugin'`), stored as a run that flows into the same dashboard + P2 diffing | **done** |

This slice delivers **P0 + P1 + P2 + P3 + P4 + P5**.

## P2 — regression diffing (done)

Once a run reaches `done`, the worker runs a **diff phase** comparing it to the
target's **previous completed run** (the baseline). Baseline linking happens at
`CreateRun`: `db.LatestDoneRunForTarget(targetID, exclude)` picks the newest
`done` run and stamps `runs.prev_run_id` (NULL/"" for a target's first run →
nothing to diff). The diff phase (`internal/worker/diff.go` `runDiff`):

- **Page-set delta** — matches current vs baseline pages by URL → added / removed.
- **Visual diff** per matched page+viewport — pulls both screenshots from the
  `Store`, runs the pure-Go pixel diff (`internal/diff`), persists `pages.diff_pct`
  (0–100, informational) + a **diff image** (changed pixels tinted red over the
  dimmed current shot) at `{target}/{run}/{page_slug}/{viewport}.diff.png`
  (`storage.DiffKey`). **Visual regression vs layout/size change (signal-quality
  rule):** captures are FULL-PAGE, so a change that shifts page height (a new top
  item, a consent banner, lazy-loaded/added content) misaligns every row below it
  and inflates a raw pixel diff toward ~100%. So a capture counts as a **visual
  regression** (drives `pages_changed`, `auditloop_visual_regressions_total`, and
  the UI regression badge) ONLY when dimensions are UNCHANGED (`!size_changed`) AND
  `diff_pct ≥ report.VisualRegressionThreshold` (1%) — the one case the aligned
  pixel diff is trustworthy. A capture whose dimensions changed is reported
  **separately** as a **layout/size change** (`pages_size_changed`, the
  `size_changed` per-page flag, a softer amber UI label) and does NOT fire the
  regression metric. For size-changed pages the (misaligned, ~all-red) diff image
  is **not generated or uploaded**. (`ChangedPage.IsRegression()` encodes the
  `!SizeChanged && ≥threshold` rule.) NOTE a top-insertion on a page whose height
  happens to stay the same is still a false-positive corner case — full
  structural/alignment diffing is future work.
- **a11y rule-set delta** — compares the set of axe rule ids (from persisted a11y
  findings), not just counts → new rules (regressions) + resolved rules.
- **console/network delta** — first-party count deltas (current − prev), run-total.

`internal/diff` is pure/stdlib (no new deps): `Compare(baseline, current []byte)
→ {DiffPct, ChangedPixels, TotalPixels, SizeChanged, TooLarge, DiffPNG}`. Per-pixel
compare with a per-channel tolerance (`DefaultTolerance=16`, absorbs anti-alias/
encoding noise). **Different dimensions never panic:** the overlap is compared
pixel-by-pixel and the non-overlapping area (`union − overlap`) counts as changed,
with `SizeChanged` set (see the regression-vs-layout rule above — callers must gate
the regression signal on `!SizeChanged`). **Memory cap:** full-page captures can be
tens of MP; above `MaxVisualizationPixels` (24 MP = the current image's w×h) the
red-tint visualization allocation (w×h×4 B ≈ 96 MB at 24 MP) is **skipped** —
`TooLarge=true`, `DiffPNG=nil`, but `diff_pct` is still computed from the cheap
counting loop. `StringSetDelta(prev, cur)` is the shared, deterministic (sorted,
de-duped) primitive behind the page-set and a11y-rule deltas.

**Storage decision:** a **new nullable `runs.diff_json` column** (migration
`0013`, dual-dialect `ALTER TABLE … ADD COLUMN … DEFAULT ''`) holds the run-level
`report.Diff` (chosen over overloading `summary_json` — keeps the P1 summary
contract stable). Per-page results live in **`pages.diff_pct REAL`** +
**`pages.diff_key TEXT`** (migrations `0011`/`0012`). The `report.json` contract
gains an **optional `diff` block** (`report.Diff`, a pointer with `omitempty`) —
pre-P2 reports without it still decode, older readers ignore it.

The run view shows a **"Changes since <prev run date>"** card (new/removed pages,
worst-first change thumbnails, new a11y rules, ▲/▼ console/network deltas). The
header carries a red **"N regression"** badge (true same-size visual regressions)
and, separately, a softer amber **"N layout change"** badge; each thumbnail
self-labels — red `diff_pct` badge for a regression, an amber "Layout changed —
page height differs, pixel diff not comparable" note for a size change, and "Not
compared — capture too large" when over the size cap. A subtle "First run — no
baseline" note shows when `prev_run_id` is empty. Metrics:
`auditloop_visual_regressions_total` (counter, regressions only) +
`auditloop_run_pages_changed` (gauge, last diffed run).

## P3 — multi-model vision-LLM UX notes (done)

On a completed run, a **"Draft UX notes"** button opens a **model picker**
(checkboxes; the first curated model is pre-checked). Submitting runs an **async
pass**: for **each selected model × each logical page (URL)** it calls a vision LLM
with **both the desktop AND mobile screenshots** plus grounding context (page URL +
viewports, axe violation rule-ids/count, first-party console/network error counts,
and — when the run has a P2 diff — that page's visual-regression/layout-change
status), producing an **editable free-text markdown UX-notes draft**. The run view
then shows, per page, **each model's notes side by side, labeled by model**, each in
an editable textarea (Save) with a whole-pass **Regenerate**. Opt-in and paid
(per-page, per-model API calls), so it is **key-gated** and cost-guarded.

- **`internal/llm`** — a minimal OpenRouter vision client (no heavy deps). POSTs
  `{base}/chat/completions` with a text prompt + image parts (base64 data URLs) for
  both viewports; Bearer key + `HTTP-Referer`/`X-Title`; parses
  `choices[0].message.content`. **Each screenshot is downscaled to ≤1568px on its
  longest side before sending** (`downscale`, `golang.org/x/image/draw` CatmullRom —
  the one new dep). `max_tokens` capped; context-cancellable; a per-call failure is
  returned as an error so the caller degrades. **The base URL is configurable
  (`OPENROUTER_BASE_URL`) so tests point it at an `httptest` fake; the API key is
  server-side only and NEVER sent to the browser.**
- **`internal/notes`** — the async pass + grounding prompt. `SystemPrompt` +
  `Grounding.UserPrompt()` are kept in code (documented). `Generator.Run(ctx, runID,
  models)` groups the run's pages by URL (canonical page id = the desktop row),
  loads both screenshots once per page, and issues one LLM call per (page, model)
  under a concurrency cap (`DefaultConcurrency=3`); per-(page,model) failures store
  an `error` and continue (degrade, non-fatal) — only ctx-cancel fails the pass.
  It updates run progress and finalizes the notes-job status. `CountUnits` computes
  the job total (pages × models).
- **DB (`page_notes`, migrations 0014–0016):** one row **per (page, model)** —
  `id, page_id, run_id, model, notes TEXT, edited INTEGER(0/1), error TEXT,
  created_at, updated_at`, with a **UNIQUE index on (page_id, model)**. A re-draft
  **replaces** the row (`SavePageNoteDraft`, upsert via UPDATE-else-INSERT, resets
  `edited=0`); a human edit sets `notes` + `edited=1` (`SavePageNoteEdit`). `edited`
  is INTEGER (not BOOLEAN) for dual-dialect portability. Scoping is via the
  page→run→user join (no `user_id` column). Async job tracking lives on **`runs`**
  (migrations 0017–0019): `notes_status` (idle|generating|done|failed), `notes_done`,
  `notes_total`. `ClaimNotesJob` atomically flips → generating (guards double-run);
  `UpdateNotesProgress`/`FinishNotesJob`; **`MarkGeneratingNotesFailed`** is the
  startup sweep (called in `NewRouter`, like `RecoverStaleRuns`) that settles jobs
  orphaned mid-generation by a restart → failed (the pass runs in a background
  goroutine, so a pod restart would otherwise poll forever).
- **LLM cost tracking (migrations 0024–0029):** OpenRouter reports the exact per-call
  USD cost + token counts **only when asked** — the `llm` client adds
  `"usage":{"include":true}` to the request and `Draft` returns `(text, llm.Usage{CostUSD,
  PromptTokens,CompletionTokens}, error)` (usage absent → zero-value, never an error).
  Per-cell: `page_notes.cost_usd REAL` + `prompt_tokens`/`completion_tokens INTEGER`
  (0 for pre-cost/failed rows), recorded by `SavePageNoteDraft`. Per-run:
  `runs.notes_cost_usd REAL` + `notes_prompt_tokens`/`notes_completion_tokens INTEGER`,
  incremented DB-side (`AddNotesCost`, `SET x = x + ?`) so the pass's concurrent
  per-(page,model) writes don't lose updates, and **RESET to 0 by `ClaimNotesJob`** at
  the start of a fresh pass — so a **Regenerate reflects the LATEST pass's cost, not the
  all-time sum** (the new run cols are in the shared `runCols`/`scanRun` hot path).
- **Routes (`handlers/notes.go`):** `POST /api/runs/{id}/notes` (models[] validated
  against the curated allowlist — empty → 400, any unknown id → 400; **503 if
  `!OpenRouterEnabled()`**; run must be `done`) claims the job + spawns the goroutine
  and returns the now-generating fragment. `GET /runs/{id}/notes-status` is the poll
  target (progress bar). `POST /api/pages/{pageId}/notes/{model:.+}` saves a human
  edit (ownership via page→run→user; `{model:.+}` because model ids contain slashes).
- **UI (`components/pages/notes.go`):** the `NotesSection` fragment (id
  `notes-section`) renders only when `OpenRouterEnabled()`; self-polls every 3s while
  generating; per-URL blocks with per-model editable cells (htmx `hx-boost="false"`
  forms). Nil-safe empty state ("No UX notes yet"). **Cost UI:** a per-run line under the
  header ("This pass: ~$0.0123 · N drafts — model $x · model $y") and a subtle per-cell
  badge ("$0.0021 · 1.2k tok") — both render only when cost > 0 (pre-cost notes show no
  badge). `formatUSD` shows ≥4 decimals so sub-cent costs don't collapse to "$0.00".
- **Metrics:** `auditloop_notes_generated_total{model,status}` +
  `auditloop_notes_duration_seconds`, plus LLM-cost counters (per successful draft):
  **`auditloop_notes_cost_usd_total{model}`** (USD), **`auditloop_notes_prompt_tokens_total{model}`**,
  **`auditloop_notes_completion_tokens_total{model}`**.
- **report.json seam:** `PageReport.Notes map[string]string` (`omitempty`) is a
  forward-compat seam only — the crawl writes report.json BEFORE the opt-in notes
  pass, so it is **not populated by the worker** (a future enhancement can re-emit
  report.json after a pass; a P5 plugin push can supply notes directly). `Report.NotesCost`
  (`{cost_usd,prompt_tokens,completion_tokens}`, `omitempty`) is the matching run-level
  cost seam — also nil at crawl-finish, populated by a future re-emit.
- **Env / prod note:** `OPENROUTER_API_KEY` must be wired into the prod secret at
  deploy time (server-side only). The whole feature no-ops without it (button hidden,
  route 503) — code + tests never require a real key (tests use an httptest fake).

## P4 — login recipes / authenticated crawls (done)

A target with **`auth_mode='login'`** carries a stored **login recipe** so the
worker authenticates BEFORE the BFS crawl, in the SAME chromedp browser context,
so the session/cookies carry into the crawl and gated pages get audited.

- **Canonical step model (`internal/recipe`, pure, no chromedp):** a recipe is an
  ordered list of typed steps — a CLOSED set `goto{url}` · `fill{selector,value_ref}`
  · `click{selector}` · `waitFor{selector|url_contains,timeout_ms}`. There is
  **NO `eval`/script step** (no arbitrary-JS/exfil vector); `ParseSteps` uses
  `DisallowUnknownFields` so an injected `script` key is rejected. Credentials are
  **NEVER inlined** — a `fill` step holds a placeholder `value_ref` (`username`|
  `password`); the worker substitutes the decrypted value at run time. Two authoring
  modes compile to the SAME steps: a **guided form** (`GuidedForm.Compile()` →
  goto → fill(user) → fill(pass) → click(submit) → waitFor(success)) and an
  **advanced** raw-JSON step list (multi-step/cookie-banner logins).
  `Validate(steps)` enforces the structural contract (known types, required
  fields, first step is goto, ends with a success waitFor, refs limited to the
  credential placeholders). `DeriveGuided` reverse-maps canonical steps back to the
  guided form for editing (falls back to the advanced editor when they don't match).
- **Encryption at rest (`internal/crypto`, new):** AES-256-GCM, random nonce
  prepended, key from **`AUDITLOOP_ENCRYPTION_KEY`** (hex or base64 → 32 bytes).
  `Encrypt/Decrypt` + `EncryptToBase64/DecryptFromBase64` (TEXT storage). Wrong
  key or tampered blob fails GCM auth (never partial plaintext). The credential
  blob is `recipe.Credentials{username,password,extra}` JSON, encrypted +
  base64'd. `config.LoginRecipesEnabled()` = key present; the router builds the
  cipher at startup (a present-but-INVALID key logs and DISABLES the feature —
  never stores creds under a bad key).
- **DB (`internal/db`, migration `0020`):** table **`login_recipes`** — `target_id`
  (PK, one per target), `login_url`, `steps_json` (canonical, placeholders only),
  `success_selector`/`success_url_contains`/`success_timeout_ms`, `creds_encrypted`
  (TEXT base64, dual-dialect), `created_at`/`updated_at`. `SetLoginRecipe` upserts +
  flips `targets.auth_mode='login'` in one tx; `DeleteLoginRecipe` clears + flips
  back to `'none'`; `GetLoginRecipe`. Scoped via target→user (no direct user_id).
- **Crawl integration (`internal/crawler/login.go` + `internal/worker`):**
  `crawler.Options.Login *LoginConfig` (canonical steps + DECRYPTED creds map). When
  set, `Crawl` runs `runLogin` in the shared tab BEFORE the BFS. **Every `goto` URL
  passes the existing `GuardConfig.CheckURL`** (SSRF + same-domain via `AllowedHosts`
  = the target's verified domains) at BOTH save time (handler) AND run time
  (preflight). **Login-failure detection:** the success `waitFor` not met within its
  timeout → `*crawler.ErrLoginFailed`; the worker maps it to a failed run
  ("login recipe failed — check selectors/credentials") and does **NOT** crawl the
  login wall. A post-login landing on an off-domain host → fail (third-party SSO is
  unsupported). Selectors are used only as chromedp selectors, never eval'd. The
  worker's `buildLogin` decrypts creds (needs `w.Cipher`; nil → run fails with a
  clear message). **Credentials are redacted from all logs, error messages, and
  report.json** — only `auth_mode:"login"` appears in the report, never a value.
- **Handlers + UI:** the target page gains an **Authentication** card
  (`components/pages/auth_section.go`, `AuthVM`), rendered only when the feature is
  enabled: auth-mode toggle (none/login), the guided form + an "Advanced (edit
  steps)" `<details>` with the canonical JSON, and **write-only credential fields**
  (`•••• (set — leave blank to keep)`; a blank field on update preserves the stored
  value — never rendered back). Routes (ownership-checked): `POST
  /api/targets/{id}/auth` (save/clear → validate → same-domain+SSRF guard → encrypt
  → persist) and `POST /api/targets/{id}/login-test` (runs ONLY the login steps in a
  fresh headless context via `crawler.RunLoginProbe`, stores an **end-state
  screenshot** to the `Store` under `storage.LoginTestKey`, returns pass/fail + the
  presigned screenshot; creds decrypted server-side, never returned). Both **503**
  when `!LoginRecipesEnabled()`.
- **Metrics:** `auditloop_login_attempts_total{status}` (`ok|failed|probe_ok|probe_failed`).
- **Tests:** `internal/crypto` (round-trip, wrong-key, tamper, distinct nonces, key
  parsing); `internal/recipe` (guided→canonical compile, reject unknown step type /
  inline credential, no plaintext in steps); `internal/db` (upsert + auth_mode flip +
  delete); `internal/crawler` (login preflight rejects off-domain/private/metadata
  URLs); `internal/worker` (login runs before crawl, decrypts creds, failure→run
  fails+crawl skipped, no cred leak); `handlers` (503 gated, encrypt-at-rest, reject
  foreign domain / private IP / unknown step, target page redacts creds); **e2e**
  `TestEndToEndLoginRecipe` (login-gated fixture → authed crawl reaches the gated
  deep link, unauthed does not, login-test succeeds + stores a screenshot,
  wrong-password fails the run). **Deferred:** third-party SSO/IdP unsupported (MVP
  is same-domain form login); a single credential set per target; multi-replica would
  need the same lease/TTL caveat as the run claim.

## P5 — plugin push ingestion (done)

A target with **`auth_mode='plugin'`** is **PUSH-ONLY**: auditloop never crawls
it. An external harness (an external ux-audit harness, CI, …) POSTs a completed
run's artifacts and they land as a run under that target — flowing into the SAME
dashboard, getting **P2 regression diffing** vs the previous push, and P3 notes.

- **Push token (`internal/plugin/token.go`, `internal/db/plugin_tokens.go`, migrations
  0021–0022):** creating a plugin target mints a **32-byte `crypto/rand` → base64url**
  token, **shown ONCE**. Only its **`sha256` (hex)** is stored (`plugin_tokens`, one row
  per target, `target_id` PK + unique `token_hash`) — tokens are not reversible from the
  DB. Lookup hashes the presented token → `PluginTokenLookup` JOINs `plugin_tokens`→
  `targets` **filtered on `auth_mode='plugin'`** (so a login/none target's id can't be
  pushed to) and the handler does a **`crypto/subtle` constant-time** compare of the
  stored hash. **Rotatable** (`SetPluginToken` upserts the hash → old token invalid);
  one active token per target. No encryption key needed (hashed, not encrypted).
- **Push API — `POST /api/plugins/runs` (`handlers/plugins.go`):** a PUBLIC route
  (registered outside the Supabase auth subrouter) authenticated by the token in an
  **`Authorization: Bearer <token>`** header (NOT a URL param → no token in access
  logs). Unknown/rotated token, or a non-plugin target → **401**. Body is
  **`multipart/form-data`**: a **`metadata`** part (the push-schema JSON) + one file
  part per referenced artifact (`CreateFormFile`, **form-name = filename**). Wrapped in
  `http.MaxBytesReader` (64 MiB total) + `ParseMultipartForm` (16 MiB memory), per-file
  cap 16 MiB. **Validates FULLY before storing anything:** `DisallowUnknownFields`,
  every metadata-referenced filename has a matching part AND no orphan parts, ≤200 pages,
  known viewport (`mobile|desktop`) + finding type, and each **screenshot content-type is
  sniffed** (`http.DetectContentType` → must be PNG/JPEG, declared type ignored). On any
  violation → **400** (credential-free message), nothing persisted. Over-cap body → **413**;
  per-target rate limit (2 s spacing, mirrors the P4 login-test limiter; relaxed under
  `DEV_MODE`) → **429**. On success: creates a **done** run (`db.CreatePushedRun`,
  `trigger='plugin'`, optional `label`, baseline-linked to the prev done run), inserts
  pages+findings, uploads artifacts under the normal key scheme, runs the **existing P2
  diff** (`worker.Diff` — the exact crawl-worker diffing, reused) vs the previous push,
  writes `report.json` (`auth_mode:"plugin"`), and returns JSON `{run_id, url}`.
- **Push schema (`internal/plugin/schema.go`, `map.go`):** an ingestion-oriented mirror
  of `report.PageReport` — `{label?, pages:[{url, viewport, screenshot, axe?, network?,
  axe_violations, console_first_party, console_third_party, network_first_party,
  network_third_party, findings:[{type, severity, detail}]}]}`. **`url` is the STABLE
  page identity used for P2 diff matching across pushes** — the shim MUST emit a
  consistent `url` per logical view (a real URL, or a label like `signup-step-3`) so
  regressions track the same view run to run. `Parse`/`Validate`/`MapPage` (payload +
  uploaded-file keys → the `db.Page`/`db.Finding`/`report.PageReport` the DB layer
  expects; finding `detail` is stored as **escaped JSON** — untrusted input, rendered
  escaped in the UI).
- **Pushed a11y findings must carry a top-level rule `id` for the P2 a11y-rule delta
  (`new_a11y_rules`, what the CI `--fail-on-regression` gate keys on).** The diff
  (`internal/worker/diff.go` `runA11yRuleIDs`) reads each stored a11y finding's detail for a
  top-level `"id"` field — the shape the native crawl stores (the raw axe violation object,
  which already has `id`). `MapPage`'s `a11yDetail` helper (`internal/plugin/map.go`)
  preserves this contract for pushed a11y findings specifically (all other finding types keep
  the plain `{"detail":..}` wrapping): a structured pushed detail that already carries a
  non-empty `id` (a raw axe-violation object, what a modern harness sends) is
  stored **verbatim**; a legacy plain string (a legacy harness's `"<rule-id> — <help text>"` format) has
  its `id` derived as the substring before the first `" — "`, stored as `{"id":..,"detail":..}`.
  Before this (#18), every pushed a11y detail was wrapped as `{"detail":..}` with no top-level
  `id`, so `new_a11y_rules` was **silently always empty for every plugin push** — the CI a11y
  gate was a no-op. **One-time transition:** a target's first diff after the fix compares
  against a pre-fix baseline with no ids, so every current a11y rule flags as "new" once
  (a transient false positive) before self-healing on the next push.
- **Server-computed perf/layout findings (`internal/signals`, shared with the crawl
  worker):** the push carries OPTIONAL RAW MEASUREMENT blocks — `perf` (`PushPerf`:
  `lcp_ms/cls/tbt_ms/weight_bytes/req_count`) and `layout` (`PushLayout`, JSON mirrors
  `crawler.LayoutSmells` exactly: `horizontal_overflow/scroll_width/inner_width/
  small_tap_targets/small_text/missing_viewport_meta/images_no_dims/examples`) — NOT
  perf/layout findings. `MapPage` derives the perf + layout FINDINGS server-side via
  `signals.PerfFindings`/`signals.LayoutFindings` (the EXACT thresholds + mobile-only
  gating the native crawl uses — `internal/worker/signals.go` now delegates to the same
  `internal/signals` package), so a harness never re-implements the
  thresholds. `report.PageReport.Perf`/`.Layout` echo the raw blocks. **Authority rule:
  blocks win** — when a `perf`/`layout` block is present, any hand-authored perf/layout
  finding in the same push is DROPPED (no double-emit); OTHER finding types
  (a11y/console/network/other) always pass through. `examples` selectors are
  attacker-controlled → stored as escaped JSON, rendered escaped. A push with NEITHER
  block behaves exactly as pre-block (backward compatible).
- **UI (`components/pages/plugin.go`):** the dashboard has an **"Add a plugin target
  (push-only)"** card → on create, the **token is shown ONCE** (`PluginTokenReveal`, copy
  affordance) with the push endpoint + an example `auditloop-push` command. The plugin
  target page is push-only — **no "Run audit"**; it shows push instructions + a **Rotate
  token** button (reveals the new token once) + the pushed-run list (reuses the run list +
  P2 diff UI). Ownership-scoped (target→user).
- **Reference uploader — `cmd/auditloop-push` (+ `internal/plugin/upload.go`):** a generic
  standalone CLI (`auditloop-push --url <base> --token <t> --meta metadata.json --files
  <dir>`) — reads the metadata + referenced files, builds the multipart, POSTs, prints the
  run URL. The core (`plugin.Upload`/`UploadFromDisk`) is a lib so it's httptest-testable;
  the CLI is app-agnostic and ships in THIS repo (**no producer-side changes**). A harness
  produces `metadata.json` by mapping its own ux-audit output → `plugin.PushPayload`.
  **Path containment (hardening):** `UploadFromDisk` rejects any metadata-referenced
  filename that isn't a plain basename inside `filesDir` (no `..`/absolute/dir component)
  BEFORE reading or sending anything — a malicious `metadata.json` can't exfiltrate a
  local file (e.g. `../../etc/passwd`) to the push endpoint.
- **Secure response headers (hardening):** a global middleware (`secureHeaders` in
  `handlers/app.go`, wraps the whole mux) sets `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: SAMEORIGIN`, and `Referrer-Policy: strict-origin-when-cross-origin`
  on every response (header-only → transparent to redirects/302s/htmx swaps). **No CSP**
  (deliberately omitted — htmx + inline scripts/styles + supabase-js would break under a
  strict policy; a broken app is worse than a missing CSP). The **artifact route**
  (`handleArtifact`) additionally sets `nosniff` + `Content-Disposition: inline` on BOTH
  backends (streamed FS bytes AND the S3 presign 302) so externally-pushed PNG/JSON can't
  be sniffed into HTML.
- **Metrics:** `auditloop_plugin_pushes_total{status}` (ok|rejected|unauthorized|
  rate_limited|too_large|error) + `auditloop_plugin_pages_ingested_total`.
- **No new prod secret** — reuses the existing DB + S3. Deploy is build+push.
- **Tests:** `internal/plugin` (token generate/hash/constant-time/rotate-invalidation;
  schema validation — missing ref / orphan part / unknown field / bad viewport / too many
  pages; payload→row/findings mapping incl. escaping; the uploader lib against an httptest
  fake); `internal/db` (plugin target + token lifecycle, plaintext never stored, rotate,
  non-plugin target can't resolve, pushed-run baseline linking); `handlers` (no/bad token
  →401, valid push →200 + pages/findings/images, second push →P2 regression, oversized
  →413, rate-limit →429, rotate ownership →404); **e2e** `TestEndToEndPluginPush` (builds
  + runs the real `cmd/auditloop-push` CLI to push 2 runs; asserts pages/findings render,
  images in the Store, and the second push's visual regression surfaces — no chromium).

## Read API — machine-authenticated read-only pulls (done)

Autonomous agents/CLIs pull structured audit results WITHOUT a Supabase user JWT
via a **per-user, read-only API key** (Bearer). It reuses the P5 plugin-token
security pattern (`crypto/rand` → base64url, store ONLY sha256 hex, constant-time
compare, rotatable, shown once) and is **strictly owner-scoped**: a key reads ONLY
runs/targets/artifacts whose `target.user_id == key.user_id`; anything else → **404**
(existence not leaked). **report.json stays the canonical structured contract** —
the API serves the STORED bytes, it doesn't re-serialize.

- **Token (`internal/apikey/token.go`):** `Generate` (32-byte random → base64url +
  its sha256 hex), `Hash`, `ConstantTimeEqual`. It **MIRRORS** `internal/plugin/
  token.go` rather than sharing a package — the two token kinds have different scopes
  (plugin=push-to-one-target vs api-key=read-scoped-to-a-user) and keeping them
  separate avoids coupling the read API to the ingestion package; the primitives are
  ~15 lines and each is unit-tested.
- **DB (`internal/db/api_keys.go`, migrations 0035–0037):** table **`api_keys`** —
  `id` PK, `user_id`, `name`, `token_hash` (UNIQUE index, sha256 hex — plaintext never
  stored), `scope` (DEFAULT `'read'`, future-proofing), `created_at`, `last_used_at`
  (nullable). `CreateAPIKey`, `APIKeyLookup(hash) → (userID, scope, storedHash, found)`
  (for the constant-time compare), `ListAPIKeys(userID)` (**never returns the hash/
  plaintext** — only display metadata), `RevokeAPIKey(userID, id)` (ownership-scoped
  DELETE → ErrNotFound for a foreign id; **rotate = revoke + create**), best-effort
  `TouchAPIKeyLastUsed(hash)`. Plus `LatestDoneRunForTargetOwned(userID, targetID)` —
  a user-scoped variant of the P2 baseline query. All reads reuse the existing
  user-scoped `GetTarget`/`GetRun`/`ListRuns` (the user filter IS the isolation).
- **Auth middleware (`handlers/audit_api.go` `apiKeyAuth`):** Bearer → `apikey.Hash`
  → `APIKeyLookup` → **`crypto/subtle` constant-time** compare → per-key rate limit →
  best-effort last-used stamp → stamps the owner onto the request context via
  `auth.WithClaims` (so the read handlers scope through the SAME `auth.UserID(ctx)` as
  the Supabase-authed ones). Any miss → **401** (credential-free). Guards ONLY the read
  routes — a read key is **NEVER** accepted on a push/mutation route (they live on other
  routers; a read key on `/api/plugins/runs` → 401, tested).
- **Read routes (PUBLIC, key-authed, registered OUTSIDE the Supabase subrouter):**
  - `GET /api/audit/targets/{id}/runs` → JSON `[{run_id,status,trigger,created_at,
    finished_at,prev_run_id,has_diff,pages}]` (`pages` from the run summary's
    `pages_crawled`). **`{id}` accepts the target UUID OR its name** — the
    `resolveTarget` helper tries `GetTarget(userID,{id})` (UUID path) then falls back
    to `GetTargetByName(userID,{id})`, so a consumer can reference the same **stable
    spec name the push side keys on** (`AUDITLOOP_PUSH_TOKENS` is name-keyed), not an
    opaque UUID. **BOTH lookups are `user_id`-scoped** — a name lookup never crosses
    users; a foreign target's name → 404 (no existence leak). On name collision (a
    user with duplicate names) it returns the **most recently created** target.
  - `GET /api/audit/targets/{id}/runs/latest` → the newest `done` run's stored
    `report.json` bytes (404 when none). `{id}` is UUID-or-name, same owner-scoped
    resolution as the run list.
  - `GET /api/audit/runs/{run_id}` → that run's stored `report.json` bytes
    (`GetRun` owner-scoped → 404 for a foreign run), `Content-Type: application/json` +
    nosniff.
  - `GET /api/audit/artifacts/{key:.+}` → raw artifact bytes. **Per-object ownership:**
    the key path is `{target_slug}/{run_id}/…`; the handler resolves the `run_id`
    segment → `GetRun(userID, run_id)` (owner-scoped) and only serves when it belongs to
    the key's user — closing the "authed-but-not-per-object-owner" gap for this route.
    Streams via the shared `streamArtifact` helper (nosniff + `Content-Disposition:
    inline` + content-type; FS stream or S3 — reused from `handleArtifact`).
- **Rate limit — `tokenBucketLimiter` (`handlers/ratelimit.go`):** per-key token bucket,
  **10 req/s, burst 20** (generous for an agent walking a run's pages/artifacts; unlike
  the push route's 2s spacing, which would break multi-read polling). Over-cap → **429**.
  Relaxed under `DEV_MODE` (hermetic tests) only.
- **Management (GATED — Supabase-authed, human mints keys):** `POST /api/keys`
  (form `name` optional) mints a key + returns a one-time reveal fragment
  (`pages.APIKeyReveal`, copy affordance + example curl — token shown ONCE);
  `POST /api/keys/{id}/revoke` (ownership-scoped → 404 for a foreign id → `HX-Refresh`).
  UI: an **"API access (read-only)"** dashboard card (`components/pages/apikeys.go`
  `APIAccessCard`) lists keys (name, created, last-used, Revoke) + a create form. No
  hash/plaintext ever rendered after creation.
- **Consumer env convention:** a client reads its key from **`AUDITLOOP_API_TOKEN`**
  and sends `Authorization: Bearer $AUDITLOOP_API_TOKEN`. No new SERVER secret — keys
  live in the existing DB (hashed).
- **Metrics:** `auditloop_api_reads_total{status}` (`ok|unauthorized|not_found|
  rate_limited|error`).
- **Tests:** `internal/apikey` (generate/hash/constant-time/rotate-invalidates);
  `internal/db` (create/lookup/list/revoke, plaintext+hash never returned by List,
  revoke invalidates + is ownership-scoped, `LatestDoneRunForTargetOwned` cross-user
  isolation, migration applies on sqlite); `handlers` (no/bad/**revoked** key → 401;
  valid key reads its OWN run list + report.json + latest + artifact bytes; **a 2-user
  fixture where user-A's key reading user-B's target/run/latest/artifact → 404** — the
  critical isolation test, with a user-B-reads-own sanity check; read key on a push
  route → 401; burst → 429; management routes require Supabase auth + are
  ownership-scoped create/revoke). **Caveat:** the rate limiter + last-used map are
  in-memory single-replica (same lease/TTL caveat as the run claim + push limiter); a
  multi-replica deploy would move them to a shared store.

## Persona walkthrough — task-grounded evaluator (Phase 1, done)

A SECOND, parallel, opt-in LLM pass (independent of the P3 subjective-visual notes;
the two coexist) that evaluates a completed run as a **task + persona walkthrough**
and emits **structured, selector-anchored, actionable findings** plus a synthesized
run-level "story". Phase 1 does NOT drive the app and does NOT infer goals — it
reasons over the ALREADY-CAPTURED pages of a done run. It reuses the P3 `internal/llm`
client, `OpenRouterEnabled()` gating, the async-job + startup-sweep pattern, the
reset-per-pass cost tracking, and the dual-dialect migration style.

- **Axis = PERSONA, not model.** Phase 1 uses ONE model — the FIRST curated model in
  `AUDITLOOP_LLM_MODELS` (the same config as notes) — across all personas. The four
  curated personas are **code-defined** (`eval.Personas`, documented like
  `notes.SystemPrompt`, validated server-side): `first-time-nontechnical` ·
  `returning-power-user` · `skeptical-evaluator` · `accessibility-constrained` (the
  last complements axe's mechanical checks from a lived-experience angle — it does NOT
  re-run axe).
- **`internal/eval`** — the pass + prompts (pure prompt/parse are unit-tested):
  - **Granularity: per (page × persona) with flow context.** A "flow" = the run's
    pages in creation/id order (for pushed runs = the producer's push order, each page
    a stable `url`). The prompt tells the model: step N of M toward `<job>`, the
    prev/next page URLs, the persona's concerns, and the page's already-computed
    DETERMINISTIC signals (a11y rule ids + counts, perf ratings, layout smells,
    console/network counts) — as GROUNDING, with an explicit rule **not to re-list them
    as findings** (they're captured deterministically and shown separately).
  - **Structured JSON output**, forced via the prompt + parsed + validated (lenient
    about surrounding prose/fences via `extractJSON`; a malformed body or out-of-set
    `comprehension` is an ERROR the cell stores and degrades on — never a panic). Schema
    per (page,persona): `{comprehension: clear|unclear|blocked, blockers:[{issue,
    selector,evidence}], frictions:[…], top_fix:{selector,change,rationale,impact}}`.
    `selector`/`evidence`/`issue` are model-authored + UNTRUSTED → stored as escaped
    JSON, rendered escaped (same rule as layout `examples`).
  - **Verification pass (anti-vagueness):** a SECOND cheap call per cell (toggleable
    `verify`, default on) re-reads the same screenshots + the draft and returns ONLY
    substantiated findings, each `verified:true`; unverified findings are dropped. A
    verify parse-failure degrades to the unverified draft (never loses the draft). ONE
    extra call per cell (bounded cost).
  - **Per-page completion budget (`AUDITLOOP_LLM_EVAL_MAX_TOKENS`, default 2000):** BOTH
    the generation AND the verification call get their own middle-tier budget via a
    per-call `llm.WithMaxTokens(GenMaxTokens)` override. A verbose per-page verdict (e.g.
    3 blockers + 4 frictions, each with issue/selector/evidence, + a top_fix) overflows
    the small per-page/notes cap (`AUDITLOOP_LLM_MAX_TOKENS`, 1024) and truncates mid-JSON
    → `ParseEvaluation` fails with "unexpected EOF" and that cell stores an error + loses
    its findings (observed live: 1/6 cells on a real pushed run). The verify output is the
    same structured shape → same overflow risk, so it shares the eval budget.
    `ParseEvaluation` degrades cleanly (error, no panic) but a per-page verdict is a SINGLE
    JSON object (not an array), so the array-prefix salvage `ParseSynthesis` uses does not
    apply — the larger budget is the real fix (no fragile single-object salvage added).
    **Three completion-token tiers, all via per-call `llm.WithMaxTokens`: notes 1024 <
    eval-gen/verify 2000 < synth 3000.**
  - **Synthesis pass:** ONE final call per run over the kept findings → a ranked,
    capped (≤8) list of run-level improvements `[{title,rationale,impact,affected_urls,
    affected_personas,selector?}]` (the "story"), stored run-level. **The synthesis call
    gets its OWN, larger completion budget** (`AUDITLOOP_LLM_SYNTH_MAX_TOKENS`, default
    3000) via a per-call `llm.WithMaxTokens` override — a whole run's ≤8 rich improvements
    overflow the small per-page `AUDITLOOP_LLM_MAX_TOKENS` cap (1024) and the completion
    truncates mid-JSON → `ParseSynthesis` fails with "unexpected end of JSON input" and the
    story is silently lost (observed live on a 6-page run). The per-page gen/verify calls
    get their own, smaller middle-tier budget (`AUDITLOOP_LLM_EVAL_MAX_TOKENS`, 2000 — see
    the per-page budget bullet above); notes stay at 1024. `ParseSynthesis` also SALVAGES the valid prefix of
    completed items from a still-truncated body (defense-in-depth; nothing salvageable →
    a clean logged error, never a panic or fake success). The synthesis PROMPT is bounded
    too: only salient parts per cell (comprehension + blockers + top_fix, frictions dropped)
    and cell count capped (`MaxSynthCells=120`) so a large run (e.g. 50 pages × 4 personas)
    stays bounded.
  - `Generator.Run(ctx, runID, personas, Options{Job,Verify})` groups pages, issues
    generate→verify per (page,persona) under `DefaultConcurrency` (reuses
    `notes.DefaultConcurrency=3`), then the synthesis call; updates progress; finalizes
    the job. Per-cell failures store an `error` and continue (degrade); only ctx-cancel
    fails the pass. `CountUnits = pages × personas + 1` (synthesis).
- **DB (migrations 0039–0049):** table **`page_evaluations`** — one row per (page,
  persona): `id, page_id, run_id, persona, findings_json TEXT` (the structured object),
  `comprehension TEXT` (duplicated out for cheap badging), `error TEXT`, `cost_usd REAL`
  + `prompt_tokens`/`completion_tokens INTEGER`, `created_at/updated_at`, **UNIQUE(page_id,
  persona)** (a re-run upserts). Scoped via page→run→user (no user_id). Run-level cols:
  `eval_synthesis_json` (the story), `eval_job` (the task, shown + reused on re-run),
  and async-job tracking mirroring notes — `eval_status` (idle|generating|done|failed),
  `eval_done`/`eval_total`, `eval_cost_usd`/`eval_prompt_tokens`/`eval_completion_tokens`.
  `ClaimEvalJob` (atomic idle/failed/done→generating; resets cost + synthesis),
  `UpdateEvalProgress`/`FinishEvalJob`/`MarkGeneratingEvalFailed` (startup sweep, wired
  into `NewRouter` next to the notes sweep), `AddEvalCost` (DB-side `SET x=x+?`),
  `SetRunEvalSynthesis`, `SavePageEvaluation`/`ListPageEvaluations`/`GetPageEvaluation`.
  All new run cols are in the shared `runCols`/`scanRun` hot path.
- **Routes (`handlers/eval.go`):** `POST /api/runs/{id}/evaluate` — body `personas[]`
  (validated against the 4-id allowlist; empty→400, unknown→400), optional `job`
  (trimmed, capped), optional `verify` (default on); **503 if `!OpenRouterEnabled()`**;
  run must be `done`; ownership-checked; claims the job + spawns the goroutine, returns
  the now-generating fragment. `GET /runs/{id}/eval-status` — poll target. **Read-API:**
  `GET /api/audit/runs/{run_id}/evaluation` (on the existing `apiKeyAuth` subrouter,
  owner-scoped → 404 for a foreign run) → JSON `{run_id, eval_status, job, synthesis[],
  pages:[{url,persona,error?,evaluation?}]}` so an agent/CI pulls the machine layer.
- **UI (`components/pages/eval.go`):** the `EvaluationSection` fragment (id
  `eval-section`), rendered only when `OpenRouterEnabled()`, wired into the run view next
  to `NotesSection`; a persona multi-select + optional job field + a verify toggle + a
  "Run persona walkthrough" button; self-polls every 3s while generating; renders the
  run-level synthesis "story" at the top, then per-URL blocks with per-persona cells
  (comprehension badge, blockers/frictions with escaped selectors/evidence, the top_fix,
  an "unverified" hint) + per-cell/per-pass cost badges (reuses `formatUSD`). Nil-safe
  empty state.
- **report.json seam (additive/omitempty):** `PageReport.Evaluations` (persona → the
  structured `report.PageEvaluation`), `Report.EvalSynthesis` (`[]EvalSynthItem`), and
  `Report.EvalCost`. The structured types live in `internal/report` (canonical contract;
  avoids a report→eval import cycle — eval already imports report for grounding). Like the
  P3 notes seam these are FORWARD-COMPAT seams the crawl worker does NOT populate — for
  Phase 1 the **DB + the read-API `/evaluation` endpoint are the source of truth**;
  report.json is not re-emitted after the pass.
- **Metrics:** `auditloop_eval_generated_total{persona,status}`,
  `auditloop_eval_duration_seconds`, `auditloop_eval_cost_usd_total{persona}`,
  `auditloop_eval_prompt_tokens_total{persona}`, `auditloop_eval_completion_tokens_total{persona}`
  (the synthesis call reports cost under the `_synthesis` persona label).
- **No new prod secret** — reuses `OPENROUTER_API_KEY` (server-side only; feature no-ops
  without it: button hidden, routes 503). Tests use an httptest OpenRouter fake, never a
  real key.
- **Deferred to Phase 3–4:** actually DRIVING the app (Playwright/chromedp walkthrough
  rather than evaluating captured screenshots), multi-model matrices, per-finding human
  edits (evaluations are read-only), qualitative regression (persona verdicts diffed run
  to run), and re-emitting report.json with the eval blocks. (Goal/task INFERENCE + a
  reusable per-target config landed in Phase 2, below.)
- **Tests:** `internal/eval` (prompt has persona/flow/job/grounding + forbids re-listing
  signals; structured parse valid/malformed→error-not-panic/unknown-comprehension→error;
  verification filters unverified; `CountUnits`; `Generator.Run` against an httptest fake
  — one row per (page,persona), both viewports sent, per-cell degrade, no-verify keeps
  drafts, synthesis produced, cost accumulated, ctx-cancel fails); `internal/db` (upsert +
  UNIQUE(page_id,persona); run-scoped list; `ClaimEvalJob` atomicity + cost reset;
  progress/finish/mark-failed; concurrent `AddEvalCost`); `handlers` (evaluate 503/400
  empty+unknown/run-not-done/happy-path claim+spawn; read-API `/evaluation` own-200 +
  foreign-404 + no-key-401); **e2e** `TestEndToEndPersonaEvaluation` (crawl → 2-persona
  pass against a FAKE OpenRouter → rows per (page,persona), both viewports, UI renders
  structured findings + synthesis, read-API returns them owner-scoped).

## Persona walkthrough — Phase 2 (goal inference + per-target config, done)

Phase 1 took a free-text `job` per RUN. Phase 2 makes a target's audit goals
**first-class + reusable**: auto-INFER a draft config from a completed crawl, let the
owner CONFIRM/edit it (the "hybrid infer-then-confirm" model), and DEFAULT the evaluate
trigger from it — so every future evaluation is grounded without re-typing the job. It
reuses the same `internal/llm` client, `OpenRouterEnabled()` gating, the P4 per-target
config-card + ownership patterns, and dual-dialect migrations.

- **DB — `target_audit_config` (migration `0050`, one row per target, `target_id` PK):**

  | column | meaning |
  |--------|---------|
  | `product_summary` | one-line "what this product is/does" |
  | `primary_job` | the main task the walkthrough evaluates toward (maps to the eval `job`) |
  | `primary_cta` | the main call-to-action label (e.g. "Sign up") |
  | `personas_json` | JSON array of applicable persona ids (subset of the 4 curated `eval.Personas`) |
  | `inferred` / `confirmed` | INTEGER 0/1 (dual-dialect; not BOOLEAN) — draft vs owner-confirmed |
  | `created_at` / `updated_at` | RFC3339 TEXT |

  `internal/db/audit_config.go`: `SetTargetAuditConfig` (UPDATE-else-INSERT upsert,
  unscoped — the handler owns the ownership check via `GetTarget`, like `SetLoginRecipe`),
  `GetTargetAuditConfig(userID, targetID) → (*cfg, found, err)` (owner-scoped via a
  `target→user` JOIN — a foreign target's config is never returned). Persona-id ALLOWLIST
  validation happens at the handler layer.
- **Inference — `internal/eval/infer.go` (SYNCHRONOUS, ONE LLM call — NOT the async job
  machinery):** `(g *Generator) InferConfig(ctx, runID)` loads the run's pages, sends the
  **landing page** (first page, desktop-preferred) screenshot + a compact TEXT digest of
  the crawl (the deduped page URLs, capped) to the FIRST curated model, and returns a
  structured `InferredConfig{product_summary, primary_job, primary_cta, audiences[]}`.
  `InferSystemPrompt` forces ONLY-JSON output; `ParseInferredConfig` is lenient about
  fences/prose (reuses `extractJSON`, shared with `ParseEvaluation`) and **filters
  `audiences` to the 4 curated persona ids** (unknown/blank dropped, de-duped). Malformed
  reply → a clean error (degrade), never a panic. The call gets the eval-tier budget
  (`GenMaxTokens` via `llm.WithMaxTokens` — a config draft is small) and a 60s handler
  timeout.
- **Routes (`handlers/audit_config.go`, ownership-checked, mirror the P4 `/auth` convs —
  `MaxBytesReader` body, HX fragment responses):**
  - `POST /api/targets/{id}/audit-config/infer` — **503 if `!OpenRouterEnabled()`**; 404
    for a foreign target; **409** when the target has no `done` run yet
    (`LatestDoneRunForTargetOwned`); runs `InferConfig` against the latest done run; stores
    the draft (`inferred=1, confirmed=0`); returns the pre-filled config-card fragment.
  - `POST /api/targets/{id}/audit-config` — save/confirm: form `product_summary`,
    `primary_job`, `primary_cta`, repeated `personas` (validated against the 4-id allowlist
    via the shared `validatePersonas` — unknown → 400; text fields length-capped
    defensively at `maxAuditFieldLen=300`); stores with `confirmed=1` (preserving the prior
    `inferred` provenance); returns the updated card.
  - **Read-API (on the `apiKeyAuth` subrouter):** `GET /api/audit/targets/{id}/audit-config`
    (UUID-or-name via `resolveTarget`, owner-scoped → 404 for a foreign/unknown target or one
    with no config) → JSON `{target_id, product_summary, primary_job, primary_cta, personas[],
    inferred, confirmed}` so a machine consumer/agent sees the target's job + audiences.
- **UI (`components/pages/audit_config.go`, `AuditConfigSection` id `audit-config-section`):**
  an **"Audit configuration"** card on the target page (non-plugin targets), **gated on
  `OpenRouterEnabled()`** (whole card — the primary value is the LLM infer; the same gate as
  the evaluate section, chosen for consistency). Empty state → an **"Infer from latest run"**
  button (disabled with a hint when there's no `done` run). With a config → editable
  product-summary/job/CTA fields + the 4 personas as checkboxes (pre-checked per
  `personas_json`), a **Save** (confirm) button, and a **Re-infer** button; a subtle
  amber "inferred — review & confirm" badge when `inferred=1 && confirmed=0` (else a green
  "confirmed"). htmx: JS-driven forms `hx-boost="false"`; infer/save return the card fragment
  for in-place swap. Model-authored fields (product_summary/job/cta) are rendered escaped
  via `g.Text` (never `g.Raw`).
- **Evaluate trigger DEFAULTS from the config:** `evalVM` (handlers/eval.go) resolves the
  run→target confirmed config and, **only when the run has not itself been evaluated yet**
  (`run.EvalJob` empty), pre-fills the evaluate form's `job` from `primary_job` and its
  `DefaultPersonas` from `personas_json`; `evalControls` (components/pages/eval.go)
  pre-checks personas from `DefaultPersonas` (falling back to the Phase-1 first-persona
  default when nil). The per-run `job`+`personas` override on `POST /api/runs/{id}/evaluate`
  is unchanged — the form just pre-populates so the owner doesn't re-type.
- **Key-gating (documented decision):** the WHOLE audit-config card + both mutation routes
  are gated on `OpenRouterEnabled()` (simplest, consistent with the eval section). Without
  `OPENROUTER_API_KEY` the card is hidden and the routes 503 — no manual hand-entry path,
  by choice. **No new env var** — reuses `OPENROUTER_API_KEY` + `AUDITLOOP_LLM_MODELS[0]`.
- **Tests:** `internal/eval` (infer prompt has the URL digest + forces JSON; parse
  valid/malformed→error-not-panic/unknown-persona-filtered; `InferConfig` against an
  httptest fake — landing screenshot sent, draft returned, audiences filtered, degrade on
  a bad reply, no-page run errors cleanly); `internal/db` (migration applies; upsert +
  owner-scoped get + cross-user isolation + inferred/confirmed persist); `handlers` (infer
  503-when-disabled / 404-foreign / 409-no-done-run / happy-path draft card + stored
  inferred flags + filtered personas; save ownership + persona allowlist + confirmed=1;
  evaluate form pre-fills job + pre-checks personas from a confirmed config, falls back
  cleanly with none); **e2e** `TestEndToEndGoalInference` (crawl → infer against a FAKE
  OpenRouter → draft stored + card renders → confirm/save → run-view evaluate form
  pre-fills from it).
- **Deferred to Phase 3–4:** the native DRIVER (Playwright/chromedp walkthrough), and
  qualitative regression (persona verdicts diffed run to run).

## Persona walkthrough — Phase 3 (goal-directed driver, PR-A done)

A native chromedp DRIVER that actually DRIVES a target toward a goal (rather than
reasoning over already-captured screenshots) and reports a **deterministic
success/stuck signal + an ordered action trace**. This is **PR-A of two**: the
deterministic driver + trace + the async job. **PR-B (not built here) will feed the
trace into the Phase-1 personas** (materialize a driven flow as pages the persona
evaluator critiques). The driver reuses the P4 login/intercept machinery, the
`internal/llm` client + `OpenRouterEnabled()` gate, and the async-job + startup-sweep
pattern.

- **Safety model (the non-negotiables, all landed together):**
  - **Dry-run submit-guard is the DEFAULT.** The Fetch interceptor
    (`internal/crawler/intercept.go`), when armed, additionally **`FailRequest`s
    (aborts) any request whose method is NOT GET/HEAD** (POST/PUT/PATCH/DELETE) — a
    **network-layer, deterministic** guard (not a prose heuristic). A blocked mutation
    is recorded on the step as `submit-blocked (dry-run)`. The SSRF IP-guard stays
    active in BOTH modes. **SCOPED GUARANTEE (honest):** the mutation-abort covers
    **every TOP-FRAME HTTP(S) request of any resource type** — a form POST AND every
    XHR/`fetch`/`sendBeacon` POST/PUT/PATCH/DELETE the page's own JS issues in the top
    frame; those cannot reach the network in dry-run. It does **NOT** cover
    (documented residual mutation vectors): **WebSocket messages** (the WS handshake is
    an interceptable GET, but frames over an established socket are not HTTP requests),
    **cross-origin (OOPIF) iframe requests** (a separate CDP target with its own Fetch
    domain this single-target interceptor does not attach to), and
    **service-worker-originated requests**. A site that mutates through any of these
    could still write in dry-run — so for real-submit / prod driving, **point at a
    STAGING environment and use a DISPOSABLE account**, don't rely on dry-run as an
    absolute "never writes" guarantee.
  - **The LOGIN phase is EXEMPT from the mutation submit-guard** (the SSRF guard stays
    on). In the safe default (dry-run) the mutation-abort is a runtime-toggleable
    atomic on the interceptor (`armMutationGuard`): `Drive` enables SSRF interception
    once, runs the optional P4 `runLogin` with the mutation-abort OFF (its credential
    POST must authenticate — otherwise driving login-gated funnels would break in the
    safe default), then ARMS the mutation-abort BEFORE the drive loop so the funnel is
    still fully submit-guarded. One Fetch listener serves both phases; the SSRF/IP guard
    is active the ENTIRE time (a login `goto` that 302s to a private/metadata IP is
    still aborted). For a non-login pass the guard arms before the first action.
  - **Real submissions require an explicit, loud, DEFAULT-OFF per-target flag**
    (`targets.allow_real_submit`, migration `0058`). `DryRun = !allow_real_submit`.
    The config UI foregrounds it in red ("mutates live data — off = dry-run"; point at
    staging for real-submit).
  - **`driving_enabled` default-off opt-in gate** (`targets.driving_enabled`, migration
    `0054`) — enforced at **BOTH** the route (403) **and** the generator (defense in
    depth). Two independent opt-ins: drive at all vs. mutate live data.
  - **SSRF/same-origin reused on every nav + redirect** (`enableInterception` +
    `guard.CheckURL`); **no-eval CLOSED action set**; action budget + per-action +
    overall timeouts; credential redaction (reuses the P4 rule); a guard-blocked
    navigation is never screenshotted (the step screenshot is the pre-action
    observation — no exfil).
- **`internal/action` (NEW, pure — mirrors `internal/recipe`, no chromedp/llm):** the
  CLOSED action set `click · type · press · select · scroll · waitFor · navigate ·
  finish` (**NO eval/script/js**). `ParseAction` uses `DisallowUnknownFields` (an
  injected `script`/`eval`/`js` key → hard reject) + lenient fence/prose stripping.
  `Validate` enforces required fields per type, a `press.Key` allowlist
  (Enter/Tab/Escape/ArrowUp/ArrowDown), and length caps. `SuccessAssertion{Selector,
  URLContains, TimeoutMs}` + `IsZero()`/`Validate()` is the deterministic success type.
- **`crawler.Drive` (NEW `internal/crawler/driver.go`):** `type Planner interface {
  NextAction(ctx, DriveState) (action.Action, error) }` is INJECTED (the LLM lives
  outside `crawler`, so tests pass a scripted fake and the loop is deterministic). The
  loop: DEFAULT tab (the captureBeyondViewport gotcha), `network.Enable`,
  `enableInterception(guard, dryRun)` [REUSED], optional `runLogin` [REUSED P4],
  navigate BaseURL (via `guard.CheckURL`), then up to `MaxActions` (default 20) under
  `OverallTimeout` (3m): **check the success assertion (DETERMINISTIC, observed) →
  screenshot → `buildInteractiveDigest` → `Planner.NextAction` → `action.Validate` →
  execute by selector** (a `switch` under `ActionTimeout` using `chromedp.Click/
  SendKeys/SetValue/WaitVisible/Navigate/KeyEvent` — **NEVER `chromedp.Evaluate` on
  model text**; scroll uses a FIXED constant script) → record the step. `finish` is
  ADVISORY — only the assertion firing makes `Outcome="success"`. Budget/timeout
  exhaustion without success → `stuck` (`StuckStep=len(Steps)`); guard block / login
  fail / off-domain navigate / planner error → `failed` with a deterministic `Reason`.
  **Repeat/dedup:** a `(Type,selector,url)` set + a consecutive-no-progress counter
  short-circuits to `stuck` after 3 spins (and nudges the planner). **`CheckAssertion`**
  is the ONE shared selector-visible-OR-url-contains check — `login.go`'s
  `waitForSuccess` was refactored to call it. **`buildInteractiveDigest`** evaluates a
  vendored `interactive-digest.js` (embedded) returning a BOUNDED (≤40) list of visible
  interactive elements `{tag,role,name,selector,type,disabled}` so the planner drives by
  REAL selectors (its output is untrusted — selectors round-trip only through chromedp's
  selector engine).
- **`internal/walkthrough` (NEW):** `planner.go` = a `crawler.Planner` on `internal/llm`
  (first curated model, `AUDITLOOP_LLM_DRIVE_MAX_TOKENS` default 256 via
  `llm.WithMaxTokens`); one action JSON per turn, `ParseAction`+`Validate`, ONE
  retry-with-nudge then degrade to a safe scroll (never panics; ctx-cancel is the only
  error → fails the pass). `generator.go` = the async job (mirrors eval/notes):
  `Generator.Run(ctx, walkthroughID)` loads the target's goal + success + login, calls
  `crawler.Drive` with the planner, persists each step (screenshot → Store), updates
  progress + cost, finalizes `Outcome/StuckStep/Reason`. `Drive` is injectable so
  handler/e2e tests stub the browser.
- **DB (migrations `0051`–`0058`; `internal/db/walkthrough.go`):** `0051–0053` add
  `success_selector`/`success_url_contains`/`success_timeout_ms` to `target_audit_config`
  (goal + success authored together); `0054` `targets.driving_enabled`; `0055`
  `walkthroughs` (id, target_id, run_id, goal, outcome, stuck_step, reason, dry_run,
  status idle|driving|done|failed, steps_total/done, cost_usd/prompt_tokens/
  completion_tokens); `0056–0057` `walkthrough_steps` (+ index); `0058`
  `targets.allow_real_submit`. `driving_enabled`/`allow_real_submit` are in the shared
  `targetCols`/`scanTarget` hot path. Methods (owner-scoped via target→user JOIN, no
  direct user_id): `CreateWalkthrough`, `ClaimWalkthroughJob` (atomic idle/done/failed→
  driving, rows-affected==1, resets cost + outcome), `UpdateWalkthroughProgress`,
  `AddWalkthroughCost`, `FinishWalkthrough`, `InsertWalkthroughStep`,
  `ListWalkthroughSteps`, `GetWalkthrough` (owner-scoped → ErrNotFound foreign),
  `LatestWalkthroughForTarget`, `SetDrivingConfig`, `MarkDrivingWalkthroughsFailed`
  (startup sweep, wired into `NewRouter` next to `MarkGeneratingEvalFailed`).
  `storage.WalkthroughStepKey(targetID, walkthroughID, idx)` is target-scoped (binds
  artifact authz to the owning target, like `LoginTestKey`).
- **Routes (`handlers/walkthrough.go`):** `POST /api/targets/{id}/walkthrough` —
  ownership; **403 if `!driving_enabled`**; **503 if `!OpenRouterEnabled()` or the role
  doesn't run the worker** (the driver needs chromedp); **400** for a plugin target;
  **409** if the config lacks a `primary_job` + a success assertion; claims the job +
  spawns the async goroutine + returns the driving fragment. `GET
  /targets/{id}/walkthrough-status` — htmx poll (self-polls 3s while driving). Read-API
  `GET /api/audit/walkthroughs/{id}` (on the `apiKeyAuth` subrouter, owner-scoped → 404)
  → `{id,goal,outcome,stuck_step,reason,dry_run,status,steps:[{idx,action,url,outcome,
  screenshot_key}]}`. The Phase-2 audit-config save route was EXTENDED to also set
  `driving_enabled` + `allow_real_submit` (checkboxes) + the success assertion.
- **UI (`components/pages/walkthrough.go` + extended `audit_config.go`):** the
  audit-config card gains a `driving_enabled` checkbox, the loud default-off
  **"Allow real form submissions (mutates live data — off = dry-run)"** toggle, and
  success-assertion fields (selector / url-contains / timeout) with safety copy. A
  `WalkthroughSection` (gated on `OpenRouterEnabled() && driving_enabled`, non-plugin
  targets) has a **"Run walkthrough"** button (disabled + hint if goal/success unset),
  an **outcome badge** (green "reached in K steps" / amber "stuck at step K" / red
  "failed: reason"), the **ordered step trace** (each step's escaped action via `g.Text`
  — planner selectors/reasons are UNTRUSTED, NEVER `g.Raw` — URL, screenshot via the
  artifact proxy, outcome badge), and a cost badge. Self-polls 3s while driving; wired
  onto `App` in `NewRouter` when `OpenRouterEnabled() && cfg.RunsWorker()` (reusing
  `app.Cipher`, `cfg.ChromiumPath`, `cfg.CrawlAllowLoopback`).
- **Metrics:** `auditloop_walkthrough_runs_total{outcome}`,
  `auditloop_walkthrough_steps_total{outcome}`, `auditloop_walkthrough_duration_seconds`,
  `auditloop_walkthrough_cost_usd_total`.
- **Env:** `AUDITLOOP_LLM_DRIVE_MAX_TOKENS` (default 256) — the small per-turn planner
  completion budget (one action JSON). No new REQUIRED secret (reuses `OPENROUTER_API_KEY`).
- **Tests:** `internal/action` (parse valid/malformed→error-not-panic, reject
  unknown-field/script key, Validate per type, key allowlist, SuccessAssertion);
  `internal/crawler` (`CheckAssertion` + `buildInteractiveDigest` on a loopback fixture,
  chromium-gated; `isMutatingMethod`); `internal/walkthrough` (planner parse/retry-then-
  degrade/reject-injected-script/ctx-cancel-fails; prompt content); `internal/db`
  (walkthrough lifecycle, `ClaimWalkthroughJob` atomicity + cost reset, owner-scoped Get
  cross-user isolation, driving-config scan/set, success-assertion persist);
  `handlers` (403 !driving_enabled, 503 !OpenRouter, 409 no goal/success, happy
  claim+spawn drives the stubbed trace, read-API own-200/foreign-404/no-key-401);
  **e2e** `TestEndToEndWalkthrough` (funnel fixture over loopback, SCRIPTED fake Planner:
  reaches success + screenshots in the trace; stuck-at-budget with no false success;
  SSRF `/trap` 302→metadata + off-domain navigate refused (Outcome=failed, no metadata
  screenshot); dry-run submit-guard blocks a POST-gated success → stuck (+ repeat/dedup),
  real-submit → success).
- **Deferred (Phase 4):** multi-goal, a planner matrix, walkthrough regression (traces
  diffed run to run), same-origin-on-clicks hardening, and a real-submit-on-prod
  confirmation gate.

### PR-B — personas over the driven trace (synthetic-run materialization, done)

PR-A produces a deterministic driven step-trace (`walkthrough_steps`, one screenshot per
step). **PR-B lets the EXISTING Phase-1 persona evaluator run over that trace** — each
step becomes an eval unit, in flow order — so personas critique the ACTUAL driven path
instead of the crawl BFS. **Reuse, don't rebuild:** materialize the trace as a synthetic
run + pages, then call the existing `eval.Generator.Run` keyed on that run id. Almost no
new eval code.

- **Materialization (`db.MaterializeWalkthroughRun(userID, walkthroughID) → runID`,
  `internal/db/walkthrough.go`):** mirrors P5 `CreatePushedRun` — a run made WITHOUT the
  crawl worker. Owner-scoped (via `GetWalkthrough`'s target→user join → `ErrNotFound` for
  a foreign walkthrough). Creates a **`done` run `trigger='walkthrough'`** (started+
  finished now), then **one `pages` row per step, IN ORDER**, at `viewport='desktop'`
  (width `WalkthroughSyntheticWidth=1440`, the driver's capture viewport). The page **URL
  is a stable, order-preserving per-step LABEL** `"{NN} · {action-type} · {step-url}"`
  (`walkthroughStepLabel`) — NOT the bare step URL, which would collapse steps that revisit
  a URL into one eval unit and lose the flow position; the zero-padded 1-based index keeps
  `ListPages` `ORDER BY url` in step order, so the eval's flow context (step N of M,
  prev/next) tracks the driven path. The step's existing `screenshot_key` is **REUSED**
  (no re-capture); a shot-less step still becomes a page (eval degrades to no image for
  it — the evaluator already loads whatever screenshots exist per page). **Idempotent +
  linked:** the synthetic run id is stamped on **`walkthroughs.run_id`** (the PR-A column,
  reused — no new migration); a second call whose run still exists returns the SAME id (no
  dup run/pages). The synthetic run is **NOT baseline-linked** (`prev_run_id` NULL) AND the
  P2 baseline queries (`LatestDoneRunForTarget`/`LatestDoneRunForTargetOwned`) now **EXCLUDE
  `trigger='walkthrough'`** so a synthetic eval vessel never becomes a crawl/push's P2
  baseline.
- **Route — `POST /api/targets/{id}/walkthroughs/{wid}/evaluate`
  (`handlers/walkthrough.go`):** ownership-checked (walkthrough→target→user; `wk.TargetID`
  must match `{id}`); **503 if `!OpenRouterEnabled()`**; **409** when the walkthrough isn't
  `done` or has zero steps; body `personas[]` (validated against the 4-id allowlist; empty →
  the target's confirmed `target_audit_config.personas`, else the Phase-1 default). Flow:
  **materialize** the synthetic run (idempotent) → `ClaimEvalJob(runID, job=goal, …)` →
  spawn the existing `eval.Generator.Run(ctx, runID, personas, Options{Job:goal,Verify:true})`
  goroutine (with a `recover()` → `FinishEvalJob(failed)`) → return the existing
  `EvaluationSection` fragment. **The whole Phase-1 stack is reused unchanged** — the eval
  status poll (`GET /runs/{id}/eval-status`) and the read-API (`GET /api/audit/runs/{id}/
  evaluation`) already work keyed on the synthetic run id. **NO new eval generator/prompt/
  parse.** Re-runs go through the standard `/api/runs/{runID}/evaluate` route (the synthetic
  run is a normal `done` run).
- **UI (`components/pages/walkthrough.go`):** on a TERMINAL walkthrough with steps, an
  **"Evaluate this walkthrough with personas"** control (gated on `OpenRouterEnabled()`) —
  a persona multi-select defaulting from the target config — posts to the evaluate route
  (`hx-target="#walkthrough-eval"`, innerHTML swap). Once started (synthetic run exists +
  `eval_status != idle`), `walkEvalBlock` embeds the **existing `EvaluationSection`** keyed
  on the synthetic run id (self-polls + renders the per-step × per-persona findings +
  synthesis "story"; its own "Re-run" hits the standard eval route). Planner + model text
  rendered escaped via `g.Text`.
- **Read-API:** `GET /api/audit/walkthroughs/{id}` now includes **`eval_run_id`**
  (= `walkthroughs.run_id`, set once materialized) so a machine consumer pulls the persona
  findings via `GET /api/audit/runs/{eval_run_id}/evaluation`.
- **Tests:** `internal/db` (`MaterializeWalkthroughRun` — `done`/`trigger='walkthrough'`
  run + one page per step in order, step screenshot_keys reused, same-URL steps become
  DISTINCT labeled pages, idempotent (no dup run/pages), stamps `run_id`, owner-scoped
  (foreign → ErrNotFound, no leak), never a P2 baseline); `handlers` (evaluate route
  503-disabled / 409-not-done / 409-no-steps / 404-foreign+target-mismatch / happy-path
  materialize+claim+spawn → rows per step×persona + `eval_run_id` on the read-API / personas
  default from the target config); **e2e** `TestEndToEndWalkthroughPersonas` (drive the
  funnel fixture with a scripted planner → ≥2-step success trace → persist as a walkthrough
  → POST evaluate against a FAKE OpenRouter → asserts the synthetic run + one page per step
  ordered, one eval row per step×persona, the findings + synthesis render in the walkthrough
  UI, and the read-API `/evaluation` serves them owner-scoped).
- **Deferred (Phase 4):** walkthrough regression (persona verdicts / traces diffed run to
  run), same-origin-on-clicks hardening, multi-goal.

## Persona walkthrough — Phase 4 (walkthrough regression, done)

A walkthrough (Phase-3 driver: `outcome ∈ {success, stuck@stepK, failed}` + an ordered
step trace + — via PR-B — persona findings over the driven trace) is **diffed against the
target's PREVIOUS TERMINAL walkthrough**, surfacing REGRESSIONS — the goal stopped being
reachable, it got stuck earlier, or NEW persona task-blockers appeared — plus a
machine-readable regression status a CI gate can `--fail-on-regression`. It is a **direct
mirror of the P2 crawl-regression diffing** (`runs.diff_json` / `report.Diff` /
`diff.StringSetDelta` / the "Changes since" card / `auditloop_visual_regressions_total`),
applied to the SEPARATE walkthrough axis — the two `diff_json`s never entangle.

- **Baseline link (migration `0062`):** `walkthroughs.prev_walkthrough_id` (NOT NULL
  DEFAULT '' — fully additive). Stamped at `CreateWalkthrough` via
  `latestTerminalWalkthroughID(targetID)` = the target's newest `status IN (done,failed)`
  walkthrough (unscoped — the handler already resolved target ownership), the exact analogue
  of `CreateRun` stamping `runs.prev_run_id` from `LatestDoneRunForTarget`. "" (no baseline)
  for the target's first walkthrough → no diff. A non-terminal (idle/driving) walkthrough is
  never a baseline. Synthetic PR-B runs stay EXCLUDED from the P2 baseline (unchanged) — this
  is a distinct axis.
- **The deterministic diff (`report.WalkthroughDiff`, persisted in
  `walkthroughs.diff_json`, migration `0063`, mirrors `runs.diff_json`):** a pure, unit-
  tested value computed by `walkthrough.ComputeDiff(prev, cur, prevKeys, curKeys,
  blockersCompared)` (`internal/walkthrough/diff.go`). Outcome rank `success(3) > stuck(2) >
  failed(1)`:
  - `outcome_changed` + `is_regression` — the rank DROPPED (success→stuck|failed,
    stuck→failed) OR, when BOTH stuck, it got stuck EARLIER (`stuck_step_delta < 0`).
  - `resolved` — the rank ROSE (stuck|failed→success).
  - `stuck_step_delta` — `cur − prev` stuck step, only when BOTH are stuck (else 0);
    negative = earlier = regression, positive = progress.
  - `new_task_blockers` / `resolved_task_blockers` — `diff.StringSetDelta` over a **STABLE
    task-blocker identity key** extracted from each walkthrough's persona evaluation of its
    driven trace (PR-B). **Key = `"<persona>\x1f<normalized selector | issue>"`** — persona +
    the DOM selector anchor (the most stable handle), falling back to the normalized issue
    text when the model gave no selector; normalize = lowercase + collapse whitespace. Keying
    on persona means a blocker that newly affects an ADDITIONAL persona counts as new. Only
    **verified** blockers (the ones the eval verify-pass kept) are read from
    `page_evaluations.findings_json`. **Degrade:** the blocker delta is computed ONLY when
    BOTH walkthroughs have a COMPLETED persona evaluation (`RunID != "" && eval_status=done`);
    otherwise `blockers_compared=false` and both slices are empty (never a false "everything
    resolved"). `NewTaskBlockers` is always a non-nil `[]` for stable JSON.
- **Two triggers (`walkthrough.RefreshWalkthroughDiff(ctx, db, walkthroughID)`, idempotent —
  gathers prev+cur, extracts keys, computes, stores):**
  1. **drive end** (`generator.Run`, right after `FinishWalkthrough`) — the deterministic
     outcome/stuck delta is available immediately; the current walkthrough has no eval yet so
     the blocker delta degrades. **This is the ONLY place the regression METRIC is bumped**
     (`auditloop_walkthrough_regressions_total` on `is_regression`) → counted exactly once per
     walkthrough (mirror of `auditloop_visual_regressions_total`; the blocker-only regression
     is surfaced for the gate/UI but not counted, a documented honest limitation).
  2. **after the persona eval of the driven trace COMPLETES** (the PR-B evaluate handler's
     eval goroutine, after `eval.Generator.Run`) — re-diffs so `new_task_blockers` fills in.
     No metric bump (already counted at drive end). A diff failure is non-fatal (never fails
     the drive/eval).
- **Read-API (CI gate):** `GET /api/audit/walkthroughs/{id}` (owner-scoped, `apiKeyAuth`
  subrouter) gains a `regression` block `{prev_walkthrough_id, outcome_changed, prev_outcome,
  outcome, is_regression, resolved, stuck_step_delta, new_task_blockers[],
  resolved_task_blockers[], blockers_compared}` (present once a baseline exists) — a consumer
  (an external CI gate) gates on it. 🔴 **The consumer predicate CHANGED TWICE — in #45, and
  again on 2026-08-27. `new_task_blockers` is now ADVISORY and MUST NOT fail a build.** The
  current one:

  ```
  if infra_failed || (regression && !regression.outcome_compared):  → INFRASTRUCTURE error;
        RETRY, do not report a product verdict
  elif regression && regression.is_regression:
        → REGRESSION; fail the build
  # new_task_blockers / resolved_task_blockers: REPORT them, never gate on them.
  ```

  🔴 **Why the blocker term was removed — MEASURED, not a judgement call
  (`claudedocs/evaluator-variance-2026-08-27.md`).** Three IDENTICAL evaluation passes over one
  run (same run id, personas, job, model, `verify=on`) produced a blocker-key stability of
  **0.22** — 2 of 9 keys present in all three — and `len(new_task_blockers) > 0` fired on
  **3 of 3** comparisons. **Gating on it fails every build regardless of the product**, which
  trains everyone to click through the gate: the permanently-red failure mode. Root cause is the
  IDENTITY KEY, which takes the model's selector spelling verbatim — `main` vs `[role='main']`
  are one element spelled two ways. **Re-anchoring cannot fix it, also measured:** those anchors
  are LANDMARKS, and the digest's landmark schema carries **no selector field at all**, while the
  one class selector the model did cite (`button.press.inline-flex`) occurs **46× across 6 pages**,
  so it is unresolvably ambiguous. Narrowing the gate to concrete `#id`/`[name=]` anchors gives
  0/3 firing but only 1 of 9 keys qualifies — an almost-always-empty gate, the #18 silent-no-op
  failure. **`is_regression` is unaffected and remains the real gate:** outcome/stuck-step
  regression is DETERMINISTIC and OBSERVED, never LLM-authored. `auditloop_walkthrough_regressions_total`
  already bumps only on `is_regression`, so the metric was always consistent with this.

  **Why #45's change must be adopted, loudly:** before #45 an infra stall surfaced as
  `is_regression=true`, so the old predicate FAILED the build — a false alarm, but safe.
  It now surfaces as `is_regression=false` with empty `new_task_blockers`, so a gate that
  ignores the infra fields **PASSES a walkthrough that never ran**. The failure direction
  moved from false-alarm to **silent pass**. `infra_failed` is also **top-level on
  `apiWalkthrough`**, not only inside `regression`, because the paths that produce it most
  often have NO diff at all (a config failure never reaches the diff step; a target's FIRST
  walkthrough has no baseline) — so the top-level field is the one a gate should read first.
  Existing fields (incl. `eval_run_id`) unchanged.
- **UI:** a **"Changes since last walkthrough"** card in the walkthrough section
  (`components/pages/walkthrough.go` `walkChangesCard`), mirroring the P2 "Changes since"
  card: a green **Resolved** / red **Regression** / amber **Changed** badge, the outcome
  transition, stuck-step movement, and new/resolved persona task-blocker chips (model-authored
  key text rendered ESCAPED via `g.Text`, never `g.Raw`; the `\x1f` separator prettified to
  `persona · anchor`). A subtle **"First walkthrough — no baseline"** note when
  `prev_walkthrough_id` is empty.
- **Metric:** `auditloop_walkthrough_regressions_total` (counter, outcome/stuck regressions
  only — mirror of `auditloop_visual_regressions_total`).
- **Backward-compat:** additive/omitempty seams; both migrations `NOT NULL DEFAULT ''` (pre-
  0062 rows read back "" → no baseline → no diff). Synthetic walkthrough runs stay out of the
  P2 baseline/run-list; `walkthroughs.diff_json ≠ runs.diff_json`. First walkthrough / no-eval
  / vanished-baseline all degrade cleanly (no diff, no error). Owner-scoping preserved.
- **Tests:** `internal/walkthrough` (`ComputeDiff` outcome-transition table — success→stuck /
  stuck→failed = regression, stuck→success = resolved, stuck-earlier = regression,
  stuck-later = progress, stable cases; blocker `StringSetDelta` + degrade-when-not-compared;
  `blockerKeysFromEvaluations` stable-key/dedup/malformed-skip); `internal/db`
  (`prev_walkthrough_id` baseline-linking — newest terminal, excludes self/non-terminal, ""
  for first, no cross-target; `diff_json` round-trip + owner-scoped + foreign-404);
  `handlers` (generator persists diff at drive end; read-API `regression` block own-200 with a
  real success→stuck regression; `new_task_blockers` populated end-to-end through the read-API
  when both sides evaluated; foreign-404 / no-key-401 via the existing tests); **e2e**
  `TestEndToEndWalkthroughRegression` (drive a MUTABLE funnel to success → persist walkthrough
  #1 → MUTATE the fixture so the goal is unreachable → drive again → stuck → walkthrough #2
  baseline-linked to #1 → the diff + the read-API `regression` block surface the success→stuck
  regression; the CI-gate predicate trips).
- 🔴 **An INFRASTRUCTURE failure is NOT a regression (#45, migration `0064`).** A stalled/
  killed browser (the #41 watchdog), a font-less render probe, a failed browser start, a
  config/setup failure, or a restart sweep all yield `Outcome:"failed"` with ZERO steps —
  the pass NEVER OBSERVED the goal, yet the rank drop `success → failed` scored as a
  regression and tripped a CI `--fail-on-regression` gate. The fix is **STRUCTURAL — never
  a substring match on the reason prose**:
  - `crawler.DriveTrace.InfraFailed` is set in band at exactly the two points the driver
    already knows the cause is infrastructure: the stall return in `Drive` and the
    login-phase `errors.Is(lerr, ErrBrowserStalled)` branch. It is deliberately NOT set
    on `login recipe failed: …` — a bad recipe/credential is a real, product-side failure.
    Both go through **`markStalled()`** (the ONE definition of what a stall means) and
    **`applyLoginOutcome()`** (the login-phase classification), so the two sites cannot
    drift and each is unit-testable. **MEASURED — the stall return is reached by TWO
    distinct paths, and the tests assert which:** the `runBounded("drive", …)` SESSION
    timer firing (`phase="drive"`, a hang the drive loop cannot unwind), and an inner
    startup call returning an `ErrBrowserStalled`-wrapping error that `errors.Is` catches
    (`phase="start"`, e.g. `enableInterception`). Each has its own test asserting the
    stall-phase counter delta, plus a chromium-free test so the branch stays pinned on a
    runner with no browser (both browser-backed tests SKIP there, and auditloop has no
    pre-merge CI).
  - `crawler.ErrDriverInfra` + `crawler.IsInfraFailure(err)` cover the out-of-band
    `(nil, err)` paths (browser start · render probe · enable interception) — all "the
    browser could not be brought up", never a claim about the site.
  - `walkthrough.Generator` computes `infra := trace.InfraFailed || crawler.IsInfraFailure(derr)`
    and passes it through **`FinishWalkthrough(id, outcome, stuckStep, reason, infraFailed)`**
    (a PARAMETER, not a second setter — every finisher must decide it). Every `g.fail(...)`
    for a CONFIG/setup failure (target lookup, driving not enabled, no audit config, no
    success condition, login build, ctx-cancel, a handler-level panic) passes `true`: the
    drive never ran, so it is not evidence about the product.
  - **`walkthroughs.infra_failed INTEGER NOT NULL DEFAULT 0`** (migration `0064`; INTEGER
    for dual-dialect portability) is in the shared `walkthroughCols`/`scanWalkthrough` hot
    path, RESET by `ClaimWalkthroughJob`, and SET by `MarkDrivingWalkthroughsFailed` (a
    restart-orphaned walkthrough likewise observed nothing).
  - **`latestTerminalWalkthroughID` EXCLUDES `infra_failed=1`** — an infra failure is never
    a BASELINE, so the next real walkthrough compares against the last one that actually
    ran. A genuine product-side `failed` is still a valid baseline.
  - `report.WalkthroughDiff` gains **`outcome_compared`** + **`infra_failed`** (additive;
    every existing field/tag byte-identical). `ComputeDiff` reads `cur.InfraFailed` off the
    row (no new parameter) and, when set, forces `is_regression`/`resolved`/
    `stuck_step_delta` off and `outcome_compared:false` — the descriptive fields
    (prev/cur outcome, reasons, `outcome_changed`) still populate so the UI can show what
    happened, and `blockers_compared` is forced off too rather than assumed. A defensive
    `prev.InfraFailed` check does the same (defence in depth against a pre-0064 baseline
    row). The metric bumps only on `d.IsRegression`, so it is now silent on infra.
  - **Read-API:** the `regression` block carries `outcome_compared` + `infra_failed` (no
    key removed/renamed) so a consumer distinguishes "the goal regressed" from "the driver
    could not run". `handlers.applyDiffCompat` back-fills `outcome_compared:true` on a
    diff blob persisted BEFORE 0064 (key ABSENCE is the discriminator, NOT the value —
    a post-0064 blob's explicit `outcome_compared:false` must survive) so historical,
    legitimately-scored diffs are not misread as "could not run". Both back-filled
    fields and the absence discriminator are mutation-tested.
  - **UI:** `walkChangesCard` renders a neutral amber **"Could not run"** state (with the
    escaped driver reason) instead of the red Regression badge.
  - 🔴 **Honest limit — MISCLASSIFICATION IN THE WORSE DIRECTION IS REAL.** The session
    watchdog fires whenever Chromium stops making progress; a font-less environment is the
    KNOWN trigger, not the only one. **A genuinely broken audited page that wedges the load
    event produces the same `ErrBrowserStalled`, is classified as infra, and therefore
    SILENTLY SUPPRESSES a real product regression** — the mirror of the bug #45 fixed, and a
    deliberate trade (a false PASS on a wedged page vs. a false FAIL on every CI infra
    blip). The only current discriminator is
    **`auditloop_browser_stalls_total{phase="drive"}`**: a stall that correlates with a
    deploy of the audited app, rather than with CI-host churn, is probably the PAGE. Check
    it before trusting a `Could not run` verdict. Distinguishing the two properly (e.g. a
    page-level progress probe before blaming the browser) is NOT done here.
  - **Other honest limits:** (a) steps driven BEFORE a session stall are DISCARDED — the
    stalled goroutine is abandoned and reading its trace would be a data race; do not
    attempt to persist them. (b) **One-time transition:** a target whose latest walkthrough
    is a PRE-migration infra failure reads back `infra_failed=0` and stays a valid baseline
    once (same shape as the #18 pushed-a11y-id transition); it self-heals on the next
    walkthrough. (c) The `prev.InfraFailed` term in `ComputeDiff` is belt-and-braces with
    **no known reachable path** today (the baseline query excludes infra rows at stamp time,
    the handler always creates a fresh row, and 0064 is `DEFAULT 0` so a pre-migration row
    reads back false) — it is kept for a future path that re-runs an existing row.
- **Deferred:** blocker-only regressions don't bump the counter (surfaced in the diff/gate/UI
  only); qualitative persona-verdict trend across >2 walkthroughs; multi-goal.

## DOM/a11y grounding on PUSHED runs (#35/#37 extended to `auth_mode='plugin'`)

The persona evaluator reasons over screenshots, so it re-derives mechanical accessibility
from pixels and invents objective a11y false positives (an `sr-only <label for>` it can't
see; an `<a>` styled as a card). #35 (crawl) and #37 (driven) fixed that with a bounded
per-page **DOM/a11y digest** (`internal/crawler/a11y-digest.js` → an `a11y.json` artifact +
`pages.a11y_digest_key`, migration 0060) feeding a **deterministic, no-LLM gate**
(`internal/eval/verify.go` `dropContradicted`) that drops ONLY findings the digest
POSITIVELY REFUTES. **Plugin-PUSHED runs were still screenshot-only** — measured on a real
push: 2 of 8 synthesis items were a11y claims axe on the same tree refutes outright.

A push may now carry an **OPTIONAL per-page `a11y_digest` artifact ref** (a filename in the
metadata + a matching multipart part, exactly like `axe`/`network`), so a producing harness
that already runs axe in-page emits the same digest at no extra cost.

- **Zero new schema below the ref.** The bytes are stored under the EXISTING key scheme
  (`{target}/{run}/{page_slug}/a11y.json`, `storage.A11yDigestKey`) and set the EXISTING
  `pages.a11y_digest_key` (migration 0060). **No migration** — the change is pure wiring
  (`plugin.PageKeys.A11yDigest` → `MapPage` → the page row + the `report.PageReport`
  `a11y_digest_key` seam). Eval is **UNCHANGED**: it already reads
  `pages.a11y_digest_key`, so a pushed run flows through the identical `loadDigest` →
  SEMANTIC-STRUCTURE prompt block → `dropContradicted` path. **One gate, three ingest
  paths — never forked.**
- **The pushed digest is UNTRUSTED — the fundamental difference from #35/#37.** The
  crawl/driven digest is self-generated by our own chromedp; a pushed one is authored by
  an external harness. And the gate **DROPS findings**, so a malformed/buggy/hostile digest
  can *suppress real problems* — a worse failure mode than the FP it fixes. Hence
  `plugin.NormalizeA11yDigest` (`internal/plugin/a11y.go`), with a deliberate split:
  - **FAIL-CLOSED on validation → the whole push is rejected (400, nothing persisted):**
    unparseable JSON, an unknown field (`DisallowUnknownFields`, recursive), trailing data,
    an unknown `label_source`, a missing `selector`, an element list over the crawl-path
    caps (`report.MaxA11yInteractive/FormControls/Landmarks` = 40/30/30, mirroring
    `a11y-digest.js`), over `report.MaxA11yDigestBytes` (256 KiB), or a digest that decodes
    to nothing. **A silently-ignored digest looks identical to a working one** — this repo
    has been bitten by exactly that twice on the producer side, so a broken digest is LOUD
    at ingest, never accepted-then-dropped.
  - **FAIL-OPEN on the gate → accept, but refute nothing off a questionable fact:**
    **`has_label` is NEVER trusted as pushed — it is DERIVED from `label_source`**
    (`report.IsProgrammaticLabelSource`, the #36 structural lesson applied to untrusted
    input), so a producer that omits/downgrades `label_source` gets "unknown → refute
    nothing" instead of an unearned drop. Over-long strings are TRUNCATED (a truncated
    selector stops matching a concrete key ⇒ refutes less), not rejected.
  - **`tag` has the SAME drop power as `has_label`, and gets the same treatment.**
    `dropContradicted` drops a "not keyboard operable" finding on `tag ∈ {a, button}`.
    On the crawl path `tag=a` implies a real `a[href]` (the digest's query IS `a[href]`),
    but a pushed digest has no query behind it and can simply CLAIM `tag:"a"` on an inert
    element. So the drop now ALSO requires the digest's own `focusable && !disabled` to
    agree — a self-contradictory claim (`{"tag":"a","focusable":false,"disabled":true}`)
    refutes nothing. The crawl JS computes both honestly (`tabIndex >= 0 && !disabled`),
    so the crawl/driven paths are unaffected. The validator additionally rejects a
    malformed tag token (hygiene only — an allowlist necessarily CONTAINS `a`/`button`,
    so it cannot close this hole; the `focusable`/`disabled` conjunct is what does).
  - **Conflicting duplicates are rejected at ingest, not resolved permissively.** The gate
    OR-merges label facts onto a shared concrete key — sound when a `#id` is unique by
    construction, but for producer input one buggy duplicate would license a drop (the only
    "most permissive wins" in the design). `NormalizeA11yDigest` rejects a selector that
    appears twice with CONFLICTING programmatic-ness. It compares programmatic-NESS, not the
    raw string, deliberately: a faithful port of `a11y-digest.js` legitimately reports the
    same `<input>` as `text-content` in `interactive` and `none` in `form_controls`. The
    OPERABILITY merge is separately conservative (both rows must be operable).
  - **Producer strings can never forge a prompt line.** `selector`/`role`/`tag` are rendered
    into the prompt block the model is told is AUTHORITATIVE, so an embedded newline would be
    a steer that BYPASSES the gate entirely. Two layers: `trunc` strips control characters at
    ingest, and `semanticBlock` (`internal/eval/prompt.go`) `%q`-quotes selector/role/tag —
    the rule `accessible_name` and landmark `text` already followed.
  - What is stored is the **re-serialised canonical digest** auditloop built from validated
    fields — never the producer's raw bytes. Producer strings (selectors/names) stay
    UNTRUSTED → escaped JSON, rendered escaped, never `g.Raw`.
  - **Honest limit on the loudness property:** validation failure is loud (400), but if
    `Store.Put` of an already-validated digest FAILS, the page's key is left unset and the
    push still returns **200** — the producer believes grounding is on when it isn't. This
    matches how the screenshot/axe/network artifacts already behave, and it fails in the SAFE
    direction (no digest ⇒ no drops), but the "a silently-ignored digest is indistinguishable
    from a working one" property holds at INGEST VALIDATION only, not end-to-end.
  - **Trust boundary (honest):** a digest can only be pushed with a target's own push
    token, so a target owner pushing a FALSE digest only misleads THEIR OWN audit (no
    cross-tenant impact). The realistic risk is a **buggy producer silently hiding its
    owner's findings** — which is why every ambiguous case errs toward refuting less.
    🔴 **That risk GREW with selector grounding** (see "Selector grounding" below): a pushed
    digest now also suppresses mechanical a11y findings whose selector it merely OMITS, not
    just ones it positively refutes. The empty-vocabulary and cap guards bound it (a digest
    that lists nothing drops nothing), but a producer emitting a PARTIAL element list should
    emit NO digest for that page — a partial list now deletes findings.
- **BACKWARD COMPATIBILITY IS ABSOLUTE.** A push WITHOUT `a11y_digest` behaves
  byte-for-byte as before: no key set, digest-less pages, gate never fires, screenshot-only
  evaluation. Every existing producer (any external push harness, plus auditloop's own
  self-audit) keeps working untouched.
- **What a producer must emit** (per page, optional): a multipart file part whose bytes are
  `report.A11yDigest` JSON — `{"interactive":[{tag,role?,accessible_name?,selector,type?,
  disabled?,focusable,label_source?}], "form_controls":[{selector,accessible_name?,
  has_label,label_source}], "landmarks":[{tag,role?,text?}]}` — plus `"a11y_digest":
  "<that filename>"` on the page in `metadata.json`. Rules that matter:
  - `label_source` ∈ `for | aria-label | aria-labelledby | wrapping-label | placeholder |
    text-content | value | none`, or omitted. The first four are PROGRAMMATIC and are the
    ONLY values that ever license a drop. An unknown value REJECTS the push — omit rather
    than invent (omitting safely means "refute nothing").
  - `has_label` is IGNORED and recomputed from `label_source`.
  - `focusable`/`disabled` must be HONEST on `<a>`/`<button>` — they now gate the
    not-operable drop (`tabIndex >= 0 && !disabled`, exactly what the crawl JS computes).
  - **NEVER send an empty digest — OMIT `a11y_digest` for that page instead.** A digest
    that decodes to `{"interactive":[],"form_controls":[],"landmarks":[]}` is REJECTED
    (400) and that rejects the WHOLE multi-page push, not just the page. This is the
    normal output of `a11y-digest.js` on a genuinely bare page (an error page, an
    image-only confirmation screen) AND of its catch-all on a JS exception — so a
    producer MUST check `interactive.length || form_controls.length || landmarks.length`
    before attaching the file. (Rejecting a genuinely-SENT empty digest stays right: it
    would otherwise be indistinguishable from sending nothing.)
  - `selector` is required; make it the concrete anchor (`#id` / `[name=…]`) — the gate
    only indexes those, so a class-only selector is inert (harmless, never refutes).
  - Caps: ≤40 interactive, ≤30 form controls, ≤30 landmarks, ≤256 KiB — over any REJECTS.
    Truncated silently: selector 200, accessible_name 120, landmark text 80,
    tag/role/type 64 (`report.MaxA11yTokenLen`) — **BYTES, not characters**, so a
    non-ASCII selector truncates sooner than its rune count suggests (safe direction:
    a truncated selector stops matching a concrete anchor, so it refutes LESS).
  - `tag` must be a well-formed element-name TOKEN — `^[a-z][a-z0-9-]*$`, so custom
    elements (`my-widget`) pass and garbage (`"a b"`, `"<a>"`, `"a\nrole=link"`, `1div`)
    REJECTS. There is deliberately **no allowlist of element names**: `a11y-digest.js`
    queries `[role=button]`/`[role=link]`/`[contenteditable=true]`, which match ANY
    element, so `i`, `span`, `address`, `figure`, `code`, `svg`/`g`/`path`, `tr`, `dl`,
    `dialog`, `video` … are all normal output. (An earlier revision DID carry an
    allowlist and 400'd a faithful producer over one `<i class="fa" role="button">`,
    discarding the whole push. Pinned by `TestRealA11yDigestPassesPushValidation`, which
    captures a real digest through chromium and asserts it validates.)
  - A selector appearing twice with conflicting programmatic-ness REJECTS the push —
    compared by the anchor the GATE indexes (`report.ConcreteKeys`), so `input#e` and
    `#e` are the SAME anchor and conflicting `label_source` values across those two
    spellings still reject.
  - Unknown JSON fields REJECT the push — emit exactly this shape.
  - Two pages (e.g. the mobile + desktop rows of one URL) MAY reference the SAME digest
    filename; each page gets its own stored copy.
  - **Easiest correct producer:** port `internal/crawler/a11y-digest.js` verbatim into the
    harness's page context, then apply the empty-check above before attaching it.
- **Tests:** `internal/plugin` (accept/normalise; reject malformed/unknown-field/unknown
  label_source/empty/over-cap/oversized/bad-tag/conflicting-duplicate-selector; `has_label`
  derived in BOTH directions; control characters stripped; re-serialisation; ref↔part +
  orphan integrity for the new ref; digest-less push unchanged); `internal/eval` (a claimed
  `<a>` that is not focusable / is disabled refutes NOTHING, conservative operability merge,
  a form-control row never clobbers operability, untrusted selector/role/tag cannot forge a
  prompt line); `internal/db` (the key round-trips on a pushed run + owner-scoping);
  `internal/eval` (a PUSHED run's refuted finding is dropped while a TRUE one is KEPT —
  discriminating, not blanket — with a no-digest control); `handlers` (stored under the key
  scheme + report seam, 400 on malformed with NO run persisted, digest-less push
  unchanged); **e2e** `TestEndToEndPluginPushA11yDigestGrounding` (real `auditloop-push`
  CLI pushes a run WITH a digest → persona eval against a FAKE OpenRouter → the two DOM-
  refuted FPs are gone, the true missing-label + the subjective finding survive; a second
  digest-less push is the backward-compat control where nothing is dropped).

## Selector grounding — the evaluator may cite ONLY digest-listed selectors (`internal/eval/ground.go`)

`dropContradicted` drops a finding only when the digest POSITIVELY REFUTES it, and it can
only do that through a UNIQUE CONCRETE anchor (`#id` / `[name=…]`). **Measured on a real
pushed run** (`civitai-manager-funnel`, run `9d322473`): **4 objective a11y claims in, 4
survived** — every one cited a placeholder attribute, a class, or an invented
pseudo-selector (`h3:contains('SDXL Portrait')`), so the gate had nothing to look up. The
digest reached the prompt and **nothing downstream depended on it**: grounding was
DECORATIVE. The evaluator reasons from PIXELS — it cannot know a real id, so left alone it
invents an anchor, and an invented anchor is unverifiable by construction.

- **Prompt half (`prompt.go` `selectorRule`, appended to the semantic block):** the listed
  elements are the ONLY selector vocabulary; copy a selector VERBATIM; a MECHANICAL a11y
  claim about an unlisted element is discarded. Emitted only when a digest exists.
- **Deterministic half (`groundSelectors`, runs BEFORE `dropContradicted` — order is
  load-bearing):** for each finding the existing narrow classifiers
  (`claimsMissingLabel`/`claimsNotOperable`) call a mechanical a11y claim —
  **listed** (verbatim, or by a concrete anchor so `#q` matches a digest `input#q`) → keep;
  **re-anchorable** — the finding quotes an accessible name (`input[placeholder='Email
  address']`, `:contains('…')`) that exactly ONE digest element carries (exact or
  truncated-prefix, ≥4 chars) → **rewrite the selector to the digest's own**, which then
  hands a true finding a real anchor and hands an FP to `dropContradicted`; **otherwise →
  DROP as ungrounded** (an empty selector on a mechanical claim counts as ungrounded).
  Subjective/visible-UX findings — the evaluator's actual job — are NEVER touched.
- **Why dropping is sound here and is NOT the absence-inference `dropContradicted` rightly
  refuses:** that gate asks "is the CLAIM false?" (an absent selector can never answer);
  this asks "is the claim CHECKABLE?" A mechanical a11y assertion about an element the
  authoritative DOM snapshot does not contain cannot be confirmed, refuted, or acted on —
  and mechanical a11y is ALREADY measured deterministically by axe on the same tree and
  reported separately.
- 🔴 **A drop requires the digest to be COMPLETE, and that means TWO things** — both are
  load-bearing, and either one missing turns the gate into a blanket delete:
  **(a) no list at its cap** (40/30/30) — at a cap the list is a PREFIX of the page, so
  "not listed" stops meaning "not on the page" (the LANDMARK cap counts too even though
  landmarks carry no selectors: it means a large page, where the element lists are the
  likeliest to be silently truncated); **(b) a NON-EMPTY selector vocabulary** — a digest
  can be non-empty (`IsEmpty()==false`) while carrying ONLY landmarks, or only
  selector-less entries (an error page, a confirmation screen, a page whose element
  queries matched nothing, or a pushed digest with one landmark — `NormalizeA11yDigest`
  rejects only an ALL-empty one). Such a page testifies to nothing about interactive
  elements. In both cases **re-anchoring still runs, dropping does not**.
  🔴 **`digestVocabularyComplete` is that predicate, and the PROMPT calls the SAME
  function** — the SELECTOR RULE ("a claim about an unlisted element is discarded
  automatically") is emitted only where the gate will actually enforce it. Two
  lookalike conditions disagreed on every capped page, telling the model to self-censor
  where the gate then kept its findings.
- 🔴 **The emitted anchor must survive contact with the real DOM.** Re-anchoring returns the
  digest's OWN selector string unmodified — id/`[name=…]` matching is CASE-SENSITIVE, so
  emitting a normalised `input#useremail` for a real `input#userEmail` would hand the reader
  an anchor matching nothing. Matching is case-insensitive (fail-open); the OUTPUT is not.
- **Only NAME-BEARING attributes may re-anchor** (`placeholder`/`aria-label`/`title`/`alt`/
  `value`/`label`/`aria-placeholder`, plus `:contains()`-style text pseudo-classes).
  Harvesting every quoted literal is unsound: `input[type="submit"]` re-anchored onto an
  unrelated `button#newsletter-submit` because that button's name is "Submit". The
  ambiguity check cannot catch this — it only sees two DIGEST elements sharing a name,
  never that the literal was never a name. A PREFIX match additionally needs
  `minPrefixMatch` (12 runes; exact matches need 4) **on BOTH strings** — the evidence is
  the SHARED prefix, i.e. the shorter one. "Promo" prefixes "Promotional discount code for
  returning buyers" and countless others; in the mirror direction a digest element merely
  named "Save" would otherwise absorb a finding quoting "Save these changes and publish
  now". A genuinely model-truncated quote is long by construction. Lengths are RUNES.
- **Accepted false negative (deliberate):** a mechanical a11y claim about an element the
  digest does not list — a non-interactive one (an `<img>` alt) or one hidden at capture —
  is dropped even if true. axe covers those classes deterministically.
- **Observability — `auditloop_eval_findings_gated_total{action}`** (`contradicted` |
  `ungrounded` | `reanchored`) is the ONE metric that answers "is the grounding
  load-bearing or decorative?". A flat zero across a pass means the digest changed nothing
  — exactly the state this work found. `contradicted` is bumped by `dropContradicted` too.
  It is a process-global counter: read a DELTA across a pass, not the raw value.
- 🔴 **`applyDOMGate` owns the two stages' ORDER** (ground → contradict) and is what
  `generator.go` and every test call. It is a function, not two lines in the caller,
  because with the sequence inlined the tests composed their own copy — swapping the
  production lines passed the entire suite. A claim about order needs a single definition
  the tests actually exercise.
- 🔴 **Amplified trust in an externally-PUSHED digest.** Before this, a hostile/buggy
  producer could suppress only a SPECIFIC claim, and only via a positive refutation on a
  unique concrete anchor. Now a partial digest also suppresses **every mechanical a11y
  finding whose selector it omits** on that page. The empty-vocabulary + cap guards bound
  the worst case (a digest that lists nothing drops nothing), and the blast radius stays
  single-tenant (a push token is target-scoped, so a false digest misleads only its own
  owner's audit) — but the realistic risk documented for the push path, **a buggy producer
  silently hiding its owner's findings**, is now larger. A producer that emits a PARTIAL
  element list should emit no digest at all for that page.
- **One-time transition on the Phase-4 walkthrough gate.** Blocker identity is
  `persona + normalize(selector)` (`internal/walkthrough/diff.go`), so the first walkthrough
  diff after this deploys sees re-anchored selectors as NEW blockers and the old spellings
  as RESOLVED. A consumer gating on `len(new_task_blockers) > 0` can trip once per target on
  a transition, not a regression — the same shape as the #18 pushed-a11y-id transition, and
  it self-heals on the next walkthrough. **SUPERSEDED 2026-08-27: no consumer should gate on
  `new_task_blockers` at all** (it churns 3/3 between IDENTICAL runs — see the Phase-4 read-API
  block), so this transition is now advisory noise rather than a build-breaking event.
- **Tests (`internal/eval/ground_test.go`):** the measured regression itself (the 4 real
  claims + a real-shaped digest, with `dropContradicted`-alone as the CONTROL that still
  keeps all 4 → the new pipeline keeps 0); listed-verbatim + bare-`#id` kept; re-anchor by
  quoted name (true finding survives) and re-anchored FP then refuted (proves the order);
  truncated-quote re-anchor; ambiguous name / too-short literal / short prefix / an
  identifier-valued attribute never re-anchor; re-anchoring PRESERVES the digest's selector
  case; re-anchoring still runs on a capped digest; a partial anchor is not "listed";
  subjective findings and top_fix untouched; unanchored mechanical claim dropped; **at
  either cap, and against an empty selector vocabulary, nothing drops**; nil/empty digest is
  a no-op; no in-place mutation; the metric moves once per action with the right label;
  the prompt's rule and the gate's licence agree on every digest shape (landmarks-only,
  one element, each of the three caps, selector-less entries); the prefix bar has a fixture
  ON the bar and one rune under it (12 vs 11 runes) on BOTH the quoted-literal and the
  digest-name side, so neither guard survives a `<`→`<=`. **e2e** — the fake model now also
  emits a claim anchored to an INVENTED selector, which the crawl test
  (`TestEndToEndPersonaEvaluationDOMGrounding`) asserts is DROPPED and the driven test
  (`TestEndToEndWalkthroughPersonasDOMGrounding`) asserts is dropped on the landing page and
  KEPT on `/about`, which has no interactive elements at all (landmarks-only ⇒ empty
  vocabulary ⇒ no drop) — that KEPT/dropped pair is what makes the e2e a
  test of the digest's CONTENT rather than of a blanket rule, and it is the only e2e
  exercise of the grounding stage (the two older FPs are dropped by refutation instead).

## Stack

- **Backend:** Go 1.25, `gorilla/mux`. **Single binary, two roles** selected by
  `AUDITLOOP_ROLE=web|worker|all` (default `all`) — the web/API server and the
  background crawl worker, same binary (a single binary with two roles).
- **Crawler:** **chromedp** (`github.com/chromedp/chromedp`) driving headless
  Chromium. NOT Playwright/Node. axe-core is vendored (`internal/crawler/axe.min.js`,
  v4.12.1) and injected via `chromedp.Evaluate`. Chromium resolves host-agnostically:
  `AUDITLOOP_CHROMIUM` → `chromium`/`chromium-browser`/`chrome` on PATH (chromedp `ExecPath`).
- **DB:** Postgres (`jackc/pgx`) or SQLite (`modernc.org/sqlite`, default dev/tests) —
  one query layer, placeholder rebind `?`→`$N`, portable DDL (TEXT ids + RFC3339
  TEXT timestamps), inline migrations in `internal/db/migrations.go`.
- **Object storage:** **minio-go** (`github.com/minio/minio-go/v7`) for the S3 API —
  chosen over `aws-sdk-go-v2` for its far smaller surface and one-call presigning,
  which is all we need. A **filesystem backend** (`internal/storage/fs.go`) implements
  the same `Store` interface and is used when `S3_ENDPOINT` is unset (hermetic tests +
  zero-dependency local dev). The bucket is never public — screenshots reach the
  browser via presigned GET (S3) or the authed `/artifacts/` proxy (filesystem).
- **Frontend:** server-rendered **gomponents** (`g`/`h`/`c` aliases) + **htmx 2** +
  **Tailwind v4** (CSS-first `@theme` in `static/input.css`, no JS config). Vanilla
  `static/js/app.js`, **cache-busted** via a content-hash `?v=` query. **PWA:**
  `/manifest.webmanifest` + root-scoped `/sw.js` (offline app shell) + SVG icon.

## Architecture map

```
main.go                     entrypoint: opens DB+store, builds router (starts worker if role includes it)
internal/config             AppConfig from env (server/role, DB, Supabase, S3, crawl caps, DEV_MODE)
internal/auth               Supabase HS256 JWT verify + bearer-OR-cookie Middleware + RequireAuth;
                            DEV_MODE bypass (fixed dev user); UserID(ctx) = per-user scoping key
internal/db                 sqlite/postgres wrapper + rebind + migrations; targets/runs/pages/findings
                            queries; atomic ClaimNextQueuedRun + RecoverStaleRuns (startup sweep);
                            P2: baseline linking (LatestDoneRunForTarget → runs.prev_run_id at CreateRun),
                            SetRunDiff (runs.diff_json), UpdatePageDiff (pages.diff_pct/diff_key);
                            P4: login_recipes.go (Set/Get/DeleteLoginRecipe, upsert + auth_mode flip)
internal/crypto             P4 AES-256-GCM encrypt-at-rest for login-recipe credentials (key from
                            AUDITLOOP_ENCRYPTION_KEY; hex/base64; nonce-prepended; server-side only)
internal/recipe             P4 canonical login-step model (goto/fill/click/waitFor — NO eval) +
                            guided-form compile + validation + credential JSON (pure, no chromedp)
internal/action             Phase-3 CLOSED driver action set (click/type/press/select/scroll/waitFor/
                            navigate/finish — NO eval/script) + ParseAction (DisallowUnknownFields) +
                            Validate + SuccessAssertion (pure, no chromedp/llm; mirrors internal/recipe)
internal/storage            Store iface + S3 (minio-go, presigned GET) + FS (local, authed proxy) + key scheme
internal/crawler            chromedp BFS crawler; ssrf.go (private/loopback/metadata guard, tested);
                            classify.go (first/third-party by origin); axe.go (vendored axe-core inject);
                            login.go (P4: runLogin in the shared tab before BFS; RunLoginProbe for login-test);
                            driver.go (Phase-3 goal-directed Drive loop: injected Planner + deterministic
                            success/stuck signal + CheckAssertion + buildInteractiveDigest + dry-run
                            submit-guard via intercept.go); interactive-digest.js (embedded);
                            favicon.go (#33: SSRF-safe best-effort favicon fetch — GuardConfig.CheckURL
                            + IP-pinning dialer + no-redirect + raster-only, run-scoped key, non-fatal)
internal/diff               P2 pure-Go pixel diff (Compare → diff_pct/DiffPNG, stdlib image only, no dep)
                            + StringSetDelta (page-set / a11y-rule deltas); deterministic, unit-tested
internal/signals            deterministic perf/layout FINDING logic (PerfFindings/LayoutFindings on pure
                            report.Perf/report.LayoutSmells) — ONE source of truth for the web-vitals +
                            layout-smell thresholds + mobile gating, shared by internal/worker (native
                            crawl) AND internal/plugin (push ingest); no chromedp/crawler dep
internal/report             report.json contract (versioned, forward-compatible; the P5 plugin shape) +
                            the optional P2 `diff` block (report.Diff) + the P3 per-page `notes` seam +
                            the optional P5 pushed-run `label`
internal/plugin             P5 plugin-push: push token (generate/sha256/constant-time verify/rotate),
                            the push schema + strict Validate + MapPage mapper, and the generic
                            multipart uploader (Upload/UploadFromDisk) used by cmd/auditloop-push
cmd/auditloop-push          P5 reference uploader CLI (reads metadata.json + files → multipart POST)
internal/apikey             Read-API key machinery: Generate (32-byte crypto/rand → base64url + sha256),
                            Hash, ConstantTimeEqual — mirrors internal/plugin/token.go (read-only, per-user)
internal/db/api_keys.go     Read-API keys: CreateAPIKey/APIKeyLookup/ListAPIKeys(no hash)/RevokeAPIKey/
                            TouchAPIKeyLastUsed (migrations 0035–0037) + LatestDoneRunForTargetOwned
internal/llm                P3 OpenRouter vision client: chat/completions with base64 image parts,
                            configurable base URL (testable), server-side key, screenshot downscale ≤1568px;
                            opts into usage accounting → Draft returns per-call llm.Usage{CostUSD,tokens}
internal/notes              P3 async UX-notes pass + grounding prompt (Generator.Run, CountUnits);
                            one LLM call per (page,model), concurrency-capped, per-cell degrade
internal/eval               Persona-walkthrough evaluator (Phase 1): code-defined persona set +
                            prompts (gen/verify/synth) + structured-JSON parse/validate + Generator.Run
                            (generate→verify per (page,persona) + one synthesis call); reuses internal/llm,
                            report structured types (report.PageEvaluation/EvalSynthItem), notes concurrency.
                            Phase 2: infer.go = synchronous single-call goal inference (InferConfig →
                            InferredConfig{product_summary,primary_job,primary_cta,audiences}) from a done
                            run's landing screenshot + URL digest; audiences filtered to the curated personas
internal/db/audit_config.go  Phase-2 per-target audit config: Set/GetTargetAuditConfig (target_audit_config,
                            migration 0050; upsert + owner-scoped get via target→user join) + Phase-3
                            success-assertion cols (migrations 0051–0053)
internal/walkthrough        Phase-3 goal-directed driver: planner.go (crawler.Planner on internal/llm —
                            one action/turn, retry-then-degrade) + generator.go (async job: Generator.Run
                            loads goal+success+login → crawler.Drive → persists steps/cost/outcome).
                            Phase-4: diff.go = pure walkthrough-vs-previous-terminal-walkthrough regression
                            (ComputeDiff: outcome/stuck delta + StringSetDelta over persona task-blocker keys)
                            + RefreshWalkthroughDiff (gather+store to walkthroughs.diff_json; drive-end +
                            post-eval triggers) — mirrors the P2 crawl diff
internal/db/walkthrough.go  Phase-3 walkthroughs + walkthrough_steps CRUD + async-job tracking (Claim/
                            Update/Finish/MarkDrivingWalkthroughsFailed) + SetDrivingConfig (migrations
                            0054–0058); owner-scoped via target→user. PR-B: MaterializeWalkthroughRun
                            (a done trigger='walkthrough' synthetic run + one page per step, idempotent,
                            stamps walkthroughs.run_id) → the Phase-1 eval runs over the driven trace
internal/db/page_evaluations.go  Phase-1 eval: page_evaluations CRUD + upsert (UNIQUE page_id,persona) +
                            run-level eval-job tracking (ClaimEvalJob/Update/Finish/MarkGeneratingEvalFailed/
                            AddEvalCost/SetRunEvalSynthesis) (migrations 0039–0049)
internal/worker             claims queued runs → crawl → persist pages/findings → upload artifacts +
                            report.json → finish; injectable CrawlFunc (tests stub the browser).
                            diff.go = P2 diff phase (runDiff: visual + a11y-rule + count deltas vs baseline)
internal/metrics            Prometheus collectors (auditloop_runs_total{status}, pages_crawled, blocked,
                            duration, visual_regressions_total, run_pages_changed, notes_generated{model,status},
                            notes_duration, notes_cost_usd_total{model}, notes_prompt/completion_tokens_total{model})
handlers                    router (app.go), auth sync/signout, pages (dashboard/target/run),
                            targets+runs API, artifact proxy, health, PWA manifest/sw;
                            plugins.go (P5 push ingestion + plugin-target create/rotate);
                            audit_api.go (read API: apiKeyAuth middleware + owner-scoped run-list/
                            report/latest/artifact reads + api-key mint/revoke; ratelimit.go tokenBucket);
                            eval.go (persona-walkthrough: evaluate trigger + eval-status poll + owner-scoped
                            read-API /evaluation endpoint; Phase-2 form defaults from the target audit config);
                            audit_config.go (Phase-2 per-target audit config: infer + save/confirm routes +
                            owner-scoped read-API /audit-config endpoint; Phase-3 extends save with
                            driving_enabled/allow_real_submit + the success assertion);
                            walkthrough.go (Phase-3 driver: start route [403/503/409-gated] + status poll +
                            owner-scoped read-API /walkthroughs/{id} trace; PR-B: the evaluate-walkthrough
                            route [POST /api/targets/{id}/walkthroughs/{wid}/evaluate] materializes a
                            synthetic run → reuses the Phase-1 eval stack; read-API adds eval_run_id)
components/{layouts,pages,partials}  gomponents shells + views (login, dashboard, target, run)
static                      input.css (@theme), output.css (built), js/app.js (cache-busted), img/icon.svg
tests/e2e                   hermetic browser e2e: fixture site → run via HTTP API → assert crawl outcome;
                            P2: TestEndToEndRegressionDiff crawls twice, mutates the fixture between runs
                            (visual change + new a11y violation + new page), asserts the diff surfaces all.
                            P3: TestEndToEndVisionNotes crawls, then runs a 2-model notes pass against a
                            FAKE OpenRouter httptest server (no real key), asserts one note row per
                            (URL,model), both viewports sent, the labeled UI renders, and an edit saves
tests/e2e/ux-audit         SELF-AUDIT harness (Playwright, NOT Go): `make ux-audit` boots a local
                            DEV_MODE auditloop (keys stripped; sqlite + fs storage; CRAWL_ALLOW_LOOPBACK;
                            throwaway encryption key for the P4 auth UI; dummy OpenRouter key + dead base
                            URL to render the P3 notes control without any external call) and walks
                            auditloop's OWN UI (login → dashboard → target + auth card → plugin token
                            reveal → plugin target → seeded self-crawl run → P2 diff run), screenshotting
                            each view + capturing console/network/axe. It then (opt-in, non-fatal) PUSHES
                            the run back into auditloop via its own P5 plugin API — auditloop audits itself.
                            Ported ~verbatim from the external ux-audit harnesses: `_lib/audit.ts` (UxAudit
                            capture lib), `_lib/push.ts` (push shim, schema mirrors internal/plugin), and
                            `_lib/push.test.mjs` (node:test, `make ux-audit-push-test`). Enable the self-push
                            with AUDITLOOP_PUSH_URL + AUDITLOOP_PUSH_TOKENS='{"auditloop-funnel":"<token>"}'
                            (create a plugin target in the app → token). nixos chromium via PLAYWRIGHT_CHROMIUM
                            / AUDITLOOP_CHROMIUM; never `playwright install`. See tests/e2e/ux-audit/README.md.
```

## report.json schema (the forward-compat contract)

Written per run to `{target_slug}/{run_id}/report.json`. This is the SAME shape
a future **plugin push (P5)** will emit, so it is versioned (`schema`) and
additive-only. Defined in `internal/report/report.go`:

```jsonc
{
  "schema": 1,
  "tool": "auditloop",
  "tool_version": "<build>",
  "run_id": "<uuid>",
  "target_id": "<uuid>",
  "target_name": "Acme",
  "base_url": "https://acme.com",
  "auth_mode": "none",                 // none|login|plugin (P4/P5 seam)
  "started_at": "RFC3339",
  "finished_at": "RFC3339",
  "status": "done",                    // done|failed
  "error": "",
  "summary": {
    "pages_crawled": 3,
    "urls_discovered": 5,
    "urls_blocked": 1,                 // refused by the SSRF guard
    "a11y_violations": 14,
    "console_first_party": 2, "console_third_party": 5,
    "network_first_party": 1, "network_third_party": 3
  },
  "pages": [{
    "url": "https://acme.com/",
    "viewport": "mobile", "width": 390,
    "screenshot_key": "acme/<run>/<page>/mobile.png",
    "axe_key": "acme/<run>/<page>/axe.json",
    "network_key": "acme/<run>/<page>/network.json",
    "load_ms": 812,
    "console": { "first_party": 1, "third_party": 2 },
    "network": { "first_party": 0, "third_party": 1 },
    "a11y":    { "violation_count": 7, "node_count": 12 },
    "findings": [{ "type": "a11y", "severity": "serious", "detail": { /* raw axe violation */ } }],
    "notes": { "anthropic/claude-haiku-4.5": "…markdown…" },  // OPTIONAL P3 seam; NOT written by the crawl
    "evaluations": {                                          // OPTIONAL persona-walkthrough seam (Phase 1); NOT written by the crawl
      "skeptical-evaluator": { "comprehension": "unclear",
        "blockers": [{ "issue": "…", "selector": "…", "evidence": "…", "verified": true }],
        "frictions": [], "top_fix": { "selector": "…", "change": "…", "impact": "high" } }
    }
  }],
  "versions": { "auditloop": "<build>", "chromium": "", "axe_core": "4.12.1" },
  // OPTIONAL Phase-1 persona-walkthrough run-level seams (DB is the source of truth; NOT written by the crawl):
  "eval_synthesis": [{ "title": "…", "impact": "high", "affected_urls": ["…"], "affected_personas": ["…"] }],
  "eval_cost": { "cost_usd": 0.0123, "prompt_tokens": 4200, "completion_tokens": 900 },

  // OPTIONAL P2 block (omitted entirely on a first run / pre-P2 reports):
  "diff": {
    "prev_run_id": "<uuid>",
    "prev_run_at": "RFC3339",
    "pages_added":   ["https://acme.com/new"],
    "pages_removed": ["https://acme.com/old"],
    "pages_changed": 2,                        // VISUAL REGRESSIONS: !size_changed && diff_pct >= 1%
    "pages_size_changed": 1,                   // layout/size changes (dimensions differ) — NOT regressions
    "new_a11y_rules":      ["label"],          // axe rules introduced (regressions)
    "resolved_a11y_rules": ["region"],
    "a11y_delta": 2, "console_delta": -1, "network_delta": 0,  // current − prev
    "changed_pages": [{
      "url": "https://acme.com/", "viewport": "mobile",
      "diff_pct": 12.5, "size_changed": false,  // size_changed=true ⇒ layout change, no diff image
      "not_compared": false,                    // true ⇒ capture too large to visualize (no diff image)
      "diff_key": "acme/<run>/<page>/mobile.diff.png"
    }]
  }
}
```

## S3 key scheme (content-addressable where sensible)

```
{target_slug}/{run_id}/{page_slug}/{viewport}.png
{target_slug}/{run_id}/{page_slug}/{viewport}.diff.png   # P2 visual-diff image
{target_slug}/{run_id}/{page_slug}/axe.json
{target_slug}/{run_id}/{page_slug}/network.json
{target_slug}/{run_id}/report.json
{target_slug}/{run_id}/favicon.<ext>                     # #33 run-scoped favicon (raster only; storage.FaviconKey)
```

`page_slug` = a readable path portion + an 8-char SHA-256 of the full URL (stable
+ collision-free). Bucket: `audit-artifacts`.

## Conventions

- **gomponents aliases:** `g "maragu.dev/gomponents"`, `h ".../html"`, `c ".../components"`.
  Pages call `layouts.App(ctx, …)`; partials/fragments return bare `g.Node` for htmx swaps.
- **htmx:** `<body>` is `hx-boost="true"` (nav = AJAX body swap → does NOT fire
  `DOMContentLoaded`), so per-element JS is (re)wired on **both** `DOMContentLoaded`
  and `htmx:load`, idempotently (`data-*` guards). JS-driven forms set `hx-boost="false"`.
  List mutations return `HX-Refresh: true`; run trigger returns `HX-Redirect`.
- **`app.js` is cache-busted** by a content hash (`?v=<hash>`) computed at startup
  (`assetVersion`), — Cloudflare caches `/static` for hours.
- **Design system (redesign PRs #30–#34, Tailwind 4.3.3):** `static/input.css` defines
  `@theme` semantic tokens (`info`/`success`/`warning`/`danger` + `-fg`, `brand-hover`,
  `card-hover`, `--radius-sm/lg/xl`, `--font-mono`, motion easings/durations) and an
  `@layer components` set (`.card`/`.card-interactive`, `.btn-primary`/`.btn-secondary`/
  `.btn-accent`, `.section-title`, `.badge`+`.badge-{info,success,warning,danger}`) plus
  `motion-safe:`-gated keyframes (`animate-enter`/`animate-fade`/`animate-live`) over a
  `prefers-reduced-motion: reduce` baseline. **Convention:** new UI uses these tokens +
  component classes, NOT raw `blue/red/emerald/amber` utilities; all motion is
  `motion-safe:`-gated; **NEVER put an entry animation on an htmx self-poll root** (it
  re-fires every 3s = a blink). Progressive disclosure uses native `<details>` accordions.
  Redesigned views: dashboard = first-class project cards (favicon/monogram + run-screenshot
  carousel + auth/status/stats), target = overview header + `<details>`-collapsed config,
  run = a professional report (exec summary → P2 changes → worst-first page cards → deeper
  analysis). Audit doc: `claudedocs/design-system-audit-2026-07-19.md`.
- **Favicon capture (#33/#34, `internal/crawler/favicon.go` `FetchFavicon`):** the favicon
  URL is attacker-influenced (`<link rel=icon>` / `<origin>/favicon.ico`), so the server-side
  Go fetch (a) passes the SAME `GuardConfig.CheckURL` before connecting, (b) dials the
  guard-validated literal IP via an **IP-pinning dialer** (`ipReasonForHost`, closes the
  DNS-rebind TOCTOU chromedp can't), (c) does NOT follow redirects (`ErrUseLastResponse` →
  3xx rejected), (d) is **raster-only** (`http.DetectContentType` — SVG/HTML rejected,
  stored-XSS avoidance), (e) caps ≤512 KiB + timeouts. Best-effort/**non-fatal** (never
  fails the crawl). Stored under the **run-scoped** key `{target_slug}/{run_id}/favicon.<ext>`
  (`storage.FaviconKey`) so the artifact proxy's per-object `GetRun` ownership scopes it.
  Migration **0059** adds `runs.favicon_key` (in `runCols`/`scanRun`; `SetRunFavicon`).
- **Auth (Supabase, invite-only):** the browser sends the access token as a
  **Bearer header** on htmx requests AND the app sets an **HttpOnly `auditloop_at`
  cookie** (mirrors the token) on `/api/auth/sync` for full-page navigations;
  `auth.Middleware` accepts **bearer OR cookie** (load-bearing — without the cookie,
  post-login redirects/refreshes bounce to `/login`). **Signup is disabled in-app**
  (admins create users directly in Supabase). Login page is sign-in only and **must
  load `app.js`**. **`DEV_MODE=true` bypasses auth** (fixed dev user) — dev/tests only.
- **Scoping is per-user:** targets/runs belong to `user_id` (`WHERE user_id=?`).
  Org-scoping is a future option (not built).
- **SSRF/abuse guard (real, tested — `internal/crawler/ssrf.go`):** before fetching
  any URL the guard refuses non-http(s) schemes, hosts outside the target's
  registered domain(s), and any host that resolves to loopback / private (RFC1918 +
  ULA) / link-local / cloud-metadata (169.254.169.254) / CGNAT ranges — **every
  resolved IP is checked** (DNS-rebinding pivot). Domain "ownership" is currently the
  registration itself (a real DNS-TXT verification is a documented seam/TODO). The
  **`CRAWL_ALLOW_LOOPBACK`** flag is a **dev/test-only** escape hatch (loopback only,
  so a hermetic e2e can crawl a local fixture) — **never set in production**; other
  private ranges stay blocked even when it's on.
- **Internal-host allowlist (`AUDITLOOP_INTERNAL_ALLOW_HOSTS`, empty by default):** a
  narrow, config-gated escape hatch so the crawler/driver/login can reach ONE (or a few)
  **EXACT-match** in-cluster hostname(s) that resolve to a private IP, for in-cluster dev
  targets ONLY. It is an **EXACT-host SOFT relax**: an allowlisted host is tolerated when
  it resolves into a **soft** range (private RFC1918/ULA, loopback, CGNAT 100.64/10). The
  **HARD-block invariant holds** — link-local, cloud metadata (169.254.169.254), multicast,
  and unspecified are **NEVER bypassable**, not even for an allowlisted host (`isHardBlocked`;
  fail-safe: any reason not explicitly soft is treated as hard). Matching is EXACT (a map
  key), NOT the subdomain-suffix `hostAllowed` logic, so a DNS-rebind of a *different* name
  onto the same private IP is still refused. It relaxes ONLY the private-IP half: the
  **same-domain `AllowedHosts` gate is unchanged** (an allowlisted host still must be in the
  target's verified domains) and **redirect hops are still re-checked** (the runtime
  `intercept.go` guard calls the same `checkHostIP`, so the allowlist composes into it and a
  redirect to metadata is still aborted). **Empty in every normal deployment ⇒ byte-for-byte
  the current fully-guarded behavior.** Config is `[]string` (`config.InternalAllowHosts`),
  built once into `GuardConfig.InternalAllowHosts map[string]bool` via
  `crawler.InternalAllowSet`, threaded exactly like `AllowLoopback`
  (worker/walkthrough/login-test/login-guard).
- **Runtime request-interception SSRF guard (real, tested — `internal/crawler/intercept.go`):**
  `CheckURL` only vets a URL *before* we hand it to Chromium; once `chromedp.Navigate`
  runs, Chromium follows HTTP **3xx redirects** to any address with nothing re-checking
  the resolved IP (and `runLogin`'s post-navigation check was host-allowlist only). That
  gap let a save-time-clean recipe (`goto http://attacker/x` → 302 → `169.254.169.254`)
  reach cloud metadata / internal hosts and — for **login-test** — exfil a screenshot of
  the metadata page. Fix: `enableInterception` turns on **Fetch domain interception**
  (`fetch.Enable()` + `EventRequestPaused`) on the crawler/login browser context and
  **guards every paused main-frame Document/navigation request** (incl. redirect hops AND
  click-triggered navigations) via `checkNav`, which enforces **BOTH the same-domain
  host-allowlist (`GuardConfig.hostAllowed`) AND the IP-safety check
  (`GuardConfig.checkHostIP`)** — in the same order as `CheckURL`. A host outside the
  target's `AllowedHosts`, or a literal private/metadata IP / a host that **resolves** into
  a private/loopback/link-local/ULA/metadata range, is `FailRequest`'d (aborted,
  `ErrorReasonAborted`) so Chromium never connects. Active in BOTH `runLogin`/`RunLoginProbe`
  (login-test) AND the main `Crawl`/`Drive` navigations. **The host-allowlist check closes
  the click-triggered off-domain-navigation gap** (Phase-4 same-origin-on-clicks
  hardening): the pre-nav `CheckURL` only ran for explicit `navigate` actions, so a CLICK
  that followed a *public* off-domain link (or an off-domain redirect hop) was previously
  only IP-guarded and escaped same-origin containment (observed live: a driven walkthrough
  went example.com → iana.org via a click). It is **DELIBERATELY Document-only** — applying
  `hostAllowed` to sub-resources would block third-party CDN/analytics/image assets and
  break page rendering. It is fail-closed at the pre-nav `CheckURL` already (empty
  `AllowedHosts` blocks everything; all three `enableInterception` callers populate it from
  the target's verified domains / base-URL host), so it adds no new regression. The
  `InternalAllowHosts` exact-host private-IP relaxation composes: an internal-allow host
  still must be in `AllowedHosts` to pass `hostAllowed`, then its private IP is tolerated by
  `checkHostIP` (the allowlist relaxes ONLY the IP half, never the same-domain gate). The
  pre-navigation `CheckURL` calls stay as defense in depth. **Screenshot suppression:** `RunLoginProbe` only captures/returns an
  end-state screenshot for a *legitimate* failure on an allowlisted, IP-clean page — if
  the runtime guard aborted a navigation, or the page ended off-domain, **no screenshot
  is captured/stored/presigned** (removes the exfil primitive). **Residual risk (honest):**
  DNS **rebinding** is still a TOCTOU race — Chromium re-resolves the host when it
  connects, so a name answering public here but private to the browser a moment later
  could slip a single request; blocking literal private IPs + resolve-and-check on each
  paused request closes the practical exploits, but full closure needs connection-level
  IP pinning (not exposed by chromedp/CDP — out of scope). `verified_domains` is still
  **user-asserted** (no real DNS-TXT verification yet — separate future item); the
  IP-level runtime guard is what closes the exploits regardless of the allowlist.
- **Auth-route hardening:** the `/auth` + `/login-test` bodies are wrapped in
  `http.MaxBytesReader` (64KiB → 413 on oversize), `recipe.ParseSteps` caps a recipe at
  `MaxSteps=50`, and `/login-test` (spawns a headless Chromium) is throttled by a
  per-`(user,target)` in-memory `minIntervalLimiter` (5s min spacing → 429).
- **Atomic run claim + startup sweep:** `db.ClaimNextQueuedRun` flips `queued→running`
  via `UPDATE … WHERE status='queued'` (rows-affected==1 wins) so two workers can't
  double-run. `db.RecoverStaleRuns` (called in `NewRouter`, like a sibling service's
  `RecoverStaleSending`) settles runs orphaned in `running` by a restart → `failed`.
  Single-replica-safe; multi-replica would need a lease/TTL.
- **DB access goes through `*db.DB` methods; no raw SQL in handlers.**

## Chromium / crawler gotchas (learned building this)

- **chromedp can hang PAST its own context deadline — now BOUNDED, not just avoided**
  (issue #41, `internal/crawler/watchdog.go`). chromedp's internal target bookkeeping
  (`Target.ensureFrame`) serialises on a plain `sync.RWMutex` that is **NOT
  context-aware**: when Chromium stops making progress, a chromedp goroutine parks on
  it forever, the surrounding `context.WithTimeout` fires with nobody observing it, and
  `chromedp.Run` never returns. **A bounded `chromedp.Navigate` was not actually
  bounded.** The known trigger is a **font-less environment** (the bare `nixos/nix` CI
  image): Chromium floods `TextRunHarfBuzz … font: ''` and page loads never settle
  (measured: no fonts → 360s TIMEOUT; with fonts → ok in 5.2s).
  **What is now bounded** — `runBounded(hard, kill, fn)` runs the work on its own
  goroutine and, when the WALL-CLOCK budget expires, **kills the browser** (the exec
  allocator's cancel → the chromium process dies) and returns `ErrBrowserStalled`. The
  kill is what eventually unblocks the parked goroutine (its CDP reader hits EOF); until
  then it is leaked, which is fine — the browser is gone and the caller has already
  failed cleanly. Applied at three granularities: **per-page** in `Crawl`
  (`NavTimeout + StallGrace`, default grace 20s), **per-session** in `Drive`
  (`OverallTimeout + StallGrace` around the whole drive loop — coarser on purpose: the
  drive is already one bounded unit of work, and wrapping ~10 inner call sites buys
  nothing), and **per-recipe** around `runLogin` in all three callers
  (`loginHardBudget`). Browser-start + `enableInterception`'s `fetch.Enable` + the
  login probe's end-URL read and end-state screenshot are wrapped too.
  **Scope, precisely (do not over-claim):** every `chromedp.Run` reachable from
  `Crawl` is wrapped. `Drive`'s ~10 inner per-action runs are NOT individually
  wrapped — they are covered by the session-level watchdog, and `safeShot` passes
  `hard=0` deliberately for that reason. A `chromedp.Run` added elsewhere in the
  process is NOT retroactively bounded: route it through `runHard`.
  **Blast radius (deliberate):** the crawl shares ONE default tab for every page (see
  the second-tab gotcha below), so killing the browser ENDS THE CRAWL. `Crawl` therefore
  treats `ErrBrowserStalled` as **terminal** and returns an actionable error instead of
  looping over a dead browser; `Drive` returns `Outcome:"failed"` with a deterministic
  reason. A failed run is the intended outcome — an indefinite hang is not.
  **Startup render probe** — every browser session (`Crawl`, `Drive`, `RunLoginProbe`)
  first navigates a probe page served from an EPHEMERAL 127.0.0.1 listener and asserts
  it settles within `ProbeTimeout` (default 15s; measured ~40ms on a healthy browser, so
  it does not meaningfully slow a crawl). Failure aborts the session with "NO USABLE
  FONTS — install fontconfig plus a font package (e.g. dejavu_fonts) and set
  `FONTCONFIG_FILE`". Opt out per call with `SkipRenderProbe`.
  **Two probe details are EMPIRICALLY derived — do not "simplify" them:** measured in
  the font-less repro container (nixos/nix + chromium 146, no fontconfig), (a) a `data:`
  URL renders in ~320ms while every **http(s)** navigation never completes, so the probe
  must go over a real HTTP navigation, not `data:`; and (b) plain text (23ms),
  `<button>` (27ms) and `<a href>` (28ms) all settle fine — **only a text `<input>`
  wedges the load event**, so the probe page MUST contain one. Earlier revisions used a
  `data:` URL and then an input-less page; both passed cleanly in exactly the
  environment the probe exists to catch.
  **Accepted trade-off:** a font-less browser can still crawl a page that has no text
  input (the e2e fixture did), and the probe now fails those too. That is deliberate —
  a browser that cannot lay out a form cannot audit a real site.
  **Still AVOIDED, not bounded:** providing fonts remains the actual fix — the Tekton CI
  pipeline realises `fontconfig`+`dejavu_fonts` and exports `FONTCONFIG_FILE` (a sibling project's
  devShell bundles them for the same reason); **do not remove that**. The watchdog turns
  a silent hang into a fast, actionable failure; it does not make a font-less container
  usable. The two gotchas below are likewise still avoided by not triggering them —
  but if either one ever fires anyway, the watchdog now bounds it.
  **Testing a hang:** `runTasks` is the `chromedp.Run` seam, held as an
  `atomic.Pointer` (NOT a plain var — the watchdog ABANDONS the stuck goroutine, which
  may still be reading the seam while a test's `t.Cleanup` restores it; a plain var is
  an unsynchronised read/write pair that `go test -race` catches and `make test` does
  NOT, since it passes no `-race`). Tests install a stub via `setRunTasks` that blocks
  on an already-held `sync.Mutex` (the exact non-context-aware shape) — see
  `watchdog_test.go`, which HANGS if the watchdog is removed. **Run
  `go test -race ./internal/crawler` when touching this file.**
  **Budgets are CAPPED, because one of them is user-supplied.** The login budget
  (`loginHardBudget`) is computed from the recipe — step count × per-step waits — so
  without a ceiling a user could extend the "a stall is BOUNDED" guarantee to hours
  (`timeout_ms: 3600000` → days) and, since the crawl worker loop is single-threaded
  with no run-level deadline, **pin the whole worker** for that budget. Two bounds:
  `recipe.MaxWaitTimeoutMs` (2 min) rejects the input at validation, and
  `crawler.MaxLoginHardBudget` (5 min) clamps the computed total as defence in depth.
  **Diagnosis is unified:** `crawler.StallHint` + `stallError()` are the ONE message
  every path reports (crawl capture, crawl login, drive, login probe), so an infra
  stall is never dressed up as "login recipe failed — check selectors/credentials".
  Anything persisting a driver error must keep it long enough to survive
  (`walkthrough.maxDriverErr` = 600; the probe message is ~344 chars with the REMEDY at
  the end, so the old 200-char truncation silently discarded the actionable half).
  Observability: **`auditloop_browser_stalls_total{phase}`** (`probe|capture|login|
  drive|start`) — any non-zero value means runs are failing on infrastructure, not on
  the audited site.
  **⚠️ CI BLAST RADIUS — a stall USED to look like a product regression; FIXED in #45
  by a STRUCTURAL flag, never a substring match on the reason prose.** A `Drive` stall
  yields `Outcome:"failed"` with `Steps: nil`, so zero steps are persisted and
  `RefreshWalkthroughDiff` saw `success → failed` = a rank drop → `IsRegression` fired,
  `auditloop_walkthrough_regressions_total` bumped, and a CI `--fail-on-regression` gate
  tripped on an INFRASTRUCTURE stall indistinguishable from a real product regression.
  Now the driver reports **`DriveTrace.InfraFailed`** (set at the session-watchdog return
  and the login-phase `ErrBrowserStalled` branch — NOT on `login recipe failed`, which is
  a real product-side failure) plus the sentinel **`crawler.ErrDriverInfra`** on the
  out-of-band `(nil, err)` paths (browser start, render probe, enable interception), with
  **`crawler.IsInfraFailure(err)`** the one predicate. The generator persists it to
  `walkthroughs.infra_failed`; `ComputeDiff` then sets `outcome_compared:false` +
  `infra_failed:true` and forces `is_regression`/`resolved`/`stuck_step_delta` off, so the
  metric never bumps and the gate never trips. See the Phase-4 section for the full
  semantics. **What is still NOT covered:** (a) any steps driven BEFORE a session stall are
  DISCARDED (the stalled goroutine is abandoned; reading its trace would be a data race —
  do not "recover" them); (b) 🔴 **a genuinely broken PAGE that wedges the load event stalls
  the browser too, so it is now classified as infra and its real regression is silently
  suppressed** — the deliberate mirror-risk of this fix, with
  `auditloop_browser_stalls_total{phase="drive"}` the only discriminator (a stall
  correlating with a deploy of the AUDITED app, not with CI-host churn, is probably the
  page); (c) consumers MUST adopt the new gate predicate — see the Phase-4 read-API
  section — because an unpatched gate now SILENTLY PASSES an infra failure instead of
  noisily failing it.
- **Do NOT open a second tab** via `chromedp.NewContext(browserCtx)` for captures —
  under chromium 149 a `captureBeyondViewport` screenshot on a non-default tab
  **intermittently hangs** after a few captures (reproduced deterministically). The
  crawler uses the browser's **default tab** for the whole crawl, one shared listener
  routing CDP events to the current page's collector.
- **Do NOT use `chromedp.FullScreenshot`** combined with viewport emulation — it hangs
  on the 2nd+ capture. Use `emulation.SetDeviceMetricsOverride` +
  `page.CaptureScreenshot().WithCaptureBeyondViewport(true)` (see `crawler.go`
  `fullPageScreenshot`).
- Each page capture runs under a **per-page timeout** derived from the shared tab
  context, so a hang on one page cancels only that page — the browser survives.

## Build / run / test

```bash
npm install                          # once: Tailwind v4 CLI + vendored axe-core (npx is broken on nixos)
make css                             # build static/output.css
make dev                             # DEV_MODE=true, role=all, CRAWL_ALLOW_LOOPBACK, :8112
make build && make run               # production-style
make test                            # go test ./... (hermetic: fs storage + sqlite; e2e needs chromium)
make test-e2e                        # just the browser e2e (fixture site + real chromium)
make test-docker                     # bring up Postgres+MinIO (docker-compose.test.yml) → S3/PG integration tests
```

`go test ./...` is **fully hermetic** — the mandatory e2e uses the **filesystem
storage backend + sqlite** and a local fixture HTTP server, so it needs no docker.
The real S3/Postgres paths are exercised by `TestS3RoundTrip` (gated on `S3_ENDPOINT`)
and the Postgres query tests via `make test-docker`.

## nixos gotchas

- **`npx` is broken** — call `./node_modules/.bin/tailwindcss` directly.
- **`make` may not be on PATH** — run the underlying commands (see the Makefile).
- **Chromium won't download** — use the nix-provided binary via `AUDITLOOP_CHROMIUM`
  (or it's found on PATH). Never `playwright install`.
- Use `/usr/bin/env bash` shebangs; `git add` files individually (never `-A`).

## Key env vars

`PORT` (8112), `BASE_URL`, `AUDITLOOP_ROLE` (web|worker|all), `AUDITLOOP_CHROMIUM`,
`DATABASE_DRIVER` (sqlite|postgres), `DATABASE_PATH` (sqlite) / `DATABASE_URL` (pg DSN),
`SUPABASE_URL`, `SUPABASE_ANON_KEY`, `SUPABASE_JWT_SECRET`,
`S3_ENDPOINT` (unset → filesystem backend), `S3_BUCKET` (audit-artifacts),
`S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_REGION`, `S3_USE_PATH_STYLE`, `S3_USE_SSL`, `S3_LOCAL_DIR`,
`CRAWL_MAX_PAGES` (50), `CRAWL_MAX_DEPTH` (3), `CRAWL_ALLOW_LOOPBACK` (dev/test only),
`AUDITLOOP_INTERNAL_ALLOW_HOSTS` (comma-separated EXACT-match hostnames whose resolution
into a private/loopback/CGNAT range is tolerated — in-cluster dev targets ONLY; NEVER
relaxes link-local/metadata/multicast/unspecified; empty by default = fully guarded),
`DEV_MODE` (bypass auth — dev/test only).

**P4 login recipes:** `AUDITLOOP_ENCRYPTION_KEY` (**server-side only, never sent to
the browser**) — a 32-byte AES-256 key as hex (64 chars) or base64. **Required for
the login-recipe feature**: without it the Authentication UI is hidden and the
`/auth` + `/login-test` routes return 503 (crawls still run unauthenticated). Mint
one with `crypto.GenerateKey()` (hex) or `openssl rand -hex 32`. **Wire it into the
prod secret at deploy** (the interactive session handles this) — the code/tests
generate/use a throwaway test key and never require the prod one. Same-domain form
login only (every `goto` URL is enforced within the target's verified domains via
the SSRF guard); third-party SSO is unsupported. Credentials are write-only in the
UI and redacted from logs/errors/report.json.

**P3 vision-LLM UX notes:** `OPENROUTER_API_KEY` (**server-side only, never sent to
the browser**; the whole feature is gated on it — button hidden + route 503 without
it), `OPENROUTER_BASE_URL` (default `https://openrouter.ai/api/v1`; overridable so
tests point it at an httptest fake), `AUDITLOOP_LLM_MODELS` (comma-separated curated
vision-model allowlist, default `anthropic/claude-haiku-4.5,anthropic/claude-sonnet-4.6`;
the FIRST is default-checked; server rejects any model id outside this list),
`AUDITLOOP_LLM_MAX_TOKENS` (default 1024, per-page/per-notes completion cap),
`AUDITLOOP_LLM_SYNTH_MAX_TOKENS` (default 3000) — the LARGEST completion budget for the
run-level persona-walkthrough SYNTHESIS call ONLY (its ranked ≤8-item JSON overflows the
1024 per-page cap and truncates mid-JSON); applied per-call via `llm.WithMaxTokens`, so it
does NOT inflate the per-page calls. `AUDITLOOP_LLM_EVAL_MAX_TOKENS` (default 2000) — the
middle-tier budget for the per-page persona-walkthrough GENERATION + VERIFICATION calls (a
verbose per-page verdict overflows the 1024 cap and truncates → `ParseEvaluation` fails
"unexpected EOF" + lost findings); also per-call via `llm.WithMaxTokens`. **Three tiers:
notes 1024 (`AUDITLOOP_LLM_MAX_TOKENS`) < eval gen/verify 2000 < synth 3000.** The
**persona walkthrough** (Phase 1) reuses the SAME
`OPENROUTER_API_KEY` + config, running on the FIRST curated model
(`AUDITLOOP_LLM_MODELS[0]`) across all four personas (personas — not models — are the
axis); no new REQUIRED env var. The **goal-directed walkthrough driver** (Phase 3)
adds `AUDITLOOP_LLM_DRIVE_MAX_TOKENS` (default 256) — the SMALL per-turn planner
completion budget (one action JSON) — reusing `OPENROUTER_API_KEY` +
`AUDITLOOP_LLM_MODELS[0]` on a worker-running role (it drives chromedp). No new
REQUIRED secret; the driver is default-OFF per target (`driving_enabled`) with a
default dry-run submit-guard (`allow_real_submit` is a separate, loud default-off flag).

**Read API (machine pulls):** no new SERVER secret — per-user read-only keys live
in the existing DB (sha256-hashed). A CONSUMER (agent/CLI) reads its key from
**`AUDITLOOP_API_TOKEN`** and sends it as `Authorization: Bearer $AUDITLOOP_API_TOKEN`
to the `/api/audit/*` read routes. Keys are minted in-app (dashboard → "API access")
and shown once; rotate = revoke + create. Per-key rate limit is 10 req/s (burst 20).

## Deployment (SEPARATE SLICE — not done here)

Deployment manifests are NOT part of this repo. The shape the app expects: a
Kubernetes Deployment (or any container runtime), a GoTrue/Supabase instance for
auth, an S3-compatible bucket (`audit-artifacts`), Postgres, and a Prometheus
scrape of `/metrics`. Health probes: `/healthz` (liveness), `/readyz` (readiness
— DB + storage). The container listens on **8112**. See the Dockerfile for the
runtime image (it bundles Chromium for the worker role).
```
