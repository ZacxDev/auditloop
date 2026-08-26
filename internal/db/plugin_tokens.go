package db

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// CreatePluginTarget creates a PUSH-ONLY target (auth_mode='plugin', P5). Such a
// target is never crawled: an external harness pushes completed audit runs to it
// via POST /api/plugins/runs. baseURL is stored as a free-text label (may be
// empty) — it is not required to be crawlable and no verified_domains are set.
// The caller mints a push token separately (SetPluginToken).
func (d *DB) CreatePluginTarget(userID, name, label string) (*Target, error) {
	t := &Target{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      name,
		BaseURL:   label,
		AuthMode:  AuthPlugin,
		CreatedAt: parseTime(nowRFC()),
	}
	_, err := d.exec(
		`INSERT INTO targets (id, user_id, name, base_url, verified_domains, auth_mode, created_at)
		 VALUES (?, ?, ?, ?, '[]', ?, ?)`,
		t.ID, t.UserID, t.Name, t.BaseURL, t.AuthMode, toRFC(t.CreatedAt),
	)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// SetPluginToken upserts the (single) push-token hash for a plugin target. Only
// the sha256 hash is stored — never the plaintext token. Calling it again
// ROTATES the token (replaces the hash on the target_id PK), invalidating the
// previous token.
func (d *DB) SetPluginToken(targetID, tokenHash string) error {
	now := nowRFC()
	// Portable upsert: try UPDATE, fall back to INSERT (no ON CONFLICT so the DDL
	// stays dual-dialect-simple, matching the rest of the layer).
	res, err := d.exec(`UPDATE plugin_tokens SET token_hash=?, updated_at=? WHERE target_id=?`, tokenHash, now, targetID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = d.exec(
		`INSERT INTO plugin_tokens (target_id, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		targetID, tokenHash, now, now,
	)
	return err
}

// PluginTokenLookup resolves a plugin target by the sha256 hash of a presented
// push token. It returns the target AND the stored hash (so the caller can do a
// final constant-time comparison). The JOIN enforces auth_mode='plugin', so a
// token whose target is not a plugin target (or any non-existent token) yields
// ErrNotFound → the handler answers 401. It never returns credential material.
func (d *DB) PluginTokenLookup(tokenHash string) (*Target, string, error) {
	row := d.queryRow(
		`SELECT t.id, t.user_id, t.name, t.base_url, t.verified_domains, t.auth_mode, t.created_at, pt.token_hash
		 FROM plugin_tokens pt JOIN targets t ON t.id = pt.target_id
		 WHERE pt.token_hash = ? AND t.auth_mode = ?`, tokenHash, AuthPlugin)
	var t Target
	var domains, created, storedHash string
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.BaseURL, &domains, &t.AuthMode, &created, &storedHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	t.VerifiedDomains = unmarshalStrings(domains)
	t.CreatedAt = parseTime(created)
	return &t, storedHash, nil
}

// HasPluginToken reports whether a plugin target already has a push token.
func (d *DB) HasPluginToken(targetID string) (bool, error) {
	var n int
	if err := d.queryRow(`SELECT COUNT(*) FROM plugin_tokens WHERE target_id=?`, targetID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
