package db

import "github.com/google/uuid"

// InsertFinding persists one finding attached to a page.
func (d *DB) InsertFinding(f *Finding) (string, error) {
	if f.ID == "" {
		f.ID = uuid.NewString()
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = parseTime(nowRFC())
	}
	if f.Detail == "" {
		f.Detail = "{}"
	}
	_, err := d.exec(
		`INSERT INTO findings (id, page_id, type, severity, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.ID, f.PageID, f.Type, f.Severity, f.Detail, toRFC(f.CreatedAt),
	)
	return f.ID, err
}

// ListFindings returns findings for a page.
func (d *DB) ListFindings(pageID string) ([]*Finding, error) {
	rows, err := d.query(
		`SELECT id, page_id, type, severity, detail, created_at
		 FROM findings WHERE page_id=? ORDER BY severity ASC, created_at ASC`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Finding
	for rows.Next() {
		var f Finding
		var created string
		if err := rows.Scan(&f.ID, &f.PageID, &f.Type, &f.Severity, &f.Detail, &created); err != nil {
			return nil, err
		}
		f.CreatedAt = parseTime(created)
		out = append(out, &f)
	}
	return out, rows.Err()
}
