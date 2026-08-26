package db

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// Token helpers duplicated locally to avoid an import cycle (internal/plugin
// imports internal/db for the mapper; a same-package db test importing plugin
// would cycle). These mirror internal/plugin.{GenerateToken,HashToken,
// ConstantTimeEqual} exactly.
func genToken(t *testing.T) (token, hash string, err error) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func ctEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func TestPluginTargetAndTokenLifecycle(t *testing.T) {
	d := testDB(t)
	const uid = "user-1"

	tgt, err := d.CreatePluginTarget(uid, "CI funnel", "https://app.acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.AuthMode != AuthPlugin {
		t.Fatalf("auth_mode = %q, want plugin", tgt.AuthMode)
	}
	if ok, _ := d.HasPluginToken(tgt.ID); ok {
		t.Fatal("new plugin target should have no token yet")
	}

	// Mint + store a token (only the hash).
	token, hash, err := genToken(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPluginToken(tgt.ID, hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.HasPluginToken(tgt.ID); !ok {
		t.Fatal("token not recorded")
	}

	// The plaintext token must NOT appear anywhere in the plugin_tokens table.
	var storedHash string
	if err := d.queryRow(`SELECT token_hash FROM plugin_tokens WHERE target_id=?`, tgt.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == token {
		t.Fatal("plaintext token stored in DB")
	}
	if storedHash != hash {
		t.Fatalf("stored hash mismatch")
	}

	// Lookup by presented token resolves the right target + returns the stored hash.
	got, gotHash, err := d.PluginTokenLookup(hashToken(token))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != tgt.ID {
		t.Fatalf("lookup resolved wrong target: %s vs %s", got.ID, tgt.ID)
	}
	if !ctEqual(gotHash, hashToken(token)) {
		t.Fatal("returned hash does not verify constant-time")
	}

	// A wrong/unknown token → not found.
	if _, _, err := d.PluginTokenLookup(hashToken("nope")); err != ErrNotFound {
		t.Fatalf("unknown token lookup = %v, want ErrNotFound", err)
	}
}

func TestRotateInvalidatesOldToken(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreatePluginTarget("u", "T", "")

	oldTok, oldHash, _ := genToken(t)
	if err := d.SetPluginToken(tgt.ID, oldHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.PluginTokenLookup(hashToken(oldTok)); err != nil {
		t.Fatalf("old token should resolve before rotation: %v", err)
	}

	// Rotate.
	newTok, newHash, _ := genToken(t)
	if err := d.SetPluginToken(tgt.ID, newHash); err != nil {
		t.Fatal(err)
	}
	// Old token no longer works.
	if _, _, err := d.PluginTokenLookup(hashToken(oldTok)); err != ErrNotFound {
		t.Fatalf("old token still valid after rotation: %v", err)
	}
	// New token works.
	if _, _, err := d.PluginTokenLookup(hashToken(newTok)); err != nil {
		t.Fatalf("new token should resolve: %v", err)
	}
	// Still exactly one token row (upsert, not append).
	var n int
	d.queryRow(`SELECT COUNT(*) FROM plugin_tokens WHERE target_id=?`, tgt.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 token row after rotation, got %d", n)
	}
}

func TestNonPluginTargetCannotBePushedTo(t *testing.T) {
	d := testDB(t)
	// A normal (crawl) target with the same id space.
	normal, err := d.CreateTarget("u", "Normal", "https://acme.com", []string{"acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Even if a token hash were somehow associated with a non-plugin target id, the
	// JOIN filters auth_mode='plugin', so it must not resolve.
	tok, hash, _ := genToken(t)
	if err := d.SetPluginToken(normal.ID, hash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.PluginTokenLookup(hashToken(tok)); err != ErrNotFound {
		t.Fatalf("token for a non-plugin target resolved: %v", err)
	}
}

func TestCreatePushedRunLinksBaseline(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreatePluginTarget("u", "T", "")

	// First pushed run: no baseline.
	r1, err := d.CreatePushedRun("u", tgt.ID, "first", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if r1.PrevRunID != "" {
		t.Fatalf("first run should have no baseline, got %q", r1.PrevRunID)
	}
	if r1.Trigger != "plugin" || r1.Label != "first" || r1.Status != RunRunning {
		t.Fatalf("bad pushed run: %+v", r1)
	}
	if r1.StartedAt == nil {
		t.Fatal("pushed run should have started_at set")
	}
	// Finish it so it becomes a baseline.
	if err := d.FinishRun(r1.ID, RunDone, "{}", ""); err != nil {
		t.Fatal(err)
	}

	// Second pushed run links to r1.
	r2, err := d.CreatePushedRun("u", tgt.ID, "second", "")
	if err != nil {
		t.Fatal(err)
	}
	if r2.PrevRunID != r1.ID {
		t.Fatalf("run2 baseline = %q, want %q", r2.PrevRunID, r1.ID)
	}
	// Label round-trips through GetRun.
	got, err := d.GetRun("u", r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "second" {
		t.Fatalf("label = %q, want second", got.Label)
	}
	if got.Environment != "" {
		t.Fatalf("environment = %q, want empty", got.Environment)
	}
	// Environment round-trips through GetRun (r1 was created with "lab").
	got1, err := d.GetRun("u", r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.Environment != "lab" {
		t.Fatalf("environment = %q, want lab", got1.Environment)
	}
}
