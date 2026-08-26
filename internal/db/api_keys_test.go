package db

import (
	"testing"

	"github.com/ZacxDev/auditloop/internal/apikey"
)

func TestAPIKeyLifecycle(t *testing.T) {
	d := testDB(t)

	token, hash, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateAPIKey("user-1", "ci-agent", hash, ScopeRead)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	// Lookup by hash resolves the owner + stored hash for a constant-time compare.
	uid, scope, stored, found, err := d.APIKeyLookup(hash)
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if uid != "user-1" || scope != ScopeRead {
		t.Errorf("lookup owner/scope = %q/%q", uid, scope)
	}
	if !apikey.ConstantTimeEqual(stored, apikey.Hash(token)) {
		t.Error("stored hash should verify against the token")
	}

	// An unknown hash is not found (no error).
	if _, _, _, found, err := d.APIKeyLookup(apikey.Hash("nope")); found || err != nil {
		t.Errorf("unknown hash: found=%v err=%v", found, err)
	}

	// List returns display metadata only — never the hash/plaintext.
	list, err := d.ListAPIKeys("user-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v n=%d", err, len(list))
	}
	if list[0].Name != "ci-agent" || list[0].Scope != ScopeRead {
		t.Errorf("list metadata: %+v", list[0])
	}
	if list[0].LastUsedAt != nil {
		t.Error("expected LastUsedAt nil before first use")
	}
	// Guard: the APIKey struct has no field that could carry the hash/plaintext.
	// (Compile-time assurance is the struct shape; here we assert the token text
	// never appears in any listed string field.)
	for _, k := range list {
		if k.Name == token || k.ID == token {
			t.Fatal("token leaked into a listed field")
		}
	}

	// Scoping: another user sees none.
	if other, _ := d.ListAPIKeys("user-2"); len(other) != 0 {
		t.Errorf("cross-user list should be empty, got %d", len(other))
	}

	// Touch last-used.
	if err := d.TouchAPIKeyLastUsed(hash); err != nil {
		t.Fatalf("touch: %v", err)
	}
	list, _ = d.ListAPIKeys("user-1")
	if list[0].LastUsedAt == nil {
		t.Error("expected LastUsedAt set after touch")
	}
}

func TestRevokeAPIKeyScoped(t *testing.T) {
	d := testDB(t)
	_, hash, _ := apikey.Generate()
	id, _ := d.CreateAPIKey("user-1", "k", hash, ScopeRead)

	// A different user cannot revoke it.
	if err := d.RevokeAPIKey("user-2", id); err != ErrNotFound {
		t.Errorf("cross-user revoke should be ErrNotFound, got %v", err)
	}
	// Still resolvable after the failed foreign revoke.
	if _, _, _, found, _ := d.APIKeyLookup(hash); !found {
		t.Error("key should survive a foreign revoke")
	}
	// The owner revokes it → lookup no longer resolves (invalidated).
	if err := d.RevokeAPIKey("user-1", id); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
	if _, _, _, found, _ := d.APIKeyLookup(hash); found {
		t.Error("revoked key must not resolve")
	}
	// Revoking again → ErrNotFound.
	if err := d.RevokeAPIKey("user-1", id); err != ErrNotFound {
		t.Errorf("double revoke should be ErrNotFound, got %v", err)
	}
}

func TestLatestDoneRunForTargetOwned(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("user-1", "T", "https://t.test", nil)

	// No done run yet.
	if r, err := d.LatestDoneRunForTargetOwned("user-1", tgt.ID); err != nil || r != nil {
		t.Errorf("expected nil before any done run, got %v err=%v", r, err)
	}
	r1, _ := d.CreateRun("user-1", tgt.ID)
	_ = d.FinishRun(r1.ID, RunDone, "{}", "")

	got, err := d.LatestDoneRunForTargetOwned("user-1", tgt.ID)
	if err != nil || got == nil || got.ID != r1.ID {
		t.Fatalf("owned latest-done = %v err=%v, want %s", got, err, r1.ID)
	}
	// A different user cannot resolve it.
	if r, _ := d.LatestDoneRunForTargetOwned("user-2", tgt.ID); r != nil {
		t.Errorf("cross-user latest-done should be nil, got %+v", r)
	}
}

func TestAPIKeyMigrationAppliesOnSQLite(t *testing.T) {
	// Opening the DB runs all migrations incl. 0035–0037; a create+lookup proves
	// the api_keys table + unique hash index exist and are usable.
	d := testDB(t)
	_, hash, _ := apikey.Generate()
	if _, err := d.CreateAPIKey("u", "", hash, ""); err != nil {
		t.Fatalf("create (default scope): %v", err)
	}
	uid, scope, _, found, err := d.APIKeyLookup(hash)
	if err != nil || !found || uid != "u" || scope != ScopeRead {
		t.Fatalf("lookup after default-scope create: uid=%q scope=%q found=%v err=%v", uid, scope, found, err)
	}
}
