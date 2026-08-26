package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/storage"
)

var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

// fakeOR is a stub OpenRouter server returning a canned completion.
func fakeOR(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "## notes\n- ok"}}},
		})
	}))
}

// testAppLLM builds an app+router with the notes feature enabled (key set, base
// URL pointed at a fake OpenRouter).
func testAppLLM(t *testing.T, baseURL string) (*App, http.Handler) {
	t.Helper()
	tmp := t.TempDir()
	cfg := config.AppConfig{
		Role:              config.RoleWeb,
		DatabaseDriver:    "sqlite",
		DatabasePath:      filepath.Join(tmp, "h.db"),
		S3Local:           filepath.Join(tmp, "art"),
		DevMode:           true,
		OpenRouterAPIKey:  "test-key",
		OpenRouterBaseURL: baseURL,
		LLMMaxTokens:      256,
	}
	database, err := db.Open("sqlite", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFS(cfg.S3Local)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(context.Background(), cfg, database, store)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, DB: database, Store: store}
	return app, router
}

// seedDoneRun creates a completed run (owned by the dev user) with one page + a
// screenshot in storage. Returns run id and the page id.
func seedDoneRun(t *testing.T, app *App) (string, string) {
	t.Helper()
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "Acme", "https://acme.test", []string{"acme.test"})
	run, _ := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID)
	key := storage.ScreenshotKey("acme", run.ID, storage.PageSlug("https://acme.test/"), "desktop")
	_ = app.Store.Put(context.Background(), key, "image/png", bytes.NewReader(tinyPNG), int64(len(tinyPNG)))
	pid, _ := app.DB.InsertPage(&db.Page{RunID: run.ID, URL: "https://acme.test/", Viewport: "desktop", ScreenshotKey: key})
	_ = app.DB.FinishRun(run.ID, db.RunDone, "{}", "")
	return run.ID, pid
}

func TestGenerateNotesDisabled503(t *testing.T) {
	app, router := testApp(t) // no OpenRouter key
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	run, _ := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID)
	_ = app.DB.FinishRun(run.ID, db.RunDone, "{}", "")

	rw := httptest.NewRecorder()
	req := formPost("/api/runs/"+run.ID+"/notes", url.Values{"models": {"anthropic/claude-haiku-4.5"}})
	router.ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when disabled, got %d", rw.Code)
	}
}

func TestGenerateNotesValidation(t *testing.T) {
	srv := fakeOR(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)

	// Empty models → 400.
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/notes", url.Values{}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("empty models should 400, got %d", rw.Code)
	}

	// Unknown model → 400 (arbitrary user-supplied id rejected).
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/notes", url.Values{"models": {"evil/backdoor"}}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("unknown model should 400, got %d", rw.Code)
	}

	// Valid models → 200, job starts generating.
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+runID+"/notes", url.Values{"models": {"anthropic/claude-haiku-4.5"}}))
	if rw.Code != http.StatusOK {
		t.Fatalf("valid request should 200, got %d (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "notes-section") {
		t.Error("expected the notes-section fragment back")
	}
	run, _ := app.DB.GetRunByID(runID)
	if run.NotesStatus != db.NotesGenerating && run.NotesStatus != db.NotesDone {
		t.Errorf("notes status = %q, want generating/done", run.NotesStatus)
	}
	// Let the background pass finish (against the fake OpenRouter) before cleanup
	// closes the DB — and assert it produced a note row.
	waitNotesDone(t, app, runID)
	notes, _ := app.DB.ListPageNotes(runID)
	if len(notes) != 1 {
		t.Errorf("expected 1 note row after the pass, got %d", len(notes))
	}
}

func waitNotesDone(t *testing.T, app *App, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := app.DB.GetRunByID(runID)
		if err == nil && (run.NotesStatus == db.NotesDone || run.NotesStatus == db.NotesFailed) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("notes job for %s did not finish", runID)
}

func TestGenerateNotesRunNotDone(t *testing.T) {
	srv := fakeOR(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	tgt, _ := app.DB.CreateTarget(auth.DefaultDevUser, "T", "https://t.test", nil)
	run, _ := app.DB.CreateRun(auth.DefaultDevUser, tgt.ID) // queued, not done

	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/runs/"+run.ID+"/notes", url.Values{"models": {"anthropic/claude-haiku-4.5"}}))
	if rw.Code != http.StatusBadRequest {
		t.Errorf("notes on a non-done run should 400, got %d", rw.Code)
	}
}

func TestSaveNoteEditAndOwnership(t *testing.T) {
	srv := fakeOR(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, pageID := seedDoneRun(t, app)

	// Seed a draft to edit.
	if err := app.DB.SavePageNoteDraft(pageID, runID, "anthropic/claude-haiku-4.5", "auto draft", "", 0.0012, 900, 210); err != nil {
		t.Fatal(err)
	}

	// Edit it.
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/pages/"+pageID+"/notes/anthropic/claude-haiku-4.5", url.Values{"notes": {"human edited"}}))
	if rw.Code != http.StatusOK {
		t.Fatalf("save edit = %d (%s)", rw.Code, rw.Body.String())
	}
	got, _ := app.DB.GetPageNote(pageID, "anthropic/claude-haiku-4.5")
	if got.Notes != "human edited" || !got.Edited {
		t.Errorf("edit not persisted: %+v", got)
	}

	// A non-existent page → 404.
	rw = httptest.NewRecorder()
	router.ServeHTTP(rw, formPost("/api/pages/does-not-exist/notes/m", url.Values{"notes": {"x"}}))
	if rw.Code != http.StatusNotFound {
		t.Errorf("unknown page should 404, got %d", rw.Code)
	}
}

func TestNotesStatusFragment(t *testing.T) {
	srv := fakeOR(t)
	defer srv.Close()
	app, router := testAppLLM(t, srv.URL)
	runID, _ := seedDoneRun(t, app)
	rw := httptest.NewRecorder()
	router.ServeHTTP(rw, httptest.NewRequest("GET", "/runs/"+runID+"/notes-status", nil))
	if rw.Code != 200 || !strings.Contains(rw.Body.String(), "notes-section") {
		t.Errorf("notes-status fragment bad: %d", rw.Code)
	}
}

func formPost(path string, vals url.Values) *http.Request {
	req := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
