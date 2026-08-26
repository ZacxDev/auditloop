package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"sort"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/diff"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"
)

// Diff runs the P2 diff phase for a completed run against its baseline
// (prev_run_id) and returns the run-level report.Diff (nil when there is no
// baseline). It is exported so the P5 plugin-push path reuses the EXACT same
// diffing the crawl worker performs — a pushed run gets regression detection vs
// the previous push for free. It performs storage reads/writes (diff images) but
// never fails: per-page I/O errors are logged and skipped.
func Diff(ctx context.Context, database *db.DB, store storage.Store, run *db.Run, targetSlug string) *report.Diff {
	w := &Worker{DB: database, Store: store}
	return w.runDiff(ctx, run, targetSlug)
}

// runDiff is the P2 "diff phase": once a run's pages are persisted, compare it
// to its baseline (prev_run_id) and produce a run-level report.Diff. It:
//   - computes the page-set delta (added/removed URLs),
//   - pixel-diffs each matched page+viewport screenshot, persisting diff_pct +
//     a diff image (red-tinted changes over the dimmed current shot),
//   - computes the a11y rule-set delta (new/resolved axe rules),
//   - computes first-party console/network + violation-count deltas.
//
// It returns nil when there is no baseline (first run for the target) or the
// baseline can't be loaded — the run simply has no diff. It never fails the run:
// per-page I/O errors are logged and skipped.
func (w *Worker) runDiff(ctx context.Context, run *db.Run, targetSlug string) *report.Diff {
	if run.PrevRunID == "" {
		return nil
	}
	prev, err := w.DB.GetRunByID(run.PrevRunID)
	if err != nil {
		log.Printf("worker: diff: load baseline %s: %v", run.PrevRunID, err)
		return nil
	}
	prevPages, err := w.DB.ListPages(prev.ID)
	if err != nil {
		log.Printf("worker: diff: list baseline pages: %v", err)
		return nil
	}
	curPages, err := w.DB.ListPages(run.ID)
	if err != nil {
		log.Printf("worker: diff: list current pages: %v", err)
		return nil
	}

	d := &report.Diff{PrevRunID: prev.ID}
	if prev.FinishedAt != nil {
		d.PrevRunAt = *prev.FinishedAt
	} else {
		d.PrevRunAt = prev.CreatedAt
	}

	// Page-set delta by unique URL.
	added, removed := diff.StringSetDelta(uniqueURLs(prevPages), uniqueURLs(curPages))
	d.PagesAdded, d.PagesRemoved = added, removed

	// Visual diff per matched page+viewport.
	prevByKey := map[string]*db.Page{}
	for _, p := range prevPages {
		prevByKey[p.URL+"\x00"+p.Viewport] = p
	}
	for _, cp := range curPages {
		base, ok := prevByKey[cp.URL+"\x00"+cp.Viewport]
		if !ok || base.ScreenshotKey == "" || cp.ScreenshotKey == "" {
			continue
		}
		baseImg, err := w.fetch(ctx, base.ScreenshotKey)
		if err != nil {
			log.Printf("worker: diff: fetch baseline shot %s: %v", base.ScreenshotKey, err)
			continue
		}
		curImg, err := w.fetch(ctx, cp.ScreenshotKey)
		if err != nil {
			log.Printf("worker: diff: fetch current shot %s: %v", cp.ScreenshotKey, err)
			continue
		}
		dr, err := diff.Compare(baseImg, curImg)
		if err != nil {
			log.Printf("worker: diff: compare %s @ %s: %v", cp.URL, cp.Viewport, err)
			continue
		}
		// Only generate/upload a diff image when the pixel comparison is meaningful:
		// SAME dimensions (aligned) and under the visualization size cap. A
		// size-changed page's diff image is a misaligned, ~all-red artifact, and an
		// over-cap page has no visualization at all — skip the Store upload for both.
		diffKey := ""
		if !dr.SizeChanged && !dr.TooLarge && dr.DiffPct > 0 && len(dr.DiffPNG) > 0 {
			diffKey = storage.DiffKey(targetSlug, run.ID, storage.PageSlug(cp.URL), cp.Viewport)
			if err := w.Store.Put(ctx, diffKey, "image/png", bytes.NewReader(dr.DiffPNG), int64(len(dr.DiffPNG))); err != nil {
				log.Printf("worker: diff: put diff image %s: %v", diffKey, err)
				diffKey = ""
			}
		}
		// diff_pct is persisted as-is (informational), regardless of size change.
		if err := w.DB.UpdatePageDiff(cp.ID, dr.DiffPct, diffKey); err != nil {
			log.Printf("worker: diff: persist page diff %s: %v", cp.ID, err)
		}
		if dr.DiffPct > 0 || dr.SizeChanged || dr.TooLarge {
			d.ChangedPages = append(d.ChangedPages, report.ChangedPage{
				URL: cp.URL, Viewport: cp.Viewport, DiffPct: dr.DiffPct,
				DiffKey: diffKey, SizeChanged: dr.SizeChanged, NotCompared: dr.TooLarge,
			})
		}
		// A VISUAL REGRESSION requires SAME dimensions (aligned pixels) + diff_pct
		// over threshold. A dimension change is reported separately as a
		// layout/size change and never fires the regression metric/badge.
		switch {
		case !dr.SizeChanged && dr.DiffPct >= report.VisualRegressionThreshold:
			d.PagesChanged++
		case dr.SizeChanged:
			d.PagesSizeChanged++
		}
	}
	// Worst-first so the biggest regressions surface at the top.
	sort.SliceStable(d.ChangedPages, func(i, j int) bool {
		return d.ChangedPages[i].DiffPct > d.ChangedPages[j].DiffPct
	})

	// a11y rule-set delta (axe rule ids across the run).
	prevRules := w.runA11yRuleIDs(prevPages)
	curRules := w.runA11yRuleIDs(curPages)
	newRules, resolved := diff.StringSetDelta(prevRules, curRules)
	d.NewA11yRules, d.ResolvedA11yRules = newRules, resolved

	// Count deltas (current − previous; positive = regression).
	pv, pc, pn := runTotals(prevPages)
	cv, cc, cn := runTotals(curPages)
	d.A11yDelta = cv - pv
	d.ConsoleDelta = cc - pc
	d.NetworkDelta = cn - pn

	metrics.VisualRegressions.Add(float64(d.PagesChanged))
	metrics.RunPagesChanged.Set(float64(d.PagesChanged))
	return d
}

// fetch reads an artifact fully from the store.
func (w *Worker) fetch(ctx context.Context, key string) ([]byte, error) {
	rc, err := w.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// runA11yRuleIDs returns the de-duplicated set of axe rule ids that have
// violations across a run's pages (read from persisted a11y findings).
func (w *Worker) runA11yRuleIDs(pages []*db.Page) []string {
	seen := map[string]bool{}
	for _, p := range pages {
		finds, err := w.DB.ListFindings(p.ID)
		if err != nil {
			continue
		}
		for _, f := range finds {
			if f.Type != db.FindingA11y {
				continue
			}
			var meta struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(f.Detail), &meta) == nil && meta.ID != "" {
				seen[meta.ID] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func uniqueURLs(pages []*db.Page) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pages {
		if !seen[p.URL] {
			seen[p.URL] = true
			out = append(out, p.URL)
		}
	}
	return out
}

// runTotals sums a run's violation, first-party console, and first-party network
// counts across pages.
func runTotals(pages []*db.Page) (a11y, console, network int) {
	for _, p := range pages {
		a11y += p.AxeViolationCount
		console += p.ConsoleFirstPartyCount
		network += p.NetworkFirstPartyCount
	}
	return a11y, console, network
}
