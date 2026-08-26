package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/storage"

	"github.com/gorilla/mux"
)

// seedOwnedArtifactKey creates a target + run owned by auth.DefaultDevUser (the
// identity DevMode authenticates as) and returns an artifact key scoped under the
// run's id, so the per-object ownership check on the browser artifact route
// resolves. The returned runID is the 2nd path segment of the key.
func seedOwnedArtifactKey(t *testing.T, app *App) (string, string) {
	t.Helper()
	tgt, err := app.DB.CreateTarget(auth.DefaultDevUser, "Acme", "https://acme.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return app.targetSlug(tgt) + "/" + run.ID + "/home-desktop.png", run.ID
}

// fakeS3Store implements storage.Store as an "s3" backend that streams fixed
// bytes from Get, so we can exercise handleArtifact's proxy/stream path.
type fakeS3Store struct {
	presignURL string
	body       string
}

func (f *fakeS3Store) Put(context.Context, string, string, io.Reader, int64) error { return nil }
func (f *fakeS3Store) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}
func (f *fakeS3Store) PresignGet(context.Context, string, time.Duration) (string, error) {
	return f.presignURL, nil
}
func (f *fakeS3Store) List(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeS3Store) Backend() string                                { return "s3" }

var _ storage.Store = (*fakeS3Store)(nil)

// TestArtifactS3StreamsThroughApp verifies the S3 backend now STREAMS the object
// through the app's own origin (200 + bytes) rather than 302-redirecting to the
// internal MinIO host (which a browser can't reach/trust), with the security
// headers + content-type set on the streamed response.
func TestArtifactS3StreamsThroughApp(t *testing.T) {
	app, _ := testApp(t)
	app.Store = &fakeS3Store{body: "PNGBYTES"}

	// The browser artifact route is per-object owner-checked: seed an owned run and
	// key the artifact under its run_id so ownership resolves.
	key, _ := seedOwnedArtifactKey(t, app)

	rw := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/artifacts/"+key, nil)
	req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: auth.DefaultDevUser}))
	req = mux.SetURLVars(req, map[string]string{"key": key})
	app.handleArtifact(rw, req)

	if rw.Code != 200 {
		t.Fatalf("stream path = %d, want 200", rw.Code)
	}
	if rw.Body.String() != "PNGBYTES" {
		t.Errorf("body = %q, want streamed bytes", rw.Body.String())
	}
	if got := rw.Header().Get("Location"); got != "" {
		t.Errorf("should NOT redirect (no Location); got %q", got)
	}
	if got := rw.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if got := rw.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing: %q", got)
	}
}

// TestArtifactSecurityHeaders verifies the artifact-serving route sets nosniff +
// inline disposition on the streamed (filesystem-backend) response, so
// externally-pushed bytes can't be sniffed into HTML.
func TestArtifactSecurityHeaders(t *testing.T) {
	app, router := testApp(t)

	// Seed an owned run + store an artifact keyed under its run_id so the FS backend
	// streams it (200, not 404) past the per-object ownership check. DevMode's router
	// authenticates as auth.DefaultDevUser (the run's owner).
	key, _ := seedOwnedArtifactKey(t, app)
	if err := app.Store.Put(context.Background(), key, "image/png", strings.NewReader("PNGDATA"), 7); err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/artifacts/"+key, nil))
	if rw.Code != 200 {
		t.Fatalf("artifact GET = %d, body=%s", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rw.Header().Get("Content-Disposition"); got != "inline" {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
	if got := rw.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
}

// TestArtifactCrossUserOwnership verifies the browser artifact route is per-object
// owner-checked (mirrors the read-API cross-user test): user B cannot fetch an
// artifact whose run belongs to user A — it 404s before streaming any bytes — while
// the owner gets 200 + bytes + nosniff, and a malformed/short key cleanly 404s
// (no panic, no traversal).
func TestArtifactCrossUserOwnership(t *testing.T) {
	app, _ := testApp(t)

	// User A owns a target+run; store an artifact under A's run_id.
	tgtA, err := app.DB.CreateTarget("user-A", "A", "https://a.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	runA, err := app.DB.CreateRun("user-A", tgtA.ID)
	if err != nil {
		t.Fatal(err)
	}
	keyA := app.targetSlug(tgtA) + "/" + runA.ID + "/home-desktop.png"
	if err := app.Store.Put(context.Background(), keyA, "image/png", strings.NewReader("PNGDATA"), 7); err != nil {
		t.Fatal(err)
	}

	fetch := func(user, key string) *httptest.ResponseRecorder {
		rw := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/artifacts/"+key, nil)
		req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: user}))
		req = mux.SetURLVars(req, map[string]string{"key": key})
		app.handleArtifact(rw, req)
		return rw
	}

	// User B fetching A's key → 404, and NO bytes leaked.
	rwB := fetch("user-B", keyA)
	if rwB.Code != http.StatusNotFound {
		t.Fatalf("cross-user fetch = %d, want 404", rwB.Code)
	}
	if strings.Contains(rwB.Body.String(), "PNGDATA") {
		t.Error("cross-user fetch leaked artifact bytes")
	}

	// Owner A fetching own key → 200 + bytes + nosniff.
	rwA := fetch("user-A", keyA)
	if rwA.Code != http.StatusOK {
		t.Fatalf("owner fetch = %d, want 200 (%s)", rwA.Code, rwA.Body.String())
	}
	if rwA.Body.String() != "PNGDATA" {
		t.Errorf("owner body = %q, want streamed bytes", rwA.Body.String())
	}
	if got := rwA.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("owner response missing nosniff: %q", got)
	}

	// A malformed/short key (no run_id segment) → clean 404, no panic.
	rwBad := fetch("user-A", "single")
	if rwBad.Code != http.StatusNotFound {
		t.Fatalf("short key = %d, want 404", rwBad.Code)
	}

	// Login-test screenshots ({target_id}/login-tests/{id}.png) aren't tied to a run —
	// authorized by owner-scoped GetTarget on the globally-unique target_id. A's
	// login-test key is fetchable by A, not by B.
	ltKey := tgtA.ID + "/login-tests/probe123.png"
	if err := app.Store.Put(context.Background(), ltKey, "image/png", strings.NewReader("LTDATA"), 6); err != nil {
		t.Fatal(err)
	}
	if rw := fetch("user-B", ltKey); rw.Code != http.StatusNotFound || strings.Contains(rw.Body.String(), "LTDATA") {
		t.Fatalf("cross-user login-test fetch = %d (leak=%v), want 404 no-leak", rw.Code, strings.Contains(rw.Body.String(), "LTDATA"))
	}
	if rw := fetch("user-A", ltKey); rw.Code != http.StatusOK || rw.Body.String() != "LTDATA" {
		t.Fatalf("owner login-test fetch = %d body=%q, want 200 + bytes", rw.Code, rw.Body.String())
	}
}

// TestLoginTestArtifactSameNameSlugCollision is the auditor's exact repro: two users
// each own a target with the SAME name (→ same name-slug), and user-A ran a login
// test. Because login-test keys are now keyed by the globally-unique target_id and
// authorized via owner-scoped GetTarget, user-B (identical target name) CANNOT read
// user-A's login-test screenshot — even though a name-slug check would have passed.
func TestLoginTestArtifactSameNameSlugCollision(t *testing.T) {
	app, _ := testApp(t)

	// Both users register a target literally named "Acme".
	tgtA, err := app.DB.CreateTarget("user-A", "Acme", "https://a.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	tgtB, err := app.DB.CreateTarget("user-B", "Acme", "https://b.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the (now unused-for-authz) name-slugs really do collide — this is what
	// the old slug-based check keyed on.
	if app.targetSlug(tgtA) != app.targetSlug(tgtB) {
		t.Fatalf("guard: expected colliding name-slugs, got %q vs %q", app.targetSlug(tgtA), app.targetSlug(tgtB))
	}
	// A's login-test screenshot is keyed by A's globally-unique target_id.
	keyA := tgtA.ID + "/login-tests/" + "probeA.png"
	if err := app.Store.Put(context.Background(), keyA, "image/png", strings.NewReader("SECRET-A"), 8); err != nil {
		t.Fatal(err)
	}

	fetch := func(user, key string) *httptest.ResponseRecorder {
		rw := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/artifacts/"+key, nil)
		req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: user}))
		req = mux.SetURLVars(req, map[string]string{"key": key})
		app.handleArtifact(rw, req)
		return rw
	}

	// User-B (identical target name) must NOT read A's login-test screenshot.
	rwB := fetch("user-B", keyA)
	if rwB.Code != http.StatusNotFound {
		t.Fatalf("same-name-slug user-B fetch = %d, want 404", rwB.Code)
	}
	if strings.Contains(rwB.Body.String(), "SECRET-A") {
		t.Error("same-name-slug user-B fetch leaked A's login-test bytes")
	}
	// Owner A still reads it.
	rwA := fetch("user-A", keyA)
	if rwA.Code != http.StatusOK || rwA.Body.String() != "SECRET-A" {
		t.Fatalf("owner login-test fetch = %d body=%q, want 200 + bytes", rwA.Code, rwA.Body.String())
	}
}

// TestGlobalSecurityHeaders verifies the global middleware sets the three
// defense-in-depth headers on a normal page response.
func TestGlobalSecurityHeaders(t *testing.T) {
	_, router := testApp(t)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/dashboard", nil))
	if rw.Code != 200 {
		t.Fatalf("dashboard = %d", rw.Code)
	}
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for h, v := range want {
		if got := rw.Header().Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
	// No CSP is set (deliberate: htmx/inline/supabase-js would break under a strict one).
	if got := rw.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("unexpected CSP set: %q", got)
	}
}
