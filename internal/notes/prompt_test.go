package notes

import (
	"strings"
	"testing"
)

// TestSystemPromptScopedToVisual asserts the trimmed system prompt keeps the
// vision pass on SUBJECTIVE VISUAL critique and explicitly tells the model not to
// re-list the deterministic signals (a11y/console/network/perf/layout).
func TestSystemPromptScopedToVisual(t *testing.T) {
	p := SystemPrompt
	lower := strings.ToLower(p)

	// Must carry the explicit "do not re-list" instruction.
	if !strings.Contains(lower, "do not") || !strings.Contains(lower, "redundant") {
		t.Errorf("system prompt must instruct the model NOT to re-narrate deterministic issues:\n%s", p)
	}
	// Must name the visual concerns it SHOULD cover.
	for _, want := range []string{"visual hierarchy", "call-to-action", "trust", "responsive"} {
		if !strings.Contains(lower, want) {
			t.Errorf("system prompt should mention %q (visual critique scope)", want)
		}
	}
	// Must reference that the deterministic issues are captured separately.
	if !strings.Contains(lower, "accessibility") || !strings.Contains(lower, "performance") {
		t.Errorf("system prompt should acknowledge the deterministic issues are captured separately")
	}
}

// TestUserPromptNoDeterministicNarration asserts the per-page user prompt no longer
// enumerates axe rules or console/network counts, but KEEPS the P2 visual-diff
// grounding (a regression is a visual concern).
func TestUserPromptNoDeterministicNarration(t *testing.T) {
	g := Grounding{
		URL:       "https://acme.test/",
		Viewports: []string{"desktop", "mobile"},
	}
	up := g.UserPrompt()
	lower := strings.ToLower(up)

	for _, banned := range []string{"axe", "violation", "console error", "network error", "first-party"} {
		if strings.Contains(lower, banned) {
			t.Errorf("user prompt should NOT contain %q (deterministic narration), got:\n%s", banned, up)
		}
	}
	if !strings.Contains(up, "https://acme.test/") {
		t.Errorf("user prompt should include the page URL, got:\n%s", up)
	}

	// A visual regression IS a visual concern → it stays in the prompt.
	gr := Grounding{URL: "https://acme.test/", Viewports: []string{"desktop"}, HasDiff: true, IsRegression: true, DiffPct: 12.5}
	if rp := gr.UserPrompt(); !strings.Contains(strings.ToLower(rp), "visual regression") {
		t.Errorf("user prompt should keep the P2 visual-regression grounding, got:\n%s", rp)
	}
}
