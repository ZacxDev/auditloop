// Package walkthrough runs the Phase-3 goal-directed walkthrough DRIVER pass: an
// async job that drives headless Chromium toward a target's goal using the
// deterministic crawler.Drive loop, with an LLM planner that proposes ONE action
// per turn. The LLM lives here (outside internal/crawler) so the crawler stays
// hermetic/testable and the driver loop is deterministic. The success/stuck signal
// is DETERMINISTIC (an observed success assertion), never the planner's word.
package walkthrough

import (
	"context"
	"fmt"
	"strings"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/llm"
)

// Drafter is the vision-LLM call the planner depends on (satisfied by
// *llm.Client). Injectable so tests stub the model.
type Drafter interface {
	Draft(ctx context.Context, model, systemPrompt, userPrompt string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error)
}

// DefaultMaxTokens is a small per-turn planner completion budget (one action JSON).
const DefaultMaxTokens = 256

// DriveSystemPrompt instructs the planner to act as a user driving toward a goal
// and to reply with EXACTLY one action JSON from the CLOSED action set. It forbids
// any script/eval field (the parser rejects unknown fields regardless).
const DriveSystemPrompt = `You are driving a real web browser toward a GOAL, one step at a time, like a determined but ordinary user.
Each turn you are given: the goal, the current page URL, a screenshot of what is currently visible, and a DIGEST of the
visible interactive elements (each with a real CSS selector, its accessible name, tag/role/type, and whether it is disabled).

Decide the SINGLE next action that best moves toward the goal. Prefer selectors from the digest — they are real. Do not
invent selectors for elements you cannot see. If a form field is needed, type into it; then click the submit/continue control.
If you believe the goal is already reached, use "finish".

Reply with ONLY a single JSON object — no prose, no markdown fences — matching EXACTLY ONE of these shapes:
  {"type":"click","selector":"<css>","reason":"<why>"}
  {"type":"type","selector":"<css>","text":"<text to type>","reason":"<why>"}
  {"type":"select","selector":"<css>","value":"<option value>","reason":"<why>"}
  {"type":"press","key":"Enter|Tab|Escape|ArrowUp|ArrowDown","reason":"<why>"}
  {"type":"scroll","direction":"down|up","reason":"<why>"}
  {"type":"waitFor","selector":"<css>","url_contains":"<substr>","timeout_ms":8000,"reason":"<why>"}
  {"type":"navigate","url":"<same-site url>","reason":"<why>"}
  {"type":"finish","reason":"<why you believe the goal is reached>"}
Rules: choose exactly ONE action. Never include any other field (no script/eval/js). Keep "reason" short. Do not repeat an
action that already failed to make progress — try something different or finish.`

// Planner is a crawler.Planner backed by the vision LLM. It calls the model once
// per turn (with a single retry-with-nudge on a parse/validate failure), then
// degrades to a low-risk scroll so the pass keeps going rather than hard-failing.
// OnUsage (optional) receives each call's cost for live accounting.
type Planner struct {
	LLM       Drafter
	Model     string
	MaxTokens int
	OnUsage   func(llm.Usage)
}

// NextAction implements crawler.Planner. It returns an error ONLY on context
// cancellation (which fails the whole pass); every other failure degrades to a
// safe fallback action so the deterministic loop keeps control.
func (p *Planner) NextAction(ctx context.Context, st crawler.DriveState) (action.Action, error) {
	if ctx.Err() != nil {
		return action.Action{}, ctx.Err()
	}
	maxTok := p.MaxTokens
	if maxTok <= 0 {
		maxTok = DefaultMaxTokens
	}
	images := []llm.Image{}
	if len(st.ScreenshotPNG) > 0 {
		images = append(images, llm.Image{Label: "viewport", PNG: st.ScreenshotPNG})
	}
	prompt := DrivePrompt(st)

	text, usage, err := p.LLM.Draft(ctx, p.Model, DriveSystemPrompt, prompt, images, llm.WithMaxTokens(maxTok))
	p.report(usage)
	if err == nil {
		if a, ok := parseValid(text); ok {
			return a, nil
		}
	} else if ctx.Err() != nil {
		return action.Action{}, ctx.Err()
	}

	// One retry with a nudge to emit a single valid action JSON.
	nudged := prompt + "\n\nIMPORTANT: your previous reply was not a single valid action JSON. Reply with EXACTLY one JSON object from the allowed shapes, nothing else."
	text2, usage2, err2 := p.LLM.Draft(ctx, p.Model, DriveSystemPrompt, nudged, images, llm.WithMaxTokens(maxTok))
	p.report(usage2)
	if err2 == nil {
		if a, ok := parseValid(text2); ok {
			return a, nil
		}
	}
	if ctx.Err() != nil {
		return action.Action{}, ctx.Err()
	}
	// Degrade: a low-risk scroll keeps the loop moving; the no-progress guard will
	// short-circuit to stuck if the planner never recovers.
	return action.Action{Type: action.Scroll, Direction: "down", Reason: "planner could not produce a valid action; scrolling to reveal more"}, nil
}

func (p *Planner) report(u llm.Usage) {
	if p.OnUsage != nil {
		p.OnUsage(u)
	}
}

func parseValid(text string) (action.Action, bool) {
	a, err := action.ParseAction([]byte(text))
	if err != nil {
		return action.Action{}, false
	}
	if action.Validate(a) != nil {
		return action.Action{}, false
	}
	return a, true
}

// DrivePrompt builds the per-turn grounding prompt: goal, plain-words success hint,
// current URL, step N/M, the interactive digest, the action history, and the last
// outcome. The screenshot is attached separately by the caller.
func DrivePrompt(st crawler.DriveState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GOAL: %s\n", strings.TrimSpace(nonEmpty(st.Goal, "complete the primary task on this site")))
	fmt.Fprintf(&b, "This is step %d of at most %d.\n", st.StepIdx+1, st.MaxActions)
	fmt.Fprintf(&b, "Current URL: %s\n", st.URL)

	b.WriteString("\nVISIBLE INTERACTIVE ELEMENTS (use these selectors):\n")
	if len(st.Digest.Elements) == 0 {
		b.WriteString("(none detected — rely on the screenshot; you may scroll or navigate)\n")
	}
	for _, e := range st.Digest.Elements {
		disabled := ""
		if e.Disabled {
			disabled = " [disabled]"
		}
		kind := e.Tag
		if e.Type != "" {
			kind += "/" + e.Type
		}
		if e.Role != "" {
			kind += " role=" + e.Role
		}
		name := e.Name
		if name == "" {
			name = "(no accessible name)"
		}
		fmt.Fprintf(&b, "- %s — %q — selector: %s%s\n", kind, name, e.Selector, disabled)
	}

	if len(st.History) > 0 {
		b.WriteString("\nRECENT ACTIONS (most recent last):\n")
		start := 0
		if len(st.History) > 6 {
			start = len(st.History) - 6
		}
		for _, h := range st.History[start:] {
			fmt.Fprintf(&b, "- step %d: %s → %s\n", h.Idx+1, describeAction(h.Action), h.Outcome)
		}
	}
	if strings.TrimSpace(st.LastOutcome) != "" {
		fmt.Fprintf(&b, "\nLAST OUTCOME: %s\n", st.LastOutcome)
	}
	b.WriteString("\nReturn the single next action JSON now.")
	return b.String()
}

func describeAction(a action.Action) string {
	switch a.Type {
	case action.Navigate:
		return "navigate " + a.URL
	case action.TypeText:
		return "type into " + a.Selector
	case action.Press:
		return "press " + a.Key
	case action.Scroll:
		return "scroll " + nonEmpty(a.Direction, "down")
	case action.WaitFor:
		return "waitFor " + nonEmpty(a.Selector, a.URLContains)
	case action.Finish:
		return "finish"
	default:
		return string(a.Type) + " " + a.Selector
	}
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
