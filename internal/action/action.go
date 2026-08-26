// Package action defines the canonical, CLOSED set of browser actions the
// goal-directed walkthrough DRIVER (Phase 3) may execute, plus parsing and
// validation. It mirrors internal/recipe: a pure package with NO chromedp / LLM
// dependency, importable by the crawler, the planner, and handlers alike.
//
// The set is deliberately CLOSED and excludes any eval/script/js action — the
// planner (an LLM) authors these, so an arbitrary-JavaScript action would be an
// injection/exfiltration vector. Every selector/text a planner emits round-trips
// ONLY through chromedp's selector engine, never through chromedp.Evaluate.
package action

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Type is one of the closed set of action kinds the driver can execute.
type Type string

const (
	Click    Type = "click"    // click Selector
	TypeText Type = "type"     // type Text into Selector
	Press    Type = "press"    // press a single allowlisted Key
	Select   Type = "select"   // choose Value in the <select> at Selector
	Scroll   Type = "scroll"   // scroll the page up/down (Direction)
	WaitFor  Type = "waitFor"  // wait for Selector to appear OR URL to contain URLContains
	Navigate Type = "navigate" // navigate to URL (SSRF-guarded, same-origin)
	Finish   Type = "finish"   // advisory: the planner believes the goal is reached
)

// Field length caps — defensive bounds on planner-authored, UNTRUSTED strings
// (they are stored + rendered escaped and fed to chromedp as selectors).
const (
	MaxSelectorLen = 400
	MaxTextLen     = 2000
	MaxReasonLen   = 500
	MaxURLLen      = 2000
)

// allowedKeys is the closed set of keys a press action may use. Free-text key
// injection is refused (a planner can only drive with these navigation keys).
var allowedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Escape": true, "ArrowUp": true, "ArrowDown": true,
}

// allowedDirections is the closed set for a scroll action ("" defaults to down).
var allowedDirections = map[string]bool{"up": true, "down": true}

// AllowedKeys returns the allowlist (for prompts/tests).
func AllowedKeys() []string { return []string{"Enter", "Tab", "Escape", "ArrowUp", "ArrowDown"} }

// Action is one canonical driver action. Only the fields relevant to its Type
// are populated; the rest omit from JSON.
type Action struct {
	Type        Type   `json:"type"`
	Selector    string `json:"selector,omitempty"`     // click/type/select/waitFor
	Text        string `json:"text,omitempty"`         // type
	Key         string `json:"key,omitempty"`          // press
	Value       string `json:"value,omitempty"`        // select
	Direction   string `json:"direction,omitempty"`    // scroll (up|down)
	URL         string `json:"url,omitempty"`          // navigate
	URLContains string `json:"url_contains,omitempty"` // waitFor
	TimeoutMs   int    `json:"timeout_ms,omitempty"`   // waitFor
	Reason      string `json:"reason,omitempty"`       // planner's rationale (advisory, all types)
}

// ParseAction decodes ONE action object from a (possibly fenced/prose-wrapped)
// model reply. It is lenient about surrounding markdown/prose (extractJSON) but
// STRICT about the schema: DisallowUnknownFields rejects an injected
// script/eval/js key outright. A malformed body is an error (never a panic).
func ParseAction(b []byte) (Action, error) {
	raw, err := extractJSON(string(b))
	if err != nil {
		return Action{}, err
	}
	var a Action
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return Action{}, fmt.Errorf("action: decode: %w", err)
	}
	a.Type = Type(strings.TrimSpace(string(a.Type)))
	a.Selector = strings.TrimSpace(a.Selector)
	a.Key = strings.TrimSpace(a.Key)
	a.Direction = strings.ToLower(strings.TrimSpace(a.Direction))
	a.URL = strings.TrimSpace(a.URL)
	a.URLContains = strings.TrimSpace(a.URLContains)
	a.Reason = strings.TrimSpace(a.Reason)
	return a, nil
}

// Validate enforces the per-type structural contract: a known type, the required
// fields for that type, an allowlisted press key, an allowlisted scroll
// direction, and length caps on the untrusted strings. Returns a user-facing
// error on the first violation.
func Validate(a Action) error {
	if len(a.Selector) > MaxSelectorLen {
		return fmt.Errorf("selector too long (%d > %d)", len(a.Selector), MaxSelectorLen)
	}
	if len(a.Text) > MaxTextLen {
		return fmt.Errorf("text too long (%d > %d)", len(a.Text), MaxTextLen)
	}
	if len(a.Reason) > MaxReasonLen {
		return fmt.Errorf("reason too long (%d > %d)", len(a.Reason), MaxReasonLen)
	}
	if len(a.URL) > MaxURLLen {
		return fmt.Errorf("url too long (%d > %d)", len(a.URL), MaxURLLen)
	}
	switch a.Type {
	case Click:
		if a.Selector == "" {
			return fmt.Errorf("click: selector is required")
		}
	case TypeText:
		if a.Selector == "" {
			return fmt.Errorf("type: selector is required")
		}
		if a.Text == "" {
			return fmt.Errorf("type: text is required")
		}
	case Press:
		if !allowedKeys[a.Key] {
			return fmt.Errorf("press: key %q not allowed (allowed: %s)", a.Key, strings.Join(AllowedKeys(), ", "))
		}
	case Select:
		if a.Selector == "" {
			return fmt.Errorf("select: selector is required")
		}
		if a.Value == "" {
			return fmt.Errorf("select: value is required")
		}
	case Scroll:
		if a.Direction != "" && !allowedDirections[a.Direction] {
			return fmt.Errorf("scroll: direction %q not allowed (up|down)", a.Direction)
		}
	case WaitFor:
		if a.Selector == "" && a.URLContains == "" {
			return fmt.Errorf("waitFor: a selector or url_contains is required")
		}
	case Navigate:
		if a.URL == "" {
			return fmt.Errorf("navigate: url is required")
		}
	case Finish:
		// advisory — no required fields.
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
	return nil
}

// DedupKey is a stable identity for a (type, selector-or-target, url) triple used
// by the driver's repeat/no-progress detection.
func (a Action) DedupKey(currentURL string) string {
	target := a.Selector
	switch a.Type {
	case Navigate:
		target = a.URL
	case Press:
		target = a.Key
	case Scroll:
		target = a.Direction
	case WaitFor:
		if target == "" {
			target = a.URLContains
		}
	}
	return string(a.Type) + "|" + target + "|" + currentURL
}

// SuccessAssertion is the DETERMINISTIC, observed success condition for a
// walkthrough: the goal is reached when Selector becomes visible OR the URL
// contains URLContains, within TimeoutMs. It generalizes the P4 login recipe's
// success waitFor. A zero assertion means "no success condition configured".
type SuccessAssertion struct {
	Selector    string `json:"selector,omitempty"`
	URLContains string `json:"url_contains,omitempty"`
	TimeoutMs   int    `json:"timeout_ms,omitempty"`
}

// IsZero reports whether no success condition is set (neither a selector nor a
// url substring). A walkthrough with a zero assertion can never deterministically
// reach "success" — the handler refuses to start one (409).
func (s SuccessAssertion) IsZero() bool {
	return strings.TrimSpace(s.Selector) == "" && strings.TrimSpace(s.URLContains) == ""
}

// Validate enforces the success-assertion contract: at least one of selector /
// url_contains, and defensive length caps.
func (s SuccessAssertion) Validate() error {
	if s.IsZero() {
		return fmt.Errorf("success condition needs a selector or url_contains")
	}
	if len(s.Selector) > MaxSelectorLen {
		return fmt.Errorf("success selector too long")
	}
	if len(s.URLContains) > MaxURLLen {
		return fmt.Errorf("success url_contains too long")
	}
	return nil
}

// extractJSON pulls the first top-level JSON object out of a model reply,
// tolerating markdown code fences and surrounding prose. Mirrors the eval
// package's helper (kept local so action stays dependency-free).
func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = strings.TrimSpace(rest)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return "", fmt.Errorf("action: no JSON object found")
	}
	return s[start : end+1], nil
}
