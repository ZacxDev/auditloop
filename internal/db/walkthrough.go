package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/ZacxDev/auditloop/internal/storage"

	"github.com/google/uuid"
)

// Persona-walkthrough Phase 3 (goal-directed DRIVER) persistence. Mirrors the
// eval/notes async-job pattern and login_recipes ownership convention: all reads
// are OWNER-SCOPED via a target→user JOIN (no direct user_id column on the
// walkthrough tables); the atomic ClaimWalkthroughJob guards a double-run and
// resets the reset-per-pass cost accumulators.

// CreateWalkthrough inserts a fresh walkthrough row (status=idle) for a target and
// returns it. dryRun records whether the pass will run under the submit-guard.
func (d *DB) CreateWalkthrough(targetID, runID, goal string, dryRun bool) (*Walkthrough, error) {
	// Baseline link (Phase 4): the target's newest TERMINAL walkthrough is this one's
	// diff baseline. Mirrors CreateRun stamping runs.prev_run_id. "" for the first.
	prev, err := d.latestTerminalWalkthroughID(targetID)
	if err != nil {
		return nil, err
	}
	w := &Walkthrough{
		ID:                uuid.NewString(),
		TargetID:          targetID,
		RunID:             runID,
		Goal:              goal,
		DryRun:            dryRun,
		Status:            WalkIdle,
		PrevWalkthroughID: prev,
		CreatedAt:         parseTime(nowRFC()),
	}
	w.UpdatedAt = w.CreatedAt
	_, err = d.exec(
		`INSERT INTO walkthroughs (id, target_id, run_id, goal, outcome, stuck_step, reason, dry_run, status,
			steps_total, steps_done, cost_usd, prompt_tokens, completion_tokens, prev_walkthrough_id, diff_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, '', 0, '', ?, ?, 0, 0, 0, 0, 0, ?, '', ?, ?)`,
		w.ID, w.TargetID, w.RunID, w.Goal, boolToInt(w.DryRun), w.Status, w.PrevWalkthroughID, toRFC(w.CreatedAt), toRFC(w.UpdatedAt))
	if err != nil {
		return nil, err
	}
	return w, nil
}

// latestTerminalWalkthroughID returns the id of a target's most recent TERMINAL
// (done|failed) walkthrough, or "" when it has none. Unscoped (the caller —
// CreateWalkthrough — already resolved target ownership), mirroring how CreateRun
// picks the P2 baseline via LatestDoneRunForTarget.
//
// INFRA-FAILED walkthroughs are EXCLUDED (#45): one never observed the goal, so
// diffing against it would compare a real pass to a non-observation. The next real
// walkthrough therefore compares against the last one that actually ran. A genuine
// product-side `failed` is still a valid baseline (infra_failed=0).
func (d *DB) latestTerminalWalkthroughID(targetID string) (string, error) {
	row := d.queryRow(
		`SELECT id FROM walkthroughs WHERE target_id=? AND status IN (?, ?) AND infra_failed=0
		 ORDER BY created_at DESC LIMIT 1`, targetID, WalkDone, WalkFailed)
	var id string
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// SetWalkthroughDiff persists the run-level walkthrough regression summary
// (report.WalkthroughDiff JSON) vs prev_walkthrough_id. Mirror of SetRunDiff (P2).
func (d *DB) SetWalkthroughDiff(id, diffJSON string) error {
	_, err := d.exec(`UPDATE walkthroughs SET diff_json=?, updated_at=? WHERE id=?`, diffJSON, nowRFC(), id)
	return err
}

const walkthroughCols = `id, target_id, run_id, goal, outcome, stuck_step, reason, dry_run, status,
	steps_total, steps_done, cost_usd, prompt_tokens, completion_tokens, prev_walkthrough_id, diff_json,
	infra_failed, created_at, updated_at`

// GetWalkthrough returns a walkthrough owner-scoped via the target→user join. A
// foreign/unknown id → ErrNotFound (never leaks another user's walkthrough).
func (d *DB) GetWalkthrough(userID, id string) (*Walkthrough, error) {
	row := d.queryRow(
		`SELECT `+walkthroughColsPrefixed("w")+` FROM walkthroughs w
		 JOIN targets t ON t.id = w.target_id
		 WHERE w.id=? AND t.user_id=?`, id, userID)
	return scanWalkthrough(row)
}

// GetWalkthroughByID returns a walkthrough without user scoping (generator use —
// the caller already resolved ownership when claiming the job).
func (d *DB) GetWalkthroughByID(id string) (*Walkthrough, error) {
	row := d.queryRow(`SELECT `+walkthroughCols+` FROM walkthroughs WHERE id=?`, id)
	return scanWalkthrough(row)
}

// ClaimWalkthroughJob atomically starts a walkthrough pass: it flips status →
// 'driving', resets progress + the reset-per-pass cost accumulators + any prior
// outcome, and records the total step budget. It succeeds only when the job is not
// already driving (idle/done/failed → driving) — returns true when THIS caller won
// (guards a duplicate POST / a second worker).
func (d *DB) ClaimWalkthroughJob(id string, stepsTotal int) (bool, error) {
	res, err := d.exec(
		// infra_failed is reset with the rest of the prior outcome — a fresh pass has not
		// failed on infrastructure until FinishWalkthrough says so (#45).
		`UPDATE walkthroughs SET status=?, steps_done=0, steps_total=?, outcome='', stuck_step=0, reason='',
			cost_usd=0, prompt_tokens=0, completion_tokens=0, infra_failed=0, updated_at=?
		 WHERE id=? AND status<>?`,
		WalkDriving, stepsTotal, nowRFC(), id, WalkDriving)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateWalkthroughProgress records how many steps have completed.
func (d *DB) UpdateWalkthroughProgress(id string, done int) error {
	_, err := d.exec(`UPDATE walkthroughs SET steps_done=?, updated_at=? WHERE id=?`, done, nowRFC(), id)
	return err
}

// AddWalkthroughCost accumulates one planner call's cost + tokens (DB-side SET x=x+?
// so concurrent-safe, though the driver is sequential). Reset by ClaimWalkthroughJob.
func (d *DB) AddWalkthroughCost(id string, costUSD float64, promptTokens, completionTokens int) error {
	_, err := d.exec(
		`UPDATE walkthroughs SET cost_usd = cost_usd + ?, prompt_tokens = prompt_tokens + ?,
			completion_tokens = completion_tokens + ?, updated_at=? WHERE id=?`,
		costUSD, promptTokens, completionTokens, nowRFC(), id)
	return err
}

// FinishWalkthrough finalizes a pass: records the deterministic outcome
// (success|stuck|failed), where it got stuck, a credential-free reason, and marks
// the job terminal (done for success/stuck; failed for a hard failure).
//
// infraFailed says the pass failed because the DRIVER could not run (a stalled/killed
// browser, a setup/config failure) rather than because the product regressed (#45). It
// is a PARAMETER rather than a separate setter deliberately: every finisher must decide
// it, so a caller cannot forget to record it.
func (d *DB) FinishWalkthrough(id, outcome string, stuckStep int, reason string, infraFailed bool) error {
	status := WalkDone
	if outcome == WalkOutcomeFailed {
		status = WalkFailed
	}
	_, err := d.exec(
		`UPDATE walkthroughs SET status=?, outcome=?, stuck_step=?, reason=?, infra_failed=?, updated_at=? WHERE id=?`,
		status, outcome, stuckStep, reason, boolToInt(infraFailed), nowRFC(), id)
	return err
}

// MarkDrivingWalkthroughsFailed settles any walkthrough stuck in 'driving' at boot
// → failed (the pass runs in a background goroutine, so a restart would otherwise
// leave the poll spinning forever). Returns the number swept. Mirrors
// MarkGeneratingEvalFailed. Single-replica-safe.
//
// The swept rows are marked infra_failed=1 (#45): a walkthrough orphaned mid-drive by
// a restart likewise produced no observation of the goal, so it must not be scored as a
// regression and must never become a baseline.
func (d *DB) MarkDrivingWalkthroughsFailed() (int, error) {
	res, err := d.exec(
		`UPDATE walkthroughs SET status=?, outcome=?, reason=?, infra_failed=1, updated_at=? WHERE status=?`,
		WalkFailed, WalkOutcomeFailed, "interrupted by restart — re-run the walkthrough", nowRFC(), WalkDriving)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// InsertWalkthroughStep appends one recorded step to a walkthrough's trace.
func (d *DB) InsertWalkthroughStep(s *WalkthroughStep) (string, error) {
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	now := nowRFC()
	_, err := d.exec(
		`INSERT INTO walkthrough_steps (id, walkthrough_id, idx, action_json, url, screenshot_key, outcome, planner_reason, digest_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.WalkthroughID, s.Idx, s.ActionJSON, s.URL, s.ScreenshotKey, s.Outcome, s.PlannerReason, s.DigestJSON, now)
	return s.ID, err
}

// ListWalkthroughSteps returns a walkthrough's steps in trace order.
func (d *DB) ListWalkthroughSteps(walkthroughID string) ([]*WalkthroughStep, error) {
	rows, err := d.query(
		`SELECT id, walkthrough_id, idx, action_json, url, screenshot_key, outcome, planner_reason, digest_json, created_at
		 FROM walkthrough_steps WHERE walkthrough_id=? ORDER BY idx ASC`, walkthroughID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WalkthroughStep
	for rows.Next() {
		var s WalkthroughStep
		var created string
		if err := rows.Scan(&s.ID, &s.WalkthroughID, &s.Idx, &s.ActionJSON, &s.URL, &s.ScreenshotKey,
			&s.Outcome, &s.PlannerReason, &s.DigestJSON, &created); err != nil {
			return nil, err
		}
		s.CreatedAt = parseTime(created)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// LatestWalkthroughForTarget returns a target's most recent walkthrough
// (owner-scoped), or (nil, nil) when the target has none. Used by the target view.
func (d *DB) LatestWalkthroughForTarget(userID, targetID string) (*Walkthrough, error) {
	row := d.queryRow(
		`SELECT `+walkthroughColsPrefixed("w")+` FROM walkthroughs w
		 JOIN targets t ON t.id = w.target_id
		 WHERE w.target_id=? AND t.user_id=? ORDER BY w.created_at DESC LIMIT 1`, targetID, userID)
	wk, err := scanWalkthrough(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return wk, nil
}

// WalkthroughSyntheticWidth is the (desktop) viewport width recorded on a synthetic
// walkthrough page. The driver captures every step at the desktop 1440px viewport
// (crawler.DriveOptions), so the materialized pages carry that width. Kept here so the
// db package has no crawler dependency.
const WalkthroughSyntheticWidth = 1440

// MaterializeWalkthroughRun turns a COMPLETED walkthrough's deterministic step trace
// into a synthetic 'done' run + one page per step, so the EXISTING Phase-1 persona
// evaluator (eval.Generator.Run keyed on a run id) can critique the ACTUAL driven flow
// instead of the crawl BFS (Phase-3 PR-B). It mirrors CreatePushedRun (P5): a run made
// WITHOUT the crawl worker.
//
//   - Owner-scoped: the walkthrough must belong to userID (via GetWalkthrough's
//     target→user join) — a foreign/unknown walkthrough → ErrNotFound (no dup, no leak).
//   - Idempotent: if the walkthrough already carries a run_id AND that run still exists,
//     the same run id is returned (no duplicate run/pages). Re-materialize is a no-op.
//   - One page per step, IN ORDER, at viewport='desktop'. The page URL is a STABLE,
//     order-preserving, per-step LABEL ("{NN} · {action-type} · {step-url}") — NOT the
//     bare step URL, which would collapse steps that revisit the same URL into a single
//     eval unit and lose the flow position. The 2-digit zero-padded index keeps ListPages'
//     `ORDER BY url` in step order. The step's existing screenshot_key is REUSED (the
//     driver already stored it) — a step with no screenshot still becomes a page (eval
//     degrades to no image for it).
//   - The synthetic run is trigger='walkthrough', NOT baseline-linked (prev_run_id="") —
//     it is an eval vessel, never a P2 baseline. The baseline queries also EXCLUDE
//     trigger='walkthrough' so a subsequent crawl/push never diffs against a synthetic run.
//   - Race-safe: a concurrent double-submit (a double-click / htmx double-fire on the
//     evaluate form) must NOT create two synthetic runs and two paid eval passes. The
//     old read-run_id → create → stamp sequence was a TOCTOU (both peers read run_id=”
//     and each created a distinct run). Fix: a compare-and-swap CLAIM on walkthroughs.run_id
//     inside a tx — exactly one caller stamps its fresh run id (and, in the same tx,
//     creates the run+pages); the loser sees rows-affected==0, rolls back, re-reads the
//     winner's run_id and reuses it. SQLite serializes writers (MaxOpenConns=1), so the
//     loser's CAS runs only after the winner's tx commits the run+pages → the reused id
//     always resolves to a fully-materialized run. Guarantee: at most one synthetic run
//     and one eval pass per walkthrough, idempotent across legitimate re-evaluates.
func (d *DB) MaterializeWalkthroughRun(ctx context.Context, store storage.Store, userID, walkthroughID string) (string, error) {
	wk, err := d.GetWalkthrough(userID, walkthroughID) // owner-scoped → ErrNotFound for foreign
	if err != nil {
		return "", err
	}
	// Idempotent fast path: an already-materialized synthetic run that still exists
	// is reused as-is (a legitimate re-evaluate → same run, no duplicate run/pages).
	if wk.RunID != "" {
		if _, gerr := d.GetRunByID(wk.RunID); gerr == nil {
			return wk.RunID, nil
		}
	}
	steps, err := d.ListWalkthroughSteps(walkthroughID)
	if err != nil {
		return "", err
	}
	// targetSlug for the per-page a11y digest artifact key (mirrors the crawl path's
	// {target_slug}/{run_id}/{page_slug}/a11y.json). Best-effort: an unresolvable target
	// name just yields a "root" slug — the key is still unique per (run, page) and the
	// eval reads it back by the stored pages.a11y_digest_key, not by reconstructing it.
	targetSlug := ""
	if tgt, terr := d.GetTargetByID(wk.TargetID); terr == nil {
		targetSlug = storage.Slug(tgt.Name)
	}

	now := parseTime(nowRFC())
	runID := uuid.NewString()
	// The zero-padded index prefix keeps ListPages' `ORDER BY url` in step order; the
	// pad width is derived from the step count so a future action-budget bump past 99
	// can't silently reorder (%02d would sort "100" before "99").
	pad := labelPadWidth(len(steps))

	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// CAS claim: atomically swap run_id from the value we observed (wk.RunID — "" on the
	// first materialize, a stale/deleted id on a re-materialize) to our fresh run id.
	// Under a concurrent double-submit exactly ONE caller's CAS affects a row.
	res, err := tx.Exec(d.rebind(`UPDATE walkthroughs SET run_id=?, updated_at=? WHERE id=? AND run_id=?`),
		runID, toRFC(now), walkthroughID, wk.RunID)
	if err != nil {
		return "", err
	}
	won, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if won != 1 {
		// A concurrent caller won the claim; abandon ours and reuse theirs.
		_ = tx.Rollback()
		cur, gerr := d.GetWalkthroughByID(walkthroughID)
		if gerr != nil {
			return "", gerr
		}
		return cur.RunID, nil
	}

	// We won: create the synthetic, already-'done' run (started+finished now) + one page
	// per step in the SAME tx, so the stamped run_id and its run/pages commit atomically.
	// trigger='walkthrough' and prev_run_id NULL (never a baseline). Reuses the standard
	// runs table so the whole run/eval/read-API stack works unchanged.
	if _, err := tx.Exec(d.rebind(
		`INSERT INTO runs (id, target_id, user_id, status, trigger, prev_run_id, summary_json, diff_json, error, created_at, started_at, finished_at)
		 VALUES (?, ?, ?, ?, 'walkthrough', NULL, '{}', '', '', ?, ?, ?)`),
		runID, wk.TargetID, userID, RunDone, toRFC(now), toRFC(now), toRFC(now),
	); err != nil {
		return "", err
	}
	// a11y digest artifacts are Put to the (non-transactional) Store ONLY AFTER the tx
	// commits: during the tx we compute each step's a11y_digest_key + set it on the pages
	// row, and BUFFER the (key, bytes) pairs here — but write nothing to the Store yet. If
	// any INSERT below fails, the deferred tx.Rollback() unwinds the run/pages/CAS and,
	// because nothing was Put, zero a11y.json blobs are orphaned (the storage-leak fix).
	type pendingDigest struct {
		key  string
		blob []byte
	}
	var pending []pendingDigest
	for _, s := range steps {
		label := walkthroughStepLabel(s, pad)
		// Phase 2: carry the step's captured DOM/a11y digest through to a per-synthetic-
		// page a11y.json artifact + pages.a11y_digest_key, so the EXISTING eval read path
		// (loadDigest) and the deterministic dropContradicted gate work UNCHANGED on the
		// DRIVEN path — converging both paths on one eval read path. A step with no digest
		// (pre-0061 walkthrough / capture failure) → no key → the page degrades to
		// screenshot-only (load-bearing backward-compat).
		a11yKey := ""
		if store != nil && strings.TrimSpace(s.DigestJSON) != "" {
			a11yKey = storage.A11yDigestKey(targetSlug, runID, storage.PageSlug(label))
			pending = append(pending, pendingDigest{key: a11yKey, blob: []byte(s.DigestJSON)})
		}
		// One page per step, IN ORDER, at the desktop viewport. Screenshot key REUSED
		// (a step with none → empty, eval degrades to no image). Remaining page columns
		// take their zero values (this is an eval vessel, not a crawled page).
		if _, err := tx.Exec(d.rebind(
			`INSERT INTO pages (id, run_id, url, viewport, screenshot_key, axe_key, a11y_digest_key,
				axe_violation_count, console_first_party_count, console_third_party_count,
				network_first_party_count, network_third_party_count, load_ms,
				diff_pct, diff_key, lcp_ms, cls, tbt_ms, weight_bytes, req_count, created_at)
			 VALUES (?, ?, ?, ?, ?, '', ?, 0, 0, 0, 0, 0, 0, 0, '', 0, 0, 0, 0, 0, ?)`),
			uuid.NewString(), runID, label, "desktop", s.ScreenshotKey, a11yKey, toRFC(now),
		); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	// Post-commit: the run + pages are durably committed and valid, so now Put each
	// buffered a11y digest. A Put failure here is NON-FATAL: the page row already
	// references the key, so a missing blob makes loadDigest fetch-error → nil → that
	// page degrades to screenshot-only — the EXACT same graceful degradation as a
	// digest-less step. Log it best-effort; never fail the materialization (the run is
	// already committed). Only the CAS winner reaches this loop; the losing racer rolled
	// back above and never Puts.
	for _, p := range pending {
		if perr := store.Put(ctx, p.key, "application/json", bytes.NewReader(p.blob), int64(len(p.blob))); perr != nil {
			log.Printf("materialize: put a11y digest for walkthrough %s (key %s): %v", walkthroughID, p.key, perr)
		}
	}
	return runID, nil
}

// labelPadWidth returns the zero-pad width for the step-index prefix: enough digits
// to hold nSteps (min 2, so the common ≤99-step case reads "01".."99"). Deriving it
// from the count keeps ListPages' lexical `ORDER BY url` in true step order even if a
// future action budget pushes the step count past 99.
func labelPadWidth(nSteps int) int {
	w := 2
	for n := nSteps; n >= 100; n /= 10 {
		w++
	}
	return w
}

// walkthroughStepLabel builds the stable, order-preserving synthetic-page label for one
// step: "{NN} · {action-type} · {step-url}". The zero-padded 1-based index (pad width
// from labelPadWidth) keeps the per-step pages distinct AND in flow order under
// ListPages' `ORDER BY url`.
func walkthroughStepLabel(s *WalkthroughStep, pad int) string {
	typ := "step"
	var a struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(s.ActionJSON), &a) == nil && a.Type != "" {
		typ = a.Type
	}
	return fmt.Sprintf("%0*d · %s · %s", pad, s.Idx+1, typ, s.URL)
}

// walkthroughColsPrefixed returns walkthroughCols with each column prefixed by the
// table alias (for the owner-scoped JOIN queries).
func walkthroughColsPrefixed(alias string) string {
	return alias + `.id, ` + alias + `.target_id, ` + alias + `.run_id, ` + alias + `.goal, ` +
		alias + `.outcome, ` + alias + `.stuck_step, ` + alias + `.reason, ` + alias + `.dry_run, ` +
		alias + `.status, ` + alias + `.steps_total, ` + alias + `.steps_done, ` + alias + `.cost_usd, ` +
		alias + `.prompt_tokens, ` + alias + `.completion_tokens, ` + alias + `.prev_walkthrough_id, ` +
		alias + `.diff_json, ` + alias + `.infra_failed, ` + alias + `.created_at, ` + alias + `.updated_at`
}

func scanWalkthrough(s scanner) (*Walkthrough, error) {
	var w Walkthrough
	var created, updated string
	var dryRun, infraFailed int
	if err := s.Scan(&w.ID, &w.TargetID, &w.RunID, &w.Goal, &w.Outcome, &w.StuckStep, &w.Reason, &dryRun,
		&w.Status, &w.StepsTotal, &w.StepsDone, &w.CostUSD, &w.PromptTokens, &w.CompletionTokens,
		&w.PrevWalkthroughID, &w.DiffJSON, &infraFailed, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	w.DryRun = dryRun != 0
	w.InfraFailed = infraFailed != 0
	w.CreatedAt = parseTime(created)
	w.UpdatedAt = parseTime(updated)
	return &w, nil
}
