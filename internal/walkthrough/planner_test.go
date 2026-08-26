package walkthrough

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/action"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/llm"
)

// fakeDrafter returns scripted replies in order (one per Draft call).
type fakeDrafter struct {
	replies []string
	calls   int
}

func (f *fakeDrafter) Draft(ctx context.Context, model, sys, user string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error) {
	i := f.calls
	f.calls++
	if i >= len(f.replies) {
		return "", llm.Usage{}, nil
	}
	return f.replies[i], llm.Usage{CostUSD: 0.001, PromptTokens: 10, CompletionTokens: 5}, nil
}

func TestPlannerParsesValidAction(t *testing.T) {
	f := &fakeDrafter{replies: []string{`{"type":"click","selector":"#go","reason":"proceed"}`}}
	var usage llm.Usage
	p := &Planner{LLM: f, Model: "m", OnUsage: func(u llm.Usage) { usage.CostUSD += u.CostUSD }}
	a, err := p.NextAction(context.Background(), crawler.DriveState{Goal: "sign up"})
	if err != nil {
		t.Fatalf("NextAction: %v", err)
	}
	if a.Type != action.Click || a.Selector != "#go" {
		t.Fatalf("parsed wrong: %+v", a)
	}
	if usage.CostUSD <= 0 {
		t.Error("OnUsage was not called")
	}
}

func TestPlannerRetriesThenDegrades(t *testing.T) {
	// First reply is garbage, retry also garbage → degrade to a safe scroll (never
	// an error, never a panic).
	f := &fakeDrafter{replies: []string{"not json", "still not json"}}
	p := &Planner{LLM: f, Model: "m"}
	a, err := p.NextAction(context.Background(), crawler.DriveState{})
	if err != nil {
		t.Fatalf("degrade must not error: %v", err)
	}
	if a.Type != action.Scroll {
		t.Fatalf("expected scroll fallback, got %+v", a)
	}
	if f.calls != 2 {
		t.Fatalf("expected one retry (2 calls), got %d", f.calls)
	}
}

func TestPlannerRetryRecovers(t *testing.T) {
	f := &fakeDrafter{replies: []string{"garbage", `{"type":"finish","reason":"done"}`}}
	p := &Planner{LLM: f, Model: "m"}
	a, err := p.NextAction(context.Background(), crawler.DriveState{})
	if err != nil || a.Type != action.Finish {
		t.Fatalf("retry should recover to finish: %+v err=%v", a, err)
	}
}

// A script/eval field in the reply must NOT be honored — DisallowUnknownFields
// rejects it, so the planner degrades rather than executing an injected key.
func TestPlannerRejectsInjectedScript(t *testing.T) {
	f := &fakeDrafter{replies: []string{
		`{"type":"click","selector":"#x","script":"fetch('//evil')"}`,
		`{"type":"click","selector":"#x","eval":"x"}`,
	}}
	p := &Planner{LLM: f, Model: "m"}
	a, err := p.NextAction(context.Background(), crawler.DriveState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Type == action.Click {
		t.Fatal("an action carrying an injected script field must be rejected, not executed")
	}
}

func TestPlannerCtxCancelFails(t *testing.T) {
	f := &fakeDrafter{replies: []string{`{"type":"click","selector":"#x"}`}}
	p := &Planner{LLM: f, Model: "m"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.NextAction(ctx, crawler.DriveState{}); err == nil {
		t.Fatal("a cancelled context must fail the planner (fails the whole pass)")
	}
}

func TestDrivePromptContent(t *testing.T) {
	st := crawler.DriveState{
		Goal: "create an account", StepIdx: 2, MaxActions: 20, URL: "https://x.test/signup",
		Digest: crawler.InteractiveDigest{Elements: []crawler.InteractiveElement{
			{Tag: "button", Name: "Sign up", Selector: "button#su"},
		}},
		History:     []crawler.StepRecord{{Idx: 0, Action: action.Action{Type: action.Click, Selector: "#a"}, Outcome: "ok"}},
		LastOutcome: "ok",
	}
	got := DrivePrompt(st)
	for _, want := range []string{"create an account", "step 3 of", "https://x.test/signup", "button#su", "LAST OUTCOME"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}
