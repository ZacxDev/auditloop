package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// fakeDrafter routes each call by system prompt to a canned response
// (generation / verification / synthesis) and records what it saw. It can be told
// to fail generation for a persona whose 'cares' text contains failCaresSubstr.
type fakeDrafter struct {
	mu              sync.Mutex
	failCaresSubstr string
	cost            float64
	calls           []fakeCall
}

type fakeCall struct {
	Kind      string // gen|verify|synth
	System    string
	User      string
	NumImages int
	MaxTokens int // the per-call max_tokens override requested (0 = client default)
}

func (f *fakeDrafter) Draft(ctx context.Context, model, system, user string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error) {
	kind := "gen"
	switch {
	case strings.Contains(system, "fact-checker"):
		kind = "verify"
	case strings.Contains(system, "product lead synthesizing"):
		kind = "synth"
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Kind: kind, System: system, User: user, NumImages: len(images), MaxTokens: llm.ResolveMaxTokens(opts...)})
	f.mu.Unlock()

	cost := f.cost
	if cost == 0 {
		cost = 0.001
	}
	usage := llm.Usage{CostUSD: cost, PromptTokens: 500, CompletionTokens: 100}

	switch kind {
	case "gen":
		if f.failCaresSubstr != "" && strings.Contains(user, f.failCaresSubstr) {
			return "", llm.Usage{}, fmt.Errorf("simulated generation failure")
		}
		// Two blockers + one friction.
		return `{"comprehension":"unclear",
			"blockers":[
				{"issue":"real blocker","selector":"#a","evidence":"missing"},
				{"issue":"speculative","selector":"#b","evidence":"maybe"}],
			"frictions":[{"issue":"dense","selector":"p","evidence":"lots"}],
			"top_fix":{"selector":"#a","change":"add a CTA","rationale":"trust","impact":"high"}}`, usage, nil
	case "verify":
		// Keep only the substantiated blocker; drop the friction.
		return `{"comprehension":"unclear",
			"blockers":[{"issue":"real blocker","selector":"#a","evidence":"missing","verified":true}],
			"frictions":[],
			"top_fix":{"selector":"#a","change":"add a CTA","rationale":"trust","impact":"high"}}`, usage, nil
	default: // synth
		return `{"improvements":[{"title":"Add a prominent CTA","rationale":"blocks signup","impact":"high","affected_urls":["https://acme.test/"],"affected_personas":["skeptical-evaluator"]}]}`, usage, nil
	}
}

func (f *fakeDrafter) countKind(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

func setup(t *testing.T) (*db.DB, *storage.FS) {
	t.Helper()
	d, err := db.Open("sqlite", filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d, st
}

var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

// seedTwoPageRun creates a done run with 2 URLs × 2 viewports (4 page rows) with
// screenshots in the store and an a11y finding on the home page.
func seedTwoPageRun(t *testing.T, d *db.DB, st *storage.FS) string {
	t.Helper()
	tgt, _ := d.CreateTarget("u", "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := d.CreateRun("u", tgt.ID)
	urls := []string{"https://acme.test/", "https://acme.test/about"}
	for _, u := range urls {
		for _, vp := range []string{"desktop", "mobile"} {
			key := storage.ScreenshotKey("acme", run.ID, storage.PageSlug(u), vp)
			if err := st.Put(context.Background(), key, "image/png", bytes.NewReader(tinyPNG), int64(len(tinyPNG))); err != nil {
				t.Fatal(err)
			}
			pid, _ := d.InsertPage(&db.Page{RunID: run.ID, URL: u, Viewport: vp, ScreenshotKey: key, AxeViolationCount: 2})
			if u == urls[0] {
				_, _ = d.InsertFinding(&db.Finding{PageID: pid, Type: db.FindingA11y, Severity: "serious", Detail: `{"id":"image-alt"}`})
			}
		}
	}
	_ = d.FinishRun(run.ID, db.RunDone, "{}", "")
	return run.ID
}

func TestPersonaPass(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &fakeDrafter{}
	g := New(d, st, fake, "test-model")

	personas := []string{"first-time-nontechnical", "skeptical-evaluator"}
	total, err := g.CountUnits(runID, personas)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2*2+1 { // 2 pages × 2 personas + synthesis
		t.Fatalf("CountUnits = %d, want 5", total)
	}

	if err := g.Run(context.Background(), runID, personas, Options{Job: "sign up", Verify: true}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// One row per (page, persona) = 4.
	rows, _ := d.ListPageEvaluations(runID)
	if len(rows) != 4 {
		t.Fatalf("expected 4 page_evaluation rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Error != "" || r.FindingsJSON == "" {
			t.Errorf("row %s/%s should be a clean verdict: err=%q", r.PageID, r.Persona, r.Error)
		}
		if r.Comprehension != "unclear" {
			t.Errorf("comprehension not persisted: %q", r.Comprehension)
		}
		// The verification pass filtered the 2 draft blockers down to 1 verified.
		var pe report.PageEvaluation
		if err := json.Unmarshal([]byte(r.FindingsJSON), &pe); err != nil {
			t.Fatal(err)
		}
		if len(pe.Blockers) != 1 || !pe.Blockers[0].Verified {
			t.Errorf("verification should leave exactly 1 verified blocker: %+v", pe.Blockers)
		}
		if len(pe.Frictions) != 0 {
			t.Errorf("unverified friction should be dropped: %+v", pe.Frictions)
		}
		// Per-cell cost = gen + verify = 0.002.
		if r.CostUSD < 0.0019 || r.CostUSD > 0.0021 {
			t.Errorf("per-cell cost = %v, want ~0.002 (gen+verify)", r.CostUSD)
		}
	}

	// Both viewports were sent to every gen + verify call, AND the user's job
	// ("sign up") reached the per-page generation + verification prompts (not just
	// the synthesis pass) — the whole point of a *task-grounded* walkthrough.
	fake.mu.Lock()
	var genWithJob, verifyWithJob int
	for _, c := range fake.calls {
		if c.Kind != "synth" && c.NumImages != 2 {
			t.Errorf("%s call sent %d images, want 2 (desktop+mobile)", c.Kind, c.NumImages)
		}
		if strings.Contains(c.User, "sign up") {
			switch c.Kind {
			case "gen":
				genWithJob++
			case "verify":
				verifyWithJob++
			}
		}
	}
	fake.mu.Unlock()
	if genWithJob != 4 {
		t.Errorf("the user job 'sign up' reached %d/4 generation prompts, want all 4 (task must ground the per-page eval, not just synthesis)", genWithJob)
	}
	if verifyWithJob != 4 {
		t.Errorf("the user job 'sign up' reached %d/4 verification prompts, want all 4", verifyWithJob)
	}
	if fake.countKind("gen") != 4 || fake.countKind("verify") != 4 || fake.countKind("synth") != 1 {
		t.Errorf("call counts gen=%d verify=%d synth=%d, want 4/4/1", fake.countKind("gen"), fake.countKind("verify"), fake.countKind("synth"))
	}

	// Synthesis produced + persisted.
	run, _ := d.GetRunByID(runID)
	if run.EvalStatus != db.EvalDone {
		t.Errorf("eval status = %q, want done", run.EvalStatus)
	}
	if run.EvalDone != total {
		t.Errorf("eval done = %d, want %d", run.EvalDone, total)
	}
	var synth []report.EvalSynthItem
	if err := json.Unmarshal([]byte(run.EvalSynthesisJSON), &synth); err != nil || len(synth) != 1 {
		t.Errorf("synthesis not persisted: %q (%v)", run.EvalSynthesisJSON, err)
	}
	// Cost accumulated: 4 cells × 0.002 + synthesis 0.001 = 0.009.
	if run.EvalCostUSD < 0.0089 || run.EvalCostUSD > 0.0091 {
		t.Errorf("run eval_cost_usd = %v, want ~0.009", run.EvalCostUSD)
	}
}

// TestPerCallTokenBudgets asserts the three-tier completion budget: the per-page
// generation AND verification calls opt into the eval budget (GenMaxTokens — a
// verbose per-page verdict overflows the 1024 notes cap and truncates), while the
// run-level synthesis call opts into its own, larger SynthMaxTokens budget. Both
// are the fix for the "unexpected EOF"/"unexpected end of JSON input" truncation on
// multi-page runs.
func TestPerCallTokenBudgets(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &fakeDrafter{}
	g := New(d, st, fake, "test-model")
	// New defaults both budgets; set distinctive values to prove each field flows
	// through to the right calls (gen/verify vs synthesis).
	g.GenMaxTokens = 2222
	g.SynthMaxTokens = 4242

	if err := g.Run(context.Background(), runID, []string{"skeptical-evaluator"}, Options{Job: "sign up", Verify: true}); err != nil {
		t.Fatalf("run: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var sawGen, sawVerify, sawSynth bool
	for _, c := range fake.calls {
		switch c.Kind {
		case "gen":
			sawGen = true
			if c.MaxTokens != 2222 {
				t.Errorf("generation call requested max_tokens=%d, want the eval budget 2222", c.MaxTokens)
			}
		case "verify":
			sawVerify = true
			if c.MaxTokens != 2222 {
				t.Errorf("verification call requested max_tokens=%d, want the eval budget 2222 (same overflow risk as generation)", c.MaxTokens)
			}
		case "synth":
			sawSynth = true
			if c.MaxTokens != 4242 {
				t.Errorf("synthesis call requested max_tokens=%d, want the synth budget 4242", c.MaxTokens)
			}
		}
	}
	if !sawGen || !sawVerify {
		t.Fatalf("expected gen (%v) and verify (%v) calls", sawGen, sawVerify)
	}
	if !sawSynth {
		t.Fatal("expected a synthesis call")
	}
}

// TestNewDefaultsBudgets guards the defaults so a caller that never sets the fields
// still gets budgets large enough to fit a verbose per-page verdict (eval) and the
// ranked synthesis JSON (synth).
func TestNewDefaultsBudgets(t *testing.T) {
	d, st := setup(t)
	g := New(d, st, &fakeDrafter{}, "test-model")
	if g.SynthMaxTokens != DefaultSynthMaxTokens {
		t.Errorf("New should default SynthMaxTokens to %d, got %d", DefaultSynthMaxTokens, g.SynthMaxTokens)
	}
	if g.GenMaxTokens != DefaultEvalMaxTokens {
		t.Errorf("New should default GenMaxTokens to %d, got %d", DefaultEvalMaxTokens, g.GenMaxTokens)
	}
}

func TestPassWithoutVerify(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	fake := &fakeDrafter{}
	g := New(d, st, fake, "test-model")

	if err := g.Run(context.Background(), runID, []string{"first-time-nontechnical"}, Options{Verify: false}); err != nil {
		t.Fatal(err)
	}
	if fake.countKind("verify") != 0 {
		t.Errorf("verify disabled but %d verify calls made", fake.countKind("verify"))
	}
	// Without verification the draft findings are kept as-is (2 blockers, 1 friction).
	rows, _ := d.ListPageEvaluations(runID)
	for _, r := range rows {
		var pe report.PageEvaluation
		_ = json.Unmarshal([]byte(r.FindingsJSON), &pe)
		if len(pe.Blockers) != 2 || len(pe.Frictions) != 1 {
			t.Errorf("no-verify should keep all draft findings: %+v", pe)
		}
	}
}

func TestPassDegradesOnPerCellFailure(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	// Fail generation for the returning-power-user persona.
	fake := &fakeDrafter{failCaresSubstr: "frequent, expert user"}
	g := New(d, st, fake, "test-model")

	if err := g.Run(context.Background(), runID, []string{"returning-power-user", "skeptical-evaluator"}, Options{Verify: true}); err != nil {
		t.Fatalf("pass should not fail wholesale: %v", err)
	}
	rows, _ := d.ListPageEvaluations(runID)
	if len(rows) != 4 {
		t.Fatalf("still expect 4 rows, got %d", len(rows))
	}
	var powerErr, skepticalOK int
	for _, r := range rows {
		switch r.Persona {
		case "returning-power-user":
			if r.Error != "" {
				powerErr++
			}
			if r.CostUSD != 0 {
				t.Errorf("failed cell should have cost 0, got %v", r.CostUSD)
			}
		case "skeptical-evaluator":
			if r.Error == "" && r.FindingsJSON != "" {
				skepticalOK++
			}
		}
	}
	if powerErr != 2 {
		t.Errorf("returning-power-user errored rows = %d, want 2", powerErr)
	}
	if skepticalOK != 2 {
		t.Errorf("skeptical-evaluator clean rows = %d, want 2", skepticalOK)
	}
	// The pass still completes.
	run, _ := d.GetRunByID(runID)
	if run.EvalStatus != db.EvalDone {
		t.Errorf("status = %q, want done (degraded, not failed)", run.EvalStatus)
	}
}

func TestPassCtxCancelFails(t *testing.T) {
	d, st := setup(t)
	runID := seedTwoPageRun(t, d, st)
	g := New(d, st, &fakeDrafter{}, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Run(ctx, runID, []string{"first-time-nontechnical"}, Options{}); err == nil {
		t.Error("a cancelled context should fail the pass")
	}
	run, _ := d.GetRunByID(runID)
	if run.EvalStatus != db.EvalFailed {
		t.Errorf("status = %q, want failed after ctx-cancel", run.EvalStatus)
	}
}
