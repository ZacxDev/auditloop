package db

import (
	"github.com/google/uuid"
)

// InsertPage persists one crawled page row and returns its id.
func (d *DB) InsertPage(p *Page) (string, error) {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = parseTime(nowRFC())
	}
	_, err := d.exec(
		`INSERT INTO pages (id, run_id, url, viewport, screenshot_key, axe_key, a11y_digest_key,
			axe_violation_count, console_first_party_count, console_third_party_count,
			network_first_party_count, network_third_party_count, load_ms,
			diff_pct, diff_key, lcp_ms, cls, tbt_ms, weight_bytes, req_count, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.RunID, p.URL, p.Viewport, p.ScreenshotKey, p.AxeKey, p.A11yDigestKey,
		p.AxeViolationCount, p.ConsoleFirstPartyCount, p.ConsoleThirdPartyCount,
		p.NetworkFirstPartyCount, p.NetworkThirdPartyCount, p.LoadMS,
		p.DiffPct, p.DiffKey, p.LCPMs, p.CLS, p.TBTMs, p.WeightBytes, p.ReqCount, toRFC(p.CreatedAt),
	)
	return p.ID, err
}

// UpdatePageDiff records a page's visual-diff result (P2): the percent of pixels
// changed vs the matched baseline and the diff image's storage key.
func (d *DB) UpdatePageDiff(pageID string, diffPct float64, diffKey string) error {
	_, err := d.exec(`UPDATE pages SET diff_pct=?, diff_key=? WHERE id=?`, diffPct, diffKey, pageID)
	return err
}

// ListPages returns all pages for a run, ordered by url then viewport.
func (d *DB) ListPages(runID string) ([]*Page, error) {
	rows, err := d.query(
		`SELECT id, run_id, url, viewport, screenshot_key, axe_key, a11y_digest_key,
			axe_violation_count, console_first_party_count, console_third_party_count,
			network_first_party_count, network_third_party_count, load_ms,
			diff_pct, diff_key, lcp_ms, cls, tbt_ms, weight_bytes, req_count, created_at
		 FROM pages WHERE run_id=? ORDER BY url ASC, viewport ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Page
	for rows.Next() {
		var p Page
		var created string
		if err := rows.Scan(&p.ID, &p.RunID, &p.URL, &p.Viewport, &p.ScreenshotKey, &p.AxeKey, &p.A11yDigestKey,
			&p.AxeViolationCount, &p.ConsoleFirstPartyCount, &p.ConsoleThirdPartyCount,
			&p.NetworkFirstPartyCount, &p.NetworkThirdPartyCount, &p.LoadMS,
			&p.DiffPct, &p.DiffKey, &p.LCPMs, &p.CLS, &p.TBTMs, &p.WeightBytes, &p.ReqCount, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTime(created)
		out = append(out, &p)
	}
	return out, rows.Err()
}
