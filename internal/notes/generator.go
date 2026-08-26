package notes

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/llm"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// DefaultConcurrency bounds how many pages are drafted in parallel. Each page in
// flight holds its (downscaled) screenshots and issues one LLM call per model.
const DefaultConcurrency = 3

// Drafter is the vision-LLM call the generator depends on (satisfied by
// *llm.Client). Injectable so tests can stub the model.
type Drafter interface {
	Draft(ctx context.Context, model, systemPrompt, userPrompt string, images []llm.Image, opts ...llm.DraftOption) (string, llm.Usage, error)
}

// Generator runs the async multi-model UX-notes pass for a completed run.
type Generator struct {
	DB          *db.DB
	Store       storage.Store
	LLM         Drafter
	Concurrency int
}

// New builds a Generator from a live OpenRouter client.
func New(database *db.DB, store storage.Store, client Drafter) *Generator {
	return &Generator{DB: database, Store: store, LLM: client, Concurrency: DefaultConcurrency}
}

// pageWork is one logical page (URL) to draft: its canonical page id (the row
// notes attach to), its grounding, and the screenshot keys to attach.
type pageWork struct {
	CanonicalPageID string
	Grounding       Grounding
	ShotKeys        []shotKey
}

type shotKey struct {
	Label string // viewport name
	Key   string
}

// CountUnits returns how many (page, model) LLM calls a pass over runID with the
// given models will make — the job's total for progress tracking. It groups the
// run's pages by URL (one call per URL per model). Zero when the run has no pages.
func (g *Generator) CountUnits(runID string, models []string) (int, error) {
	works, err := g.plan(runID, nil)
	if err != nil {
		return 0, err
	}
	return len(works) * len(models), nil
}

// Run executes the pass: for each page × each model, downscale+call the vision
// LLM and persist the note row. Per-(page,model) failures store an error and
// continue (non-fatal, degrade). It updates run progress and finalizes the
// notes-job status. Only ctx-cancellation returns a non-nil error.
func (g *Generator) Run(ctx context.Context, runID string, models []string) error {
	run, err := g.DB.GetRunByID(runID)
	if err != nil {
		g.finish(runID, db.NotesFailed)
		return err
	}
	diffByKey := parseDiff(run.DiffJSON)

	works, err := g.plan(runID, diffByKey)
	if err != nil {
		g.finish(runID, db.NotesFailed)
		return err
	}

	total := len(works) * len(models)
	_ = g.DB.UpdateNotesProgress(runID, 0)

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
			g.finish(runID, db.NotesFailed)
			return ctx.Err()
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(w pageWork) {
			defer wg.Done()
			defer func() { <-sem }()

			// Load the screenshots once per page and reuse across models.
			images := g.loadImages(ctx, w.ShotKeys)
			userPrompt := w.Grounding.UserPrompt()

			for _, model := range models {
				t0 := time.Now()
				text, usage, callErr := g.LLM.Draft(ctx, model, SystemPrompt, userPrompt, images)
				status := "ok"
				errMsg := ""
				if callErr != nil {
					status = "error"
					errMsg = callErr.Error()
					// A failed call has no usage → cost 0 (zero-value Usage).
					usage = llm.Usage{}
					log.Printf("notes: run %s page %s model %s: %v", runID, w.CanonicalPageID, model, callErr)
				}
				if err := g.DB.SavePageNoteDraft(w.CanonicalPageID, runID, model, text, errMsg,
					usage.CostUSD, usage.PromptTokens, usage.CompletionTokens); err != nil {
					log.Printf("notes: save %s/%s: %v", w.CanonicalPageID, model, err)
				}
				metrics.NotesGenerated.WithLabelValues(model, status).Inc()
				metrics.NotesDuration.Observe(time.Since(t0).Seconds())
				// Accumulate cost per-run + emit per-call cost metrics on success.
				if callErr == nil {
					if err := g.DB.AddNotesCost(runID, usage.CostUSD, usage.PromptTokens, usage.CompletionTokens); err != nil {
						log.Printf("notes: add cost %s/%s: %v", w.CanonicalPageID, model, err)
					}
					metrics.NotesCostUSD.WithLabelValues(model).Add(usage.CostUSD)
					metrics.NotesPromptTokens.WithLabelValues(model).Add(float64(usage.PromptTokens))
					metrics.NotesCompletionTokens.WithLabelValues(model).Add(float64(usage.CompletionTokens))
				}
				n := atomic.AddInt64(&done, 1)
				_ = g.DB.UpdateNotesProgress(runID, int(n))
			}
		}(w)
	}
	wg.Wait()

	_ = g.DB.UpdateNotesProgress(runID, total)
	g.finish(runID, db.NotesDone)
	log.Printf("notes: run %s done (%d pages × %d models = %d drafts)", runID, len(works), len(models), total)
	return nil
}

// plan groups a run's pages by URL into per-page work units. diffByKey (may be
// nil) maps "url\x00viewport" → the P2 ChangedPage so grounding can mention a
// regression/layout change.
func (g *Generator) plan(runID string, diffByKey map[string]report.ChangedPage) ([]pageWork, error) {
	pages, err := g.DB.ListPages(runID)
	if err != nil {
		return nil, err
	}

	type acc struct {
		w         pageWork
		hasDesk   bool
		viewports []string
	}
	byURL := map[string]*acc{}
	var order []string

	for _, p := range pages {
		a, ok := byURL[p.URL]
		if !ok {
			a = &acc{w: pageWork{Grounding: Grounding{URL: p.URL}}}
			byURL[p.URL] = a
			order = append(order, p.URL)
		}
		a.viewports = append(a.viewports, p.Viewport)

		// Canonical page id: prefer the desktop row; else the first seen. Notes
		// attach to this row. The deterministic counts are intentionally NOT fed to
		// the prompt (the vision pass is subjective-visual only — see Grounding).
		if a.w.CanonicalPageID == "" || (p.Viewport == "desktop" && !a.hasDesk) {
			a.w.CanonicalPageID = p.ID
			if p.Viewport == "desktop" {
				a.hasDesk = true
			}
		}
		if p.ScreenshotKey != "" {
			a.w.ShotKeys = append(a.w.ShotKeys, shotKey{Label: p.Viewport, Key: p.ScreenshotKey})
		}

		// Diff status (worst viewport wins).
		if diffByKey != nil {
			if cp, ok := diffByKey[p.URL+"\x00"+p.Viewport]; ok {
				gr := &a.w.Grounding
				if !gr.HasDiff || cp.DiffPct > gr.DiffPct {
					gr.HasDiff = true
					gr.DiffPct = cp.DiffPct
					gr.SizeChanged = cp.SizeChanged
					gr.IsRegression = cp.IsRegression()
				}
			}
		}
	}

	works := make([]pageWork, 0, len(order))
	for _, u := range order {
		a := byURL[u]
		a.w.Grounding.Viewports = a.viewports
		// Order screenshots desktop-first for a stable, human-sensible prompt.
		sort.SliceStable(a.w.ShotKeys, func(i, j int) bool {
			return a.w.ShotKeys[i].Label == "desktop" && a.w.ShotKeys[j].Label != "desktop"
		})
		works = append(works, a.w)
	}
	return works, nil
}

// loadImages fetches each screenshot from the store into an llm.Image (downscaled
// later by the client). A fetch failure is skipped (the page still drafts from
// whatever images loaded + the grounding text).
func (g *Generator) loadImages(ctx context.Context, keys []shotKey) []llm.Image {
	var out []llm.Image
	for _, sk := range keys {
		b, err := g.fetch(ctx, sk.Key)
		if err != nil {
			log.Printf("notes: fetch shot %s: %v", sk.Key, err)
			continue
		}
		out = append(out, llm.Image{Label: sk.Label, PNG: b})
	}
	return out
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
	if err := g.DB.FinishNotesJob(runID, status); err != nil {
		log.Printf("notes: finish job %s=%s: %v", runID, status, err)
	}
}

// parseDiff builds a "url\x00viewport" → ChangedPage lookup from a run's persisted
// diff JSON (empty/invalid → nil).
func parseDiff(diffJSON string) map[string]report.ChangedPage {
	if diffJSON == "" {
		return nil
	}
	var d report.Diff
	if err := json.Unmarshal([]byte(diffJSON), &d); err != nil {
		return nil
	}
	m := map[string]report.ChangedPage{}
	for _, cp := range d.ChangedPages {
		m[cp.URL+"\x00"+cp.Viewport] = cp
	}
	return m
}
