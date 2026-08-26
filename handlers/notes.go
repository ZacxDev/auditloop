package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"

	"github.com/gorilla/mux"
)

// notesVM builds the P3 "AI UX notes" view model for a run: the enabled flag +
// model allowlist, the async job status/progress, and the per-URL, per-model
// drafts persisted so far. Notes only render on a completed run.
func (a *App) notesVM(run *db.Run) *pages.NotesVM {
	vm := &pages.NotesVM{
		RunID:   run.ID,
		Enabled: a.Cfg.OpenRouterEnabled(),
		Status:  run.NotesStatus,
		Done:    run.NotesDone,
		Total:   run.NotesTotal,
		Models:  a.Cfg.Models(),
		CostUSD: run.NotesCostUSD, // latest pass's accumulated cost
	}
	if !vm.Enabled {
		return vm
	}

	// Map page_id → URL (for grouping notes) preserving page/URL order.
	pageRows, err := a.DB.ListPages(run.ID)
	if err != nil {
		return vm
	}
	urlByPage := map[string]string{}
	var urlOrder []string
	seen := map[string]bool{}
	for _, p := range pageRows {
		urlByPage[p.ID] = p.URL
		if !seen[p.URL] {
			seen[p.URL] = true
			urlOrder = append(urlOrder, p.URL)
		}
	}

	noteRows, err := a.DB.ListPageNotes(run.ID)
	if err != nil {
		return vm
	}
	byURL := map[string][]pages.PageNoteVM{}
	costByModel := map[string]float64{}
	var modelOrder []string
	for _, n := range noteRows {
		url := urlByPage[n.PageID]
		if url == "" {
			continue // orphaned note (page removed) — skip
		}
		byURL[url] = append(byURL[url], pages.PageNoteVM{
			PageID: n.PageID, Model: n.Model, Notes: n.Notes, Edited: n.Edited, Error: n.Error,
			CostUSD: n.CostUSD, Tokens: n.PromptTokens + n.CompletionTokens,
		})
		if n.CostUSD > 0 {
			if _, seen := costByModel[n.Model]; !seen {
				modelOrder = append(modelOrder, n.Model)
			}
			costByModel[n.Model] += n.CostUSD
			vm.DraftCount++
		}
	}
	for _, m := range modelOrder {
		vm.ByModel = append(vm.ByModel, pages.ModelCostVM{Model: m, CostUSD: costByModel[m]})
	}
	for _, url := range urlOrder {
		if ns := byURL[url]; len(ns) > 0 {
			vm.Pages = append(vm.Pages, pages.NotePageVM{URL: url, Notes: ns})
		}
	}
	return vm
}

// handleNotesStatus returns the notes-section fragment (htmx poll target while a
// pass is generating).
func (a *App) handleNotesStatus(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, pages.NotesSection(a.notesVM(run)))
}

// handleGenerateNotes enqueues the async multi-model notes pass for a completed
// run. Body carries selected models[] (validated against the curated allowlist).
// 503 when the feature is disabled; 400 on an empty/unknown model set.
func (a *App) handleGenerateNotes(w http.ResponseWriter, r *http.Request) {
	if a.Notes == nil || !a.Cfg.OpenRouterEnabled() {
		http.Error(w, "AI UX notes are not enabled (no OpenRouter API key configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if run.Status != db.RunDone {
		http.Error(w, "the run must be complete before drafting notes", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	models, ok := validateModels(a.Cfg, r.Form["models"])
	if !ok {
		http.Error(w, "unknown model requested", http.StatusBadRequest)
		return
	}
	if len(models) == 0 {
		http.Error(w, "select at least one valid model", http.StatusBadRequest)
		return
	}

	// Compute the job total (pages × models) and atomically claim the job so a
	// duplicate POST / second worker can't double-run.
	total, err := a.Notes.CountUnits(run.ID, models)
	if err != nil {
		http.Error(w, "failed to plan notes pass", http.StatusInternalServerError)
		return
	}
	won, err := a.DB.ClaimNotesJob(run.ID, total)
	if err != nil {
		http.Error(w, "failed to start notes pass", http.StatusInternalServerError)
		return
	}
	if won {
		// Detached background goroutine (survives the request; a restart mid-run is
		// swept back to 'failed' at boot).
		go func(runID string, models []string) {
			_ = a.Notes.Run(context.Background(), runID, models)
		}(run.ID, models)
	}

	// Return the refreshed (now generating) section so it starts polling.
	run.NotesStatus = db.NotesGenerating
	run.NotesTotal = total
	run.NotesDone = 0
	render(w, pages.NotesSection(a.notesVM(run)))
}

// handleSaveNote persists a human edit to one (page, model) note. Ownership is
// verified via the page → run → user join.
func (a *App) handleSaveNote(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	pageID := mux.Vars(r)["pageId"]
	model := mux.Vars(r)["model"]

	page, err := a.DB.GetPageByID(pageID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The run must belong to the caller (GetRun is user-scoped).
	if _, err := a.DB.GetRun(uid, page.RunID); err != nil {
		http.NotFound(w, r)
		return
	}

	notesText := strings.ReplaceAll(r.FormValue("notes"), "\r\n", "\n")
	if err := a.DB.SavePageNoteEdit(pageID, model, notesText); err != nil {
		http.Error(w, "note not found", http.StatusNotFound)
		return
	}
	render(w, pages.NoteCell(pages.PageNoteVM{
		PageID: pageID, Model: model, Notes: notesText, Edited: true,
	}))
}

// validateModels returns the de-duplicated, order-preserving set of requested
// models. ok is false if ANY requested (non-blank) model is outside the curated
// allowlist — the server rejects arbitrary user-supplied model ids outright.
func validateModels(cfg interface{ ModelAllowed(string) bool }, requested []string) (models []string, ok bool) {
	seen := map[string]bool{}
	for _, m := range requested {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !cfg.ModelAllowed(m) {
			return nil, false
		}
		seen[m] = true
		models = append(models, m)
	}
	return models, true
}
