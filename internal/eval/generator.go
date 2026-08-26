package eval

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/notes"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// DefaultConcurrency bounds how many (page,persona) cells run in parallel. Reuses
// the P3 notes concurrency cap.
const DefaultConcurrency = notes.DefaultConcurrency

// Drafter is the vision-LLM call the generator depends on (satisfied by
// *llm.Client). Injectable so tests can stub the model. Same shape as
// notes.Drafter. The variadic opts let a single call request a larger completion
// budget (the synthesis pass) without changing the per-page calls.
type Drafter interface {
	Draft(ctx context.Context, model, systemPrompt, userPrompt string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error)
}

// DefaultSynthMaxTokens is the completion budget for the run-level synthesis call
// when none is configured. A per-page verdict fits the small default cap, but the
// ranked ≤8-item synthesis JSON overflows it and truncates — so synthesis gets its
// own, larger budget (see config.AUDITLOOP_LLM_SYNTH_MAX_TOKENS).
const DefaultSynthMaxTokens = 3000

// DefaultEvalMaxTokens is the completion budget for the per-page GENERATION +
// VERIFICATION calls when none is configured. A verbose verdict (several
// blockers/frictions, each with issue/selector/evidence, + a top_fix) overflows the
// small per-notes cap (1024) and truncates mid-JSON → ParseEvaluation fails and the
// cell's findings are lost. Richer than a notes draft but far smaller than the
// run-level synthesis, so it sits in the middle tier (notes 1024 < eval 2000 < synth
// 3000). See config.AUDITLOOP_LLM_EVAL_MAX_TOKENS.
const DefaultEvalMaxTokens = 2000

// Generator runs the async persona-walkthrough evaluation pass for a completed run.
type Generator struct {
	DB          *db.DB
	Store       storage.Store
	LLM         Drafter
	Model       string // the single curated model used for Phase 1 (personas are the axis)
	Concurrency int
	// SynthMaxTokens is the completion budget for the run-level synthesis call only
	// (0 → DefaultSynthMaxTokens).
	SynthMaxTokens int
	// GenMaxTokens is the completion budget for the per-page generation AND
	// verification calls (0 → DefaultEvalMaxTokens). Larger than the notes cap so a
	// verbose per-page verdict doesn't truncate mid-JSON; smaller than the synthesis
	// budget.
	GenMaxTokens int
}

// Options tunes a pass.
type Options struct {
	Job    string // free-text task/job the walkthrough evaluates toward
	Verify bool   // run the anti-vagueness verification pass (default on via the handler)
}

// New builds a Generator bound to a single model (the first curated model).
func New(database *db.DB, store storage.Store, client Drafter, model string) *Generator {
	return &Generator{DB: database, Store: store, LLM: client, Model: model, Concurrency: DefaultConcurrency, SynthMaxTokens: DefaultSynthMaxTokens, GenMaxTokens: DefaultEvalMaxTokens}
}

// pageWork is one logical page (URL) in flow order to evaluate.
type pageWork struct {
	CanonicalPageID string
	Grounding       Grounding
	ShotKeys        []shotKey
	// A11yDigestKey is the storage key of the page's DOM/a11y digest (set by plan;
	// the digest itself is loaded per-page in Run so plan/CountUnits do no I/O). ""
	// ⇒ no digest ⇒ screenshot-only, no deterministic drop (backward-compat).
	A11yDigestKey string
}

type shotKey struct {
	Label string
	Key   string
}

// CountUnits returns the job total for progress tracking: pages × personas cells,
// plus one synthesis unit. Zero-page runs still count the synthesis unit.
func (g *Generator) CountUnits(runID string, personas []string) (int, error) {
	works, err := g.plan(runID)
	if err != nil {
		return 0, err
	}
	return len(works)*len(personas) + 1, nil
}

// Run executes the pass: for each page × persona, generate → (optionally) verify →
// persist the structured verdict; then ONE run-level synthesis call. Per-cell
// failures store an error and continue (non-fatal, degrade). It updates run
// progress and finalizes the eval-job status. Only ctx-cancellation returns a
// non-nil error.
func (g *Generator) Run(ctx context.Context, runID string, personas []string, opts Options) error {
	works, err := g.plan(runID)
	if err != nil {
		g.finish(runID, db.EvalFailed)
		return err
	}
	total := len(works)*len(personas) + 1
	// Thread the user's job into every per-page grounding so the generation AND
	// verification prompts are task-grounded — not just the run-level synthesis.
	// Without this the per-(page,persona) evaluation (which produces the findings)
	// silently falls back to the default job and ignores the task the user typed.
	for i := range works {
		works[i].Grounding.Job = opts.Job
	}
	_ = g.DB.UpdateEvalProgress(runID, 0)

	var done int64
	conc := g.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for _, w := range works {
		select {
		case <-ctx.Done():
			g.finish(runID, db.EvalFailed)
			return ctx.Err()
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(w pageWork) {
			defer wg.Done()
			defer func() { <-sem }()

			images := g.loadImages(ctx, w.ShotKeys)
			// Load the DOM/a11y digest artifact once per page (like loadImages) and
			// thread it into grounding — nil-safe: a missing/absent/corrupt digest
			// leaves it nil → screenshot-only behavior + no deterministic drop.
			w.Grounding.A11yDigest = g.loadDigest(ctx, w.A11yDigestKey)
			for _, personaID := range personas {
				persona, ok := PersonaByID(personaID)
				if !ok {
					continue // guarded at the handler; skip defensively
				}
				g.evalCell(ctx, runID, w, persona, images, opts)
				n := atomic.AddInt64(&done, 1)
				_ = g.DB.UpdateEvalProgress(runID, int(n))
			}
		}(w)
	}
	wg.Wait()

	select {
	case <-ctx.Done():
		g.finish(runID, db.EvalFailed)
		return ctx.Err()
	default:
	}

	// Synthesis pass (run-level "story") over the verified findings.
	g.synthesize(ctx, runID, opts.Job)
	_ = g.DB.UpdateEvalProgress(runID, total)
	g.finish(runID, db.EvalDone)
	log.Printf("eval: run %s done (%d pages × %d personas + synthesis)", runID, len(works), len(personas))
	return nil
}

// evalCell runs generation (+ optional verification) for one (page,persona) and
// persists the row. Failures degrade in place (store an error, keep going).
func (g *Generator) evalCell(ctx context.Context, runID string, w pageWork, persona Persona, images []llm.Image, opts Options) {
	t0 := time.Now()
	var totalUsage llm.Usage
	status := "ok"
	var comprehension, findingsJSON, errMsg string

	// The per-page verdict (blockers/frictions with issue/selector/evidence + a
	// top_fix) overflows the small per-notes cap and truncates mid-JSON → the parse
	// fails and the cell's findings are lost. Give both the generation AND the
	// verification call (same structured shape → same overflow risk) their own,
	// larger budget.
	genMax := g.GenMaxTokens
	if genMax <= 0 {
		genMax = DefaultEvalMaxTokens
	}

	genText, genUsage, err := g.LLM.Draft(ctx, g.Model, GenSystemPrompt, w.Grounding.GenUserPrompt(persona), images, llm.WithMaxTokens(genMax))
	totalUsage = addUsage(totalUsage, genUsage)
	if err != nil {
		status, errMsg = "error", err.Error()
		totalUsage = llm.Usage{}
	} else {
		pe, perr := ParseEvaluation(genText)
		if perr != nil {
			status, errMsg = "error", perr.Error()
		} else {
			// Verification pass (bounded to one extra call per cell).
			if opts.Verify {
				draftJSON, _ := json.Marshal(pe)
				vText, vUsage, verr := g.LLM.Draft(ctx, g.Model, VerifySystemPrompt, VerifyUserPrompt(w.Grounding, persona, string(draftJSON)), images, llm.WithMaxTokens(genMax))
				if verr == nil {
					totalUsage = addUsage(totalUsage, vUsage)
					pe = applyVerification(pe, vText)
				} else {
					log.Printf("eval: verify %s/%s: %v (keeping unverified draft)", w.CanonicalPageID, persona.ID, verr)
				}
			}
			// Deterministic DOM-grounded gate (§5.2): drop objective a11y findings the
			// captured digest contradicts (missing-label refuted by a resolved label,
			// not-operable refuted by an <a>/button/focusable element). No-op when no
			// digest is present (pre-0060 / pushed run) — strictly opt-in on digest.
			// Plus selector grounding: a mechanical a11y claim citing a selector the
			// digest does not list is re-anchored or dropped (ground.go). applyDOMGate
			// owns the two stages' ORDER — see its doc comment.
			pe = applyDOMGate(pe, w.Grounding.A11yDigest)
			comprehension = pe.Comprehension
			b, _ := json.Marshal(pe)
			findingsJSON = string(b)
		}
	}
	if status == "error" {
		totalUsage = llm.Usage{}
		log.Printf("eval: run %s page %s persona %s: %s", runID, w.CanonicalPageID, persona.ID, errMsg)
	}

	if err := g.DB.SavePageEvaluation(w.CanonicalPageID, runID, persona.ID, findingsJSON, comprehension, errMsg,
		totalUsage.CostUSD, totalUsage.PromptTokens, totalUsage.CompletionTokens); err != nil {
		log.Printf("eval: save %s/%s: %v", w.CanonicalPageID, persona.ID, err)
	}
	metrics.EvalGenerated.WithLabelValues(persona.ID, status).Inc()
	metrics.EvalDuration.Observe(time.Since(t0).Seconds())
	if status == "ok" {
		if err := g.DB.AddEvalCost(runID, totalUsage.CostUSD, totalUsage.PromptTokens, totalUsage.CompletionTokens); err != nil {
			log.Printf("eval: add cost %s/%s: %v", w.CanonicalPageID, persona.ID, err)
		}
		metrics.EvalCostUSD.WithLabelValues(persona.ID).Add(totalUsage.CostUSD)
		metrics.EvalPromptTokens.WithLabelValues(persona.ID).Add(float64(totalUsage.PromptTokens))
		metrics.EvalCompletionTokens.WithLabelValues(persona.ID).Add(float64(totalUsage.CompletionTokens))
	}
}

// synthesize runs the single run-level synthesis call over the stored verdicts and
// persists the ranked "story". A failure is non-fatal (logged; the pass still
// completes with per-cell findings).
func (g *Generator) synthesize(ctx context.Context, runID, job string) {
	rows, err := g.DB.ListPageEvaluations(runID)
	if err != nil {
		log.Printf("eval: synthesize load %s: %v", runID, err)
		return
	}
	// Map page id → URL for labeling.
	urlByPage := map[string]string{}
	if pages, err := g.DB.ListPages(runID); err == nil {
		for _, p := range pages {
			urlByPage[p.ID] = p.URL
		}
	}
	var cells []SynthCell
	for _, r := range rows {
		if r.Error != "" || r.FindingsJSON == "" {
			continue
		}
		var pe report.PageEvaluation
		if err := json.Unmarshal([]byte(r.FindingsJSON), &pe); err != nil {
			continue
		}
		if len(pe.Blockers) == 0 && len(pe.Frictions) == 0 && pe.TopFix == nil {
			continue
		}
		cells = append(cells, SynthCell{URL: urlByPage[r.PageID], Persona: r.Persona, Eval: pe})
	}
	if len(cells) == 0 {
		return // nothing verified to synthesize
	}
	// Bound the synthesis PROMPT on large runs (e.g. 50 pages × 4 personas = 200
	// cells) so its prompt-token cost + context size stay bounded. NOTE: cells come
	// from ListPageEvaluations ordered by page_id (a random UUID), so the retained
	// subset is deterministic-but-ARBITRARY, not flow-ordered — synthesizing by true
	// funnel position is a follow-up. Log the drop so a bounded synthesis on a big run
	// never silently reads as "covered everything".
	if len(cells) > MaxSynthCells {
		log.Printf("eval: synthesize run %s: %d cells exceed MaxSynthCells=%d — synthesizing over the first %d only (arbitrary by page_id order)", runID, len(cells), MaxSynthCells, MaxSynthCells)
		cells = cells[:MaxSynthCells]
	}
	// The synthesis output (a ranked ≤8-item JSON list, each item rich) overflows the
	// per-page completion cap and truncates mid-JSON → ParseSynthesis fails and the
	// story is lost. Give this one call its own, larger budget.
	synthMax := g.SynthMaxTokens
	if synthMax <= 0 {
		synthMax = DefaultSynthMaxTokens
	}
	text, usage, err := g.LLM.Draft(ctx, g.Model, SynthSystemPrompt, SynthUserPrompt(job, cells), nil, llm.WithMaxTokens(synthMax))
	if err != nil {
		log.Printf("eval: synthesize call %s: %v", runID, err)
		return
	}
	items, perr := ParseSynthesis(text)
	if perr != nil {
		log.Printf("eval: synthesize parse %s: %v", runID, perr)
		return
	}
	b, _ := json.Marshal(items)
	if err := g.DB.SetRunEvalSynthesis(runID, string(b)); err != nil {
		log.Printf("eval: synthesize save %s: %v", runID, err)
		return
	}
	if err := g.DB.AddEvalCost(runID, usage.CostUSD, usage.PromptTokens, usage.CompletionTokens); err == nil {
		metrics.EvalCostUSD.WithLabelValues("_synthesis").Add(usage.CostUSD)
		metrics.EvalPromptTokens.WithLabelValues("_synthesis").Add(float64(usage.PromptTokens))
		metrics.EvalCompletionTokens.WithLabelValues("_synthesis").Add(float64(usage.CompletionTokens))
	}
}

// plan groups a run's pages by URL into per-page work units in FLOW ORDER (page
// creation/id order — for pushed runs this is the producer's push order). Each
// unit carries the deterministic grounding (a11y rule ids/counts, perf, layout
// smells, console/network counts) for the URL.
func (g *Generator) plan(runID string) ([]pageWork, error) {
	pages, err := g.DB.ListPages(runID)
	if err != nil {
		return nil, err
	}
	type acc struct {
		w         pageWork
		hasDesk   bool
		viewports []string
		perf      *report.Perf
		a11yRules map[string]bool
		a11yNodes map[string][]string
		layout    map[string]bool
	}
	byURL := map[string]*acc{}
	var order []string

	for _, p := range pages {
		a, ok := byURL[p.URL]
		if !ok {
			a = &acc{w: pageWork{Grounding: Grounding{URL: p.URL}}, a11yRules: map[string]bool{}, a11yNodes: map[string][]string{}, layout: map[string]bool{}}
			byURL[p.URL] = a
			order = append(order, p.URL)
		}
		a.viewports = append(a.viewports, p.Viewport)

		if a.w.CanonicalPageID == "" || (p.Viewport == "desktop" && !a.hasDesk) {
			a.w.CanonicalPageID = p.ID
			if p.Viewport == "desktop" {
				a.hasDesk = true
			}
		}
		if p.ScreenshotKey != "" {
			a.w.ShotKeys = append(a.w.ShotKeys, shotKey{Label: p.Viewport, Key: p.ScreenshotKey})
		}
		// The DOM/a11y digest is per page_slug (viewport-independent) — take the first
		// non-empty key (both viewports of a URL reference the same artifact).
		if a.w.A11yDigestKey == "" && p.A11yDigestKey != "" {
			a.w.A11yDigestKey = p.A11yDigestKey
		}

		// Aggregate deterministic counts (max across viewports — worst wins).
		gr := &a.w.Grounding
		gr.A11yCount = max(gr.A11yCount, p.AxeViolationCount)
		gr.ConsoleFirst = max(gr.ConsoleFirst, p.ConsoleFirstPartyCount)
		gr.ConsoleThird = max(gr.ConsoleThird, p.ConsoleThirdPartyCount)
		gr.NetworkFirst = max(gr.NetworkFirst, p.NetworkFirstPartyCount)
		gr.NetworkThird = max(gr.NetworkThird, p.NetworkThirdPartyCount)

		// Perf: prefer a viewport that actually captured perf (mobile is where the
		// crawl gates most perf findings; either is fine as grounding).
		if a.perf == nil && (p.LCPMs > 0 || p.CLS > 0 || p.TBTMs > 0 || p.WeightBytes > 0 || p.ReqCount > 0) {
			a.perf = &report.Perf{LCPMs: p.LCPMs, CLS: p.CLS, TBTMs: p.TBTMs, WeightBytes: p.WeightBytes, ReqCount: p.ReqCount}
		}

		// Rule ids + layout smells from the page's findings.
		if findings, err := g.DB.ListFindings(p.ID); err == nil {
			for _, f := range findings {
				switch f.Type {
				case db.FindingA11y:
					if id := jsonField(f.Detail, "id"); id != "" {
						a.a11yRules[id] = true
						// Surface up to a few offending node selectors (already stored in
						// the raw axe violation detail) so grounding says WHERE, not just
						// which rule — the persona no longer has to guess selectors.
						if _, seen := a.a11yNodes[id]; !seen {
							if sels := axeNodeSelectors(f.Detail, 3); len(sels) > 0 {
								a.a11yNodes[id] = sels
							}
						}
					}
				case db.FindingLayout:
					if s := jsonField(f.Detail, "smell"); s != "" {
						a.layout[s] = true
					}
				}
			}
		}
	}

	works := make([]pageWork, 0, len(order))
	for i, u := range order {
		a := byURL[u]
		gr := &a.w.Grounding
		gr.Viewports = a.viewports
		gr.FlowPos = i + 1
		gr.FlowTotal = len(order)
		if i > 0 {
			gr.PrevURL = order[i-1]
		}
		if i < len(order)-1 {
			gr.NextURL = order[i+1]
		}
		gr.Perf = a.perf
		gr.A11yRuleIDs = sortedKeys(a.a11yRules)
		if len(a.a11yNodes) > 0 {
			gr.A11yRuleNodes = a.a11yNodes
		}
		gr.LayoutSmells = sortedKeys(a.layout)
		// desktop-first screenshot order (stable, human-sensible).
		sort.SliceStable(a.w.ShotKeys, func(i, j int) bool {
			return a.w.ShotKeys[i].Label == "desktop" && a.w.ShotKeys[j].Label != "desktop"
		})
		works = append(works, a.w)
	}
	return works, nil
}

func (g *Generator) loadImages(ctx context.Context, keys []shotKey) []llm.Image {
	var out []llm.Image
	for _, sk := range keys {
		b, err := g.fetch(ctx, sk.Key)
		if err != nil {
			log.Printf("eval: fetch shot %s: %v", sk.Key, err)
			continue
		}
		out = append(out, llm.Image{Label: sk.Label, PNG: b})
	}
	return out
}

// loadDigest fetches + parses the page's DOM/a11y digest artifact. It is nil-safe
// and best-effort: an empty key, a fetch error, or a corrupt/empty payload all yield
// nil → the evaluator degrades to screenshot-only and the deterministic gate no-ops.
func (g *Generator) loadDigest(ctx context.Context, key string) *report.A11yDigest {
	if key == "" {
		return nil
	}
	b, err := g.fetch(ctx, key)
	if err != nil {
		log.Printf("eval: fetch a11y digest %s: %v", key, err)
		return nil
	}
	// Belt-and-suspenders: the payload is UNTRUSTED (page-authored) and capped at capture
	// time, but an older/foreign artifact might exceed it — over-cap ⇒ treat as absent.
	if len(b) > report.MaxA11yDigestBytes {
		log.Printf("eval: a11y digest %s: %d bytes over cap %d — ignoring", key, len(b), report.MaxA11yDigestBytes)
		return nil
	}
	var d report.A11yDigest
	if err := json.Unmarshal(b, &d); err != nil {
		log.Printf("eval: parse a11y digest %s: %v", key, err)
		return nil
	}
	if d.IsEmpty() {
		return nil
	}
	return &d
}

func (g *Generator) fetch(ctx context.Context, key string) ([]byte, error) {
	rc, err := g.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func (g *Generator) finish(runID, status string) {
	if err := g.DB.FinishEvalJob(runID, status); err != nil {
		log.Printf("eval: finish job %s=%s: %v", runID, status, err)
	}
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		CostUSD:          a.CostUSD + b.CostUSD,
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
	}
}

// jsonField extracts a top-level string field from a JSON object (best effort; ""
// on any error).
func jsonField(jsonStr, field string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return ""
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// axeNodeSelectors pulls up to max offending node selectors from a stored axe
// violation detail (nodes[].target[0]). Best effort — "" fields are skipped, and a
// non-axe/legacy detail yields nil.
func axeNodeSelectors(detail string, maxN int) []string {
	var v struct {
		Nodes []struct {
			Target []string `json:"target"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(detail), &v); err != nil {
		return nil
	}
	var out []string
	for _, n := range v.Nodes {
		if len(n.Target) == 0 {
			continue
		}
		s := strings.TrimSpace(n.Target[0])
		if s == "" {
			continue
		}
		out = append(out, s)
		if len(out) >= maxN {
			break
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
