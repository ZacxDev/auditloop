package db

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// --- Async notes-job tracking on runs (P3) ---

// ClaimNotesJob atomically starts a notes-generation job for a run: it flips
// notes_status → 'generating' and resets progress (done=0, total), but only if a
// job isn't already generating. Returns true when this caller won the claim (so a
// duplicate POST or a second worker can't double-run). Mirrors the crawl claim.
func (d *DB) ClaimNotesJob(runID string, total int) (bool, error) {
	// Reset the cost accumulators too so a Regenerate reflects THIS pass's cost, not
	// the sum of every pass ever run (see AddNotesCost).
	res, err := d.exec(
		`UPDATE runs SET notes_status=?, notes_done=0, notes_total=?,
			notes_cost_usd=0, notes_prompt_tokens=0, notes_completion_tokens=0
		 WHERE id=? AND notes_status<>?`,
		NotesGenerating, total, runID, NotesGenerating)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateNotesProgress records how many (page,model) units have completed.
func (d *DB) UpdateNotesProgress(runID string, done int) error {
	_, err := d.exec(`UPDATE runs SET notes_done=? WHERE id=?`, done, runID)
	return err
}

// AddNotesCost atomically accumulates one (page,model) call's cost + token counts
// into the run totals. The increment is DB-side (SET x = x + ?) so the pass's
// concurrent per-model writes don't lose updates (mirrors the atomic notes_done
// increment pattern). ClaimNotesJob resets these to 0 at the start of a fresh pass.
func (d *DB) AddNotesCost(runID string, costUSD float64, promptTokens, completionTokens int) error {
	_, err := d.exec(
		`UPDATE runs SET
			notes_cost_usd = notes_cost_usd + ?,
			notes_prompt_tokens = notes_prompt_tokens + ?,
			notes_completion_tokens = notes_completion_tokens + ?
		 WHERE id=?`,
		costUSD, promptTokens, completionTokens, runID)
	return err
}

// FinishNotesJob marks a notes job terminal (done|failed).
func (d *DB) FinishNotesJob(runID, status string) error {
	_, err := d.exec(`UPDATE runs SET notes_status=? WHERE id=?`, status, runID)
	return err
}

// MarkGeneratingNotesFailed settles any notes job stuck in 'generating' at boot →
// 'failed'. The pass runs in a background goroutine, so a pod restart would
// otherwise leave the poll spinning forever. Returns the number swept. Mirrors
// RecoverStaleRuns / a sibling service's MarkGeneratingReportsFailed. Single-replica-safe.
func (d *DB) MarkGeneratingNotesFailed() (int, error) {
	res, err := d.exec(`UPDATE runs SET notes_status=? WHERE notes_status=?`, NotesFailed, NotesGenerating)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- page_notes CRUD (P3) ---

// SavePageNoteDraft upserts a generated draft for a (page, model): it REPLACES any
// existing row for that pair (edited reset to false, error set/cleared). This is
// the write path for both a fresh draft and a re-draft.
func (d *DB) SavePageNoteDraft(pageID, runID, model, notes, errMsg string, cost float64, promptTokens, completionTokens int) error {
	now := nowRFC()
	res, err := d.exec(
		`UPDATE page_notes SET notes=?, error=?, edited=0, run_id=?, cost_usd=?, prompt_tokens=?, completion_tokens=?, updated_at=?
		 WHERE page_id=? AND model=?`,
		notes, errMsg, runID, cost, promptTokens, completionTokens, now, pageID, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = d.exec(
		`INSERT INTO page_notes (id, page_id, run_id, model, notes, edited, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), pageID, runID, model, notes, errMsg, cost, promptTokens, completionTokens, now, now)
	return err
}

// SavePageNoteEdit updates the text of an existing (page, model) note and marks it
// edited. Returns ErrNotFound if no such note exists (nothing to edit).
func (d *DB) SavePageNoteEdit(pageID, model, notes string) error {
	res, err := d.exec(
		`UPDATE page_notes SET notes=?, edited=1, updated_at=? WHERE page_id=? AND model=?`,
		notes, nowRFC(), pageID, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPageNotes returns all notes for a run, ordered by page then model.
func (d *DB) ListPageNotes(runID string) ([]*PageNote, error) {
	rows, err := d.query(
		`SELECT id, page_id, run_id, model, notes, edited, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at
		 FROM page_notes WHERE run_id=? ORDER BY page_id ASC, model ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PageNote
	for rows.Next() {
		pn, err := scanPageNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pn)
	}
	return out, rows.Err()
}

// GetPageNote returns a single (page, model) note, or ErrNotFound.
func (d *DB) GetPageNote(pageID, model string) (*PageNote, error) {
	row := d.queryRow(
		`SELECT id, page_id, run_id, model, notes, edited, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at
		 FROM page_notes WHERE page_id=? AND model=?`, pageID, model)
	return scanPageNote(row)
}

func scanPageNote(s scanner) (*PageNote, error) {
	var pn PageNote
	var edited int
	var created, updated string
	if err := s.Scan(&pn.ID, &pn.PageID, &pn.RunID, &pn.Model, &pn.Notes, &edited, &pn.Error,
		&pn.CostUSD, &pn.PromptTokens, &pn.CompletionTokens, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	pn.Edited = edited != 0
	pn.CreatedAt = parseTime(created)
	pn.UpdatedAt = parseTime(updated)
	return &pn, nil
}

// GetPageByID returns a single page row (no user scoping — ownership is checked by
// the caller via the run/target join).
func (d *DB) GetPageByID(pageID string) (*Page, error) {
	row := d.queryRow(
		`SELECT id, run_id, url, viewport, screenshot_key, axe_key, a11y_digest_key,
			axe_violation_count, console_first_party_count, console_third_party_count,
			network_first_party_count, network_third_party_count, load_ms,
			diff_pct, diff_key, created_at
		 FROM pages WHERE id=?`, pageID)
	var p Page
	var created string
	if err := row.Scan(&p.ID, &p.RunID, &p.URL, &p.Viewport, &p.ScreenshotKey, &p.AxeKey, &p.A11yDigestKey,
		&p.AxeViolationCount, &p.ConsoleFirstPartyCount, &p.ConsoleThirdPartyCount,
		&p.NetworkFirstPartyCount, &p.NetworkThirdPartyCount, &p.LoadMS,
		&p.DiffPct, &p.DiffKey, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.CreatedAt = parseTime(created)
	return &p, nil
}
