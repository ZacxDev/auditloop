package recipe

import (
	"strings"
	"testing"
)

func guided() GuidedForm {
	return GuidedForm{
		LoginURL:         "https://app.example.com/login",
		UsernameSelector: "#email",
		PasswordSelector: "#password",
		SubmitSelector:   "button[type=submit]",
		SuccessSelector:  "nav.dashboard",
		SuccessTimeoutMs: 10000,
	}
}

func TestCompileGuided(t *testing.T) {
	steps := guided().Compile()
	if len(steps) != 5 {
		t.Fatalf("want 5 canonical steps, got %d", len(steps))
	}
	want := []StepType{StepGoto, StepFill, StepFill, StepClick, StepWaitFor}
	for i, wt := range want {
		if steps[i].Type != wt {
			t.Errorf("step %d type = %q, want %q", i, steps[i].Type, wt)
		}
	}
	// Credentials are placeholders, NOT inline values.
	if steps[1].ValueRef != RefUsername || steps[2].ValueRef != RefPassword {
		t.Errorf("fill steps must reference credential placeholders")
	}
	if err := Validate(steps); err != nil {
		t.Fatalf("compiled guided form should validate: %v", err)
	}
}

func TestCompileDefaultsTimeout(t *testing.T) {
	g := guided()
	g.SuccessTimeoutMs = 0
	steps := g.Compile()
	if steps[4].TimeoutMs != DefaultWaitTimeoutMs {
		t.Errorf("timeout default = %d, want %d", steps[4].TimeoutMs, DefaultWaitTimeoutMs)
	}
}

func TestDeriveGuidedRoundTrip(t *testing.T) {
	orig := guided()
	steps := orig.Compile()
	got, ok := DeriveGuided(steps)
	if !ok {
		t.Fatal("DeriveGuided should recognize a compiled guided recipe")
	}
	if got.LoginURL != orig.LoginURL || got.UsernameSelector != orig.UsernameSelector ||
		got.PasswordSelector != orig.PasswordSelector || got.SubmitSelector != orig.SubmitSelector {
		t.Errorf("derived guided fields mismatch: %+v", got)
	}
	// A multi-step advanced recipe should NOT derive as guided.
	adv := append(steps, Step{Type: StepClick, Selector: ".cookie-accept"})
	if _, ok := DeriveGuided(adv); ok {
		t.Error("a 6-step recipe should not be treated as guided")
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	steps := []Step{
		{Type: StepGoto, URL: "https://x.example.com/login"},
		{Type: "eval", Selector: "x"}, // arbitrary-JS vector — must be rejected
		{Type: StepWaitFor, Selector: "nav"},
	}
	err := Validate(steps)
	if err == nil || !strings.Contains(err.Error(), "unknown step type") {
		t.Fatalf("expected unknown-step-type rejection, got %v", err)
	}
}

func TestValidateRejectsInlineCredential(t *testing.T) {
	steps := []Step{
		{Type: StepGoto, URL: "https://x.example.com/login"},
		{Type: StepFill, Selector: "#p", ValueRef: "hunter2"}, // not a placeholder → reject
		{Type: StepWaitFor, Selector: "nav"},
	}
	err := Validate(steps)
	if err == nil || !strings.Contains(err.Error(), "value_ref") {
		t.Fatalf("expected value_ref rejection (no inline credentials), got %v", err)
	}
}

func TestValidateRequiresGotoAndSuccess(t *testing.T) {
	// Missing goto.
	if err := Validate([]Step{{Type: StepWaitFor, Selector: "nav"}}); err == nil {
		t.Error("expected error when first step is not goto")
	}
	// Missing success waitFor.
	steps := []Step{
		{Type: StepGoto, URL: "https://x.example.com/login"},
		{Type: StepFill, Selector: "#u", ValueRef: RefUsername},
	}
	if err := Validate(steps); err == nil || !strings.Contains(err.Error(), "waitFor") {
		t.Errorf("expected success-waitFor requirement, got %v", err)
	}
	// Empty.
	if err := Validate(nil); err == nil {
		t.Error("expected error for empty recipe")
	}
}

// timeout_ms is USER-SUPPLIED and feeds the #41 watchdog's wall-clock budget for
// the whole recipe, so it must be capped at the validation boundary — otherwise
// a recipe can extend the "a stall is bounded" guarantee to hours.
func TestValidateCapsWaitTimeout(t *testing.T) {
	mk := func(ms int) []Step {
		return []Step{
			{Type: StepGoto, URL: "https://x.example.com/login"},
			{Type: StepWaitFor, Selector: "#dash", TimeoutMs: ms},
		}
	}
	if err := Validate(mk(MaxWaitTimeoutMs + 1)); err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Errorf("expected timeout_ms cap rejection, got %v", err)
	}
	if err := Validate(mk(3600000)); err == nil {
		t.Error("a one-hour timeout_ms must be rejected")
	}
	if err := Validate(mk(-1)); err == nil {
		t.Error("a negative timeout_ms must be rejected")
	}
	// Boundary + the common cases stay valid.
	for _, ok := range []int{0, 1000, DefaultWaitTimeoutMs, MaxWaitTimeoutMs} {
		if err := Validate(mk(ok)); err != nil {
			t.Errorf("timeout_ms %d should be valid: %v", ok, err)
		}
	}
}

func TestValidateRequiresFields(t *testing.T) {
	steps := []Step{
		{Type: StepGoto, URL: ""}, // missing url
		{Type: StepWaitFor, Selector: "nav"},
	}
	if err := Validate(steps); err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("expected goto url requirement, got %v", err)
	}
}

func TestParseStepsRejectsUnknownFields(t *testing.T) {
	// A "script" field should be refused (DisallowUnknownFields keeps the model closed).
	if _, err := ParseSteps(`[{"type":"goto","url":"https://x","script":"alert(1)"}]`); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
	if _, err := ParseSteps(""); err == nil {
		t.Fatal("expected empty-steps error")
	}
	steps, err := ParseSteps(`[{"type":"goto","url":"https://x.example.com/login"},{"type":"waitFor","selector":"nav"}]`)
	if err != nil {
		t.Fatalf("valid parse failed: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("parsed %d steps", len(steps))
	}
}

func TestParseStepsRejectsTooManySteps(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < MaxSteps+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"click","selector":"#x"}`)
	}
	b.WriteString("]")
	if _, err := ParseSteps(b.String()); err == nil {
		t.Fatalf("expected rejection of %d steps (max %d)", MaxSteps+1, MaxSteps)
	}
	// Exactly MaxSteps is allowed.
	var ok strings.Builder
	ok.WriteString("[")
	for i := 0; i < MaxSteps; i++ {
		if i > 0 {
			ok.WriteString(",")
		}
		ok.WriteString(`{"type":"click","selector":"#x"}`)
	}
	ok.WriteString("]")
	if _, err := ParseSteps(ok.String()); err != nil {
		t.Fatalf("MaxSteps steps should parse: %v", err)
	}
}

func TestGotoURLsAndRequiredRefs(t *testing.T) {
	steps := guided().Compile()
	if urls := GotoURLs(steps); len(urls) != 1 || urls[0] != "https://app.example.com/login" {
		t.Errorf("GotoURLs = %v", urls)
	}
	refs := RequiredRefs(steps)
	if len(refs) != 2 {
		t.Errorf("RequiredRefs = %v, want [username password]", refs)
	}
}

func TestCredentialsMapNoPlaintextInSteps(t *testing.T) {
	// The canonical steps must never contain the plaintext credential values.
	steps := guided().Compile()
	js, _ := MarshalSteps(steps)
	if strings.Contains(js, "hunter2") || strings.Contains(js, "alice@example.com") {
		t.Fatal("steps JSON leaked a credential value")
	}
	// Credentials round-trip via their own blob.
	c := Credentials{Username: "alice@example.com", Password: "hunter2"}
	b, _ := c.Marshal()
	got, err := ParseCredentials(b)
	if err != nil {
		t.Fatalf("ParseCredentials: %v", err)
	}
	m := got.Map()
	if m[RefUsername] != "alice@example.com" || m[RefPassword] != "hunter2" {
		t.Errorf("credential map = %v", m)
	}
}
