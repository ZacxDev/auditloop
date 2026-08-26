// Package notes runs the P3 vision-LLM "draft UX notes" pass: for each selected
// model × each crawled page it calls the vision LLM with the desktop AND mobile
// screenshots plus grounding context (axe violations, first-party console/network
// error counts, and — when present — the page's P2 visual-diff status), producing
// an editable free-text markdown UX-notes draft. It is opt-in and paid (per-page,
// per-model API calls), so the caller gates it on OpenRouterEnabled().
package notes

import (
	"fmt"
	"strings"
)

// Grounding is the per-page context fed to the model alongside the screenshots.
// The vision pass is deliberately scoped to SUBJECTIVE VISUAL critique: the
// deterministic signals (accessibility, console/network errors, performance,
// layout smells) are captured separately by the crawler and shown next to these
// notes, so they are intentionally NOT passed here (no re-narration, fewer tokens).
// The ONE exception is the P2 visual-diff status: a visual regression is itself a
// visual concern, so it stays as grounding.
type Grounding struct {
	URL       string
	Viewports []string // e.g. ["desktop","mobile"]
	// Diff status (P2), when this run has a baseline and the page changed.
	HasDiff      bool
	DiffPct      float64
	SizeChanged  bool // layout/height change (pixel diff not comparable)
	IsRegression bool // same-size visual regression above threshold
}

// SystemPrompt is the role/instruction prompt sent as the system message. It is
// intentionally kept in code (documented, reviewable) rather than user-editable.
//
// Scope: SUBJECTIVE VISUAL critique only. The crawler already captures the
// deterministic issues (a11y, console/network errors, performance metrics, layout
// smells) and shows them alongside these notes, so the model must NOT re-list or
// narrate them — that would be redundant and waste tokens.
const SystemPrompt = `You are a senior product-design and UX reviewer auditing a web page.
You are shown full-page screenshots of ONE page at desktop and mobile viewports.

Write concise, prioritized, ACTIONABLE observations about the page's SUBJECTIVE VISUAL
QUALITY in GitHub-flavored Markdown. Focus ONLY on things a human judges by eye:
- Visual hierarchy — does the eye land on the right thing first? Is emphasis used well?
- Aesthetic quality & polish — spacing, alignment, balance, colour, typography, imagery.
- Clarity — is the page's purpose obvious at a glance? Is anything visually confusing or cluttered?
- Trust & credibility cues — does it look professional, current, and trustworthy?
- Call-to-action prominence — is the primary action visually obvious and compelling?
- Copy tone & voice — as far as the visible wording conveys it.
- Responsive visual quality — compare the desktop and mobile renderings; note where the
  mobile layout looks cramped, broken, or worse than desktop (visually — not metrics).

Rules:
- Lead with the highest-impact visual issues. Use a short bulleted list; group by theme if helpful.
- Tie every observation to what is actually visible in the screenshots. Do not invent copy or features.
- IMPORTANT: Do NOT re-list or narrate the deterministic issues (accessibility violations,
  JavaScript/console errors, network failures, performance metrics like LCP/CLS/TBT, page weight,
  or DOM layout smells). Those are captured automatically by the crawler and shown alongside your
  notes — commenting on them here is redundant. Stick to subjective visual judgment.
- Be specific and brief. No preamble, no restating the task, no closing summary.
- If the page looks visually solid, say so briefly and note only minor refinements.`

// UserPrompt builds the per-page user prompt. It is deliberately minimal — the page
// URL, the viewports shown, and (only) the P2 visual-diff status. The deterministic
// signals are intentionally omitted (see Grounding). Images are attached separately
// by the llm client.
func (g Grounding) UserPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Page URL: %s\n", g.URL)
	if len(g.Viewports) > 0 {
		fmt.Fprintf(&b, "Viewports shown: %s\n", strings.Join(g.Viewports, ", "))
	}

	if g.HasDiff {
		switch {
		case g.IsRegression:
			fmt.Fprintf(&b, "\nVisual regression vs the previous run: %.1f%% of pixels changed at the same page size — call out what appears to have changed visually.\n", g.DiffPct)
		case g.SizeChanged:
			b.WriteString("\nLayout/size change vs the previous run: the page height changed (added/removed content) — note any visual layout shifts.\n")
		case g.DiffPct > 0:
			fmt.Fprintf(&b, "\nMinor visual change vs the previous run: %.1f%% of pixels changed.\n", g.DiffPct)
		}
	}

	b.WriteString("\nWrite the visual UX notes for this page now.")
	return b.String()
}
