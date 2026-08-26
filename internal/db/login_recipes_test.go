package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open("sqlite", filepath.Join(t.TempDir(), "lr.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestLoginRecipeUpsertAndAuthModeFlip(t *testing.T) {
	d := openTestDB(t)
	tgt, err := d.CreateTarget("u1", "Acme", "https://acme.test", []string{"acme.test"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if tgt.AuthMode != AuthNone {
		t.Fatalf("new target auth_mode = %q, want none", tgt.AuthMode)
	}

	lr := &LoginRecipe{
		TargetID:         tgt.ID,
		LoginURL:         "https://acme.test/login",
		StepsJSON:        `[{"type":"goto","url":"https://acme.test/login"}]`,
		SuccessSelector:  "nav.dash",
		SuccessTimeoutMs: 12000,
		CredsEncrypted:   "ENCRYPTED-BLOB-1",
	}
	if err := d.SetLoginRecipe(lr); err != nil {
		t.Fatalf("SetLoginRecipe: %v", err)
	}

	// auth_mode flipped to login.
	got, _ := d.GetTargetByID(tgt.ID)
	if got.AuthMode != AuthLogin {
		t.Fatalf("auth_mode after save = %q, want login", got.AuthMode)
	}

	// Read back.
	rr, err := d.GetLoginRecipe(tgt.ID)
	if err != nil {
		t.Fatalf("GetLoginRecipe: %v", err)
	}
	if rr.CredsEncrypted != "ENCRYPTED-BLOB-1" || rr.SuccessTimeoutMs != 12000 || rr.LoginURL != "https://acme.test/login" {
		t.Fatalf("read-back mismatch: %+v", rr)
	}

	// Upsert (same target_id) updates, not duplicates.
	lr.CredsEncrypted = "ENCRYPTED-BLOB-2"
	lr.SuccessTimeoutMs = 20000
	if err := d.SetLoginRecipe(lr); err != nil {
		t.Fatalf("SetLoginRecipe upsert: %v", err)
	}
	rr2, _ := d.GetLoginRecipe(tgt.ID)
	if rr2.CredsEncrypted != "ENCRYPTED-BLOB-2" || rr2.SuccessTimeoutMs != 20000 {
		t.Fatalf("upsert did not update: %+v", rr2)
	}

	// Delete clears the recipe AND flips auth_mode back to none.
	if err := d.DeleteLoginRecipe(tgt.ID); err != nil {
		t.Fatalf("DeleteLoginRecipe: %v", err)
	}
	if _, err := d.GetLoginRecipe(tgt.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	got2, _ := d.GetTargetByID(tgt.ID)
	if got2.AuthMode != AuthNone {
		t.Fatalf("auth_mode after delete = %q, want none", got2.AuthMode)
	}
}

func TestGetLoginRecipeNotFound(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.GetLoginRecipe("nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
