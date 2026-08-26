package crawler

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxFaviconBytes caps a fetched favicon so a hostile/oversized asset can never
// balloon memory or storage. 512 KiB is generous for any real favicon.
const MaxFaviconBytes = 512 * 1024

// faviconHrefScript resolves the page's declared favicon to an ABSOLUTE url (the
// browser resolves `.href` against the document base), or "" when the page
// declares none — the caller then falls back to <origin>/favicon.ico. It reads
// the first <link rel~="icon"> (covers "icon", "shortcut icon", "apple-touch-icon"
// via the ~= word match on the primary "icon" rel). Pure read, no mutation.
const faviconHrefScript = `(() => {
  const l = document.querySelector("link[rel~='icon']") ||
            document.querySelector("link[rel='shortcut icon']");
  return (l && l.href) ? l.href : "";
})()`

// faviconExtFor maps a sniffed content-type to the storage extension we serve it
// under, and reports whether the type is an accepted RASTER image. SVG is
// deliberately REJECTED (returns ok=false): a stored SVG served inline over the
// app's own origin could carry <script> (stored-XSS), so favicon capture only
// accepts raster formats — an SVG-only favicon degrades to the name monogram in
// the UI. The declared content-type is ignored; the caller sniffs the bytes with
// http.DetectContentType so a mislabeled asset can't slip through.
func faviconExtFor(sniffedType string) (ext string, ok bool) {
	switch sniffedType {
	case "image/png":
		return "png", true
	case "image/jpeg":
		return "jpg", true
	case "image/gif":
		return "gif", true
	case "image/webp":
		return "webp", true
	case "image/x-icon", "image/vnd.microsoft.icon":
		return "ico", true
	default:
		// text/xml (SVG), text/plain, application/*, etc. — not a raster image.
		return "", false
	}
}

// FetchFavicon fetches a favicon over an SSRF-guarded, IP-PINNED connection,
// size-caps it, and validates it is a raster image — returning its bytes + the
// storage extension. It is best-effort: every failure (guard refusal, non-2xx,
// oversize, non-image, decode) is an error the caller degrades on (no favicon).
//
// SSRF safety: the url first passes the full guard.CheckURL (scheme + same-domain
// host-allowlist + resolve-and-check every IP), exactly like every other fetch in
// the crawler. Then the HTTP connection is PINNED to a guard-validated resolved IP
// (the dialer connects to that literal address, never re-resolving the host), so a
// DNS-rebind between check and connect cannot pivot the fetch to a private/metadata
// address. Redirects are NOT followed (a 3xx could point off-guard) — a favicon
// that only exists behind a redirect degrades.
func (g GuardConfig) FetchFavicon(ctx context.Context, rawURL string) (data []byte, ext string, err error) {
	if err := g.CheckURL(rawURL); err != nil {
		return nil, "", err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	host := strings.ToLower(u.Hostname())

	// Resolve + pick a guard-validated IP to pin the connection to.
	pinned, err := g.pinnedIP(host)
	if err != nil {
		return nil, "", err
	}

	baseDialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			// Ignore addr's host and dial the pre-validated IP:port — no re-resolution.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, splitErr := net.SplitHostPort(addr)
				if splitErr != nil {
					return nil, splitErr
				}
				return baseDialer.DialContext(ctx, network, net.JoinHostPort(pinned.String(), port))
			},
			DisableKeepAlives: true,
		},
		// Do not follow redirects — a 3xx could escape the guard.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New("favicon fetch: non-200 status")
	}

	// Read at most MaxFaviconBytes+1 so an over-cap asset is detected and rejected.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxFaviconBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(buf) == 0 {
		return nil, "", errors.New("favicon fetch: empty body")
	}
	if int64(len(buf)) > MaxFaviconBytes {
		return nil, "", errors.New("favicon too large")
	}
	// Sniff the bytes (declared content-type ignored) — accept raster images only.
	ext, ok := faviconExtFor(http.DetectContentType(buf))
	if !ok {
		return nil, "", errors.New("favicon is not a raster image")
	}
	return buf, ext, nil
}

// pinnedIP resolves host (or parses a literal IP) and returns the first address
// that passes the guard, so the favicon connection can be pinned to it. A literal
// host is checked directly (no DNS). Fail-closed: no allowed address → error.
func (g GuardConfig) pinnedIP(host string) (net.IP, error) {
	if lit := net.ParseIP(host); lit != nil {
		if g.ipReasonForHost(host, lit) == "" {
			return lit, nil
		}
		return nil, &ErrBlockedURL{host, "literal ip refused"}
	}
	resolve := g.Resolve
	if resolve == nil {
		resolve = net.LookupIP
	}
	ips, err := resolve(host)
	if err != nil {
		return nil, &ErrBlockedURL{host, "dns resolution failed: " + err.Error()}
	}
	for _, ip := range ips {
		if g.ipReasonForHost(host, ip) == "" {
			return ip, nil
		}
	}
	return nil, &ErrBlockedURL{host, "host resolved to no allowed address"}
}

// FaviconURLFor derives the favicon url to fetch: the page-declared href when
// present, else <origin>/favicon.ico derived from the landing url. Returns "" when
// no candidate can be formed. (The result still passes guard.CheckURL before any
// fetch — this only chooses the candidate.)
func FaviconURLFor(landingURL, declaredHref string) string {
	if declaredHref != "" {
		return declaredHref
	}
	u, err := url.Parse(landingURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/favicon.ico"
}
