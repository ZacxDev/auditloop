// Plugin-push e2e: stands up the app (DEV_MODE, filesystem storage + sqlite),
// creates a push-only plugin target + token, then uses the REAL cmd/auditloop-push
// CLI binary to push a synthetic run (2 pages + real PNG bytes + counts). It
// asserts the run + pages + findings render and the images landed in the Store,
// then pushes a SECOND run with one page's screenshot changed and asserts the P2
// visual-regression diff surfaces. No chromium is needed (nothing is crawled).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/handlers"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/config"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/storage"

	"net/http/httptest"
)

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// buildPushCLI compiles cmd/auditloop-push into a temp binary.
func buildPushCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "auditloop-push")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/ZacxDev/auditloop/cmd/auditloop-push")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build auditloop-push: %v\n%s", err, out)
	}
	return bin
}

func TestEndToEndPluginPush(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in -short mode")
	}
	cli := buildPushCLI(t)

	tmp := t.TempDir()
	cfg := config.AppConfig{
		Port: "0", Role: config.RoleWeb, DatabaseDriver: "sqlite",
		DatabasePath: filepath.Join(tmp, "e2e-plugin.db"), S3Local: filepath.Join(tmp, "artifacts"),
		DevMode: true,
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

	// Create a plugin target + push token.
	tgt, err := database.CreatePluginTarget(auth.DefaultDevUser, "CI funnel", "")
	if err != nil {
		t.Fatalf("create plugin target: %v", err)
	}
	token, hash, _ := plugin.GenerateToken()
	if err := database.SetPluginToken(tgt.ID, hash); err != nil {
		t.Fatalf("set token: %v", err)
	}

	// --- Push 1: two pages (home desktop+mobile), a real PNG, an a11y finding. ---
	dir1 := t.TempDir()
	white := solidPNG(t, 80, 80, color.White)
	os.WriteFile(filepath.Join(dir1, "home-desktop.png"), white, 0o644)
	os.WriteFile(filepath.Join(dir1, "home-mobile.png"), white, 0o644)
	os.WriteFile(filepath.Join(dir1, "home-axe.json"), []byte(`{"violations":[]}`), 0o644)
	meta1 := `{"label":"CI build 1","pages":[
		{"url":"home","viewport":"desktop","screenshot":"home-desktop.png","axe":"home-axe.json",
		 "axe_violations":1,"console_first_party":2,
		 "findings":[{"type":"a11y","severity":"serious","detail":"button has no accessible name"}]},
		{"url":"home","viewport":"mobile","screenshot":"home-mobile.png"}
	]}`
	os.WriteFile(filepath.Join(dir1, "metadata.json"), []byte(meta1), 0o644)

	out := runPushCLI(t, cli, appSrv.URL, token, filepath.Join(dir1, "metadata.json"), dir1)
	if !strings.Contains(out, "/runs/") {
		t.Fatalf("CLI output missing run URL: %q", out)
	}

	runs, _ := database.ListRuns(auth.DefaultDevUser, tgt.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run after push, got %d", len(runs))
	}
	run1 := runs[0]
	if run1.Status != db.RunDone || run1.Trigger != "plugin" || run1.Label != "CI build 1" {
		t.Fatalf("bad pushed run: status=%s trigger=%s label=%q", run1.Status, run1.Trigger, run1.Label)
	}

	// Pages + findings persisted.
	prs, _ := database.ListPages(run1.ID)
	if len(prs) != 2 {
		t.Fatalf("expected 2 page rows, got %d", len(prs))
	}
	foundFinding := false
	for _, p := range prs {
		finds, _ := database.ListFindings(p.ID)
		for _, f := range finds {
			if f.Type == db.FindingA11y && strings.Contains(f.Detail, "accessible name") {
				foundFinding = true
			}
		}
	}
	if !foundFinding {
		t.Error("expected the pushed a11y finding to be persisted")
	}

	// Images + report.json in storage.
	fs := store.(*storage.FS)
	keys, _ := fs.List(context.Background(), "")
	var pngs, reports int
	for _, k := range keys {
		if strings.HasSuffix(k, ".png") {
			pngs++
		}
		if strings.HasSuffix(k, "/report.json") {
			reports++
		}
	}
	if pngs < 2 {
		t.Errorf("expected >=2 screenshots in storage, got %d (%v)", pngs, keys)
	}
	if reports != 1 {
		t.Errorf("expected 1 report.json, got %d", reports)
	}

	// The run view renders.
	body := getBody(t, appSrv.URL+"/runs/"+run1.ID)
	if !strings.Contains(body, "Audit report") {
		t.Error("pushed run view did not render")
	}

	// --- Push 2: same URL+viewport, CHANGED same-size screenshot → P2 regression. ---
	dir2 := t.TempDir()
	red := solidPNG(t, 80, 80, color.RGBA{200, 20, 20, 255})
	os.WriteFile(filepath.Join(dir2, "home-desktop.png"), red, 0o644)
	os.WriteFile(filepath.Join(dir2, "home-mobile.png"), white, 0o644)
	meta2 := `{"label":"CI build 2","pages":[
		{"url":"home","viewport":"desktop","screenshot":"home-desktop.png"},
		{"url":"home","viewport":"mobile","screenshot":"home-mobile.png"}
	]}`
	os.WriteFile(filepath.Join(dir2, "metadata.json"), []byte(meta2), 0o644)

	runPushCLI(t, cli, appSrv.URL, token, filepath.Join(dir2, "metadata.json"), dir2)

	runs, _ = database.ListRuns(auth.DefaultDevUser, tgt.ID)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs after second push, got %d", len(runs))
	}
	run2 := runs[0] // newest first
	if run2.PrevRunID != run1.ID {
		t.Fatalf("run2 baseline = %q, want %q", run2.PrevRunID, run1.ID)
	}
	if run2.DiffJSON == "" {
		t.Fatal("run2 must carry a P2 diff")
	}
	var d report.Diff
	if err := json.Unmarshal([]byte(run2.DiffJSON), &d); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if d.PagesChanged < 1 {
		t.Errorf("expected >=1 visual regression (home desktop changed), got pages_changed=%d", d.PagesChanged)
	}
	// A diff image landed under run2.
	keys, _ = fs.List(context.Background(), "")
	var diffs int
	for _, k := range keys {
		if strings.HasSuffix(k, ".diff.png") && strings.Contains(k, run2.ID) {
			diffs++
		}
	}
	if diffs < 1 {
		t.Errorf("expected >=1 diff image for run2, got %d", diffs)
	}

	// The run view renders the "Changes since" section.
	body = getBody(t, appSrv.URL+"/runs/"+run2.ID)
	if !strings.Contains(body, "Changes since") {
		t.Error("run2 view did not render the P2 changes section")
	}

	t.Logf("plugin-push e2e OK: pushed 2 runs via CLI, regression detected (pages_changed=%d, diffImages=%d)", d.PagesChanged, diffs)
}

func runPushCLI(t *testing.T, cli, url, token, metaPath, filesDir string) string {
	t.Helper()
	cmd := exec.Command(cli, "--url", url, "--token", token, "--meta", metaPath, "--files", filesDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("auditloop-push failed: %v\n%s", err, out)
	}
	return string(out)
}
