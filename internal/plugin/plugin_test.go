package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/signals"
)

// dbSmellSet extracts the layout-smell ids present in a set of db.Findings.
func dbSmellSet(fs []*db.Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		if f.Type != "layout" {
			continue
		}
		var d signals.LayoutDetail
		if json.Unmarshal([]byte(f.Detail), &d) == nil {
			out[d.Smell] = true
		}
	}
	return out
}

// dbPerfMetrics extracts the perf metric ids present in a set of db.Findings.
func dbPerfMetrics(fs []*db.Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		if f.Type != "perf" {
			continue
		}
		var d struct {
			Metric string `json:"metric"`
		}
		if json.Unmarshal([]byte(f.Detail), &d) == nil {
			out[d.Metric] = true
		}
	}
	return out
}

// --- token tests ---

func TestGenerateTokenHashStored(t *testing.T) {
	token, hash, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || hash == "" {
		t.Fatal("empty token/hash")
	}
	// The stored hash must NOT be the plaintext token (never store plaintext).
	if hash == token {
		t.Fatal("hash equals plaintext token")
	}
	// The hash must be reproducible from the token.
	if HashToken(token) != hash {
		t.Fatal("HashToken not reproducible")
	}
	// sha256 hex is 64 chars.
	if len(hash) != 64 {
		t.Fatalf("hash len = %d, want 64", len(hash))
	}
	// Two generations differ.
	t2, h2, _ := GenerateToken()
	if t2 == token || h2 == hash {
		t.Fatal("two tokens collided")
	}
}

func TestVerifyToken(t *testing.T) {
	token, hash, _ := GenerateToken()
	if !VerifyToken(hash, token) {
		t.Fatal("valid token rejected")
	}
	if VerifyToken(hash, token+"x") {
		t.Fatal("tampered token accepted")
	}
	if VerifyToken(hash, "") {
		t.Fatal("empty token accepted")
	}
	// A different token's hash must not verify (rotation invalidation).
	other, _, _ := GenerateToken()
	if VerifyToken(hash, other) {
		t.Fatal("a different token verified against the stored hash")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") {
		t.Fatal("equal strings not equal")
	}
	if ConstantTimeEqual("abc", "abd") {
		t.Fatal("different strings equal")
	}
	if ConstantTimeEqual("abc", "ab") {
		t.Fatal("different-length strings equal")
	}
}

// --- schema / validation tests ---

func validPayloadJSON() string {
	return `{
	  "label": "signup funnel",
	  "pages": [
	    {"url":"signup-step-1","viewport":"desktop","screenshot":"s1.png","axe":"a1.json","axe_violations":2,
	     "findings":[{"type":"a11y","severity":"serious","detail":"missing label"}]},
	    {"url":"signup-step-1","viewport":"mobile","screenshot":"s2.png"}
	  ]
	}`
}

func TestParseAndValidateOK(t *testing.T) {
	p, err := Parse(strings.NewReader(validPayloadJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Label != "signup funnel" || len(p.Pages) != 2 {
		t.Fatalf("bad parse: %+v", p)
	}
	files := map[string]bool{"s1.png": true, "s2.png": true, "a1.json": true}
	if err := p.Validate(files); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	refs := p.ReferencedFiles()
	for _, want := range []string{"s1.png", "s2.png", "a1.json"} {
		if !refs[want] {
			t.Errorf("ReferencedFiles missing %q", want)
		}
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"pages":[],"bogus":1}`))
	if err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

func TestValidateMissingScreenshotRef(t *testing.T) {
	p, _ := Parse(strings.NewReader(validPayloadJSON()))
	// s2.png not provided → missing part.
	err := p.Validate(map[string]bool{"s1.png": true, "a1.json": true})
	if err == nil || !strings.Contains(err.Error(), "s2.png") {
		t.Fatalf("expected missing-part error for s2.png, got %v", err)
	}
}

func TestValidateOrphanFilePart(t *testing.T) {
	p, _ := Parse(strings.NewReader(validPayloadJSON()))
	err := p.Validate(map[string]bool{"s1.png": true, "s2.png": true, "a1.json": true, "orphan.png": true})
	if err == nil || !strings.Contains(err.Error(), "orphan.png") {
		t.Fatalf("expected orphan-part error, got %v", err)
	}
}

func TestValidateEnvironment(t *testing.T) {
	// Valid environments (and absent) accepted.
	for _, env := range []string{"lab", "staging", "prod", ""} {
		body := `{"environment":"` + env + `","pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`
		if env == "" {
			body = `{"pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`
		}
		p, err := Parse(strings.NewReader(body))
		if err != nil {
			t.Fatalf("env %q: parse failed: %v", env, err)
		}
		if err := p.Validate(map[string]bool{"s.png": true}); err != nil {
			t.Errorf("env %q: valid environment rejected: %v", env, err)
		}
		if p.Environment != env {
			t.Errorf("env %q: parsed as %q", env, p.Environment)
		}
	}
	// An unknown environment → validation error.
	p, err := Parse(strings.NewReader(`{"environment":"local","pages":[{"url":"x","viewport":"desktop","screenshot":"s.png"}]}`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if err := p.Validate(map[string]bool{"s.png": true}); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("expected environment error for %q, got %v", "local", err)
	}
}

func TestValidateBadViewport(t *testing.T) {
	p, _ := Parse(strings.NewReader(`{"pages":[{"url":"x","viewport":"tablet","screenshot":"s.png"}]}`))
	err := p.Validate(map[string]bool{"s.png": true})
	if err == nil || !strings.Contains(err.Error(), "viewport") {
		t.Fatalf("expected viewport error, got %v", err)
	}
}

func TestValidateEmptyURL(t *testing.T) {
	p, _ := Parse(strings.NewReader(`{"pages":[{"url":"  ","viewport":"desktop","screenshot":"s.png"}]}`))
	err := p.Validate(map[string]bool{"s.png": true})
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Fatalf("expected url error, got %v", err)
	}
}

func TestValidateUnknownFindingType(t *testing.T) {
	p, _ := Parse(strings.NewReader(`{"pages":[{"url":"x","viewport":"desktop","screenshot":"s.png","findings":[{"type":"malware","detail":"x"}]}]}`))
	err := p.Validate(map[string]bool{"s.png": true})
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("expected finding-type error, got %v", err)
	}
}

func TestValidateNoPages(t *testing.T) {
	p, _ := Parse(strings.NewReader(`{"pages":[]}`))
	if err := p.Validate(map[string]bool{}); err == nil {
		t.Fatal("expected no-pages rejection")
	}
}

func TestValidateTooManyPages(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"pages":[`)
	files := map[string]bool{}
	for i := 0; i <= MaxPages; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		name := "s" + itoa(i) + ".png"
		files[name] = true
		b.WriteString(`{"url":"u` + itoa(i) + `","viewport":"desktop","screenshot":"` + name + `"}`)
	}
	b.WriteString(`]}`)
	p, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(files); err == nil || !strings.Contains(err.Error(), "too many pages") {
		t.Fatalf("expected too-many-pages rejection, got %v", err)
	}
}

// --- mapper tests ---

func TestMapPage(t *testing.T) {
	pg := PushPage{
		URL: "signup-step-2", Viewport: "mobile", Screenshot: "s.png",
		AxeViolations: 3, ConsoleFirstParty: 1, ConsoleThirdParty: 4,
		NetworkFirstParty: 2, NetworkThirdParty: 5,
		Findings: []PushFinding{{Type: "a11y", Severity: "critical", Detail: "no <label> for #email"}},
	}
	keys := PageKeys{Screenshot: "slug/mobile.png", Axe: "slug/axe.json"}
	m := MapPage("run-123", pg, keys, "")

	if m.Page.RunID != "run-123" || m.Page.URL != "signup-step-2" || m.Page.Viewport != "mobile" {
		t.Fatalf("bad page identity: %+v", m.Page)
	}
	if m.Page.ScreenshotKey != "slug/mobile.png" || m.Page.AxeKey != "slug/axe.json" {
		t.Fatalf("bad keys: %+v", m.Page)
	}
	if m.Page.AxeViolationCount != 3 || m.Page.ConsoleThirdPartyCount != 4 || m.Page.NetworkThirdPartyCount != 5 {
		t.Fatalf("counts not mapped: %+v", m.Page)
	}
	if len(m.Findings) != 1 || m.Findings[0].Type != "a11y" || m.Findings[0].Severity != "critical" {
		t.Fatalf("finding not mapped: %+v", m.Findings)
	}
	// PageID left blank for the caller to set post-insert.
	if m.Findings[0].PageID != "" {
		t.Error("PageID should be blank until inserted")
	}
	// Detail is stored as JSON with HTML metacharacters ESCAPED (no injection):
	// "<" becomes <, so the raw "<label>" must NOT appear verbatim.
	if strings.Contains(m.Findings[0].Detail, "<label>") {
		t.Errorf("detail not escaped: %q", m.Findings[0].Detail)
	}
	if !strings.Contains(m.Findings[0].Detail, "label") {
		t.Errorf("detail lost: %q", m.Findings[0].Detail)
	}
	if m.Report.Width != widthMobile {
		t.Errorf("width = %d, want %d", m.Report.Width, widthMobile)
	}
	if m.Report.A11y.ViolationCount != 3 {
		t.Errorf("report a11y not mapped")
	}
}

func TestMapPageDefaultSeverity(t *testing.T) {
	m := MapPage("r", PushPage{URL: "u", Viewport: "desktop", Screenshot: "s.png",
		Findings: []PushFinding{{Type: "console", Detail: "x"}}}, PageKeys{Screenshot: "k"}, "")
	if m.Findings[0].Severity != "info" {
		t.Errorf("default severity = %q, want info", m.Findings[0].Severity)
	}
	if m.Report.Width != widthDesktop {
		t.Errorf("desktop width = %d, want %d", m.Report.Width, widthDesktop)
	}
}

// TestMapPageA11yDetailCarriesRuleID guards the P2-diff contract: a PUSHED a11y
// finding's stored Detail MUST expose a top-level "id" (the axe rule id) so
// internal/worker/diff.go runA11yRuleIDs can recover it — otherwise new_a11y_rules is
// always empty for pushed runs and the CI a11y-regression gate is a no-op.
func TestMapPageA11yDetailCarriesRuleID(t *testing.T) {
	extractID := func(detail string) string {
		var meta struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(detail), &meta); err != nil {
			t.Fatalf("stored detail not valid JSON: %q (%v)", detail, err)
		}
		return meta.ID
	}

	// (a) structured pushed detail already carrying an "id" → recovered as-is.
	structured := `{"id":"region","impact":"serious","help":"All content must be in landmarks"}`
	m := MapPage("r", PushPage{URL: "u", Viewport: "mobile", Screenshot: "s.png",
		Findings: []PushFinding{{Type: "a11y", Severity: "serious", Detail: structured}}},
		PageKeys{Screenshot: "k"}, "")
	if got := extractID(m.Findings[0].Detail); got != "region" {
		t.Errorf("(a) structured a11y id = %q, want %q", got, "region")
	}

	// (b) legacy "<rule-id> — <help text>" string → id derived, original text preserved.
	legacy := "aria-prohibited-attr — Elements must only use permitted ARIA attributes"
	m = MapPage("r", PushPage{URL: "u", Viewport: "mobile", Screenshot: "s.png",
		Findings: []PushFinding{{Type: "a11y", Severity: "serious", Detail: legacy}}},
		PageKeys{Screenshot: "k"}, "")
	if got := extractID(m.Findings[0].Detail); got != "aria-prohibited-attr" {
		t.Errorf("(b) legacy a11y id = %q, want %q", got, "aria-prohibited-attr")
	}
	if !strings.Contains(m.Findings[0].Detail, "permitted ARIA attributes") {
		t.Errorf("(b) original help text lost: %q", m.Findings[0].Detail)
	}

	// (c) a non-a11y finding is still wrapped as {"detail": ...} (unchanged) — no id.
	m = MapPage("r", PushPage{URL: "u", Viewport: "mobile", Screenshot: "s.png",
		Findings: []PushFinding{{Type: "console", Severity: "info", Detail: "Uncaught TypeError"}}},
		PageKeys{Screenshot: "k"}, "")
	var wrapped map[string]string
	if err := json.Unmarshal([]byte(m.Findings[0].Detail), &wrapped); err != nil {
		t.Fatalf("(c) console detail not valid JSON: %v", err)
	}
	if _, hasID := wrapped["id"]; hasID {
		t.Errorf("(c) console detail must not carry an id: %q", m.Findings[0].Detail)
	}
	if wrapped["detail"] != "Uncaught TypeError" {
		t.Errorf("(c) console detail = %q, want wrapped {\"detail\":...}", m.Findings[0].Detail)
	}

	// (d) block-wins unchanged: a pushed perf finding alongside a perf block is dropped.
	m = MapPage("r", PushPage{URL: "u", Viewport: "mobile", Screenshot: "s.png",
		Perf:     &PushPerf{LCPMs: 100, CLS: 0.0, TBTMs: 0, WeightBytes: 1000, ReqCount: 1},
		Findings: []PushFinding{{Type: "perf", Severity: "serious", Detail: "hand-authored LCP"}}},
		PageKeys{Screenshot: "k"}, "")
	for _, f := range m.Findings {
		if f.Type == "perf" && strings.Contains(f.Detail, "hand-authored LCP") {
			t.Errorf("(d) hand-authored perf finding should be dropped when a perf block is present: %+v", f)
		}
	}
}

// --- perf + layout tests (deterministic-signal parity with the crawl) ---

func TestValidateAcceptsPerfAndLayout(t *testing.T) {
	js := `{"pages":[{"url":"home","viewport":"mobile","screenshot":"s.png",
	  "perf":{"lcp_ms":2600,"cls":0.12,"tbt_ms":320,"weight_bytes":2500000,"req_count":80},
	  "findings":[{"type":"layout","severity":"moderate","detail":"horizontal overflow"},
	              {"type":"perf","severity":"serious","detail":"LCP slow"}]}]}`
	p, err := Parse(strings.NewReader(js))
	if err != nil {
		t.Fatalf("valid perf/layout payload rejected at parse: %v", err)
	}
	if err := p.Validate(map[string]bool{"s.png": true}); err != nil {
		t.Fatalf("valid perf/layout payload rejected at validate: %v", err)
	}
	if p.Pages[0].Perf == nil || p.Pages[0].Perf.LCPMs != 2600 {
		t.Fatalf("perf block not parsed: %+v", p.Pages[0].Perf)
	}
}

func TestValidateRejectsNegativePerf(t *testing.T) {
	js := `{"pages":[{"url":"home","viewport":"mobile","screenshot":"s.png","perf":{"lcp_ms":-1}}]}`
	p, err := Parse(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Validate(map[string]bool{"s.png": true})
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("expected non-negative perf rejection, got %v", err)
	}
}

func TestValidateAcceptsLayoutBlock(t *testing.T) {
	js := `{"pages":[{"url":"home","viewport":"mobile","screenshot":"s.png",
	  "layout":{"horizontal_overflow":true,"scroll_width":500,"inner_width":390,
	            "small_tap_targets":3,"small_text":2,"missing_viewport_meta":true,
	            "images_no_dims":4,"examples":{"small_tap_targets":["a.btn"]}}}]}`
	p, err := Parse(strings.NewReader(js))
	if err != nil {
		t.Fatalf("valid layout payload rejected at parse: %v", err)
	}
	if err := p.Validate(map[string]bool{"s.png": true}); err != nil {
		t.Fatalf("valid layout payload rejected at validate: %v", err)
	}
	l := p.Pages[0].Layout
	if l == nil || !l.HorizontalOverflow || l.SmallTapTargets != 3 || l.Examples["small_tap_targets"][0] != "a.btn" {
		t.Fatalf("layout block not parsed: %+v", l)
	}
}

func TestValidateRejectsNegativeLayout(t *testing.T) {
	js := `{"pages":[{"url":"home","viewport":"mobile","screenshot":"s.png","layout":{"small_tap_targets":-1}}]}`
	p, err := Parse(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Validate(map[string]bool{"s.png": true})
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("expected non-negative layout rejection, got %v", err)
	}
}

func TestParseRejectsUnknownFieldInLayout(t *testing.T) {
	// DisallowUnknownFields must fire inside the layout block too.
	_, err := Parse(strings.NewReader(`{"pages":[{"url":"u","viewport":"desktop","screenshot":"s.png","layout":{"bogus":1}}]}`))
	if err == nil {
		t.Fatal("expected unknown-field rejection inside layout block")
	}
}

func TestParseStillRejectsUnknownFieldWithPerf(t *testing.T) {
	// DisallowUnknownFields must still fire even now that `perf` is a known key.
	_, err := Parse(strings.NewReader(`{"pages":[{"url":"u","viewport":"desktop","screenshot":"s.png","perf":{"bogus":1}}]}`))
	if err == nil {
		t.Fatal("expected unknown-field rejection inside perf block")
	}
}

// TestMapPagePerfFindingsComputedServerSide asserts a pushed perf block yields the
// SAME perf findings a native crawl would (internal/signals is the one source of
// truth for the thresholds — no re-implementation in the harness).
func TestMapPagePerfFindingsComputedServerSide(t *testing.T) {
	perf := PushPerf{LCPMs: 3200, CLS: 0.3, TBTMs: 450, WeightBytes: 1 << 20, ReqCount: 120}
	pg := PushPage{URL: "home", Viewport: "mobile", Screenshot: "s.png", Perf: &perf}
	m := MapPage("run-1", pg, PageKeys{Screenshot: "k"}, "")

	// Perf columns on the db.Page row + the report block still populate as before.
	if m.Page.LCPMs != 3200 || m.Page.CLS != 0.3 || m.Page.ReqCount != 120 {
		t.Fatalf("perf columns not mapped onto Page: %+v", m.Page)
	}
	if m.Report.Perf == nil || m.Report.Perf.LCPMs != 3200 {
		t.Fatalf("report Perf not mapped: %+v", m.Report.Perf)
	}

	// The server-computed perf findings must equal what signals.PerfFindings emits
	// from the SAME raw block (LCP 3200 needs-improvement, CLS 0.3 poor, TBT 450
	// needs-improvement, 120 requests over the request budget → page-weight finding).
	want := signals.PerfFindings(report.Perf{LCPMs: 3200, CLS: 0.3, TBTMs: 450, WeightBytes: 1 << 20, ReqCount: 120})
	if len(want) != 4 {
		t.Fatalf("guard: expected signals to emit 4 perf findings, got %d", len(want))
	}
	gotMetrics := dbPerfMetrics(m.Findings)
	for _, wantMetric := range []string{"LCP", "CLS", "TBT", "page-weight"} {
		if !gotMetrics[wantMetric] {
			t.Errorf("expected server-computed perf finding for %q, got %v", wantMetric, gotMetrics)
		}
	}
	// Severities match the native-crawl thresholds: LCP 3200 → moderate (between
	// 2500 good and 4000 poor); CLS 0.3 → serious (> 0.25 poor).
	sev := map[string]string{}
	for _, f := range m.Findings {
		if f.Type != "perf" {
			continue
		}
		var d signals.PerfDetail
		_ = json.Unmarshal([]byte(f.Detail), &d)
		sev[d.Metric] = f.Severity
	}
	if sev["LCP"] != "moderate" {
		t.Errorf("LCP 3200 severity = %q, want moderate (parity with native crawl)", sev["LCP"])
	}
	if sev["CLS"] != "serious" {
		t.Errorf("CLS 0.3 severity = %q, want serious (parity with native crawl)", sev["CLS"])
	}
}

// TestMapPageLabSuppressesPerfFindings asserts the perf-honesty rule: a "lab"
// environment keeps the raw perf COLUMNS + report.Perf (numbers preserved for
// reference/trend) but emits NO type:perf FINDINGS (localhost lab perf is not
// field-representative). staging/prod/unspecified still emit perf findings. Layout
// findings are environment-independent and fire regardless.
func TestMapPageLabSuppressesPerfFindings(t *testing.T) {
	// A breaching perf block AND a layout block on a mobile page.
	perf := PushPerf{LCPMs: 3200, CLS: 0.3, TBTMs: 450, WeightBytes: 4 << 20, ReqCount: 120}
	layout := PushLayout{MissingViewportMeta: true, SmallText: 2}
	mk := func(env string) MappedPage {
		pg := PushPage{URL: "home", Viewport: "mobile", Screenshot: "s.png", Perf: &perf, Layout: &layout}
		return MapPage("run-1", pg, PageKeys{Screenshot: "k"}, env)
	}

	// Lab: raw perf columns + report.Perf STILL populated.
	lab := mk(EnvLab)
	if lab.Page.LCPMs != 3200 || lab.Page.CLS != 0.3 || lab.Page.WeightBytes != 4<<20 || lab.Page.ReqCount != 120 {
		t.Fatalf("lab: raw perf columns must be preserved, got %+v", lab.Page)
	}
	if lab.Report.Perf == nil || lab.Report.Perf.LCPMs != 3200 {
		t.Fatalf("lab: report.Perf must be preserved, got %+v", lab.Report.Perf)
	}
	// Lab: NO perf findings emitted.
	if got := dbPerfMetrics(lab.Findings); len(got) != 0 {
		t.Errorf("lab: expected zero perf findings, got %v", got)
	}
	// Lab: layout findings ARE still emitted (environment-independent).
	if smells := dbSmellSet(lab.Findings); !smells["missing-viewport-meta"] || !smells["small-text"] {
		t.Errorf("lab: layout findings must still fire, got %v", smells)
	}

	// prod + unspecified: perf findings ARE emitted (unchanged behavior).
	for _, env := range []string{EnvProd, EnvStaging, ""} {
		m := mk(env)
		if got := dbPerfMetrics(m.Findings); !got["LCP"] || !got["CLS"] || !got["page-weight"] {
			t.Errorf("env %q: expected perf findings, got %v", env, got)
		}
		// Raw perf columns preserved here too.
		if m.Page.LCPMs != 3200 {
			t.Errorf("env %q: raw perf column lost", env)
		}
	}
}

// TestMapPageLayoutFindingsViewportGated asserts a pushed layout block yields the
// gated layout findings server-side: the mobile-only smells fire on a mobile page
// and NOT on a desktop page (mirroring the crawler's gating). The untrusted
// `examples` selectors are stored ESCAPED.
func TestMapPageLayoutFindingsViewportGated(t *testing.T) {
	layout := PushLayout{
		HorizontalOverflow: true, ScrollWidth: 500, InnerWidth: 390,
		SmallTapTargets: 3, SmallText: 2, MissingViewportMeta: true, ImagesNoDims: 4,
		Examples: map[string][]string{"small_tap_targets": {"<a class=btn> (20x20)"}},
	}

	// Mobile: all five smells fire.
	mob := MapPage("run-1", PushPage{URL: "home", Viewport: "mobile", Screenshot: "s.png", Layout: &layout}, PageKeys{Screenshot: "k"}, "")
	got := dbSmellSet(mob.Findings)
	for _, want := range []string{"horizontal-overflow", "small-tap-targets", "small-text", "missing-viewport-meta", "images-without-dimensions"} {
		if !got[want] {
			t.Errorf("mobile: expected %q layout finding, got %v", want, got)
		}
	}
	// report.LayoutSmells populated (raw block echoed for the UI/report).
	if mob.Report.Layout == nil || !mob.Report.Layout.HorizontalOverflow || mob.Report.Layout.SmallTapTargets != 3 {
		t.Fatalf("report Layout not mapped: %+v", mob.Report.Layout)
	}
	// The attacker-controlled example selector is escaped in the stored detail.
	for _, f := range mob.Findings {
		if f.Type == "layout" && strings.Contains(f.Detail, "<a class=btn>") {
			t.Errorf("layout example selector not escaped: %q", f.Detail)
		}
	}

	// Desktop: the mobile-only smells (overflow + tap targets) must NOT fire; the
	// viewport-agnostic ones still do.
	desk := MapPage("run-1", PushPage{URL: "home", Viewport: "desktop", Screenshot: "s.png", Layout: &layout}, PageKeys{Screenshot: "k"}, "")
	deskGot := dbSmellSet(desk.Findings)
	if deskGot["horizontal-overflow"] || deskGot["small-tap-targets"] {
		t.Errorf("desktop: mobile-only smells should not fire, got %v", deskGot)
	}
	if !deskGot["missing-viewport-meta"] || !deskGot["images-without-dimensions"] || !deskGot["small-text"] {
		t.Errorf("desktop: viewport-agnostic smells should still fire, got %v", deskGot)
	}
}

// TestMapPageBlocksWinOverHandAuthoredFindings asserts the authority rule: when a
// perf/layout BLOCK is present, a hand-authored perf/layout finding in the same
// push is DROPPED (server-computed findings win — no double-emit), while OTHER
// finding types always pass through.
func TestMapPageBlocksWinOverHandAuthoredFindings(t *testing.T) {
	pg := PushPage{
		URL: "home", Viewport: "mobile", Screenshot: "s.png",
		Perf:   &PushPerf{LCPMs: 3200},
		Layout: &PushLayout{MissingViewportMeta: true},
		Findings: []PushFinding{
			{Type: "perf", Severity: "serious", Detail: "hand-authored LCP note"},
			{Type: "layout", Severity: "moderate", Detail: "hand-authored layout note"},
			{Type: "a11y", Severity: "serious", Detail: "color-contrast"},
		},
	}
	m := MapPage("run-1", pg, PageKeys{Screenshot: "k"}, "")

	// The hand-authored perf/layout detail text must NOT survive (block wins).
	for _, f := range m.Findings {
		if strings.Contains(f.Detail, "hand-authored") {
			t.Errorf("hand-authored %s finding should be dropped when a block is present: %q", f.Type, f.Detail)
		}
	}
	// The a11y finding passes through.
	var sawA11y bool
	for _, f := range m.Findings {
		if f.Type == "a11y" && strings.Contains(f.Detail, "color-contrast") {
			sawA11y = true
		}
	}
	if !sawA11y {
		t.Error("a11y finding should pass through unchanged")
	}
	// The perf finding present is the SERVER-COMPUTED one (LCP from the block).
	if !dbPerfMetrics(m.Findings)["LCP"] {
		t.Error("expected server-computed LCP perf finding")
	}
}

// TestMapPageHandAuthoredPerfPassesThroughWithoutBlock asserts backward compat: a
// push with a hand-authored perf/layout finding and NO block still passes those
// findings through (blocks-win only applies when the block is present).
func TestMapPageHandAuthoredPerfPassesThroughWithoutBlock(t *testing.T) {
	pg := PushPage{
		URL: "home", Viewport: "mobile", Screenshot: "s.png",
		Findings: []PushFinding{
			{Type: "perf", Severity: "serious", Detail: "LCP 3100ms on <div>"},
			{Type: "layout", Severity: "moderate", Detail: "overflow"},
		},
	}
	m := MapPage("run-1", pg, PageKeys{Screenshot: "k"}, "")
	if len(m.Findings) != 2 {
		t.Fatalf("no-block push should pass both findings through, got %d", len(m.Findings))
	}
	if strings.Contains(m.Findings[0].Detail, "<div>") {
		t.Errorf("perf detail not escaped: %q", m.Findings[0].Detail)
	}
	if !strings.Contains(m.Findings[0].Detail, "LCP 3100ms") {
		t.Errorf("perf detail lost: %q", m.Findings[0].Detail)
	}
	if m.Report.Perf != nil || m.Report.Layout != nil {
		t.Errorf("no block → nil report Perf/Layout, got %+v / %+v", m.Report.Perf, m.Report.Layout)
	}
}

func TestMapPageNoPerfLeavesZero(t *testing.T) {
	// A pre-perf push (no perf block) persists with zero perf + nil report.Perf.
	m := MapPage("r", PushPage{URL: "u", Viewport: "desktop", Screenshot: "s.png"}, PageKeys{Screenshot: "k"}, "")
	if m.Page.LCPMs != 0 || m.Page.WeightBytes != 0 || m.Page.ReqCount != 0 {
		t.Fatalf("expected zero perf, got %+v", m.Page)
	}
	if m.Report.Perf != nil {
		t.Fatalf("expected nil report.Perf, got %+v", m.Report.Perf)
	}
}

// itoa avoids importing strconv just for the too-many-pages builder.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
