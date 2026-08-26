package db

import "testing"

func TestTargetAuditConfigUpsertAndOwnerScope(t *testing.T) {
	d := openTestDB(t)
	tgt, err := d.CreateTarget("u1", "Acme", "https://acme.test", []string{"acme.test"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	// No config yet.
	if _, found, err := d.GetTargetAuditConfig("u1", tgt.ID); err != nil || found {
		t.Fatalf("expected no config; found=%v err=%v", found, err)
	}

	cfg := &TargetAuditConfig{
		TargetID:       tgt.ID,
		ProductSummary: "A UX auditor",
		PrimaryJob:     "sign up and run an audit",
		PrimaryCTA:     "Sign up",
		Personas:       []string{"skeptical-evaluator", "first-time-nontechnical"},
		Inferred:       true,
		Confirmed:      false,
	}
	if err := d.SetTargetAuditConfig(cfg); err != nil {
		t.Fatalf("SetTargetAuditConfig: %v", err)
	}

	got, found, err := d.GetTargetAuditConfig("u1", tgt.ID)
	if err != nil || !found {
		t.Fatalf("GetTargetAuditConfig: found=%v err=%v", found, err)
	}
	if got.ProductSummary != cfg.ProductSummary || got.PrimaryJob != cfg.PrimaryJob || got.PrimaryCTA != cfg.PrimaryCTA {
		t.Errorf("fields not persisted: %+v", got)
	}
	if len(got.Personas) != 2 || got.Personas[0] != "skeptical-evaluator" {
		t.Errorf("personas not persisted in order: %v", got.Personas)
	}
	if !got.Inferred || got.Confirmed {
		t.Errorf("inferred/confirmed flags not persisted: inferred=%v confirmed=%v", got.Inferred, got.Confirmed)
	}

	// Upsert: confirm + change fields.
	cfg.PrimaryJob = "sign up and create a project"
	cfg.Personas = []string{"returning-power-user"}
	cfg.Inferred = false
	cfg.Confirmed = true
	if err := d.SetTargetAuditConfig(cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = d.GetTargetAuditConfig("u1", tgt.ID)
	if got.PrimaryJob != "sign up and create a project" || len(got.Personas) != 1 || got.Personas[0] != "returning-power-user" {
		t.Errorf("upsert did not replace fields: %+v", got)
	}
	if got.Inferred || !got.Confirmed {
		t.Errorf("upsert flags wrong: inferred=%v confirmed=%v", got.Inferred, got.Confirmed)
	}

	// Cross-user isolation: u2 must not see u1's target config.
	if _, found, err := d.GetTargetAuditConfig("u2", tgt.ID); err != nil || found {
		t.Errorf("foreign user resolved another user's config: found=%v err=%v", found, err)
	}
}
