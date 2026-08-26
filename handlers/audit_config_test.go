package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
)

// capField must truncate on a RUNE boundary so it never emits invalid UTF-8
// (Postgres `text` rejects invalid UTF-8 → 500; SQLite would silently accept it,
// so the sqlite-only suite can't catch a byte-split without this test).
func TestCapFieldRuneSafe(t *testing.T) {
	// Multibyte runes (each '世' is 3 bytes) well past the 300-char cap.
	long := strings.Repeat("世", maxAuditFieldLen+50)
	got := capField(long)
	if !utf8.ValidString(got) {
		t.Fatalf("capField produced invalid UTF-8 (byte-split a rune)")
	}
	if n := utf8.RuneCountInString(got); n != maxAuditFieldLen {
		t.Errorf("capField kept %d runes, want %d", n, maxAuditFieldLen)
	}
	// A short multibyte string is returned unchanged.
	if s := "café ☕"; capField(s) != s {
		t.Errorf("capField mangled a short string: %q", capField(s))
	}
}

// fakeORInfer is a stub OpenRouter that returns a canned audit-config draft when
// it sees the inference system prompt ("product analyst").
func fakeORInfer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		content := `{"product_summary":"A generic UX auditor","primary_job":"sign up and run an audit","primary_cta":"Sign up","audiences":["skeptical-evaluator","bogus-persona"]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

func TestInferAuditConfigDisabled503(t *testing.T) {
	app, router := testApp(t) // no OpenRouter key
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+tgt.ID+"/audit-config/infer", url.Values{}))
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when disabled, got %d", rw.Code)
	}
}

func TestInferAuditConfigForeignTarget404(t *testing.T) {
	srv := fakeORInfer(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	// A target owned by another user is invisible to the dev user → 404.
	other, _ := app.DB.CreateTarget("someone-else", "Foreign", "https://f.test", nil)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+other.ID+"/audit-config/infer", url.Values{}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("foreign target infer = %d, want 404", rw.Code)
	}
}

func TestInferAuditConfigNoDoneRun409(t *testing.T) {
	srv := fakeORInfer(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	// No completed run.
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+tgt.ID+"/audit-config/infer", url.Values{}))
	if rw.Code != http.StatusConflict {
		t.Errorf("infer with no done run = %d, want 409", rw.Code)
	}
}

func TestInferAuditConfigHappyPath(t *testing.T) {
	srv := fakeORInfer(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)
	run, _ := app.DB.GetRunByID(runID)

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+run.TargetID+"/audit-config/infer", url.Values{}))
	if rw.Code != http.StatusOK {
		t.Fatalf("infer = %d (%s)", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, "audit-config-section") {
		t.Error("expected the audit-config-section fragment back")
	}
	if !strings.Contains(body, "sign up and run an audit") {
		t.Errorf("draft job not pre-filled in the card: %s", body)
	}
	if !strings.Contains(body, "inferred — review") {
		t.Error("expected the inferred (unconfirmed) badge")
	}

	cfg, found, err := app.DB.GetTargetAuditConfig(auth.DefaultDevUser, run.TargetID)
	if err != nil || !found {
		t.Fatalf("config not stored: found=%v err=%v", found, err)
	}
	if !cfg.Inferred || cfg.Confirmed {
		t.Errorf("stored draft flags: inferred=%v confirmed=%v, want inferred/unconfirmed", cfg.Inferred, cfg.Confirmed)
	}
	if cfg.PrimaryJob != "sign up and run an audit" || cfg.PrimaryCTA != "Sign up" {
		t.Errorf("draft fields not stored: %+v", cfg)
	}
	// audiences filtered to the allowlist ("bogus-persona" dropped).
	if len(cfg.Personas) != 1 || cfg.Personas[0] != "skeptical-evaluator" {
		t.Errorf("personas not filtered to allowlist: %v", cfg.Personas)
	}
}

func TestSaveAuditConfig(t *testing.T) {
	srv := fakeORInfer(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)

	// Unknown persona → 400.
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+tgt.ID+"/audit-config", url.Values{"personas": {"evil/backdoor"}}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("unknown persona should 400, got %d", rw.Code)
	}

	// Valid save/confirm.
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+tgt.ID+"/audit-config", url.Values{
		"product_summary": {"My product"},
		"primary_job":     {"do the thing"},
		"primary_cta":     {"Start"},
		"personas":        {"returning-power-user", "skeptical-evaluator"},
	}))
	if rw.Code != http.StatusOK {
		t.Fatalf("valid save = %d (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "confirmed") {
		t.Error("expected the confirmed badge after save")
	}
	cfg, found, _ := app.DB.GetTargetAuditConfig(auth.DefaultDevUser, tgt.ID)
	if !found || !cfg.Confirmed {
		t.Fatalf("config not confirmed: found=%v cfg=%+v", found, cfg)
	}
	if cfg.PrimaryJob != "do the thing" || len(cfg.Personas) != 2 {
		t.Errorf("saved fields wrong: %+v", cfg)
	}

	// Foreign target → 404.
	other, _ := app.DB.CreateTarget("someone-else", "Foreign", "https://f.test", nil)
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/targets/"+other.ID+"/audit-config", url.Values{"primary_job": {"x"}}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("foreign target save = %d, want 404", rw.Code)
	}
}

// TestEvalFormPrefillsFromConfig asserts the evaluate form defaults its job +
// pre-checks personas from the target's CONFIRMED audit config, and falls back to
// the Phase-1 defaults (empty job, first-persona-checked) when there's no config.
func TestEvalFormPrefillsFromConfig(t *testing.T) {
	srv := fakeOREval(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)
	run, _ := app.DB.GetRunByID(runID)

	// Baseline: no config → the eval form has an empty job and the first persona
	// (first-time-nontechnical) pre-checked (Phase-1 default).
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/runs/"+runID+"/eval-status", nil))
	if inputChecked(rw.Body.String(), "returning-power-user") {
		t.Error("with no config, returning-power-user should not be pre-checked")
	}
	if !inputChecked(rw.Body.String(), "first-time-nontechnical") {
		t.Error("with no config, the first persona should be pre-checked (Phase-1 default)")
	}

	// Confirm a config with a distinct job + non-default personas.
	if err := app.DB.SetTargetAuditConfig(&db.TargetAuditConfig{
		TargetID:   run.TargetID,
		PrimaryJob: "sign up and create a project",
		Personas:   []string{"returning-power-user"},
		Confirmed:  true,
	}); err != nil {
		t.Fatal(err)
	}

	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/runs/"+runID+"/eval-status", nil))
	body := rw.Body.String()
	if !strings.Contains(body, "sign up and create a project") {
		t.Errorf("eval form job not pre-filled from the config: %s", body)
	}
	if !inputChecked(body, "returning-power-user") {
		t.Error("config persona returning-power-user should be pre-checked")
	}
	if inputChecked(body, "first-time-nontechnical") {
		t.Error("the Phase-1 default persona should NOT be checked once the config overrides it")
	}
}

// inputChecked reports whether the <input> tag carrying value="<val>" also has the
// boolean `checked` attribute (checks the substring from the value to the tag's
// closing '>').
func inputChecked(html, val string) bool {
	needle := `value="` + val + `"`
	i := strings.Index(html, needle)
	if i < 0 {
		return false
	}
	end := strings.IndexByte(html[i:], '>')
	if end < 0 {
		return false
	}
	return strings.Contains(html[i:i+end], "checked")
}
