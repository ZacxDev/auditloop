// Package recipe defines the canonical login-recipe step model (P4) plus the
// two authoring modes that compile to it (a guided login form and a raw
// advanced step list) and the validation that gates saving one.
//
// A recipe is an ordered list of typed browser Steps. Step types are a CLOSED
// set — goto, fill, click, waitFor — deliberately EXCLUDING any script/eval step
// so a recipe can never inject arbitrary JavaScript (an exfiltration vector).
// Credentials are NEVER inlined in a step: a fill step carries a placeholder
// ValueRef (username|password), and the worker substitutes the decrypted value
// at run time. This package holds ZERO plaintext credentials and no chromedp
// dependency, so it is importable by handlers, the worker, and the crawler alike.
package recipe

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StepType is one of the closed set of canonical step kinds.
type StepType string

const (
	StepGoto    StepType = "goto"    // navigate to URL
	StepFill    StepType = "fill"    // type a credential (by ValueRef) into Selector
	StepClick   StepType = "click"   // click Selector
	StepWaitFor StepType = "waitFor" // wait for Selector to appear OR URL to contain URLContains
)

// Credential placeholder refs. A fill step's ValueRef must be one of these; the
// actual secret lives only in the encrypted credentials blob.
const (
	RefUsername = "username"
	RefPassword = "password"
)

// DefaultWaitTimeoutMs is the fallback per-step wait timeout when a waitFor step
// omits one.
const DefaultWaitTimeoutMs = 15000

// MaxWaitTimeoutMs caps a waitFor step's timeout_ms (2 minutes). This is a
// SAFETY bound, not a usability one: the timeout is user-supplied and feeds the
// #41 watchdog's wall-clock budget for the whole recipe
// (crawler.loginHardBudget). Uncapped, a recipe could set timeout_ms to hours
// and push that budget past any useful bound, degrading the "a stall is
// BOUNDED" guarantee to "a stall ends eventually" on the one path where the
// user supplies the numbers — while pinning the single-threaded crawl worker
// for the duration. A real login success condition resolves in seconds.
const MaxWaitTimeoutMs = 120000

// Step is one canonical browser action. Only the fields relevant to its Type are
// populated; the rest omit from JSON.
type Step struct {
	Type        StepType `json:"type"`
	URL         string   `json:"url,omitempty"`          // goto
	Selector    string   `json:"selector,omitempty"`     // fill/click/waitFor(selector)
	ValueRef    string   `json:"value_ref,omitempty"`    // fill (username|password)
	URLContains string   `json:"url_contains,omitempty"` // waitFor(url)
	TimeoutMs   int      `json:"timeout_ms,omitempty"`   // waitFor
}

// GuidedForm is the common-case authoring mode: a same-domain form login. It
// compiles deterministically to the canonical step list.
type GuidedForm struct {
	LoginURL           string
	UsernameSelector   string
	PasswordSelector   string
	SubmitSelector     string
	SuccessSelector    string // success condition: an element that appears once logged in
	SuccessURLContains string // …OR a substring the post-login URL must contain
	SuccessTimeoutMs   int
}

// Compile turns the guided fields into the canonical ordered steps:
// goto(login) → fill(username) → fill(password) → click(submit) → waitFor(success).
func (f GuidedForm) Compile() []Step {
	to := f.SuccessTimeoutMs
	if to <= 0 {
		to = DefaultWaitTimeoutMs
	}
	return []Step{
		{Type: StepGoto, URL: strings.TrimSpace(f.LoginURL)},
		{Type: StepFill, Selector: strings.TrimSpace(f.UsernameSelector), ValueRef: RefUsername},
		{Type: StepFill, Selector: strings.TrimSpace(f.PasswordSelector), ValueRef: RefPassword},
		{Type: StepClick, Selector: strings.TrimSpace(f.SubmitSelector)},
		{Type: StepWaitFor, Selector: strings.TrimSpace(f.SuccessSelector), URLContains: strings.TrimSpace(f.SuccessURLContains), TimeoutMs: to},
	}
}

// DeriveGuided best-effort reverse-maps canonical steps back to guided fields so
// the guided form can be pre-filled on edit. ok is false when the step list does
// not match the canonical guided shape (then the recipe is "advanced" and the UI
// shows the raw JSON editor instead).
func DeriveGuided(steps []Step) (GuidedForm, bool) {
	var f GuidedForm
	var sawUser, sawPass, sawClick, sawWait bool
	for _, s := range steps {
		switch s.Type {
		case StepGoto:
			if f.LoginURL == "" {
				f.LoginURL = s.URL
			}
		case StepFill:
			switch s.ValueRef {
			case RefUsername:
				f.UsernameSelector, sawUser = s.Selector, true
			case RefPassword:
				f.PasswordSelector, sawPass = s.Selector, true
			}
		case StepClick:
			if !sawClick {
				f.SubmitSelector, sawClick = s.Selector, true
			}
		case StepWaitFor:
			f.SuccessSelector = s.Selector
			f.SuccessURLContains = s.URLContains
			f.SuccessTimeoutMs = s.TimeoutMs
			sawWait = true
		}
	}
	ok := f.LoginURL != "" && sawUser && sawPass && sawClick && sawWait && len(steps) == 5
	return f, ok
}

// MaxSteps bounds a login recipe's step count. A legitimate form login is a
// handful of steps; a huge list is either a mistake or an abuse/DoS attempt
// (each step drives a headless browser action), so parsing rejects it.
const MaxSteps = 50

// ParseSteps decodes a canonical step list from JSON (the advanced authoring
// mode and the stored steps_json). It does NOT validate — call Validate. It
// rejects a list longer than MaxSteps.
func ParseSteps(s string) ([]Step, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("recipe: empty steps")
	}
	var steps []Step
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("recipe: invalid steps JSON: %w", err)
	}
	if len(steps) > MaxSteps {
		return nil, fmt.Errorf("recipe: too many steps (%d, max %d)", len(steps), MaxSteps)
	}
	return steps, nil
}

// MarshalSteps encodes steps to compact JSON for storage.
func MarshalSteps(steps []Step) (string, error) {
	b, err := json.Marshal(steps)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MarshalStepsPretty encodes steps to indented JSON for the advanced editor.
func MarshalStepsPretty(steps []Step) (string, error) {
	b, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Validate enforces the structural contract: a non-empty list, a known type for
// every step, the required fields per type, fill refs limited to the credential
// placeholders (never inline values), at least one goto and one success waitFor,
// and a sane first step (goto). It returns a user-facing error on the first
// violation. It NEVER inspects credential values (there are none in steps).
func Validate(steps []Step) error {
	if len(steps) == 0 {
		return fmt.Errorf("a login recipe needs at least one step")
	}
	var hasGoto, hasSuccess bool
	for i, s := range steps {
		switch s.Type {
		case StepGoto:
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("step %d (goto): url is required", i+1)
			}
			hasGoto = true
		case StepFill:
			if strings.TrimSpace(s.Selector) == "" {
				return fmt.Errorf("step %d (fill): selector is required", i+1)
			}
			if s.ValueRef != RefUsername && s.ValueRef != RefPassword {
				return fmt.Errorf("step %d (fill): value_ref must be %q or %q (no inline credentials allowed)", i+1, RefUsername, RefPassword)
			}
		case StepClick:
			if strings.TrimSpace(s.Selector) == "" {
				return fmt.Errorf("step %d (click): selector is required", i+1)
			}
		case StepWaitFor:
			if strings.TrimSpace(s.Selector) == "" && strings.TrimSpace(s.URLContains) == "" {
				return fmt.Errorf("step %d (waitFor): a selector or url_contains is required", i+1)
			}
			if s.TimeoutMs < 0 {
				return fmt.Errorf("step %d (waitFor): timeout_ms cannot be negative", i+1)
			}
			if s.TimeoutMs > MaxWaitTimeoutMs {
				return fmt.Errorf("step %d (waitFor): timeout_ms %d exceeds the maximum of %d (%s)",
					i+1, s.TimeoutMs, MaxWaitTimeoutMs, time.Duration(MaxWaitTimeoutMs)*time.Millisecond)
			}
			hasSuccess = true
		default:
			return fmt.Errorf("step %d: unknown step type %q (allowed: goto, fill, click, waitFor)", i+1, s.Type)
		}
	}
	if !hasGoto {
		return fmt.Errorf("a login recipe must include a goto step (the login page)")
	}
	if steps[0].Type != StepGoto {
		return fmt.Errorf("the first step must be a goto (navigate to the login page)")
	}
	if !hasSuccess {
		return fmt.Errorf("a login recipe must end with a waitFor step (the success condition)")
	}
	return nil
}

// GotoURLs returns every goto step's URL (used by callers to enforce
// same-domain + SSRF at save/run time via the crawler guard).
func GotoURLs(steps []Step) []string {
	var out []string
	for _, s := range steps {
		if s.Type == StepGoto && strings.TrimSpace(s.URL) != "" {
			out = append(out, strings.TrimSpace(s.URL))
		}
	}
	return out
}

// RequiredRefs returns the distinct credential refs referenced by fill steps
// (so the worker can confirm the decrypted blob supplies them).
func RequiredRefs(steps []Step) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range steps {
		if s.Type == StepFill && s.ValueRef != "" && !seen[s.ValueRef] {
			seen[s.ValueRef] = true
			out = append(out, s.ValueRef)
		}
	}
	return out
}
