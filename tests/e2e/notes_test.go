package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
)

// fakeOpenRouter serves a canned vision completion and records how many image
// parts each request carried (proving both viewports are sent).
func fakeOpenRouter(t *testing.T, imageParts *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			for _, part := range m.Content {
				if part.Type == "image_url" {
					atomic.AddInt64(imageParts, 1)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "## UX notes (" + req.Model + ")\n- The hero could be clearer.\n- Fix the unlabeled image."}},
			},
		})
	}))
}

// TestEndToEndVisionNotes (P3) crawls the fixture site, then runs a two-model
// vision-notes pass against a FAKE OpenRouter server (no real key needed) and
// asserts: notes rows exist per page per model, both screenshots were sent, the
// run view renders the labeled side-by-side notes, and a human edit saves.
func TestEndToEndVisionNotes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser e2e in -short mode")
	}
	chromium := resolveChromium(t)

	var mutated atomic.Bool // unused mutation; reuse the mutable fixture in baseline state
	fixture := mutableFixtureSite(&mutated)
	defer fixture.Close()
	fixtureHost := hostOnly(fixture.URL)

	var imageParts int64
	or := fakeOpenRouter(t, &imageParts)
	defer or.Close()

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleAll, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-notes.db"), S3Local: filepath.Join(tmp, "artifacts"),
		CrawlMaxPages: 10, CrawlMaxDepth: 2, CrawlAllowLoopback: true,
		ChromiumPath: chromium, DevMode: true,
		// P3: point the OpenRouter client at the fake server with a dummy key.
		OpenRouterAPIKey:  "dummy-key",
		OpenRouterBaseURL: or.URL,
		LLMModels:         []string{"anthropic/claude-haiku-4.5", "anthropic/claude-sonnet-4.6"},
		LLMMaxTokens:      512,
	}
	database, err := db.Open(cfg.DatabaseDriver, cfg.DatabasePath)
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	store, err := handlers.OpenStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router, err := handlers.NewRouter(ctx, cfg, database, store)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	appSrv := httptest.NewServer(router)
	defer appSrv.Close()

	tgt, err := database.CreateTarget(auth.DefaultDevUser, "NotesFixture", fixture.URL, []string{fixtureHost})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	// Crawl.
	run := triggerAndWait(t, appSrv.URL, database, tgt.ID)
	if run.Status != db.RunDone {
		t.Fatalf("run did not complete: %s / %s", run.Status, run.Error)
	}
	pages, _ := database.ListPages(run.ID)
	if len(pages) < 2 {
		t.Fatalf("expected >=2 page rows, got %d", len(pages))
	}
	// Distinct logical pages (URLs) — the notes pass is one call per URL per model.
	urlSet := map[string]bool{}
	for _, p := range pages {
		urlSet[p.URL] = true
	}
	numURLs := len(urlSet)

	// Trigger the two-model notes pass THROUGH the HTTP API.
	models := []string{"anthropic/claude-haiku-4.5", "anthropic/claude-sonnet-4.6"}
	form := url.Values{"models": models}
	req, _ := http.NewRequest("POST", appSrv.URL+"/api/runs/"+run.ID+"/notes", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("trigger notes: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notes trigger status = %d", resp.StatusCode)
	}

	// Poll notes-status until done.
	deadline := time.Now().Add(60 * time.Second)
	var got *db.Run
	for time.Now().Before(deadline) {
		got, _ = database.GetRunByID(run.ID)
		if got.NotesStatus == db.NotesDone || got.NotesStatus == db.NotesFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got == nil || got.NotesStatus != db.NotesDone {
		t.Fatalf("notes job did not complete: status=%v", statusOfNotes(got))
	}

	// One note row per (URL, model).
	notes, _ := database.ListPageNotes(run.ID)
	wantRows := numURLs * len(models)
	if len(notes) != wantRows {
		t.Fatalf("expected %d note rows (%d URLs × %d models), got %d", wantRows, numURLs, len(models), len(notes))
	}
	perModel := map[string]int{}
	for _, n := range notes {
		if n.Error != "" || n.Notes == "" {
			t.Errorf("note %s/%s should be a clean draft: err=%q", n.PageID, n.Model, n.Error)
		}
		perModel[n.Model]++
	}
	for _, m := range models {
		if perModel[m] != numURLs {
			t.Errorf("model %s produced %d notes, want %d", m, perModel[m], numURLs)
		}
	}

	// Both viewports were sent to the model: >= 2 image parts per (URL,model) call.
	if got := atomic.LoadInt64(&imageParts); got < int64(2*wantRows) {
		t.Errorf("image parts sent = %d, want >= %d (2 viewports per call)", got, 2*wantRows)
	}

	// The run view renders the labeled side-by-side notes.
	body := getBody(t, appSrv.URL+"/runs/"+run.ID)
	if !strings.Contains(body, "AI UX notes") {
		t.Error("run view did not render the AI UX notes section")
	}
	if !strings.Contains(body, "anthropic/claude-haiku-4.5") || !strings.Contains(body, "anthropic/claude-sonnet-4.6") {
		t.Error("run view did not label both models")
	}

	// A human edit saves (edited=true).
	editPage := notes[0].PageID
	editModel := notes[0].Model
	editForm := url.Values{"notes": {"# My edited notes\n- Manually curated."}}
	editReq, _ := http.NewRequest("POST", appSrv.URL+"/api/pages/"+editPage+"/notes/"+editModel, strings.NewReader(editForm.Encode()))
	editReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	editResp, err := http.DefaultClient.Do(editReq)
	if err != nil {
		t.Fatalf("edit note: %v", err)
	}
	editResp.Body.Close()
	if editResp.StatusCode != http.StatusOK {
		t.Fatalf("edit status = %d", editResp.StatusCode)
	}
	edited, _ := database.GetPageNote(editPage, editModel)
	if edited == nil || !edited.Edited || !strings.Contains(edited.Notes, "Manually curated") {
		t.Errorf("edit not persisted: %+v", edited)
	}

	t.Logf("e2e notes OK: urls=%d models=%d rows=%d imageParts=%d", numURLs, len(models), len(notes), atomic.LoadInt64(&imageParts))
}

func statusOfNotes(r *db.Run) string {
	if r == nil {
		return "nil"
	}
	return r.NotesStatus
}
