package db

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// CreateRun enqueues a run for a target (status=queued). It links the run's
// baseline (prev_run_id) to the target's most recent completed ('done') run, if
// any, so the worker can diff against it once this run finishes. The first run
// for a target has no baseline (prev_run_id stays "").
func (d *DB) CreateRun(userID, targetID string) (*Run, error) {
	r := &Run{
		ID:          uuid.NewString(),
		TargetID:    targetID,
		UserID:      userID,
		Status:      RunQueued,
		Trigger:     "manual",
		SummaryJSON: "{}",
		CreatedAt:   parseTime(nowRFC()),
	}
	// Baseline: the most recent 'done' run for this target (this run isn't
	// inserted yet, so nothing to exclude).
	if prev, err := d.LatestDoneRunForTarget(targetID, ""); err == nil && prev != nil {
		r.PrevRunID = prev.ID
	}
	var prevArg any
	if r.PrevRunID != "" {
		prevArg = r.PrevRunID
	}
	_, err := d.exec(
		`INSERT INTO runs (id, target_id, user_id, status, trigger, prev_run_id, summary_json, diff_json, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, '{}', '', '', ?)`,
		r.ID, r.TargetID, r.UserID, r.Status, r.Trigger, prevArg, toRFC(r.CreatedAt),
	)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// CreatePushedRun creates a run for a PLUGIN target from an external push (P5).
// Unlike CreateRun it does not enqueue for the crawl worker: the run is inserted
// already in 'running' with started_at set (the caller persists the pushed pages,
// runs the diff, then FinishRun → done). trigger='plugin' and the optional label
// distinguish it. It links prev_run_id to the target's most recent 'done' run so
// the pushed run gets the SAME P2 regression diff a crawled run does.
func (d *DB) CreatePushedRun(userID, targetID, label, environment string) (*Run, error) {
	now := parseTime(nowRFC())
	r := &Run{
		ID:          uuid.NewString(),
		TargetID:    targetID,
		UserID:      userID,
		Status:      RunRunning,
		Trigger:     "plugin",
		Label:       label,
		Environment: environment,
		SummaryJSON: "{}",
		CreatedAt:   now,
		StartedAt:   &now,
	}
	if prev, err := d.LatestDoneRunForTarget(targetID, ""); err == nil && prev != nil {
		r.PrevRunID = prev.ID
	}
	var prevArg any
	if r.PrevRunID != "" {
		prevArg = r.PrevRunID
	}
	_, err := d.exec(
		`INSERT INTO runs (id, target_id, user_id, status, trigger, label, prev_run_id, summary_json, diff_json, error, created_at, started_at, environment)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '{}', '', '', ?, ?, ?)`,
		r.ID, r.TargetID, r.UserID, r.Status, r.Trigger, r.Label, prevArg, toRFC(r.CreatedAt), toRFC(now), r.Environment,
	)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// LatestDoneRunForTarget returns the most recent 'done' run for a target,
// excluding excludeRunID (pass "" to exclude nothing). Returns (nil, nil) when
// the target has no completed run yet — i.e. no baseline to diff against. Synthetic
// walkthrough runs (trigger='walkthrough', made only to feed the persona evaluator)
// are EXCLUDED — they must never become a crawl/push's P2 baseline.
func (d *DB) LatestDoneRunForTarget(targetID, excludeRunID string) (*Run, error) {
	row := d.queryRow(
		`SELECT `+runCols+` FROM runs
		 WHERE target_id=? AND status=? AND id<>? AND trigger<>'walkthrough' ORDER BY created_at DESC LIMIT 1`,
		targetID, RunDone, excludeRunID)
	r, err := scanRun(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SetRunDiff persists a run's regression summary (report.Diff JSON).
func (d *DB) SetRunDiff(runID, diffJSON string) error {
	_, err := d.exec(`UPDATE runs SET diff_json=? WHERE id=?`, diffJSON, runID)
	return err
}

const runCols = `id, target_id, user_id, status, trigger, COALESCE(label,''), COALESCE(prev_run_id,''), summary_json, COALESCE(diff_json,''), error, created_at, started_at, finished_at, COALESCE(notes_status,'idle'), COALESCE(notes_done,0), COALESCE(notes_total,0), COALESCE(notes_cost_usd,0), COALESCE(notes_prompt_tokens,0), COALESCE(notes_completion_tokens,0), COALESCE(environment,''), COALESCE(eval_status,'idle'), COALESCE(eval_done,0), COALESCE(eval_total,0), COALESCE(eval_job,''), COALESCE(eval_synthesis_json,''), COALESCE(eval_cost_usd,0), COALESCE(eval_prompt_tokens,0), COALESCE(eval_completion_tokens,0), COALESCE(favicon_key,'')`

// GetRun returns a run scoped to userID.
func (d *DB) GetRun(userID, id string) (*Run, error) {
	row := d.queryRow(`SELECT `+runCols+` FROM runs WHERE id=? AND user_id=?`, id, userID)
	return scanRun(row)
}

// GetRunByID returns a run without user scoping (worker use).
func (d *DB) GetRunByID(id string) (*Run, error) {
	row := d.queryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id)
	return scanRun(row)
}

// ListRuns returns a target's crawl/push runs, newest first, scoped to userID.
// Synthetic walkthrough runs (trigger='walkthrough') are EXCLUDED: they are an
// eval vessel for the persona evaluator (pages but ZERO findings, summary '{}'),
// so surfacing one in the run list (UI + read-API) would misreport it as a
// 0-page audit run. The walkthrough stays reachable via its own walkthrough view
// and /api/audit/walkthroughs/{id}. trigger is NOT NULL DEFAULT 'manual', so `<>`
// is dual-dialect-safe.
func (d *DB) ListRuns(userID, targetID string) ([]*Run, error) {
	rows, err := d.query(`SELECT `+runCols+` FROM runs WHERE user_id=? AND target_id=? AND trigger<>'walkthrough' ORDER BY created_at DESC`, userID, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimNextQueuedRun atomically claims one queued run (queued→running), setting
// started_at. Returns (nil, nil) when the queue is empty. The claim uses an
// UPDATE … WHERE status='queued' guarded by a SELECT of the oldest queued id, so
// two workers can't double-run the same row (rows-affected==1 is the winner).
func (d *DB) ClaimNextQueuedRun() (*Run, error) {
	// Pick the oldest queued id.
	var id string
	err := d.queryRow(`SELECT id FROM runs WHERE status=? ORDER BY created_at ASC LIMIT 1`, RunQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Atomically flip only if still queued.
	res, err := d.exec(`UPDATE runs SET status=?, started_at=? WHERE id=? AND status=?`,
		RunRunning, nowRFC(), id, RunQueued)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		// Lost the race to another worker; caller should try again.
		return nil, nil
	}
	return d.GetRunByID(id)
}

// SetRunFavicon records the captured site-favicon storage key on a run (best-effort;
// "" leaves the card falling back to a name monogram). Unscoped — the worker owns the
// run it is finalizing (mirrors SetRunDiff).
func (d *DB) SetRunFavicon(runID, key string) error {
	_, err := d.exec(`UPDATE runs SET favicon_key=? WHERE id=?`, key, runID)
	return err
}

// FinishRun marks a run done/failed with a summary and optional error.
func (d *DB) FinishRun(id, status, summaryJSON, errMsg string) error {
	_, err := d.exec(`UPDATE runs SET status=?, summary_json=?, error=?, finished_at=? WHERE id=?`,
		status, summaryJSON, errMsg, nowRFC(), id)
	return err
}

// RecoverStaleRuns marks any run stuck in 'running' at boot → 'failed' with a
// regenerate hint. Single-replica-safe (a multi-replica deploy would need a
// lease/TTL). Returns the number swept. Mirrors a sibling service's RecoverStaleSending.
func (d *DB) RecoverStaleRuns() (int, error) {
	res, err := d.exec(`UPDATE runs SET status=?, error=?, finished_at=? WHERE status=?`,
		RunFailed, "interrupted by restart — re-run the audit", nowRFC(), RunRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanRun(s scanner) (*Run, error) {
	var r Run
	var created string
	var started, finished sql.NullString
	if err := s.Scan(&r.ID, &r.TargetID, &r.UserID, &r.Status, &r.Trigger, &r.Label, &r.PrevRunID,
		&r.SummaryJSON, &r.DiffJSON, &r.Error, &created, &started, &finished,
		&r.NotesStatus, &r.NotesDone, &r.NotesTotal,
		&r.NotesCostUSD, &r.NotesPromptTokens, &r.NotesCompletionTokens, &r.Environment,
		&r.EvalStatus, &r.EvalDone, &r.EvalTotal, &r.EvalJob, &r.EvalSynthesisJSON,
		&r.EvalCostUSD, &r.EvalPromptTokens, &r.EvalCompletionTokens, &r.FaviconKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.CreatedAt = parseTime(created)
	r.StartedAt = parseTimePtr(started)
	r.FinishedAt = parseTimePtr(finished)
	return &r, nil
}
