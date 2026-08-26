package db

import (
	"database/sql"
	"time"
)

// TrendPoint is one completed run's per-type finding counts, for the target's
// findings-over-time trend chart. Counts come from the findings table (one row
// per finding: a11y=one per axe violation, console/network=one per error,
// layout/perf=one per deterministic smell/breach), grouped by run and type.
type TrendPoint struct {
	RunID   string
	At      time.Time // run's finished_at (chronological x-axis)
	A11y    int
	Layout  int
	Console int
	Network int
	Perf    int
}

// TargetFindingTrend returns, in chronological order (oldest→newest), the
// per-type finding counts for each COMPLETED ('done') run of a target. It is a
// single owner-scoped aggregate: findings→pages→runs joined to targets, filtered
// on targets.user_id so another user's target returns nothing. A run with zero
// findings still appears (all counts 0) via LEFT JOINs. Callers use this to plot
// slow debt creep the consecutive-run P2 diff can't see. Returns an empty slice
// (not nil error) when the target has 0 or 1 completed run — the UI decides
// whether there's enough history to draw a trend.
//
// Synthetic walkthrough runs (trigger='walkthrough') are EXCLUDED: they are
// 'done' but carry ZERO findings (an eval vessel, not an audit), so including one
// would inject a false zero as the newest point and read as "findings just dropped
// to zero", corrupting the slow-creep signal. The trigger column is NOT NULL
// DEFAULT 'manual', so `<>` is safe in both dialects (no three-valued logic).
func (d *DB) TargetFindingTrend(userID, targetID string) ([]TrendPoint, error) {
	rows, err := d.query(`
		SELECT r.id, r.finished_at,
		       SUM(CASE WHEN f.type='a11y'    THEN 1 ELSE 0 END) AS a11y,
		       SUM(CASE WHEN f.type='layout'  THEN 1 ELSE 0 END) AS layout,
		       SUM(CASE WHEN f.type='console' THEN 1 ELSE 0 END) AS console,
		       SUM(CASE WHEN f.type='network' THEN 1 ELSE 0 END) AS network,
		       SUM(CASE WHEN f.type='perf'    THEN 1 ELSE 0 END) AS perf
		FROM runs r
		JOIN targets t ON t.id = r.target_id
		LEFT JOIN pages p ON p.run_id = r.id
		LEFT JOIN findings f ON f.page_id = p.id
		WHERE t.user_id = ? AND r.target_id = ? AND r.status = ? AND r.trigger <> 'walkthrough'
		GROUP BY r.id, r.finished_at, r.created_at
		ORDER BY r.finished_at ASC, r.created_at ASC`,
		userID, targetID, RunDone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TrendPoint
	for rows.Next() {
		var (
			p                                    TrendPoint
			finished                             sql.NullString
			a11y, layout, console, network, perf int64
		)
		if err := rows.Scan(&p.RunID, &finished, &a11y, &layout, &console, &network, &perf); err != nil {
			return nil, err
		}
		if finished.Valid {
			p.At = parseTime(finished.String)
		}
		p.A11y, p.Layout, p.Console, p.Network, p.Perf = int(a11y), int(layout), int(console), int(network), int(perf)
		out = append(out, p)
	}
	return out, rows.Err()
}
