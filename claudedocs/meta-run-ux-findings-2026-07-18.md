# auditloop meta-run — UX findings (2026-07-18)

auditloop auditing its own UI. Loop: `make ux-audit` captured 9 views → pushed as a
plugin run into a local DEV_MODE instance wired with the real OpenRouter key →
auditloop's own **persona evaluator** (`first-time-nontechnical` + `skeptical-evaluator`,
job "First-time user: understand what this tool does and run my first audit") + **Draft
UX notes** (claude-haiku-4.5) ran over it.

- Pushed run: `35452c0d-ba51-46e0-96c3-ccadbee9f4b7` (trigger=plugin, done, 9 views)
- Eval: done, 19/19 units, 18/18 cells clean, synthesis 8 items. Notes: 9/9 clean.
- Cost: eval $0.2394 + notes $0.0378 = **$0.277**.
- Artifacts left at `$SP/meta.db` + `$SP/store` (scratchpad).

## Signal vs. meta-run artifact (honest filter)

**Discount these — artifacts of the capture setup or a persona/context mismatch:**
- **"login blocked"** — driven by (a) the invite-only wall (a deliberate product
  decision, not a UI bug) and (b) the DEV_MODE auth-bypass banner, which only appears
  because the capture instance ran in DEV_MODE — prod login has no such banner. Inflated.
- **skeptic's "no pricing / testimonials / social proof / trial CTA"** — a
  marketing-landing-page critique. auditloop is an invite-only internal tool, not a
  public SaaS funnel; this persona partly mismatches the context. Low relevance.

**Real, in-scope, corroborated (this is the "information overload / unintuitive" the
user reported — confirmed by the personas, the vision notes, AND direct screenshot review):**

## Ranked fixes

### 1. Dashboard: three equal-weight setup cards bury the actual content (HIGH)
`/dashboard` stacks *Add a target* + *Add a plugin target (push-only)* + *API access
(read-only)*, each full-width with a paragraph of jargon, above the **Targets list** —
the thing a returning user came for. No hierarchy, no recommended starting point.
- Fix: collapse creation behind one primary **"＋ New target"** affordance (the 3 types
  as a choice inside a disclosure/modal); make the Targets list the primary content;
  defer the plugin/API cards until after a first audit (or move to a settings area).

### 2. Target page: a wall of login-recipe fields you didn't ask for (HIGH)
`/targets/{id}` renders the **Authentication** card fully expanded — 8 selector inputs
(login URL, submit/username/password selectors, success selector/URL, timeout) +
guided/advanced toggle + credentials — **even though "No authentication" is selected.**
The one thing a first-timer wants ("Run audit") competes with a login-recipe form.
- Fix: hide the guided form until the "Login recipe" radio is chosen; collapse into an
  "Advanced: add login" disclosure; keep "Run audit" as the clear primary action at top.
  Same treatment for the Audit-configuration card.

### 3. Results pages read as a data dump, no plain-language summary (HIGH)
`run-done` / `run-diff` lead with raw metrics/jargon ("LCP 24ms CLS 0.000 TBT 0ms",
"a11y violations", "first-party network") and cramped thumbnails, with no "what was
audited / did it pass / what now." "First run — no baseline" emphasizes a *limitation*
over the findings that exist. Empty Persona/AI-notes sections read as broken features.
- Fix: a plain-language summary box at the top (what was audited, finding counts by
  category with severity color, one clear next step); define/annotate the metric jargon
  (tooltip or inline legend); soften the empty-state copy.

### 4. Undefined jargon throughout (MED)
"plugin target", "push token", "a11y", "LCP/CLS/TBT", "Bearer token", "first-party
network", "external harness / CI / ux-audit loop" used with no definition across most views.
- Fix: plain-language microcopy + tooltips; a one-line "what is an audit?" explainer.

### 5. Weak visual hierarchy / primary-CTA distinction (MED — vision notes)
Cards all share the same dark styling; "Run audit" competes visually with
"Save authentication" / "Create…" (all same blue/purple, right-aligned). Placeholder
text blends into the dark background and reads as pre-filled content. run-inprogress is
~90% empty and reads as a broken/loading state.
- Fix: one visually-distinct primary CTA per page; lighten/italicize placeholders for
  contrast; add progress/polling feedback (+ ETA) to the in-progress state.

### 6. Deterministic a11y defects (MED — from the axe layer, independent of the LLM)
- token-reveal `color-contrast` **2.44:1** (`.text-brand #4f46e5` on `#0f2830`, needs ≥4.5).
- token-reveal: a **critical unlabeled form field** (`label`).
- login: missing `main` landmark + content outside landmarks (`region`).

## Note also surfaced (small correctness bug)
Both personas independently flagged that the plugin-target page shows
**"No runs yet. Click 'Run audit'"** — but a plugin (push-only) target has **no
"Run audit" button** by design. The empty-state copy points at a nonexistent control.
Fix the copy for plugin targets ("This target receives pushed runs — see the push
instructions above").
