package crawler

import (
	"net/url"
	"strings"
)

// Origin classification: console + network events are split into first-party
// (the crawled page's own origin — real UX signal) and third-party (analytics,
// CDNs, embeds — bucketed separately, never dropped). Comparison is by origin
// (scheme://host:port) via url.Parse, so it survives new SDKs without a
// denylist.

// SameOrigin reports whether candidate shares an origin with base. Origin =
// scheme + host + explicit-or-default port. Unparseable or relative candidates
// (data:, blob:, about:, inline) are treated as first-party (they belong to the
// page, not a third party).
func SameOrigin(base, candidate string) bool {
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	c, err := url.Parse(candidate)
	if err != nil {
		return true // unparseable → attribute to the page, not a 3rd party
	}
	if c.Scheme == "" && c.Host == "" {
		return true // relative / inline (data:, blob:, "javascript:")
	}
	if c.Scheme == "data" || c.Scheme == "blob" || c.Scheme == "about" || c.Scheme == "javascript" {
		return true
	}
	return strings.EqualFold(originKey(b), originKey(c))
}

func originKey(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + host + ":" + port
}

// OriginCounts tallies first- vs third-party events.
type OriginCounts struct {
	FirstParty int
	ThirdParty int
}

// Classify tallies a set of event URLs against the page's base origin.
func Classify(base string, urls []string) OriginCounts {
	var oc OriginCounts
	for _, u := range urls {
		if SameOrigin(base, u) {
			oc.FirstParty++
		} else {
			oc.ThirdParty++
		}
	}
	return oc
}
