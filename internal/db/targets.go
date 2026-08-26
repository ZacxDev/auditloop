package db

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a scoped lookup misses.
var ErrNotFound = errors.New("db: not found")

// targetCols is the shared SELECT column list for a Target (kept in one place so
// the driving_enabled / allow_real_submit hot-path columns stay consistent across
// every target query). COALESCE guards rows written before the Phase-3 migrations.
const targetCols = `id, user_id, name, base_url, verified_domains, auth_mode, COALESCE(driving_enabled,0), COALESCE(allow_real_submit,0), created_at`

// CreateTarget inserts a target owned by userID and returns it. verifiedDomains
// defaults to the base URL's host when empty (the registration IS the trust
// signal for now; a real DNS-TXT verification is a later seam).
func (d *DB) CreateTarget(userID, name, baseURL string, verifiedDomains []string) (*Target, error) {
	t := &Target{
		ID:              uuid.NewString(),
		UserID:          userID,
		Name:            name,
		BaseURL:         baseURL,
		VerifiedDomains: verifiedDomains,
		AuthMode:        AuthNone,
		CreatedAt:       parseTime(nowRFC()),
	}
	_, err := d.exec(
		`INSERT INTO targets (id, user_id, name, base_url, verified_domains, auth_mode, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.BaseURL, marshalStrings(t.VerifiedDomains), t.AuthMode, toRFC(t.CreatedAt),
	)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// GetTarget returns a target scoped to userID.
func (d *DB) GetTarget(userID, id string) (*Target, error) {
	row := d.queryRow(
		`SELECT `+targetCols+` FROM targets WHERE id=? AND user_id=?`, id, userID)
	return scanTarget(row)
}

// GetTargetByName returns a target owned by userID whose name matches exactly.
// ALWAYS filtered by user_id — a name lookup never crosses users. Names are not
// unique per user, so on collision (a user with duplicate names) this
// deterministically returns the MOST RECENTLY CREATED target (ORDER BY
// created_at DESC LIMIT 1). Miss → ErrNotFound. This lets the read API resolve a
// target by the same stable spec name the push side keys on, owner-scoped.
func (d *DB) GetTargetByName(userID, name string) (*Target, error) {
	row := d.queryRow(
		`SELECT `+targetCols+` FROM targets WHERE user_id=? AND name=? ORDER BY created_at DESC LIMIT 1`, userID, name)
	return scanTarget(row)
}

// GetTargetByID returns a target without user scoping (worker use, where the
// run already carries the owning user).
func (d *DB) GetTargetByID(id string) (*Target, error) {
	row := d.queryRow(
		`SELECT `+targetCols+` FROM targets WHERE id=?`, id)
	return scanTarget(row)
}

// ListTargets returns a user's targets, newest first.
func (d *DB) ListTargets(userID string) ([]*Target, error) {
	rows, err := d.query(
		`SELECT `+targetCols+` FROM targets WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTarget(s scanner) (*Target, error) {
	var t Target
	var domains, created string
	var driving, allowSubmit int
	if err := s.Scan(&t.ID, &t.UserID, &t.Name, &t.BaseURL, &domains, &t.AuthMode, &driving, &allowSubmit, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.VerifiedDomains = unmarshalStrings(domains)
	t.DrivingEnabled = driving != 0
	t.AllowRealSubmit = allowSubmit != 0
	t.CreatedAt = parseTime(created)
	return &t, nil
}

// SetDrivingConfig sets a target's Phase-3 driver flags (owner-scoped). It is the
// write path for the loud, default-off opt-ins: driving_enabled (may we drive at
// all) and allow_real_submit (may we perform REAL mutating submissions vs. the
// default dry-run submit-guard). Returns ErrNotFound for a foreign/unknown target.
func (d *DB) SetDrivingConfig(userID, targetID string, drivingEnabled, allowRealSubmit bool) error {
	res, err := d.exec(
		`UPDATE targets SET driving_enabled=?, allow_real_submit=? WHERE id=? AND user_id=?`,
		boolToInt(drivingEnabled), boolToInt(allowRealSubmit), targetID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
