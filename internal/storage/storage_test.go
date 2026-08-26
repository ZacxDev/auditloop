package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Acme Corp":            "acme-corp",
		"https://x.test/":      "https-x-test",
		"":                     "root",
		"  Multiple   Spaces ": "multiple-spaces",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPageSlugStableAndUnique(t *testing.T) {
	a := PageSlug("https://x.test/products?id=1")
	b := PageSlug("https://x.test/products?id=2")
	if a == b {
		t.Error("distinct URLs should not collide")
	}
	if a != PageSlug("https://x.test/products?id=1") {
		t.Error("same URL should be stable")
	}
	if !strings.HasPrefix(a, "products") && !strings.Contains(a, "products") {
		t.Errorf("slug not readable: %q", a)
	}
}

func TestKeyScheme(t *testing.T) {
	ps := "home-abc12345"
	if got := ScreenshotKey("acme", "run1", ps, "mobile"); got != "acme/run1/home-abc12345/mobile.png" {
		t.Errorf("screenshot key = %q", got)
	}
	if got := ReportKey("acme", "run1"); got != "acme/run1/report.json" {
		t.Errorf("report key = %q", got)
	}
	if got := AxeKey("acme", "run1", ps); got != "acme/run1/home-abc12345/axe.json" {
		t.Errorf("axe key = %q", got)
	}
	if got := A11yDigestKey("acme", "run1", ps); got != "acme/run1/home-abc12345/a11y.json" {
		t.Errorf("a11y digest key = %q, want acme/run1/home-abc12345/a11y.json", got)
	}
}

func TestFaviconKey(t *testing.T) {
	// Run-scoped (target_slug/run_id/…) so the artifact proxy's run_id ownership
	// check authorizes it like a screenshot.
	if got := FaviconKey("acme", "run1", "png"); got != "acme/run1/favicon.png" {
		t.Errorf("favicon key = %q, want acme/run1/favicon.png", got)
	}
	if got := FaviconKey("acme", "run1", "ico"); got != "acme/run1/favicon.ico" {
		t.Errorf("favicon key (ico) = %q", got)
	}
	// Empty ext defaults to png so the key is always well-formed.
	if got := FaviconKey("acme", "run1", ""); got != "acme/run1/favicon.png" {
		t.Errorf("favicon key (empty ext) = %q, want …/favicon.png", got)
	}
	// The 2nd path segment must be the run_id (per-object ownership contract).
	parts := strings.Split(FaviconKey("acme", "run-xyz", "webp"), "/")
	if len(parts) != 3 || parts[1] != "run-xyz" {
		t.Errorf("run_id not the 2nd segment: %v", parts)
	}
}

func TestFaviconContentType(t *testing.T) {
	cases := map[string]string{
		"png":  "image/png",
		"jpg":  "image/jpeg",
		"jpeg": "image/jpeg",
		"gif":  "image/gif",
		"webp": "image/webp",
		"ico":  "image/x-icon",
		".ico": "image/x-icon",
		"":     "application/octet-stream", // never text/html — no active-content sniff
		"exe":  "application/octet-stream",
	}
	for ext, want := range cases {
		if got := FaviconContentType(ext); got != want {
			t.Errorf("FaviconContentType(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestFSRoundTrip(t *testing.T) {
	st, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := []byte("hello-artifact")
	key := "acme/run1/home/mobile.png"
	if err := st.Put(ctx, key, "image/png", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("roundtrip mismatch: %q", got)
	}
	// List by prefix.
	if err := st.Put(ctx, "acme/run1/report.json", "application/json", strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	keys, err := st.List(ctx, "acme/run1/")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	if len(keys) != 2 {
		t.Errorf("list returned %v", keys)
	}
	// Presign returns the proxy path.
	u, err := st.PresignGet(ctx, key, time.Minute)
	if err != nil || u != "/artifacts/"+key {
		t.Errorf("presign = %q err=%v", u, err)
	}
}

// TestS3RoundTrip runs against a real MinIO/S3 when S3_ENDPOINT is set (e.g. the
// docker-compose.test.yml stack). Skipped otherwise so `go test ./...` stays
// hermetic and green without external services.
func TestS3RoundTrip(t *testing.T) {
	ep := os.Getenv("S3_ENDPOINT")
	if ep == "" {
		t.Skip("S3_ENDPOINT unset — skipping real MinIO integration test")
	}
	ctx := context.Background()
	st, err := NewS3(ctx, S3Config{
		Endpoint:     ep,
		Bucket:       envOr("S3_BUCKET", "audit-artifacts"),
		AccessKey:    os.Getenv("S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("S3_SECRET_KEY"),
		Region:       envOr("S3_REGION", "us-east-1"),
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	key := "test/roundtrip/obj.txt"
	body := []byte("s3-roundtrip")
	if err := st.Put(ctx, key, "text/plain", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Errorf("mismatch: %q", got)
	}
	keys, err := st.List(ctx, "test/roundtrip/")
	if err != nil || len(keys) == 0 {
		t.Errorf("list: %v %v", keys, err)
	}
	if u, err := st.PresignGet(ctx, key, time.Minute); err != nil || !strings.Contains(u, key) {
		t.Errorf("presign: %q %v", u, err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
