# Scoping Doc: DOM/a11y-grounding for the auditloop persona evaluator (2026-07-24)

**Status:** design + plan (no code written). Read-only analysis of this repo.
Follows an internal meta-audit that graded the evaluator's output against a real
application's source (~20% precision on objective a11y/semantic claims vs. ~80% on
visible-UX; the objective-FP cluster traces to screenshot-only perception). Primary deliverable: rec #1 — feed semantic
structure (DOM/a11y digest + axe detail) to the evaluator. Recs #3 (DOM-grounded verify) and
#2 (route mechanical a11y to axe) compose as follow-ons.

## 1. Current-state map (capture → storage → eval prompt)

### 1a. Crawl path
- Capture: `internal/crawler/crawler.go:300-324` `capturePage` (default tab): device metrics → Navigate → inject `axeSource` (:321) → `axeRunScript` (:322) → `fullPageScreenshot` (:323, `captureBeyondViewport` — the chromium-149 second-tab-hang workaround). axe keeps the FULL violations payload incl. up to 5 nodes each with `target` (selector) + `html` (`axe.go:18-33`). **No DOM outerHTML, no a11y tree captured.**
- Storage: screenshot→`ScreenshotKey`, axe.json→`AxeKey`, findings inserted with the raw axe violation as `Detail` (`worker.go:260-267,313,360-364`; keys `storage/keys.go:47-59`).
- DB: `Page` (`db/models.go:245-268`) has ScreenshotKey/AxeKey/counts; **no DOM/a11y digest column**.
- Eval prompt: `internal/eval/generator.go:310-412` `plan()`/`loadImages` fetches **screenshots only**; a11y grounding = **rule-id list + count only** (`generator.go:347,360-372`) — the axe node `target`/`html` (already in DB) is **discarded**. `Grounding` `prompt.go:162-179`; `GenUserPrompt` renders a11y as "N axe violation(s) [rules: …]" (:205-213). System prompt says "Reason from what is VISIBLE" (:89-117). Verify re-reads the **same screenshots** (:122-135) — why verify dropped none of the FPs. Budgets: `DefaultEvalMaxTokens=2000` (completion), synth 3000; screenshots downscaled ≤1568px.

### 1b. Driven-trace path (walkthrough → synthetic run → eval, PR-B)
- Capture: `driver.go:227-353` `Drive` loop per step: `safeShot` (:247) + `buildInteractiveDigest` (:248) — the digest (interactive-digest.js: ≤40 visible interactive els with `tag,role,name,selector,type,disabled`) is **built for the planner but never persisted** (`StepRecord` :54-61 = screenshot only). **axe never runs during driving.**
- Storage/DB: walkthrough persists **screenshot_key only** (`walkthrough/generator.go:156-179`).
- Materialization: `db.MaterializeWalkthroughRun` (`db/walkthrough.go:217`) reuses each step's screenshot_key; **no axe/findings** → driven eval grounding says "no axe violations detected" (actively misleading — a11y was never measured). **No re-capture later → any signal must be captured DURING the drive.**

## 2. Proposed design

**2a. Capture a compacted a11y/DOM digest** (vendored JS eval, not raw outerHTML, not full AX tree):
1. Interactive elements (extend `interactive-digest.js`): `tag,role,accessibleName,selector,type,disabled` + `focusable` + for controls `hasLabel`/`labelSource` (`for`/`aria-label`/`aria-labelledby`/wrapping `<label>`/`placeholder`).
2. Form-control↔label associations: each control's resolved accessible name + derivation (or `none`) — the positive fact axe **cannot** give (a present `sr-only` label is a non-violation, never in axe output).
3. Landmark/heading skeleton: ordered landmark roles + `h1..hN` text (≤~30, truncated).
Plus (crawl path) surface the **already-stored axe node selectors** into grounding (zero new capture).
Reject `getFullAXTree` (too verbose → budget) — custom digest gives the decision-relevant facts cheaply and reuses shipped infra; note it as fallback if JS accessible-name proves lossy. Reject raw outerHTML (50–500KB).

**2b. When:** crawl path — one `chromedp.Evaluate(a11yDigestScript)` alongside the axe eval in `capturePage` (same tab, marginal cost). Driven path — persist the **already-computed** `buildInteractiveDigest` per step (cheap); axe-per-step is expensive (560KB inject × ≤20 steps) → fuller build, not MVP.

**2c. Storage:** crawl — new artifact `…/{page_slug}/a11y.json` (`storage.A11yDigestKey`, mirrors AxeKey) + `pages.a11y_digest_key TEXT` (migration `0060`), ~16KB cap. Driven — `walkthrough_steps.digest_json TEXT` (`0061`); materialization writes the step digest to `A11yDigestKey` so both paths converge on one eval read path.

**2d. Prompt:** add `A11yDigest` to `Grounding`; new "SEMANTIC STRUCTURE (from DOM/a11y tree — authoritative for label/role/focus)" block in `GenUserPrompt`, e.g. `input#campaign-name-input — role=textbox, accessible name "Campaign name" (via <label for>), focusable`; `a.client-card — role=link, focusable`. `plan()` loads the digest per page (one `Store.Get`). Backward-compat: nil/omitempty → no semantic block, screenshot-only behavior for pre-0060 runs.

**2e. Token budget (correction):** `EVAL_MAX_TOKENS=2000` is the **completion** cap — the digest doesn't touch it. Bounded digest ≈ 1–3KB ≈ 500–900 **input** tokens, small next to two ≤1568px screenshots. No completion-budget impact; modest prompt-cost bump. Real cost is capture (§9), not tokens.

## 3. How it fixes each measured FP
| FP | Root cause | Signal that prevents it |
|---|---|---|
| #11 "no `<label>`" (sr-only label shipped) | can't see the element | label-association digest (accessible name via `<label for>`). **axe alone won't help — no violation exists; positive DOM fact required.** |
| #1 cards "not keyboard-operable" (real `<a>`) | can't see the tag | interactive digest `tag=a, role=link, focusable` (already computed by interactive-digest.js, just never fed to eval) |
| #13 "add aria-label" (name already present) | can't compute accessible name | digest `accessibleName` + source |
| #12 "add autofocus/Enter" (exist via JS) | runtime behavior invisible | **partial** — digest can record `document.activeElement` + presence of native submit button; full "Enter submits" needs behavioral capture |
| #17 "no post-submit confirmation" (HX-Redirect exists) | transition unobserved | **out of rec #1 scope** — driver action-trace (rec #5); cheap adjacent win on driven path |

Directional only (needs a re-run to measure): removing the source signal for the high-volume label/role/name fires + the deterministic verify (§5) is what should move objective precision off ~20%.

## 4. Prompt narrowing (rec #2 composition)
Add to `GenSystemPrompt`: the digest is AUTHORITATIVE for label/role/focus/keyboard presence; MUST NOT assert a label/ARIA name/role/affordance is missing from the screenshot — defer to the digest; mechanical a11y is measured by axe, don't re-derive from pixels; remit = LIVED experience (comprehension, ordering, wait-state anxiety, primary-action discoverability). This narrows `accessibility-constrained` (`eval.go:56-59`) — rec #2 via prompt+grounding, no separate routing engine in phase 1.

## 5. Rec #3 — DOM-grounded Verify (highest precision-per-line)
Today verify re-reads the same screenshots (`prompt.go:122-135`). Upgrade: (1) feed the digest to verify too; (2) **deterministic pre-LLM selector check** in `applyVerification`/`evalCell` (`generator.go:198-207`): for each finding `Selector` (model-authored, `report.go:96-101`), look up in the digest —
```
el, ok := digest.bySelector(f.Selector)
if !ok { drop("selector-not-in-DOM") }
if claimsMissingLabel(f) && el.hasAccessibleName { drop }
if claimsNotOperable(f) && (el.focusable || el.tag in {a,button}) { drop }
```
Drops #1/#11/#12/#13 **without a model call** — cheaper and more reliable than LLM verify. Keep LLM verify for subjective residue.

## 6. Phasing
**Phase 1 (MVP, PR 1):** crawl path — interactive+label digest JS, capture in `capturePage`, store `a11y.json`+`pages.a11y_digest_key` (`0060`); plumb already-stored axe node selectors into grounding; feed digest into gen+verify + system-prompt "defer to digest"; deterministic selector-check verify (§5.2). Skip driven capture, axe-per-step, routing engine. This is where most evals run and the axe half is near-zero cost.
**Phase 2 (PR 2):** driven path — persist `buildInteractiveDigest` per step, carry through `MaterializeWalkthroughRun`; optional axe-per-step (latency-gated, measure first); feed observed action-trace/post-submit URL into driven grounding (rec #5, kills #17); landmark/heading section.

## 7. Migrations / storage / report.json / compat
- Migrations `0060` `pages.a11y_digest_key TEXT DEFAULT ''`; `0061` `walkthrough_steps.digest_json TEXT DEFAULT ''` (dual-dialect, inline `migrations.go`, latest is 0059). Add to shared `pageCols`/`scanPage` + walkthrough scan.
- Storage `A11yDigestKey(...)` → `…/{page_slug}/a11y.json` (mirrors AxeKey), served via existing authed proxy / per-object ownership.
- report.json: `PageReport` gains optional `a11y_digest`/`_key`, omitempty (like the P3 notes seam) — additive.
- Backward-compat (load-bearing): eval degrades to screenshot-only when the key is empty (all pre-0060 + pushed runs). Plugin push digest is out of scope.

## 8. Tests
- `internal/crawler`: digest JS on a loopback fixture (chromium-gated) — `<a>` card → `tag=a,focusable`; `sr-only <label for>` input → resolved name + `labelSource=for`; aria-labelled button → name.
- `internal/eval`: digest compaction/bounding; prompts contain the SEMANTIC block with a digest, omit it when nil (compat); deterministic verify drops "no label"/"not operable" findings contradicted by the digest.
- `internal/db`: migration on sqlite; `a11y_digest_key` round-trip; `MaterializeWalkthroughRun` carries `digest_json` (phase 2).
- e2e: extend a fixture with an `sr-only`-labelled input + an `<a>`-card → `TestEndToEndPersonaEvaluation` fake-OpenRouter asserts the semantic block is in the prompt + the deterministic verify drops seeded FPs. (Model precision can't be asserted hermetically — assert plumbing + deterministic drop.)

## 9. Effort / risk
Phase 1 ≈ one focused PR; Phase 2 ≈ smaller PR. No new deps (vendored JS like axe). Top risks: (1) driven-path capture latency (axe-per-step 560KB×≤20 — MVP persists the free already-computed digest; axe-per-step opt-in + measured); (2) token/context if digest unbounded (hard caps ≤40/≤30/≤30, ~16KB, enforced JS+server); (3) chromedp default-tab constraint (one extra `Evaluate` in the existing task list is safe; no second tab / no `FullScreenshot`); (4) JS accessible-name is a heuristic (adequate for in-scope FPs; `getFullAXTree` fallback).

## 10. Recommendation
Build **Phase 1 first, prioritizing the deterministic verify selector-check (§5.2)** — the meta-audit's decisive finding is architectural (the tool can't confirm the invisible fixes it recommends; LLM verify re-reading pixels is structurally incapable). A deterministic DOM-grounded verify drops #1/#11/#12/#13 with **no model call** — highest-confidence, lowest-risk lever. The crawl-path digest is the enabling substrate and reuses infra that already exists (axe.json detail already stored; interactive-digest.js already computes the fields). Defer driven-path capture + axe-per-step to Phase 2 (real latency risk; crawl path is where the measured failures occurred).

**Honest limits:** the FP→signal mapping is derived from code + meta-audit, not a re-run; JS accessible-name fidelity vs. `getFullAXTree` untested; plugin-push digest deliberately out of scope.

Key files: `internal/crawler/crawler.go:300-324` · `axe.go:18-33` · `interactive-digest.js` · `driver.go:227-263` · `internal/eval/generator.go:310-412` · `prompt.go:89-135,162-230` · `internal/worker/worker.go:260-267,360-364` · `internal/storage/keys.go:47-59` · `internal/db/models.go:245-278` · `internal/db/walkthrough.go:217` · `internal/report/report.go:85-110` · `internal/db/migrations.go`.
