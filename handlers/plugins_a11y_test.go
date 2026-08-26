package handlers

import (
	"context"
	"encoding/json"
	"image/color"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/report"
)

// A push may carry an OPTIONAL per-page DOM/a11y digest. These tests pin the two
// halves of the contract: it must be routed all the way to pages.a11y_digest_key (so
// the persona evaluator's existing gate fires on pushed runs), and a bad one must
// FAIL THE PUSH LOUDLY at ingest rather than be accepted-then-ignored.

// goodPushDigest is deliberately NOT a digest that raw passthrough would satisfy: the
// #promo control claims has_label:true while declaring a NON-programmatic
// label_source. Only a run through NormalizeA11yDigest downgrades it to false, so the
// stored-bytes assertion below fails if the validator is ever bypassed (the
// canonical-re-serialisation property, asserted end-to-end at the handler).
const goodPushDigest = `{
  "interactive":[{"tag":"a","selector":"a#client-1","accessible_name":"Open Acme","focusable":true,"label_source":"text-content"}],
  "form_controls":[
    {"selector":"input#signup-email","accessible_name":"Email","has_label":true,"label_source":"for"},
    {"selector":"input#promo","accessible_name":"Promo","has_label":true,"label_source":"placeholder"}
  ],
  "landmarks":[{"tag":"h1","text":"Sign up"}]
}`

func digestPushMeta() string {
	return `{"pages":[
		{"url":"signup-step-1","viewport":"desktop","screenshot":"a.png","a11y_digest":"d.json"},
		{"url":"signup-step-1","viewport":"mobile","screenshot":"b.png"}
	]}`
}

// pushWithDigest performs a push whose desktop page references digest bytes.
func pushWithDigest(t *testing.T, digest string) (*App, *http.Response, string) {
	t.Helper()
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "Funnel")
	shot := pngBytes(t, 40, 40, color.White)
	files := map[string][]byte{"a.png": shot, "b.png": shot, "d.json": []byte(digest)}
	ct, body := pushMultipart(t, digestPushMeta(), files)
	rw := doPush(router, token, ct, body)
	return app, rw.Result(), rw.Body.String()
}

func TestPluginPushStoresA11yDigest(t *testing.T) {
	app, res, bodyStr := pushWithDigest(t, goodPushDigest)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("push with a valid digest = %d (%s)", res.StatusCode, bodyStr)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &out); err != nil {
		t.Fatal(err)
	}

	pages, err := app.DB.ListPages(out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var digestKey string
	for _, p := range pages {
		if p.Viewport == "desktop" {
			digestKey = p.A11yDigestKey
		} else if p.A11yDigestKey != "" {
			t.Errorf("the mobile page referenced no digest but got key %q", p.A11yDigestKey)
		}
	}
	if digestKey == "" {
		t.Fatal("the pushed digest did not reach pages.a11y_digest_key — the eval gate would never fire")
	}
	if !strings.HasSuffix(digestKey, "/a11y.json") {
		t.Errorf("digest stored off the standard key scheme: %q", digestKey)
	}

	// The stored artifact is the CANONICAL re-serialised digest and parses back into
	// exactly what the eval read path expects.
	rc, err := app.Store.Get(context.Background(), digestKey)
	if err != nil {
		t.Fatalf("digest artifact not in the store: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	var d report.A11yDigest
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("stored digest does not parse as report.A11yDigest: %v", err)
	}
	if len(d.FormControls) != 2 {
		t.Fatalf("stored digest lost its facts: %+v", d)
	}
	byPushSelector := map[string]bool{}
	for _, c := range d.FormControls {
		byPushSelector[c.Selector] = c.HasLabel
	}
	if !byPushSelector["input#signup-email"] {
		t.Error("a genuinely programmatic label must survive normalisation")
	}
	// The load-bearing assertion: the producer CLAIMED has_label:true on a
	// placeholder-only control. What is STORED must read false — proving the bytes went
	// through NormalizeA11yDigest and are auditloop's canonical re-serialisation, not
	// the producer's raw payload. Bypass the validator and this fails.
	if byPushSelector["input#promo"] {
		t.Error("stored digest kept the producer's unearned has_label:true — raw bytes were passed through instead of the normalised digest")
	}

	// report.json carries the additive a11y_digest_key seam for the desktop page.
	rep := readReport(t, app, out.RunID)
	var sawKey bool
	for _, p := range rep.Pages {
		if p.Viewport == "desktop" && p.A11yDigestKey != "" {
			sawKey = true
		}
	}
	if !sawKey {
		t.Error("report.json should echo the page's a11y_digest_key")
	}
}

// FAIL-CLOSED: a malformed digest rejects the WHOLE push (400) and persists nothing.
// A silently-ignored digest is indistinguishable from a working one, which is exactly
// the failure mode this repo has been bitten by on the producer side.
func TestPluginPushRejectsMalformedA11yDigest(t *testing.T) {
	bad := map[string]string{
		"not JSON":             `<html>nope</html>`,
		"unknown field":        `{"landmarks":[{"tag":"h1"}],"evil":1}`,
		"unknown label_source": `{"form_controls":[{"selector":"#e","has_label":true,"label_source":"totally-made-up"}]}`,
		"empty digest":         `{}`,
		"over element cap":     `{"interactive":[` + strings.TrimSuffix(strings.Repeat(`{"tag":"a","selector":"#a"},`, report.MaxA11yInteractive+1), ",") + `]}`,
	}
	for name, digest := range bad {
		app, res, bodyStr := pushWithDigest(t, digest)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: push = %d, want 400 (%s)", name, res.StatusCode, bodyStr)
			continue
		}
		// Nothing persisted: no run at all for this target's owner.
		runs, err := app.DB.ListRuns(auth.DefaultDevUser, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 0 {
			t.Errorf("%s: a rejected push must persist NO run, got %d", name, len(runs))
		}
	}
}

// An over-cap digest FILE is caught by the per-file/body caps or the digest byte cap —
// either way the push is refused, never truncated-and-accepted.
func TestPluginPushRejectsOversizedA11yDigest(t *testing.T) {
	huge := `{"landmarks":[{"tag":"h1","text":"` + strings.Repeat("a", report.MaxA11yDigestBytes+512) + `"}]}`
	_, res, bodyStr := pushWithDigest(t, huge)
	if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized digest = %d, want 400 or 413 (%s)", res.StatusCode, bodyStr)
	}
}

// BACKWARD COMPATIBILITY: a push that sends NO digest is byte-for-byte the old
// behaviour — 200, run created, and no page carries an a11y_digest_key (so the
// deterministic gate can never fire and nothing is silently dropped).
func TestPluginPushWithoutA11yDigestUnchanged(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "Legacy")
	shot := pngBytes(t, 40, 40, color.White)
	meta := `{"pages":[{"url":"signup-step-1","viewport":"desktop","screenshot":"a.png","axe":"a.json","axe_violations":1,
		"findings":[{"type":"a11y","severity":"serious","detail":"label — form elements must have labels"}]}]}`
	files := map[string][]byte{"a.png": shot, "a.json": []byte(`{"violations":[]}`)}
	ct, body := pushMultipart(t, meta, files)
	rw := doPush(router, token, ct, body)
	if rw.Code != http.StatusOK {
		t.Fatalf("legacy push = %d (%s)", rw.Code, rw.Body.String())
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &out)

	pages, _ := app.DB.ListPages(out.RunID)
	if len(pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(pages))
	}
	if pages[0].A11yDigestKey != "" {
		t.Errorf("a digest-less push must set NO a11y_digest_key, got %q", pages[0].A11yDigestKey)
	}
	// The existing pushed-a11y-finding contract (top-level rule id for the P2 delta)
	// is untouched by this change.
	finds, _ := app.DB.ListFindings(pages[0].ID)
	if len(finds) != 1 || !strings.Contains(finds[0].Detail, `"id":"label"`) {
		t.Errorf("legacy pushed a11y finding mapping changed: %+v", finds)
	}
	rep := readReport(t, app, out.RunID)
	for _, p := range rep.Pages {
		if p.A11yDigestKey != "" {
			t.Errorf("legacy report.json must not carry an a11y_digest_key, got %q", p.A11yDigestKey)
		}
	}
}

// The orphan/missing-part integrity rules apply to the digest ref like any other.
func TestPluginPushA11yDigestRefIntegrity(t *testing.T) {
	app, router := testApp(t)
	_, token := newPluginTarget(t, app, "Refs")
	shot := pngBytes(t, 40, 40, color.White)

	// Referenced but not uploaded.
	ct, body := pushMultipart(t, digestPushMeta(), map[string][]byte{"a.png": shot, "b.png": shot})
	if rw := doPush(router, token, ct, body); rw.Code != http.StatusBadRequest {
		t.Errorf("missing digest part = %d, want 400", rw.Code)
	}
	// Uploaded but not referenced (orphan).
	meta := `{"pages":[{"url":"s1","viewport":"desktop","screenshot":"a.png"}]}`
	ct, body = pushMultipart(t, meta, map[string][]byte{"a.png": shot, "d.json": []byte(goodPushDigest)})
	if rw := doPush(router, token, ct, body); rw.Code != http.StatusBadRequest {
		t.Errorf("orphan digest part = %d, want 400", rw.Code)
	}
	if runs, _ := app.DB.ListRuns(auth.DefaultDevUser, ""); len(runs) != 0 {
		t.Errorf("ref-integrity rejections must persist no run, got %d", len(runs))
	}
}
