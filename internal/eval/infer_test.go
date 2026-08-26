package eval

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/auditloop/internal/llm"
)

// inferFake is a Drafter that returns a canned inference reply and records the
// call (system/user prompt + image count).
type inferFake struct {
	mu       sync.Mutex
	reply    string
	err      error
	system   string
	user     string
	numImage int
}

func (f *inferFake) Draft(ctx context.Context, model, system, user string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error) {
	f.mu.Lock()
	f.system, f.user, f.numImage = system, user, len(images)
	f.mu.Unlock()
	if f.err != nil {
		return "", llm.Usage{}, f.err
	}
	return f.reply, llm.Usage{CostUSD: 0.0005, PromptTokens: 300, CompletionTokens: 60}, nil
}

func TestInferUserPromptIncludesDigest(t *testing.T) {
	urls := []string{"https://acme.test/", "https://acme.test/pricing", "https://acme.test/signup"}
	p := InferUserPrompt(urls[0], urls)
	for _, u := range urls {
		if !strings.Contains(p, u) {
			t.Errorf("prompt missing URL %q", u)
		}
	}
	if !strings.Contains(p, "Landing page URL") {
		t.Error("prompt should call out the landing page URL")
	}
	if !strings.Contains(InferSystemPrompt, "ONLY a single JSON object") {
		t.Error("system prompt must force structured JSON output")
	}
}

func TestParseInferredConfigValid(t *testing.T) {
	reply := "```json\n" + `{"product_summary":"A UX auditor","primary_job":"sign up and run an audit",
		"primary_cta":"Sign up","audiences":["skeptical-evaluator","first-time-nontechnical"]}` + "\n```"
	ic, err := ParseInferredConfig(reply)
	if err != nil {
		t.Fatal(err)
	}
	if ic.ProductSummary != "A UX auditor" || ic.PrimaryJob != "sign up and run an audit" || ic.PrimaryCTA != "Sign up" {
		t.Errorf("fields not parsed: %+v", ic)
	}
	if len(ic.Audiences) != 2 {
		t.Errorf("audiences = %v, want 2", ic.Audiences)
	}
}

func TestParseInferredConfigFiltersUnknownPersonas(t *testing.T) {
	reply := `{"product_summary":"x","primary_job":"y","primary_cta":"z",
		"audiences":["skeptical-evaluator","not-a-real-persona","","skeptical-evaluator"]}`
	ic, err := ParseInferredConfig(reply)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown + blank dropped, duplicate de-duped → exactly one curated id.
	if len(ic.Audiences) != 1 || ic.Audiences[0] != "skeptical-evaluator" {
		t.Errorf("audiences should filter to curated allowlist, got %v", ic.Audiences)
	}
}

func TestParseInferredConfigMalformedErrorsNotPanic(t *testing.T) {
	if _, err := ParseInferredConfig("the model refused and wrote prose only"); err == nil {
		t.Error("a reply with no JSON object should be an error, not a panic")
	}
}

func TestInferConfigSendsLandingScreenshot(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &inferFake{reply: `{"product_summary":"Acme UX auditor","primary_job":"sign up",
		"primary_cta":"Sign up","audiences":["skeptical-evaluator","bogus"]}`}
	g := New(d, st, fake, "test-model")

	ic, err := g.InferConfig(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if fake.numImage < 1 {
		t.Error("InferConfig must send the landing screenshot to the model")
	}
	if !strings.Contains(fake.user, "https://acme.test/") {
		t.Errorf("user prompt should carry the crawl URL digest, got %q", fake.user)
	}
	if ic.ProductSummary != "Acme UX auditor" || ic.PrimaryJob != "sign up" {
		t.Errorf("draft not returned: %+v", ic)
	}
	// audiences filtered to the allowlist ("bogus" dropped).
	if len(ic.Audiences) != 1 || ic.Audiences[0] != "skeptical-evaluator" {
		t.Errorf("audiences not filtered to allowlist: %v", ic.Audiences)
	}
}

func TestInferConfigDegradesOnBadReply(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &inferFake{reply: "no json here"}
	g := New(d, st, fake, "test-model")
	if _, err := g.InferConfig(context.Background(), runID); err == nil {
		t.Error("an unparseable LLM reply should return an error (degrade), not a panic")
	}
}

func TestInferConfigNoPages(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "Empty", "https://empty.test", []string{"empty.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	_ = d.FinishRun(run.ID, "done", "{}", "")
	g := New(d, st, &inferFake{reply: "{}"}, "test-model")
	if _, err := g.InferConfig(context.Background(), run.ID); err == nil {
		t.Error("a run with no pages should error cleanly")
	}
}
