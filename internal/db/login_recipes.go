package db

// P4: login-recipe persistence. Credential VALUES never appear here — the caller
// passes an already-encrypted blob (crecdsEncrypted) and reads it back for
// server-side decryption only. Saving/clearing a recipe also flips the target's
// auth_mode between 'login' and 'none' in the same transaction.

// SetLoginRecipe upserts a target's login recipe and flips auth_mode='login'.
// One row per target (target_id PK). The steps and success condition are stored
// as given; creds must already be the encrypted (base64) blob.
func (d *DB) SetLoginRecipe(lr *LoginRecipe) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowRFC()
	// UPDATE-else-INSERT (portable upsert; avoids dialect-specific ON CONFLICT).
	res, err := tx.Exec(d.rebind(
		`UPDATE login_recipes SET login_url=?, steps_json=?, success_selector=?,
		 success_url_contains=?, success_timeout_ms=?, creds_encrypted=?, updated_at=?
		 WHERE target_id=?`),
		lr.LoginURL, lr.StepsJSON, lr.SuccessSelector, lr.SuccessURLContains,
		lr.SuccessTimeoutMs, lr.CredsEncrypted, now, lr.TargetID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec(d.rebind(
			`INSERT INTO login_recipes
			 (target_id, login_url, steps_json, success_selector, success_url_contains,
			  success_timeout_ms, creds_encrypted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			lr.TargetID, lr.LoginURL, lr.StepsJSON, lr.SuccessSelector, lr.SuccessURLContains,
			lr.SuccessTimeoutMs, lr.CredsEncrypted, now, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(d.rebind(`UPDATE targets SET auth_mode=? WHERE id=?`),
		AuthLogin, lr.TargetID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetLoginRecipe returns a target's login recipe, or ErrNotFound when none.
func (d *DB) GetLoginRecipe(targetID string) (*LoginRecipe, error) {
	row := d.queryRow(
		`SELECT target_id, login_url, steps_json, success_selector, success_url_contains,
		        success_timeout_ms, creds_encrypted, created_at, updated_at
		 FROM login_recipes WHERE target_id=?`, targetID)
	var lr LoginRecipe
	var created, updated string
	if err := row.Scan(&lr.TargetID, &lr.LoginURL, &lr.StepsJSON, &lr.SuccessSelector,
		&lr.SuccessURLContains, &lr.SuccessTimeoutMs, &lr.CredsEncrypted, &created, &updated); err != nil {
		return nil, mapNoRows(err)
	}
	lr.CreatedAt = parseTime(created)
	lr.UpdatedAt = parseTime(updated)
	return &lr, nil
}

// DeleteLoginRecipe removes a target's recipe and flips auth_mode='none'.
func (d *DB) DeleteLoginRecipe(targetID string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(d.rebind(`DELETE FROM login_recipes WHERE target_id=?`), targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(d.rebind(`UPDATE targets SET auth_mode=? WHERE id=?`), AuthNone, targetID); err != nil {
		return err
	}
	return tx.Commit()
}
