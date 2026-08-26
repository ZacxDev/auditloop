package crawler

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrBlockedURL is returned when a URL is refused by the safety guard.
type ErrBlockedURL struct {
	URL    string
	Reason string
}

func (e *ErrBlockedURL) Error() string {
	return fmt.Sprintf("blocked url %q: %s", e.URL, e.Reason)
}

// GuardConfig holds the SSRF/abuse guard configuration. AllowedHosts is the set
// of hostnames (lowercased, no port) the crawler is permitted to visit; a host
// is allowed if it equals, or is a subdomain of, any entry. Empty AllowedHosts
// means "no host is allowed" (fail-closed) — callers derive it from the
// target's registered/verified domains.
type GuardConfig struct {
	AllowedHosts []string
	// Resolve overrides DNS resolution (tests inject deterministic answers).
	// When nil, net.LookupIP is used.
	Resolve func(host string) ([]net.IP, error)
	// AllowLoopback permits loopback addresses (127.0.0.0/8, ::1) ONLY. This is
	// a dev/test escape hatch so a hermetic e2e can crawl a local fixture server;
	// it MUST never be enabled in production. All other private/link-local/
	// metadata ranges remain blocked even when true, so the guard stays tested.
	AllowLoopback bool
	// InternalAllowHosts is an EXACT-match set (lowercased hostnames) whose
	// resolution into a SOFT range (private RFC1918/ULA, loopback, or CGNAT) is
	// tolerated — an in-cluster dev target reachable only by an internal name. It
	// NEVER relaxes a HARD range (link-local, cloud metadata 169.254.169.254,
	// multicast, unspecified — see isHardBlocked): those stay blocked for an
	// allowlisted host too. Matching is EXACT (map key), NOT the subdomain-suffix
	// hostAllowed logic, so a DNS-rebind of a DIFFERENT name onto the same private
	// IP is still refused. Empty in every normal deployment. The per-target host
	// allowlist (AllowedHosts) is unaffected — this relaxes ONLY the private-IP half.
	InternalAllowHosts map[string]bool
}

// InternalAllowSet builds the exact-match internal-host allowlist map from a raw
// []string (config), lowercasing + trimming each entry and dropping blanks. A
// nil/empty input yields nil → the guard behaves byte-for-byte as before. Exported
// so handlers building a GuardConfig directly (e.g. the login save-time guard) can
// construct the same set.
func InternalAllowSet(hosts []string) map[string]bool {
	if len(hosts) == 0 {
		return nil
	}
	m := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			m[h] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// CheckURL validates a URL against scheme, host-allowlist, and private/loopback/
// link-local/metadata IP ranges. It returns *ErrBlockedURL on refusal.
//
// This is a REAL guard (not deferred): even for an allowlisted host, every
// resolved IP is checked so a hostname that resolves into a private range
// (DNS-rebinding / cloud-metadata pivot) is refused.
func (g GuardConfig) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &ErrBlockedURL{raw, "unparseable url"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &ErrBlockedURL{raw, "scheme not http(s): " + u.Scheme}
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return &ErrBlockedURL{raw, "empty host"}
	}
	if !g.hostAllowed(host) {
		return &ErrBlockedURL{raw, "host not in allowlist: " + host}
	}
	if err := g.checkHostIP(host); err != nil {
		// Re-wrap with the full raw URL for a clearer message.
		if be, ok := err.(*ErrBlockedURL); ok {
			return &ErrBlockedURL{raw, be.Reason}
		}
		return err
	}
	return nil
}

// checkHostIP resolves host (or parses it if it is already a literal IP) and
// refuses it if ANY resolved address falls in a private/loopback/link-local/
// ULA/metadata range. It does NOT enforce the host allowlist — it is the pure
// IP-safety half of CheckURL, reused by the runtime request-interception guard
// (intercept.go), where a redirect can arrive at any host and only the resolved
// IP matters. A literal private/metadata IP host is blocked without any DNS.
//
// Residual risk (documented, not fully closed here): DNS rebinding is a TOCTOU
// race — Chromium re-resolves the host independently when it actually connects,
// so a name that answers a public IP here but a private IP to the browser a
// moment later could still slip a single request through. Blocking literal
// private/metadata IPs outright + resolving-and-checking on every paused request
// closes the practical exploits; true closure needs connection-level IP pinning
// (out of scope). See intercept.go.
func (g GuardConfig) checkHostIP(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return &ErrBlockedURL{host, "empty host"}
	}
	var ips []net.IP
	if lit := net.ParseIP(host); lit != nil {
		ips = []net.IP{lit}
	} else {
		resolve := g.Resolve
		if resolve == nil {
			resolve = net.LookupIP
		}
		var err error
		ips, err = resolve(host)
		if err != nil {
			return &ErrBlockedURL{host, "dns resolution failed: " + err.Error()}
		}
		if len(ips) == 0 {
			return &ErrBlockedURL{host, "host resolved to no addresses"}
		}
	}
	for _, ip := range ips {
		if reason := g.ipReasonForHost(host, ip); reason != "" {
			return &ErrBlockedURL{host, "resolves to " + reason + " (" + ip.String() + ")"}
		}
	}
	return nil
}

// ipReasonForHost returns "" if ip is permitted for host — either public, or a
// tolerated SOFT-range exception (an exact-allowlisted internal host, or the
// dev/test loopback escape hatch) — else the non-empty block reason. It is the
// single per-IP decision shared by checkHostIP (which loops it over every
// resolved address) and the favicon fetch's IP-pinning dialer (which uses it to
// pick a validated address to connect to, closing the DNS-rebind TOCTOU gap).
func (g GuardConfig) ipReasonForHost(host string, ip net.IP) string {
	reason := blockedIP(ip)
	if reason == "" {
		return "" // public IP
	}
	// Exact-allowlisted internal host on a SOFT (private/loopback/CGNAT) IP is
	// tolerated. HARD ranges (link-local, cloud metadata, multicast, unspecified)
	// are NEVER bypassable — not even for an allowlisted host.
	if !isHardBlocked(reason) && g.InternalAllowHosts[host] {
		return ""
	}
	if g.AllowLoopback && ip.IsLoopback() {
		return "" // dev/test-only loopback exception
	}
	return reason
}

// isHardBlocked reports whether a blockedIP reason names a HARD range — one that
// the InternalAllowHosts exact-host exception must NEVER relax: link-local (incl.
// the cloud-metadata 169.254.169.254 address, both the "link-local" and
// "link-local/metadata" reasons), multicast, and unspecified. The SOFT ranges
// (private, loopback, carrier-grade-nat) return false and MAY be bypassed for an
// exact-allowlisted in-cluster host. Fail-safe: any reason NOT explicitly soft
// (including "invalid ip" or a future range added to blockedIP) is treated as
// HARD, so a new refusal is never silently bypassable by the allowlist.
func isHardBlocked(reason string) bool {
	switch reason {
	case "private", "loopback", "carrier-grade-nat":
		return false
	default:
		return true
	}
}

// hostAllowed reports whether host equals or is a subdomain of an allowed host.
func (g GuardConfig) hostAllowed(host string) bool {
	for _, a := range g.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// blockedIP returns a non-empty reason string if ip falls in a refused range:
// loopback, private (RFC1918 / ULA), link-local (incl. cloud metadata
// 169.254.169.254), unspecified, or multicast. Empty means the IP is public.
func blockedIP(ip net.IP) string {
	if ip == nil {
		return "invalid ip"
	}
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsUnspecified() {
		return "unspecified"
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "link-local" // covers 169.254.0.0/16 (incl. .169.254 metadata) + fe80::/10
	}
	if ip.IsMulticast() {
		return "multicast"
	}
	if ip.IsPrivate() {
		return "private" // RFC1918 10/8, 172.16/12, 192.168/16 + fc00::/7 ULA
	}
	// Explicit belt-and-suspenders for the cloud-metadata address and a few
	// ranges Go's helpers don't all classify as private.
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return "link-local/metadata"
		}
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return "carrier-grade-nat" // 100.64.0.0/10 RFC6598
		}
	}
	return ""
}
