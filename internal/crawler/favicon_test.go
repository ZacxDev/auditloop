package crawler

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// loopbackGuard permits the loopback fixture host so FetchFavicon can dial an
// httptest server (mirrors the crawler's dev/test AllowLoopback escape hatch).
func loopbackGuard() GuardConfig {
	return GuardConfig{AllowedHosts: []string{"127.0.0.1"}, AllowLoopback: true}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{10, 20, 30, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestFetchFaviconHappyPath(t *testing.T) {
	body := tinyPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declare a bogus content-type; the fetch sniffs the bytes, not the header.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	data, ext, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.ico")
	if err != nil {
		t.Fatalf("FetchFavicon: %v", err)
	}
	if ext != "png" {
		t.Errorf("ext = %q, want png", ext)
	}
	if !bytes.Equal(data, body) {
		t.Errorf("returned bytes differ from server body")
	}
}

func TestFetchFaviconRejectsOffDomain(t *testing.T) {
	// Host not in the allowlist → refused by CheckURL before any network access.
	guard := GuardConfig{AllowedHosts: []string{"example.com"}}
	if _, _, err := guard.FetchFavicon(context.Background(), "http://cdn.evil.test/favicon.ico"); err == nil {
		t.Fatal("expected off-domain favicon to be rejected")
	}
}

func TestFetchFaviconRejectsPrivateIP(t *testing.T) {
	guard := GuardConfig{
		AllowedHosts: []string{"internal.test"},
		Resolve:      func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.5")}, nil },
	}
	if _, _, err := guard.FetchFavicon(context.Background(), "http://internal.test/favicon.ico"); err == nil {
		t.Fatal("expected private-IP favicon to be rejected")
	}
}

func TestFetchFaviconRejectsNonImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body>not an image</body></html>"))
	}))
	defer srv.Close()
	if _, _, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.ico"); err == nil {
		t.Fatal("expected non-image favicon to be rejected")
	}
}

func TestFetchFaviconRejectsSVG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	}))
	defer srv.Close()
	// SVG sniffs as text/xml, not a raster image → rejected (stored-XSS avoidance).
	if _, _, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.svg"); err == nil {
		t.Fatal("expected SVG favicon to be rejected (raster-only)")
	}
}

func TestFetchFaviconRejectsOversize(t *testing.T) {
	big := bytes.Repeat([]byte("A"), MaxFaviconBytes+64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()
	_, _, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.ico")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected too-large rejection, got %v", err)
	}
}

func TestFetchFaviconRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, _, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.ico"); err == nil {
		t.Fatal("expected non-200 favicon to be rejected")
	}
}

// TestFetchFaviconPinRefusesDNSRebind proves the connection is PINNED to a
// guard-validated IP: FetchFavicon resolves the host twice — once inside
// CheckURL (checkHostIP) and once in pinnedIP (the address it dials). A hostile
// resolver that answers a PUBLIC IP on the first lookup (so CheckURL passes) but
// a loopback IP on the second (the pin) must be refused at the pin step, so no
// connection is ever made — closing the DNS-rebind TOCTOU window.
//
// Non-vacuous: the second lookup points at a REAL httptest server serving a
// valid favicon, so if the pin step did NOT re-check the resolved IP the fetch
// would connect and SUCCEED. The test only passes because the pin rejects the
// blocked (loopback, AllowLoopback=false) address before dialing.
func TestFetchFaviconPinRefusesDNSRebind(t *testing.T) {
	body := tinyPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// The httptest server's port — the pinned dial reuses the URL's port, so the
	// rawURL must carry it for the rebind's loopback IP to reach the server.
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv url: %v", err)
	}
	loopbackIP := net.ParseIP(u.Hostname()) // 127.0.0.1

	// First Resolve → public (CheckURL passes); every subsequent Resolve → the
	// httptest server's loopback IP (the pin must refuse it, AllowLoopback=false).
	var calls int
	guard := GuardConfig{
		AllowedHosts: []string{"internal.test"},
		Resolve: func(string) ([]net.IP, error) {
			calls++
			if calls == 1 {
				return []net.IP{net.ParseIP("93.184.216.34")}, nil // public
			}
			return []net.IP{loopbackIP}, nil // rebind → blocked at pin
		},
	}

	rawURL := "http://internal.test:" + u.Port() + "/favicon.ico"
	data, _, err := guard.FetchFavicon(context.Background(), rawURL)
	if err == nil {
		t.Fatal("expected DNS-rebind favicon to be refused at the pin step (no connection)")
	}
	if data != nil {
		t.Errorf("expected no bytes on refusal, got %d", len(data))
	}
	if calls < 2 {
		t.Fatalf("expected the pin to re-resolve (>=2 Resolve calls), got %d — test would pass vacuously", calls)
	}
}

// TestFetchFaviconDoesNotFollowRedirect proves a 3xx is NOT followed: the client
// sets CheckRedirect=ErrUseLastResponse, so the 302 response is returned as-is
// and the StatusCode != 200 check rejects it. This closes redirect-hop SSRF (a
// favicon endpoint 302-ing to a would-be-blocked address never gets fetched).
//
// Non-vacuous: the redirect Location points at a same-server endpoint serving a
// VALID favicon; if redirects WERE followed the fetch would succeed AND hit that
// endpoint. The test asserts both an error AND that the Location handler was
// never invoked, so flipping CheckRedirect to follow flips both assertions.
func TestFetchFaviconDoesNotFollowRedirect(t *testing.T) {
	body := tinyPNG(t)
	var locationHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/real-favicon.png", http.StatusFound) // 302
	})
	mux.HandleFunc("/real-favicon.png", func(w http.ResponseWriter, r *http.Request) {
		locationHit = true
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	data, _, err := loopbackGuard().FetchFavicon(context.Background(), srv.URL+"/favicon.ico")
	if err == nil {
		t.Fatal("expected a redirected favicon to be rejected (redirects not followed)")
	}
	if locationHit {
		t.Error("redirect target was fetched — the redirect was followed (SSRF hop not closed)")
	}
	if data != nil {
		t.Errorf("expected no bytes on a redirect, got %d", len(data))
	}
}

func TestFaviconExtFor(t *testing.T) {
	cases := map[string]struct {
		ext string
		ok  bool
	}{
		"image/png":                {"png", true},
		"image/jpeg":               {"jpg", true},
		"image/gif":                {"gif", true},
		"image/webp":               {"webp", true},
		"image/x-icon":             {"ico", true},
		"image/vnd.microsoft.icon": {"ico", true},
		"text/xml; charset=utf-8":  {"", false},
		"text/plain":               {"", false},
		"application/octet-stream": {"", false},
		"image/svg+xml":            {"", false},
	}
	for ct, want := range cases {
		ext, ok := faviconExtFor(strings.SplitN(ct, ";", 2)[0])
		if ext != want.ext || ok != want.ok {
			t.Errorf("faviconExtFor(%q) = (%q,%v), want (%q,%v)", ct, ext, ok, want.ext, want.ok)
		}
	}
}

func TestFaviconURLFor(t *testing.T) {
	if got := FaviconURLFor("https://acme.com/", "https://acme.com/fav.png"); got != "https://acme.com/fav.png" {
		t.Errorf("declared href should pass through, got %q", got)
	}
	if got := FaviconURLFor("https://acme.com/deep/page", ""); got != "https://acme.com/favicon.ico" {
		t.Errorf("fallback = %q, want https://acme.com/favicon.ico", got)
	}
	if got := FaviconURLFor("not-a-url", ""); got != "" {
		t.Errorf("unparseable landing should yield empty, got %q", got)
	}
}
