# auditloop — Design-System Audit & Foundation (2026-07-19)

Read-only audit ahead of the target / run / dashboard redesign. Establishes the shared
Tailwind v4 token + component + motion foundation the three redesigns build on.

---

## 1. Version verdict

| | Version |
|---|---|
| `package.json` pin | `^4.3.0` (both `tailwindcss` and `@tailwindcss/cli`) |
| Actually installed | **4.3.2** (`node_modules/tailwindcss`, CLI reports `v4.3.2`) |
| Latest stable on npm | **4.3.3** (`dist-tags.latest`) |
| Next major | **none** — `dist-tags.next` is still `4.0.0`; there is no v5 |
| v3 LTS | `3.4.19` (irrelevant — we are already on v4) |

**Verdict: we are current. The only delta is a patch (4.3.2 → 4.3.3).** No major, no
minor, no breaking change is pending. This is a safe, boring patch bump — do it in the
same PR as the redesign (or skip it; it changes nothing user-visible). **Do not treat
this audit as a "Tailwind upgrade."** Our engine, `@theme`, `@source`, `@layer
components`, and `@apply` usage all match current v4 CSS-first guidance. There is nothing
deprecated in `static/input.css`.

The only alignment note vs. current v4 docs: the v4 default palette ships in **oklch**,
and the docs' `@theme` examples now use oklch for wider-gamut, perceptually-uniform
color. Our tokens are sRGB hex. That is **not deprecated and not a bug** — hex is fully
supported — but the redesign is the right moment to move the token values to oklch so
our brand/status ramps interpolate cleanly (esp. the `/15` tint overlays and any future
hover-darken). This is optional polish, not a correctness fix.

Risk assessment: **none**. Staying on 4.3.x is the correct call.

---

## 2. Current-state analysis (what we have)

### Tokens defined (`static/input.css` `@theme`)
Seven color tokens + one font token, all hex:
`--color-brand #4f46e5`, `--color-brand-light #a5b4fc`, `--color-surface #0b1120`,
`--color-card #111827`, `--color-line #1f2937`, `--color-ink #e5e7eb`,
`--color-muted #94a3b8`, `--font-sans …`.

Good: the `brand` vs `brand-light` split is deliberate and documented (contrast — base
brand fails AA on dark cards; `brand-light` clears 4.5:1). Keep that reasoning in the new
system.

**Gaps in the token set:**
- **No status color tokens.** Success / info / warning / danger are hand-written with raw
  Tailwind palette utilities in ~10 component files (see below).
- **No radius scale** — every component hardcodes `rounded` / `rounded-full` / `rounded-lg`
  ad hoc (76× `rounded`, 12× `rounded-full`, 11× `rounded-lg`).
- **No spacing scale token** (`--spacing`) — fine, the default works, but there's no
  semantic layout rhythm.
- **No typography tokens** beyond the font family — heading/label sizing is repeated
  literally: the string `text-sm font-semibold text-muted uppercase tracking-wide` is the
  de-facto section-header and appears **11×** across `auth_section/trend/plugin/audit_config/
  dashboard/apikeys/walkthrough`.
- **No easing / duration / keyframe tokens.**

### Ad-hoc color inconsistency (the biggest real issue)
Status/accent colors bypass the token layer entirely. Raw counts from `components/`:

- Info/interactive blue: `bg-blue-500/15` (11×), `hover:bg-blue-500/25` (7×),
  `text-blue-400` (9×), `text-blue-300`, `bg-blue-500` (3×), `accent-blue-500` (7×)
- Success green: `text-emerald-400` (6×), `bg-emerald-500/15` (3×), `border-emerald-500/30`
- Warning amber: `text-amber-400` (4×), `bg-amber-500/15`, `border-amber-500/30`
- Danger red: `text-red-400` (10×), `bg-red-500/15`, `text-red-300`

These are consistent *by convention* but not enforced — one file could drift to
`text-green-500` and nothing would catch it. They should become semantic tokens.

### De-facto third button style (uncodified)
`btn-primary` (solid brand) and `btn-secondary` (outline) exist in `@layer components`.
But there is a **third** button repeated verbatim as the "LLM action" affordance
(Draft notes / Regenerate / Run walkthrough):
`rounded bg-blue-500/15 px-3 py-1.5 text-sm font-medium text-blue-400 hover:bg-blue-500/25`
— in `notes.go` (2×), `walkthrough.go` (2×), and elsewhere. This wants to be a real
`.btn-accent` / `.btn-soft` component class.

### Badge/chip (uncodified, but consistent)
`inline-flex items-center rounded-full bg-<c>-500/15 px-2.5 py-0.5 text-xs font-medium
text-<c>-400` is copy-pasted for every status pill (auth "enabled", plugin "push",
audit-config "inferred/confirmed", walkthrough outcome, dashboard). Prime candidate for a
`.badge` base + tone modifiers.

### Animation story: essentially nonexistent
Total motion in the app today:
- `animate-spin` on the run-page "Live — auto-updating" spinner (`run.go:269`) — the only
  keyframe animation, and it's the built-in one.
- `transition-all` on three htmx progress bars (`notes.go`, `walkthrough.go`, `eval.go`)
  and one misc spot.
- The "● Live" badge is a **static** text dot — no pulse.

There are **no** hover transitions on cards/links, **no** entrance animation on htmx
swaps (and we swap constantly — status polls every 3s, list refreshes, fragment
replaces), and **no** `motion-safe`/`motion-reduce` gating anywhere. This is a blank
canvas — good, because it means the redesign can introduce a *coherent* motion language
rather than untangle an inconsistent one.

### Layout shell (for reference)
`layouts` app shell: `min-h-screen bg-surface text-ink antialiased`, main is
`mx-auto w-full max-w-screen-xl px-4 sm:px-6 py-6`. Solid; keep.

### Consumption conventions the new patterns MUST respect
- Classes are applied via gomponents `h.Class("...")` (string literals) — Tailwind's
  `@source "../components"` scans the Go files, so **any class must appear as a complete
  literal string** (no runtime concatenation of partial class fragments that Tailwind
  can't see; the existing `"…"+tone` pattern works only because both halves are literal
  strings elsewhere). New component classes (`.btn-accent`, `.badge`, `.card`) sidestep
  this nicely — one literal class name instead of a long utility string.
- htmx: `<body>` is `hx-boost="true"`; per-element JS is re-wired on **both**
  `DOMContentLoaded` and `htmx:load` idempotently. Entrance animations on swapped
  fragments should be **pure CSS** (keyframe on the incoming element) so they fire on
  htmx insertion without JS wiring.
- gomponents aliases `g`/`h`/`c`; pages call `layouts.App(ctx, …)`, fragments return bare
  `g.Node`.

---

## 3. Proposed shared design-system foundation

Concrete additions to `static/input.css`. Dark-theme-first (the app has no light mode;
don't build one). Minimal — every token below is justified by current usage.

### 3a. Token layer — extend the `@theme` block

```css
@theme {
  /* ---- brand ---- */
  --color-brand: #4f46e5;         /* solid brand on white (buttons) */
  --color-brand-light: #a5b4fc;   /* brand text/links ON dark surfaces (AA-safe) */
  --color-brand-hover: #4338ca;   /* NEW: darker brand for solid-button hover */

  /* ---- surfaces & structure ---- */
  --color-surface: #0b1120;       /* app background */
  --color-card: #111827;          /* raised card */
  --color-card-hover: #161f33;    /* NEW: card hover/elevated (subtle lift) */
  --color-line: #1f2937;          /* borders / dividers */
  --color-ink: #e5e7eb;           /* primary text */
  --color-muted: #94a3b8;         /* secondary text / labels */

  /* ---- semantic status (replace the ad-hoc blue/emerald/amber/red) ----
     One hue per role; the *-fg is the AA-safe text/icon shade on dark,
     the base 500 is only used at low alpha (/10../30) for tints/borders. */
  --color-info: #3b82f6;      --color-info-fg: #60a5fa;      /* was blue-500 / blue-400 */
  --color-success: #10b981;   --color-success-fg: #34d399;   /* was emerald-500 / -400 */
  --color-warning: #f59e0b;   --color-warning-fg: #fbbf24;   /* was amber-500 / -400 */
  --color-danger: #ef4444;    --color-danger-fg: #f87171;    /* was red-500 / -400 */

  /* ---- radius scale ---- */
  --radius-sm: 0.25rem;   /* inputs, small chips */
  --radius: 0.375rem;     /* default — buttons, cards' inner elements */
  --radius-lg: 0.5rem;    /* cards */
  --radius-xl: 0.75rem;   /* hero / feature cards */
  /* rounded-full stays as-is for pills/dots */

  /* ---- typography (semantic sizes already in use) ---- */
  --font-sans: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  --font-mono: ui-monospace, "SF Mono", "Cascadia Code", Menlo, monospace; /* used 8× as font-mono */

  /* ---- motion tokens ---- */
  --ease-out-quart: cubic-bezier(0.25, 1, 0.5, 1);   /* entrances — decelerate */
  --ease-standard:  cubic-bezier(0.4, 0, 0.2, 1);    /* hovers / general */
  --duration-fast: 120ms;
  --duration-base: 200ms;
  --duration-slow: 320ms;
}
```

Notes:
- Semantic status tokens usable as `text-info-fg`, `bg-success/15`, `border-warning/30`,
  `bg-info` — Tailwind v4 generates the `/<alpha>` variants automatically for any
  `--color-*`, so `bg-info/15` replaces `bg-blue-500/15` one-for-one. **This is the single
  highest-value change**: it moves ~50 scattered raw-palette utilities behind 8 tokens.
- oklch is the v4-idiomatic form; if the team wants the wider gamut, convert these hex
  values to `oklch(...)` at adoption time (mechanical, no structural change). Hex is fine
  to ship first.

### 3b. Component layer — extend `@layer components`

```css
@layer components {
  /* --- buttons (btn-primary/secondary exist; add accent + a shared base) --- */
  .btn-primary {
    @apply rounded bg-brand px-4 py-2 font-medium text-white
           transition-colors duration-[--duration-fast] hover:bg-brand-hover;
  }
  .btn-secondary {
    @apply rounded border border-line px-4 py-2 font-medium text-ink
           transition-colors duration-[--duration-fast] hover:bg-card-hover;
  }
  /* NEW: the "LLM action" soft button, currently copy-pasted as bg-blue-500/15… */
  .btn-accent {
    @apply rounded bg-info/15 px-3 py-1.5 text-sm font-medium text-info-fg
           transition-colors duration-[--duration-fast] hover:bg-info/25;
  }

  /* --- card --- */
  .card {
    @apply rounded-lg border border-line bg-card p-4;
  }
  .card-interactive {
    @apply card transition duration-[--duration-base] ease-[--ease-standard]
           motion-safe:hover:-translate-y-0.5 hover:border-brand-light/40 hover:bg-card-hover;
  }

  /* --- section header (the 11×-repeated label) --- */
  .section-title {
    @apply text-sm font-semibold uppercase tracking-wide text-muted;
  }

  /* --- badge / status pill (base + tone modifiers) --- */
  .badge {
    @apply inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium;
  }
  .badge-info    { @apply badge bg-info/15    text-info-fg; }
  .badge-success { @apply badge bg-success/15 text-success-fg; }
  .badge-warning { @apply badge bg-warning/15 text-warning-fg; }
  .badge-danger  { @apply badge bg-danger/15  text-danger-fg; }
}
```

### 3c. Motion layer — keyframes in `@theme` + a `motion-safe` entrance utility

```css
@theme {
  /* fade+rise: default entrance for htmx-swapped fragments & cards */
  --animate-enter: enter var(--duration-slow) var(--ease-out-quart) both;
  @keyframes enter {
    from { opacity: 0; transform: translateY(6px); }
    to   { opacity: 1; transform: translateY(0); }
  }
  /* soft fade only (for text/badge swaps where a rise looks wrong) */
  --animate-fade: fade var(--duration-base) var(--ease-standard) both;
  @keyframes fade { from { opacity: 0; } to { opacity: 1; } }

  /* live pulse — replaces the static "● Live" dot with a gentle breathe */
  --animate-live: live 2s var(--ease-standard) infinite;
  @keyframes live {
    0%,100% { opacity: 1; }
    50%     { opacity: 0.35; }
  }
}
```

Apply these **only** through `motion-safe:` so `prefers-reduced-motion` users get no
motion (the app has zero reduced-motion handling today — this fixes that gap at the
source):

```html
<!-- htmx fragment root -->
<div class="motion-safe:animate-enter"> … </div>
<!-- live dot -->
<span class="motion-safe:animate-live">●</span>
```

For hover interactions the `.card-interactive` / `.btn-*` classes already gate the
*transform* behind `motion-safe:` while keeping color transitions (which are safe and
help everyone) ungated — matching the v4 docs' `motion-reduce:hover:transform-none`
pattern but expressed positively.

**Keep it minimal:** three keyframes (`enter`, `fade`, `live`), two easings, three
durations, one interactive-card class. That's the whole motion vocabulary — enough for
tasteful hover-lift, fragment entrances, and a living status dot without a carousel of
bespoke animations.

---

## 4. Best-practice gaps to fix opportunistically

1. **`prefers-reduced-motion` unhandled** — no `motion-safe`/`motion-reduce` anywhere.
   The `animate-spin` spinner and any new motion must be `motion-safe`-gated. (a11y — this
   app literally audits a11y; it should model it.)
2. **Status colors not tokenized** — ~50 raw `blue/emerald/amber/red` utilities → migrate
   to `info/success/warning/danger` tokens. Do it file-by-file as each page is redesigned;
   the utilities are drop-in (`blue-500/15` → `info/15`, `blue-400` → `info-fg`).
3. **Repeated utility strings → component classes** — the 11× section header, the badge
   pattern, and the soft-accent button. Collapsing them removes drift risk and shrinks the
   Go component strings.
4. **`brand-hover` / `card-hover` missing** — `hover:opacity-90` (used 3×) dims the whole
   element incl. text; a proper hover color token reads better and keeps text contrast.
5. **Radius is ad hoc** — standardize on the `--radius*` scale (`rounded-lg` for cards,
   `rounded` for buttons/inputs, `rounded-full` for pills) so corners are consistent.
6. **oklch (optional)** — convert token hex → oklch to align with v4's default palette and
   get clean alpha interpolation. Mechanical; do it once at adoption if desired.
7. **Patch bump 4.3.2 → 4.3.3** — trivial, no user-visible change; bundle it or skip it.

None of these are correctness bugs — the current CSS is valid v4. They are consistency and
a11y-hygiene improvements that the redesign should absorb rather than perpetuate.

---

## 5. How the three redesigns should apply this

**Shared rules (all three):** use `.card` / `.card-interactive` for every panel; use
`.section-title` for the uppercase label; use `.badge-*` for every status pill; use
`.btn-primary` (one per view) / `.btn-secondary` / `.btn-accent` (LLM actions) — never
hand-roll a button; use `info/success/warning/danger` tokens, never raw `blue/emerald/
amber/red`; gate every transform/entrance behind `motion-safe:`; wrap htmx-swapped
fragment roots in `motion-safe:animate-enter` (or `animate-fade` for text-only swaps).

- **Dashboard cards** — target/list cards become `.card-interactive` (hover-lift +
  border-brand tint + `card-hover` bg) so the grid feels alive and clickable. Status pills
  (`queued`/`running`/`done`/`failed`, plugin/login mode) use `.badge-*`. New cards
  entering after "Add a target" swap in with `motion-safe:animate-enter`. This is where the
  hover-lift micro-interaction earns its keep.

- **Run page** — the "● Live — auto-updating" dot uses `motion-safe:animate-live` (breathe)
  instead of a static glyph; the `animate-spin` spinner gets `motion-safe:`. The 3s-poll
  status fragments swap with `animate-fade` (a rise on a constantly-repolling region would
  be distracting — use fade, not enter). Diff/finding/notes/eval sections are `.card` with
  `.section-title` headers; regression/layout badges become `.badge-danger` / `.badge-warning`.

- **Target page** — the stacked config cards (Authentication, Audit configuration,
  Walkthrough, plugin) all become `.card` + `.section-title`; the inferred/confirmed and
  enabled/mode pills become `.badge-warning` / `.badge-success` / `.badge-info`. The loud
  default-off "Allow real form submissions" toggle keeps its red emphasis but sourced from
  `--color-danger` (`text-danger-fg`, `border-danger/30`) not raw `red-*`. Infer/save
  fragment responses swap in with `motion-safe:animate-enter`.

This gives the three implementation agents one vocabulary: **`.card` + `.section-title` +
`.badge-*` + `.btn-*` + `info/success/warning/danger` tokens + `motion-safe:animate-enter`/
`animate-fade`/`animate-live`** — so the redesigned target, run, and dashboard read as one
product.
