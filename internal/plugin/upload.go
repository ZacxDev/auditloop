package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PushEndpoint is the ingestion path a push is POSTed to.
const PushEndpoint = "/api/plugins/runs"

// PushResult is the JSON body auditloop returns on a successful push.
type PushResult struct {
	RunID string `json:"run_id"`
	URL   string `json:"url"`
}

// Upload builds the multipart body (a `metadata` part = metaJSON + one file part
// per entry in files, each part's form-name = its filename) and POSTs it to
// baseURL+PushEndpoint with the bearer push token. It returns the parsed result
// (run id + absolute run URL). A non-2xx response yields an error carrying the
// server's (credential-free) message.
func Upload(ctx context.Context, client *http.Client, baseURL, token string, metaJSON []byte, files map[string][]byte) (*PushResult, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// metadata part (a plain form field).
	if err := mw.WriteField("metadata", string(metaJSON)); err != nil {
		return nil, err
	}
	// One file part per artifact; the form-name IS the filename so the server maps
	// referenced filenames → parts.
	for name, data := range files {
		fw, err := mw.CreateFormFile(name, name)
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	url := strings.TrimRight(baseURL, "/") + PushEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("push failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var res PushResult
	if err := json.Unmarshal(respBody, &res); err != nil {
		return nil, fmt.Errorf("push succeeded but response was not understood: %w", err)
	}
	return &res, nil
}

// UploadFromDisk reads a metadata JSON file, resolves every referenced filename
// against filesDir, and pushes them. It is the core of the reference CLI
// (cmd/auditloop-push) and is exercised directly in tests.
func UploadFromDisk(ctx context.Context, client *http.Client, baseURL, token, metaPath, filesDir string) (*PushResult, error) {
	metaJSON, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read metadata %q: %w", metaPath, err)
	}
	payload, err := Parse(bytes.NewReader(metaJSON))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for name := range payload.ReferencedFiles() {
		// Path containment: the referenced filename comes from attacker-influenceable
		// metadata, so it must be a plain basename that stays inside filesDir. Reject
		// anything with a directory component, an absolute path, or a traversal — never
		// read (or send) a file outside filesDir (e.g. ../../etc/passwd).
		if name != filepath.Base(name) || name == "." || name == ".." ||
			filepath.IsAbs(name) || strings.Contains(name, "..") ||
			strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
			return nil, fmt.Errorf("refusing referenced file %q: must be a plain filename within the files dir", name)
		}
		data, err := os.ReadFile(filepath.Join(filesDir, name))
		if err != nil {
			return nil, fmt.Errorf("read referenced file %q: %w", name, err)
		}
		files[name] = data
	}
	return Upload(ctx, client, baseURL, token, metaJSON, files)
}
