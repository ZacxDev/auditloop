package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ScopeRead is the only API-key scope today: read-only pulls of audit results.
const ScopeRead = "read"

// APIKey is a per-user, read-only API key used for machine-authenticated pulls
// (agents/CLIs) of audit results WITHOUT a Supabase user JWT. The plaintext key
// is shown to the user ONCE at creation; only its sha256 hash is stored and it
// is never returned by ListAPIKeys (which is for the UI). A key reads ONLY data
// owned by its UserID.
type APIKey struct {
	ID         string
	UserID     string
	Name       string
	Scope      string
	CreatedAt  time.Time
	LastUsedAt *time.Time // nil until the key is first used
}

// CreateAPIKey inserts a read-only API key for userID, storing ONLY the sha256
// hash (never the plaintext). Returns the new key id.
func (d *DB) CreateAPIKey(userID, name, tokenHash, scope string) (string, error) {
	if scope == "" {
		scope = ScopeRead
	}
	id := uuid.NewString()
	_, err := d.exec(
		`INSERT INTO api_keys (id, user_id, name, token_hash, scope, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		id, userID, name, tokenHash, scope, nowRFC(),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// APIKeyLookup resolves an API key by the sha256 hash of a presented token. It
// returns the owning user id, the key scope, AND the stored hash (so the caller
// can do a final constant-time comparison). found=false (no error) when no key
// matches — the middleware answers 401. It never returns any plaintext.
func (d *DB) APIKeyLookup(tokenHash string) (userID, scope, storedHash string, found bool, err error) {
	row := d.queryRow(
		`SELECT user_id, scope, token_hash FROM api_keys WHERE token_hash = ?`, tokenHash)
	if scanErr := row.Scan(&userID, &scope, &storedHash); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return "", "", "", false, nil
		}
		return "", "", "", false, scanErr
	}
	return userID, scope, storedHash, true, nil
}

// ListAPIKeys returns a user's API keys, newest first. It NEVER returns the
// token hash or any plaintext — only display metadata (name/created/last_used).
func (d *DB) ListAPIKeys(userID string) ([]*APIKey, error) {
	rows, err := d.query(
		`SELECT id, user_id, name, scope, created_at, last_used_at
		 FROM api_keys WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		var k APIKey
		var created string
		var lastUsed sql.NullString
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Scope, &created, &lastUsed); err != nil {
			return nil, err
		}
		k.CreatedAt = parseTime(created)
		k.LastUsedAt = parseTimePtr(lastUsed)
		out = append(out, &k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deletes a key, scoped to its owner (a user can only revoke their
// own key). Returns ErrNotFound when no owned key matches (so a foreign id can't
// be probed). Rotation = RevokeAPIKey(old) + CreateAPIKey(new).
func (d *DB) RevokeAPIKey(userID, id string) error {
	res, err := d.exec(`DELETE FROM api_keys WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAPIKeyLastUsed records the last-used timestamp for a key (best effort:
// callers ignore the error — it must never fail a read).
func (d *DB) TouchAPIKeyLastUsed(tokenHash string) error {
	_, err := d.exec(`UPDATE api_keys SET last_used_at=? WHERE token_hash=?`, nowRFC(), tokenHash)
	return err
}

// LatestDoneRunForTargetOwned returns the target's most recent 'done' run,
// scoped to userID (both the target ownership AND the run's user_id are enforced
// so a key cannot resolve another user's run). Returns (nil, nil) when there is
// no completed run for the (user, target) pair.
func (d *DB) LatestDoneRunForTargetOwned(userID, targetID string) (*Run, error) {
	row := d.queryRow(
		`SELECT `+runCols+` FROM runs
		 WHERE target_id=? AND user_id=? AND status=? AND trigger<>'walkthrough' ORDER BY created_at DESC LIMIT 1`,
		targetID, userID, RunDone)
	r, err := scanRun(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
