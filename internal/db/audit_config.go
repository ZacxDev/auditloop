package db

import "encoding/json"

// Persona-walkthrough Phase-2: per-target audit config persistence. One row per
// target (target_id PK). Mirrors login_recipes.go — SetTargetAuditConfig is an
// UPDATE-else-INSERT upsert (unscoped; the handler owns the ownership check via
// GetTarget), GetTargetAuditConfig is owner-scoped via a target→user JOIN. Persona
// id ALLOWLIST validation happens at the handler layer (this layer stores whatever
// []string it is handed as a JSON array).

// SetTargetAuditConfig upserts a target's audit config. Personas is marshalled to
// the personas_json array. The created_at is preserved on update.
func (d *DB) SetTargetAuditConfig(cfg *TargetAuditConfig) error {
	personasJSON := "[]"
	if len(cfg.Personas) > 0 {
		if b, err := json.Marshal(cfg.Personas); err == nil {
			personasJSON = string(b)
		}
	}
	inferred := boolToInt(cfg.Inferred)
	confirmed := boolToInt(cfg.Confirmed)
	now := nowRFC()

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(d.rebind(
		`UPDATE target_audit_config SET product_summary=?, primary_job=?, primary_cta=?,
		 personas_json=?, inferred=?, confirmed=?, success_selector=?, success_url_contains=?,
		 success_timeout_ms=?, updated_at=? WHERE target_id=?`),
		cfg.ProductSummary, cfg.PrimaryJob, cfg.PrimaryCTA, personasJSON,
		inferred, confirmed, cfg.SuccessSelector, cfg.SuccessURLContains, cfg.SuccessTimeoutMs,
		now, cfg.TargetID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec(d.rebind(
			`INSERT INTO target_audit_config
			 (target_id, product_summary, primary_job, primary_cta, personas_json,
			  inferred, confirmed, success_selector, success_url_contains, success_timeout_ms,
			  created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			cfg.TargetID, cfg.ProductSummary, cfg.PrimaryJob, cfg.PrimaryCTA, personasJSON,
			inferred, confirmed, cfg.SuccessSelector, cfg.SuccessURLContains, cfg.SuccessTimeoutMs,
			now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetTargetAuditConfig returns a target's audit config, owner-scoped via the
// target→user join (a foreign target's config is never returned). found is false
// (nil, false, nil) when there is no config for the (user, target) pair.
func (d *DB) GetTargetAuditConfig(userID, targetID string) (*TargetAuditConfig, bool, error) {
	row := d.queryRow(
		`SELECT c.target_id, c.product_summary, c.primary_job, c.primary_cta,
		        c.personas_json, c.inferred, c.confirmed,
		        COALESCE(c.success_selector,''), COALESCE(c.success_url_contains,''), COALESCE(c.success_timeout_ms,0),
		        c.created_at, c.updated_at
		 FROM target_audit_config c
		 JOIN targets t ON t.id = c.target_id
		 WHERE c.target_id=? AND t.user_id=?`, targetID, userID)
	var cfg TargetAuditConfig
	var personasJSON, created, updated string
	var inferred, confirmed int
	if err := row.Scan(&cfg.TargetID, &cfg.ProductSummary, &cfg.PrimaryJob, &cfg.PrimaryCTA,
		&personasJSON, &inferred, &confirmed,
		&cfg.SuccessSelector, &cfg.SuccessURLContains, &cfg.SuccessTimeoutMs,
		&created, &updated); err != nil {
		if err := mapNoRows(err); err == ErrNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if personasJSON != "" {
		_ = json.Unmarshal([]byte(personasJSON), &cfg.Personas)
	}
	cfg.Inferred = inferred != 0
	cfg.Confirmed = confirmed != 0
	cfg.CreatedAt = parseTime(created)
	cfg.UpdatedAt = parseTime(updated)
	return &cfg, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
