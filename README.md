# auditloop

**A self-hostable, crawler-based UX auditor.** Point it at a site you own; it crawls
same-origin at two viewports, captures full-page screenshots, runs axe-core accessibility
scans, classifies console/network errors as first- vs third-party, diffs every run against
the previous one, and — optionally — has a vision LLM walk the product as a named persona
and report what blocked them.

One Go binary. Postgres or SQLite. S3 or the local filesystem. No Node runtime in
production.

```
┌──────────┐   crawl    ┌───────────┐   artifacts   ┌────────────┐
│  target  │ ─────────▶ │  chromedp │ ────────────▶ │  S3 / disk │
└──────────┘  BFS, 2vp  │  + axe    │  png/json     └────────────┘
                        └─────┬─────┘                      ▲
                              │ metadata                   │ presigned / proxied
                              ▼                            │
                        ┌───────────┐   diff vs prev  ┌────┴─────┐
                        │ Postgres  │ ──────────────▶ │  PWA UI  │
                        │ / SQLite  │   + LLM passes  │  + API   │
                        └───────────┘                 └──────────┘
```

---

## What it does

**Crawl & capture.** BFS over a target's own origin (depth- and page-capped), at 390px
mobile and 1440px desktop. Per page: a full-page screenshot (`captureBeyondViewport`), an
axe-core scan (vendored, injected per page), origin-classified console + network errors,
load timing, web-vitals-style perf signals, and layout smells (horizontal overflow, small
tap targets, missing viewport meta).

**Regression diffing.** Every run is compared to the target's previous completed run:
page-set delta, per-page pixel diff with a red-tint overlay, axe *rule-set* delta (new vs
resolved rules — not just counts), and console/network deltas. Full-page captures make a
naive pixel diff meaningless when page height changes, so a *visual regression* requires
unchanged dimensions **and** ≥1% changed pixels; a height change is reported separately as
a layout change.

**Persona walkthroughs.** An opt-in LLM pass evaluates a run as a task-directed walkthrough
by four code-defined personas (first-time non-technical, returning power user, skeptical
evaluator, accessibility-constrained), emitting structured, selector-anchored findings plus
a ranked run-level synthesis. A second verification call drops unsubstantiated findings.

**DOM grounding — the part that makes those findings trustworthy.** An LLM reasoning from
screenshots re-derives accessibility from pixels and invents false positives (an `sr-only`
label it cannot see; an `<a>` styled as a card). auditloop captures a bounded DOM/a11y
digest per page and runs a **deterministic, no-LLM gate** over the model's output: a
mechanical a11y claim citing a selector the digest does not list is re-anchored or dropped,
and a claim the digest positively refutes is dropped. Subjective UX findings — the part the
model is actually good at — are never touched. The gate's activity is exported as a metric
so you can tell whether it is load-bearing or decorative.

**Goal-directed driving.** Optionally, a planner LLM *drives* the site toward a stated goal
using a closed action set (click/type/press/select/scroll/waitFor/navigate — deliberately
**no** eval/script action), producing a deterministic success/stuck signal and an ordered,
screenshotted step trace that the personas can then critique. Default-off per target, with
a default-on dry-run guard that aborts non-GET requests at the network layer.

**Push ingestion.** An external harness can POST a completed run's artifacts to the plugin
API instead of being crawled; pushed runs land in the same dashboard and get the same
diffing. A reference uploader CLI ships in `cmd/auditloop-push`.

**Read API.** Per-user, read-only, rotatable API keys (`Authorization: Bearer …`) expose the
run list, `report.json`, artifacts and persona evaluations — so CI can gate on a regression.

---

## Quick start

Requirements: Go 1.26+, Chromium (system-installed), Node only to build CSS.

```bash
npm install          # Tailwind CLI + vendored axe-core (once)
make dev             # DEV_MODE, web+worker, http://localhost:8112
```

`make dev` bypasses auth with a fixed dev user, uses SQLite and local-filesystem storage,
and allows crawling loopback so you can point it at something on your own machine. Nothing
external is required — no S3, no Postgres, no API keys.

Production-style:

```bash
make build && ./bin/auditloop
```

Or use the Dockerfile, which bundles Chromium in the runtime image.

### Tests

```bash
make test            # fully hermetic: SQLite + filesystem storage + a local fixture site
make test-e2e        # the browser e2e (needs a real Chromium)
make test-docker     # brings up Postgres + MinIO, runs the real-backend integration tests
```

`go test ./...` needs no Docker and no network. The e2e suites use fixture HTTP servers and
a fake OpenRouter endpoint — never a real API key.

Secret scanning: `gitleaks detect --source . --redact` should report **no leaks**.
[`.gitleaks.toml`](.gitleaks.toml) allowlists a handful of test fixtures by exact value or
path — narrowly, so the scanner still fires on anything new. If you widen it, plant a
realistic fake credential in a tracked file first and confirm gitleaks still goes red.

---

## Configuration

Everything is env-driven. The defaults are the local-dev path.

| variable | default | meaning |
|---|---|---|
| `PORT` / `BASE_URL` | `8112` / `http://localhost:8112` | listen port, public base URL |
| `AUDITLOOP_ROLE` | `all` | `web`, `worker`, or `all` — same binary, two roles |
| `AUDITLOOP_CHROMIUM` | auto-detected | path to Chromium (else `chromium`/`chrome` on PATH) |
| `DATABASE_DRIVER` | `sqlite` | `sqlite` or `postgres` |
| `DATABASE_PATH` / `DATABASE_URL` | `auditloop.db` | SQLite path / Postgres DSN |
| `S3_ENDPOINT` | *(unset)* | unset ⇒ filesystem storage backend |
| `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` | `audit-artifacts` | S3/MinIO credentials |
| `SUPABASE_URL` / `SUPABASE_ANON_KEY` / `SUPABASE_JWT_SECRET` | — | auth (invite-only; signup is disabled in-app) |
| `DEV_MODE` | `false` | **bypasses auth** — local dev and tests only |
| `CRAWL_MAX_PAGES` / `CRAWL_MAX_DEPTH` | `50` / `3` | crawl caps |
| `CRAWL_ALLOW_LOOPBACK` | `false` | **dev/test only** — lets the crawler reach 127.0.0.1 |
| `OPENROUTER_API_KEY` | *(unset)* | enables all LLM features; **server-side only** |
| `AUDITLOOP_LLM_MODELS` | two Claude vision models | curated allowlist; the server rejects anything outside it |
| `AUDITLOOP_ENCRYPTION_KEY` | *(unset)* | 32-byte AES key (hex/base64) — enables encrypted login recipes |

Every LLM feature is **opt-in and no-ops without a key**: the buttons are hidden and the
routes return 503. auditloop never sends an API key to the browser.

---

## Security model

This crawler fetches attacker-influenced URLs, so the guards are the product:

- **SSRF guard.** Before any fetch: no non-HTTP(S) schemes, no hosts outside the target's
  registered domains, and **every resolved IP** is checked against loopback / RFC1918 / ULA
  / link-local / CGNAT / cloud-metadata ranges — so a DNS-rebinding pivot fails too.
- **Runtime request interception.** `CheckURL` only vets a URL before handing it to
  Chromium; HTTP 3xx redirects and click-triggered navigations would escape it. Every
  paused main-frame navigation is re-checked against both the host allowlist and the IP
  guard, and aborted at the network layer if it fails.
- **No arbitrary JS anywhere a model or a user can reach.** Login recipes and driver
  actions are closed, typed sets with no `eval`/`script` member; unknown JSON fields are
  rejected. Model-authored selectors round-trip only through Chromium's selector engine.
- **Credentials** for authenticated crawls are AES-256-GCM encrypted at rest, write-only in
  the UI, and redacted from logs, errors and `report.json`.
- **Tokens** (plugin push, read API) are 32 bytes of `crypto/rand`; only their SHA-256 is
  stored, comparison is constant-time, and they are shown exactly once.
- **Per-object ownership** is enforced on artifact reads — being authenticated is not
  enough; the run must belong to you, or you get a 404 (existence is not leaked).

`CLAUDE.md` documents the residual risks honestly, including the ones that are still open
(DNS-rebinding remains a TOCTOU race without connection-level IP pinning; domain ownership
is user-asserted, not DNS-TXT verified; there is deliberately no CSP because htmx plus
inline scripts would break under a strict one; rate limits are in-memory and therefore
single-replica). Read that before hosting this for anyone but yourself.

**Only audit sites you own or are authorized to test.**

---

## Documentation

- **[`CLAUDE.md`](CLAUDE.md)** — the real design document: every subsystem, the reasoning
  behind each decision, the measured gotchas, and the contracts (`report.json`, the push
  schema, the DOM digest a producer must emit). It is written as instructions for an AI
  coding agent working in this repo, which makes it unusually explicit about *why* things
  are the way they are. Start here if you intend to change anything.
- **[`claudedocs/`](claudedocs/)** — design-system audit, evaluator scoping notes, and a
  meta-run analysis of the tool's own UX.
- **[`tests/e2e/ux-audit/`](tests/e2e/ux-audit/)** — the self-audit harness: auditloop boots
  a local copy of itself, walks its own UI, and can push the result back into itself.

## Project layout

```
main.go              entrypoint — opens DB + store, builds the router, starts the worker
internal/crawler     chromedp BFS crawler, SSRF guard, axe injection, login, the driver
internal/eval        persona walkthrough: prompts, structured parse, the DOM grounding gate
internal/notes       the per-page vision-LLM notes pass
internal/walkthrough goal-directed driving + walkthrough-vs-walkthrough regression
internal/plugin      push ingestion: token, schema, validation, the uploader library
internal/diff        pure-Go pixel diff (stdlib image only)
internal/db          SQLite/Postgres query layer + inline migrations
internal/storage     S3 (minio-go) and filesystem backends behind one interface
handlers             routes: pages, APIs, the artifact proxy, health, metrics
components           server-rendered gomponents views (htmx 2 + Tailwind 4)
cmd/auditloop-push   reference uploader CLI for the push API
```

## Status

Working software, run in production by its author, but young and shaped by one person's
needs. Interfaces may move. Issues and PRs welcome.

CI currently runs outside this repository, so pushes here show no checks; the authoritative
gate is `go build ./... && go vet ./... && gofmt -l . && go test ./...`, which is what a
contributor should run locally.

## License

MIT — see [`LICENSE`](LICENSE). Vendored axe-core is MPL-2.0; see [`NOTICE`](NOTICE).
