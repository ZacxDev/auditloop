package walkthrough

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/diff"
	"github.com/ZacxDev/auditloop/internal/report"
)

// Phase 4 — walkthrough-vs-walkthrough regression diffing. Mirrors the P2 crawl
// diff (internal/worker/diff.go + report.Diff): a walkthrough is compared to the
// target's PREVIOUS terminal walkthrough (walkthroughs.prev_walkthrough_id, stamped
// at CreateWalkthrough), surfacing REGRESSIONS — the goal stopped being reachable,
// it got stuck earlier, or NEW persona task-blockers appeared — plus a machine
// signal a CI gate can fail on. Reuses diff.StringSetDelta for the blocker delta.

// outcomeRank orders the deterministic driver outcomes so a DROP is a regression and
// a RISE is a resolution. success > stuck > failed > "" (non-terminal, shouldn't occur).
func outcomeRank(outcome string) int {
	switch outcome {
	case db.WalkOutcomeSuccess:
		return 3
	case db.WalkOutcomeStuck:
		return 2
	case db.WalkOutcomeFailed:
		return 1
	default:
		return 0
	}
}

// ComputeDiff builds the deterministic walkthrough regression summary from the
// baseline + current walkthroughs and their (already-extracted) persona
// task-blocker identity keys. It is PURE (no DB/LLM) so it is table-tested directly.
//
//   - IsRegression = the outcome rank dropped (success→stuck|failed, stuck→failed)
//     OR, when BOTH are stuck, it got stuck EARLIER (StuckStepDelta < 0).
//   - Resolved = the outcome rank rose (stuck|failed→success).
//   - New task-blockers are a SEPARATE gate signal — the CI gate ORs them with
//     IsRegression. The blocker delta is only meaningful when blockersCompared is
//     true (both walkthroughs had a completed persona evaluation); otherwise both
//     blocker slices are empty (degrade — never a false "everything resolved").
//   - INFRA FAILURES ARE NOT SCORED (#45). When the current walkthrough is
//     cur.InfraFailed (the driver/browser could not run — a watchdog-killed stall, a
//     setup failure, a restart sweep) the pass produced NO OBSERVATION of the goal, so
//     OutcomeCompared=false and IsRegression/Resolved/StuckStepDelta are forced off.
//     The descriptive fields still populate so the UI can show what happened.
func ComputeDiff(prev, cur *db.Walkthrough, prevKeys, curKeys []string, blockersCompared bool) *report.WalkthroughDiff {
	// The current walkthrough's infra state comes off the row itself — no extra
	// parameter, so no caller can pass a value that disagrees with what was persisted.
	//
	// The prev check is BELT AND BRACES WITH NO KNOWN REACHABLE PATH TODAY, and the
	// honest reason to keep it is that it is one term, not that it defends a case we
	// can name. Why nothing reaches it now: latestTerminalWalkthroughID excludes
	// infra_failed rows when the link is STAMPED, handleStartWalkthrough always creates
	// a FRESH row (so an already-linked baseline is never re-driven into an infra
	// failure afterwards), and a PRE-0064 row cannot trigger it either — migration 0064
	// is DEFAULT 0, so such a row reads back false. It exists so that a future path
	// which does re-run an existing walkthrough row, or a baseline flipped by some
	// later writer, cannot silently turn a non-observation back into a scored verdict.
	// If you are here because you added such a path: this term is the guard, keep it.
	infra := cur.InfraFailed || prev.InfraFailed
	d := &report.WalkthroughDiff{
		PrevWalkthroughID: prev.ID,
		PrevAt:            prev.UpdatedAt,
		PrevOutcome:       prev.Outcome,
		Outcome:           cur.Outcome,
		OutcomeChanged:    prev.Outcome != cur.Outcome,
		PrevStuckStep:     prev.StuckStep,
		StuckStep:         cur.StuckStep,
		PrevReason:        prev.Reason,
		Reason:            cur.Reason,
		ReasonChanged:     strings.TrimSpace(prev.Reason) != strings.TrimSpace(cur.Reason),
		BlockersCompared:  blockersCompared,
		OutcomeCompared:   !infra,
		InfraFailed:       cur.InfraFailed,
	}

	if !infra {
		pr, cr := outcomeRank(prev.Outcome), outcomeRank(cur.Outcome)
		switch {
		case cr < pr:
			d.IsRegression = true
		case cr > pr:
			d.Resolved = true
		}
		// Stuck-step movement is only comparable when BOTH runs are stuck.
		if prev.Outcome == db.WalkOutcomeStuck && cur.Outcome == db.WalkOutcomeStuck {
			d.StuckStepDelta = cur.StuckStep - prev.StuckStep
			if d.StuckStepDelta < 0 { // stuck earlier than before = regression
				d.IsRegression = true
			}
		}
	} else {
		// A walkthrough that never ran also has no persona evaluation of a driven
		// trace, so blockersCompared is already false. ASSERT it rather than assume:
		// forcing it off here guarantees a non-observation can never emit a blocker
		// delta (which a CI gate ORs into --fail-on-regression).
		blockersCompared = false
		d.BlockersCompared = false
	}

	if blockersCompared {
		added, removed := diff.StringSetDelta(prevKeys, curKeys)
		d.NewTaskBlockers = added
		d.ResolvedTaskBlockers = removed
	}
	if d.NewTaskBlockers == nil {
		d.NewTaskBlockers = []string{}
	}
	return d
}

// blockerKeySep separates the persona from the normalized anchor in a task-blocker
// identity key. \x1f (ASCII unit separator) never appears in a selector or issue.
const blockerKeySep = "\x1f"

// blockerKey is the STABLE identity of one persona task-blocker across walkthroughs:
// the persona plus the normalized selector (the DOM anchor — the most stable handle),
// falling back to the normalized issue text when the model gave no selector. Keying on
// persona means a blocker that newly affects an ADDITIONAL persona is a NEW regression.
func blockerKey(persona string, b report.EvalFinding) string {
	anchor := normalizeBlocker(b.Selector)
	if anchor == "" {
		anchor = normalizeBlocker(b.Issue)
	}
	return persona + blockerKeySep + anchor
}

// normalizeBlocker lowercases + collapses whitespace + trims so trivial wording/format
// drift doesn't churn the blocker set run to run.
func normalizeBlocker(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// blockerKeysFromEvaluations parses the stored (verified) blockers out of a run's
// page_evaluations and returns their deduped stable identity keys.
func blockerKeysFromEvaluations(evals []*db.PageEvaluation) []string {
	seen := map[string]bool{}
	var keys []string
	for _, pe := range evals {
		if strings.TrimSpace(pe.FindingsJSON) == "" {
			continue
		}
		var pv report.PageEvaluation
		if err := json.Unmarshal([]byte(pe.FindingsJSON), &pv); err != nil {
			continue // malformed cell → skip (degrade, never panic)
		}
		for _, b := range pv.Blockers {
			if !b.Verified {
				// Only VERIFIED blockers (kept by the eval verify-pass) feed the
				// regression gate. An unverified / verify-degraded blocker (stored
				// with Verified=false when the verify parse fails or a re-run used
				// verify=off) is the SAFER degrade — it contributes zero blockers, so
				// it never trips the CI --fail-on-regression gate with noisy churn.
				continue
			}
			k := blockerKey(pe.Persona, b)
			if k == pe.Persona+blockerKeySep { // no anchor at all → not identifiable
				continue
			}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// walkthroughBlockerKeys loads a walkthrough's persona task-blocker keys. It returns
// (keys, true) ONLY when the walkthrough was materialized into a synthetic run whose
// persona evaluation COMPLETED (eval_status=done) — otherwise (nil, false) so the
// caller degrades (skips the blocker delta rather than reporting a false one).
func walkthroughBlockerKeys(database *db.DB, wk *db.Walkthrough) ([]string, bool) {
	if wk.RunID == "" {
		return nil, false
	}
	run, err := database.GetRunByID(wk.RunID)
	if err != nil || run.EvalStatus != db.EvalDone {
		return nil, false
	}
	evals, err := database.ListPageEvaluations(wk.RunID)
	if err != nil {
		return nil, false
	}
	return blockerKeysFromEvaluations(evals), true
}

// RefreshWalkthroughDiff (re)computes the walkthrough's regression summary vs its
// baseline (prev_walkthrough_id) and persists it to walkthroughs.diff_json. It is
// idempotent and safe to call at BOTH triggers:
//
//   - at drive end (generator.Run, after FinishWalkthrough) — the deterministic
//     outcome/stuck delta is available immediately; the current walkthrough has no
//     persona evaluation yet, so the blocker delta degrades (BlockersCompared=false).
//   - after the persona evaluation of the driven trace COMPLETES (the handler's
//     eval goroutine) — a re-run now fills in NewTaskBlockers.
//
// Returns (nil, nil) when there is no baseline (the target's first walkthrough) —
// diff_json stays "" and the caller renders the "first walkthrough" note. The caller
// decides whether to bump the regression metric (only the drive-end call does, so the
// outcome/stuck regression is counted exactly once per walkthrough).
func RefreshWalkthroughDiff(ctx context.Context, database *db.DB, walkthroughID string) (*report.WalkthroughDiff, error) {
	cur, err := database.GetWalkthroughByID(walkthroughID)
	if err != nil {
		return nil, err
	}
	if cur.PrevWalkthroughID == "" {
		return nil, nil // first walkthrough — nothing to diff
	}
	prev, err := database.GetWalkthroughByID(cur.PrevWalkthroughID)
	if err != nil {
		return nil, nil // baseline vanished — degrade to no diff
	}

	prevKeys, prevHas := walkthroughBlockerKeys(database, prev)
	curKeys, curHas := walkthroughBlockerKeys(database, cur)
	d := ComputeDiff(prev, cur, prevKeys, curKeys, prevHas && curHas)

	blob, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if err := database.SetWalkthroughDiff(walkthroughID, string(blob)); err != nil {
		return nil, err
	}
	return d, nil
}
