package action

import (
	"strings"
	"testing"
)

func TestParseActionValid(t *testing.T) {
	a, err := ParseAction([]byte(`{"type":"click","selector":"#submit","reason":"proceed"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Type != Click || a.Selector != "#submit" || a.Reason != "proceed" {
		t.Errorf("parsed wrong: %+v", a)
	}
}

func TestParseActionLenientFence(t *testing.T) {
	in := "Here is the action:\n```json\n{\"type\":\"navigate\",\"url\":\"https://x.test/next\"}\n```\nDone."
	a, err := ParseAction([]byte(in))
	if err != nil {
		t.Fatalf("parse fenced: %v", err)
	}
	if a.Type != Navigate || a.URL != "https://x.test/next" {
		t.Errorf("parsed wrong: %+v", a)
	}
}

func TestParseActionMalformedIsErrorNotPanic(t *testing.T) {
	for _, in := range []string{"", "not json at all", "{", `{"type":"click"`} {
		if _, err := ParseAction([]byte(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

// The CLOSED set: an injected script/eval/js key must be HARD-rejected
// (DisallowUnknownFields), never silently ignored — no arbitrary-JS vector.
func TestParseActionRejectsScriptKey(t *testing.T) {
	for _, in := range []string{
		`{"type":"click","selector":"#x","script":"fetch('//evil')"}`,
		`{"type":"navigate","url":"https://x.test","eval":"alert(1)"}`,
		`{"type":"click","selector":"#x","js":"x"}`,
	} {
		if _, err := ParseAction([]byte(in)); err == nil {
			t.Errorf("expected rejection of unknown field in %q", in)
		}
	}
}

func TestValidatePerType(t *testing.T) {
	cases := []struct {
		name string
		a    Action
		ok   bool
	}{
		{"click ok", Action{Type: Click, Selector: "#a"}, true},
		{"click no selector", Action{Type: Click}, false},
		{"type ok", Action{Type: TypeText, Selector: "#a", Text: "hi"}, true},
		{"type no text", Action{Type: TypeText, Selector: "#a"}, false},
		{"type no selector", Action{Type: TypeText, Text: "hi"}, false},
		{"press enter", Action{Type: Press, Key: "Enter"}, true},
		{"press bad key", Action{Type: Press, Key: "F5"}, false},
		{"press empty key", Action{Type: Press}, false},
		{"select ok", Action{Type: Select, Selector: "#s", Value: "v"}, true},
		{"select no value", Action{Type: Select, Selector: "#s"}, false},
		{"scroll default", Action{Type: Scroll}, true},
		{"scroll down", Action{Type: Scroll, Direction: "down"}, true},
		{"scroll bad", Action{Type: Scroll, Direction: "sideways"}, false},
		{"waitFor selector", Action{Type: WaitFor, Selector: "#ok"}, true},
		{"waitFor url", Action{Type: WaitFor, URLContains: "/done"}, true},
		{"waitFor empty", Action{Type: WaitFor}, false},
		{"navigate ok", Action{Type: Navigate, URL: "https://x.test"}, true},
		{"navigate empty", Action{Type: Navigate}, false},
		{"finish", Action{Type: Finish, Reason: "goal reached"}, true},
		{"unknown type", Action{Type: Type("evaljs")}, false},
	}
	for _, c := range cases {
		err := Validate(c.a)
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate ok=%v want %v (err=%v)", c.name, err == nil, c.ok, err)
		}
	}
}

func TestValidateLengthCaps(t *testing.T) {
	if err := Validate(Action{Type: Click, Selector: strings.Repeat("a", MaxSelectorLen+1)}); err == nil {
		t.Error("expected selector-too-long rejection")
	}
	if err := Validate(Action{Type: TypeText, Selector: "#a", Text: strings.Repeat("b", MaxTextLen+1)}); err == nil {
		t.Error("expected text-too-long rejection")
	}
}

func TestKeyAllowlist(t *testing.T) {
	for _, k := range AllowedKeys() {
		if err := Validate(Action{Type: Press, Key: k}); err != nil {
			t.Errorf("allowed key %q rejected: %v", k, err)
		}
	}
	for _, k := range []string{"enter", "Delete", "a", "Ctrl", ""} {
		if err := Validate(Action{Type: Press, Key: k}); err == nil {
			t.Errorf("key %q should be rejected", k)
		}
	}
}

func TestSuccessAssertion(t *testing.T) {
	if !(SuccessAssertion{}).IsZero() {
		t.Error("empty assertion should be zero")
	}
	if (SuccessAssertion{Selector: "#ok"}).IsZero() {
		t.Error("selector assertion is not zero")
	}
	if (SuccessAssertion{URLContains: "/done"}).IsZero() {
		t.Error("url assertion is not zero")
	}
	if err := (SuccessAssertion{}).Validate(); err == nil {
		t.Error("empty assertion should fail Validate")
	}
	if err := (SuccessAssertion{Selector: "#ok", TimeoutMs: 5000}).Validate(); err != nil {
		t.Errorf("valid assertion rejected: %v", err)
	}
}

func TestDedupKey(t *testing.T) {
	a := Action{Type: Click, Selector: "#a"}
	if a.DedupKey("u1") == a.DedupKey("u2") {
		t.Error("dedup key must include URL")
	}
	n := Action{Type: Navigate, URL: "https://x/next"}
	if !strings.Contains(n.DedupKey("u"), "https://x/next") {
		t.Error("navigate dedup key should use URL as target")
	}
}
