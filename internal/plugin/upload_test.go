package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePushServer parses the multipart the uploader sends and echoes back a
// PushResult, asserting the metadata + files arrived intact.
func fakePushServer(t *testing.T, wantFiles map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(PushEndpoint, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		meta := r.FormValue("metadata")
		if _, err := Parse(strings.NewReader(meta)); err != nil {
			http.Error(w, "bad meta: "+err.Error(), http.StatusBadRequest)
			return
		}
		for name, want := range wantFiles {
			fhs := r.MultipartForm.File[name]
			if len(fhs) == 0 {
				http.Error(w, "missing file "+name, http.StatusBadRequest)
				return
			}
			f, _ := fhs[0].Open()
			got, _ := io.ReadAll(f)
			f.Close()
			if string(got) != want {
				http.Error(w, "file "+name+" mismatch", http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(PushResult{RunID: "run-9", URL: r.Host + "/runs/run-9"})
	})
	return httptest.NewServer(mux)
}

func TestUpload(t *testing.T) {
	files := map[string][]byte{"a.png": []byte("PNGDATA"), "a.json": []byte(`{"violations":[]}`)}
	srv := fakePushServer(t, map[string]string{"a.png": "PNGDATA", "a.json": `{"violations":[]}`})
	defer srv.Close()

	meta := []byte(`{"pages":[{"url":"home","viewport":"desktop","screenshot":"a.png","axe":"a.json"}]}`)
	res, err := Upload(context.Background(), srv.Client(), srv.URL, "tok-123", meta, files)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.RunID != "run-9" {
		t.Fatalf("run id = %q", res.RunID)
	}
}

func TestUploadBadTokenSurfacesError(t *testing.T) {
	srv := fakePushServer(t, nil)
	defer srv.Close()
	_, err := Upload(context.Background(), srv.Client(), srv.URL, "wrong", []byte(`{"pages":[]}`), nil)
	if err == nil || !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("expected push-failed error, got %v", err)
	}
}

func TestUploadFromDisk(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "s.png"), []byte("SHOT"), 0o644)
	metaPath := filepath.Join(dir, "metadata.json")
	os.WriteFile(metaPath, []byte(`{"pages":[{"url":"home","viewport":"mobile","screenshot":"s.png"}]}`), 0o644)

	srv := fakePushServer(t, map[string]string{"s.png": "SHOT"})
	defer srv.Close()

	res, err := UploadFromDisk(context.Background(), srv.Client(), srv.URL, "tok-123", metaPath, dir)
	if err != nil {
		t.Fatalf("upload from disk: %v", err)
	}
	if res.RunID != "run-9" {
		t.Fatalf("run id = %q", res.RunID)
	}
}

// TestUploadFromDiskRejectsPathTraversal verifies that a metadata file that
// references a filename outside filesDir (traversal / absolute / directory
// component) is rejected BEFORE any file read or push request — the uploader
// must never exfiltrate a local file the attacker names in metadata.
func TestUploadFromDiskRejectsPathTraversal(t *testing.T) {
	// A canary file one dir up that a traversal would try to read.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	filesDir := filepath.Join(root, "files")
	if err := os.Mkdir(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The server must never be hit for a rejected reference.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cases := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{"parent-traversal", "../secret.txt", "refusing referenced file"},
		{"deep-traversal", "../../etc/passwd", "refusing referenced file"},
		{"absolute", "/etc/passwd", "refusing referenced file"},
		{"subdir", "sub/evil.png", "refusing referenced file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hit = false
			metaPath := filepath.Join(filesDir, "metadata.json")
			meta := `{"pages":[{"url":"home","viewport":"desktop","screenshot":` +
				mustJSON(tc.ref) + `}]}`
			if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := UploadFromDisk(context.Background(), srv.Client(), srv.URL, "tok-123", metaPath, filesDir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected containment rejection, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.ref) {
				t.Errorf("error should name the bad file %q, got %v", tc.ref, err)
			}
			if hit {
				t.Error("push server was contacted for a rejected reference — nothing should be sent")
			}
		})
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestUploadFromDiskMissingFile(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "metadata.json")
	os.WriteFile(metaPath, []byte(`{"pages":[{"url":"home","viewport":"mobile","screenshot":"gone.png"}]}`), 0o644)
	_, err := UploadFromDisk(context.Background(), nil, "http://example.invalid", "t", metaPath, dir)
	if err == nil || !strings.Contains(err.Error(), "gone.png") {
		t.Fatalf("expected missing-file error, got %v", err)
	}
}
