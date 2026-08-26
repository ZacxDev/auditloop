package db

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// --- Async persona-walkthrough eval-job tracking on runs (Phase 1) ---
//
// Mirrors the P3 notes job (ClaimNotesJob / UpdateNotesProgress / AddNotesCost /
// FinishNotesJob / MarkGeneratingNotesFailed) exactly — a separate, parallel job
// so the two LLM passes coexist without interfering.

// ClaimEvalJob atomically starts a persona-walkthrough evaluation job for a run:
// it flips eval_status → 'generating', resets progress (done=0, total), records
// the free-text job the pass evaluates toward, and resets the cost accumulators
// (so a re-run reflects THIS pass's cost, not the sum of every pass). It succeeds
// only when a job isn't already generating — returns true when this caller won the
// claim (guards a duplicate POST / second worker from double-running).
func (d *DB) ClaimEvalJob(runID, job string, total int) (bool, error) {
	res, err := d.exec(
		`UPDATE runs SET eval_status=?, eval_done=0, eval_total=?, eval_job=?, eval_synthesis_json='',
			eval_cost_usd=0, eval_prompt_tokens=0, eval_completion_tokens=0
		 WHERE id=? AND eval_status<>?`,
		EvalGenerating, total, job, runID, EvalGenerating)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateEvalProgress records how many units (page,persona cells + synthesis) have
// completed.
func (d *DB) UpdateEvalProgress(runID string, done int) error {
	_, err := d.exec(`UPDATE runs SET eval_done=? WHERE id=?`, done, runID)
	return err
}

// AddEvalCost atomically accumulates one LLM call's cost + token counts into the
// run totals. The increment is DB-side (SET x = x + ?) so the pass's concurrent
// per-cell writes don't lose updates; ClaimEvalJob resets these to 0 at the start
// of a fresh pass.
func (d *DB) AddEvalCost(runID string, costUSD float64, promptTokens, completionTokens int) error {
	_, err := d.exec(
		`UPDATE runs SET
			eval_cost_usd = eval_cost_usd + ?,
			eval_prompt_tokens = eval_prompt_tokens + ?,
			eval_completion_tokens = eval_completion_tokens + ?
		 WHERE id=?`,
		costUSD, promptTokens, completionTokens, runID)
	return err
}

// SetRunEvalSynthesis persists the run-level synthesis "story" (report.EvalSynthesis
// JSON).
func (d *DB) SetRunEvalSynthesis(runID, synthesisJSON string) error {
	_, err := d.exec(`UPDATE runs SET eval_synthesis_json=? WHERE id=?`, synthesisJSON, runID)
	return err
}

// FinishEvalJob marks an eval job terminal (done|failed).
func (d *DB) FinishEvalJob(runID, status string) error {
	_, err := d.exec(`UPDATE runs SET eval_status=? WHERE id=?`, status, runID)
	return err
}

// MarkGeneratingEvalFailed settles any eval job stuck in 'generating' at boot →
// 'failed' (the pass runs in a background goroutine, so a restart would otherwise
// leave the poll spinning forever). Returns the number swept. Mirrors
// MarkGeneratingNotesFailed. Single-replica-safe.
func (d *DB) MarkGeneratingEvalFailed() (int, error) {
	res, err := d.exec(`UPDATE runs SET eval_status=? WHERE eval_status=?`, EvalFailed, EvalGenerating)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- page_evaluations CRUD (Phase 1) ---

// SavePageEvaluation upserts a generated verdict for a (page, persona): it
// REPLACES any existing row for that pair (error set/cleared). This is the write
// path for a fresh evaluation and a re-run.
func (d *DB) SavePageEvaluation(pageID, runID, persona, findingsJSON, comprehension, errMsg string, cost float64, promptTokens, completionTokens int) error {
	now := nowRFC()
	res, err := d.exec(
		`UPDATE page_evaluations SET findings_json=?, comprehension=?, error=?, run_id=?, cost_usd=?, prompt_tokens=?, completion_tokens=?, updated_at=?
		 WHERE page_id=? AND persona=?`,
		findingsJSON, comprehension, errMsg, runID, cost, promptTokens, completionTokens, now, pageID, persona)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = d.exec(
		`INSERT INTO page_evaluations (id, page_id, run_id, persona, findings_json, comprehension, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), pageID, runID, persona, findingsJSON, comprehension, errMsg, cost, promptTokens, completionTokens, now, now)
	return err
}

// ListPageEvaluations returns all evaluations for a run, ordered by page then
// persona.
func (d *DB) ListPageEvaluations(runID string) ([]*PageEvaluation, error) {
	rows, err := d.query(
		`SELECT id, page_id, run_id, persona, findings_json, comprehension, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at
		 FROM page_evaluations WHERE run_id=? ORDER BY page_id ASC, persona ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PageEvaluation
	for rows.Next() {
		pe, err := scanPageEvaluation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pe)
	}
	return out, rows.Err()
}

// GetPageEvaluation returns a single (page, persona) evaluation, or ErrNotFound.
func (d *DB) GetPageEvaluation(pageID, persona string) (*PageEvaluation, error) {
	row := d.queryRow(
		`SELECT id, page_id, run_id, persona, findings_json, comprehension, error, cost_usd, prompt_tokens, completion_tokens, created_at, updated_at
		 FROM page_evaluations WHERE page_id=? AND persona=?`, pageID, persona)
	return scanPageEvaluation(row)
}

func scanPageEvaluation(s scanner) (*PageEvaluation, error) {
	var pe PageEvaluation
	var created, updated string
	if err := s.Scan(&pe.ID, &pe.PageID, &pe.RunID, &pe.Persona, &pe.FindingsJSON, &pe.Comprehension, &pe.Error,
		&pe.CostUSD, &pe.PromptTokens, &pe.CompletionTokens, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	pe.CreatedAt = parseTime(created)
	pe.UpdatedAt = parseTime(updated)
	return &pe, nil
}
