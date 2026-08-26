package crawler

import (
	"net"
	"strings"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		// loopback
		{"127.0.0.1", true},
		{"127.5.5.5", true},
		{"::1", true},
		// private RFC1918
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		// link-local + cloud metadata
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		{"fe80::1", true},
		// ULA v6
		{"fc00::1", true},
		{"fd12:3456::1", true},
		// carrier-grade NAT
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		// unspecified / multicast
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		// public (allowed)
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
		{"2606:2800:220:1::1", false},
		{"172.15.0.1", false},  // just outside 172.16/12
		{"172.32.0.1", false},  // just outside 172.16/12
		{"100.63.0.1", false},  // just outside CGNAT
		{"100.128.0.1", false}, // just outside CGNAT
		{"169.253.0.1", false}, // just outside link-local
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", c.ip)
		}
		got := blockedIP(ip) != ""
		if got != c.blocked {
			t.Errorf("blockedIP(%s) blocked=%v want %v (reason=%q)", c.ip, got, c.blocked, blockedIP(ip))
		}
	}
}

func TestCheckURLScheme(t *testing.T) {
	g := GuardConfig{AllowedHosts: []string{"example.com"}, Resolve: staticResolver("93.184.216.34")}
	for _, raw := range []string{"ftp://example.com", "file:///etc/passwd", "gopher://example.com"} {
		if err := g.CheckURL(raw); err == nil {
			t.Errorf("expected %q to be blocked (scheme)", raw)
		}
	}
}

func TestCheckURLHostAllowlist(t *testing.T) {
	g := GuardConfig{AllowedHosts: []string{"example.com"}, Resolve: staticResolver("93.184.216.34")}
	if err := g.CheckURL("https://example.com/page"); err != nil {
		t.Errorf("example.com should be allowed: %v", err)
	}
	if err := g.CheckURL("https://www.example.com/page"); err != nil {
		t.Errorf("subdomain should be allowed: %v", err)
	}
	if err := g.CheckURL("https://evil.com/page"); err == nil {
		t.Error("evil.com should be blocked (not in allowlist)")
	}
	// A lookalike suffix must NOT match (notexample.com is not a subdomain).
	if err := g.CheckURL("https://notexample.com/"); err == nil {
		t.Error("notexample.com should be blocked")
	}
}

func TestCheckURLEmptyAllowlistFailsClosed(t *testing.T) {
	g := GuardConfig{Resolve: staticResolver("8.8.8.8")}
	if err := g.CheckURL("https://example.com/"); err == nil {
		t.Error("empty allowlist should refuse everything")
	}
}

func TestCheckURLDNSRebind(t *testing.T) {
	// Allowlisted host that resolves into a private range must be refused —
	// this is the SSRF/DNS-rebinding pivot the guard exists for.
	g := GuardConfig{AllowedHosts: []string{"internal.example.com"}, Resolve: staticResolver("10.0.0.5")}
	err := g.CheckURL("https://internal.example.com/")
	if err == nil {
		t.Fatal("expected private-resolving host to be blocked")
	}
	if !strings.Contains(err.Error(), "private") {
		t.Errorf("reason = %v, want private", err)
	}
}

func TestCheckURLLiteralPrivateIP(t *testing.T) {
	g := GuardConfig{AllowedHosts: []string{"169.254.169.254"}}
	if err := g.CheckURL("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("literal metadata IP must be blocked even if allowlisted")
	}
}

func TestCheckURLMultipleIPsOneBad(t *testing.T) {
	// If ANY resolved IP is private, refuse.
	g := GuardConfig{
		AllowedHosts: []string{"example.com"},
		Resolve: func(string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}, nil
		},
	}
	if err := g.CheckURL("https://example.com/"); err == nil {
		t.Error("expected refusal when one of several IPs is private")
	}
}

func staticResolver(ips ...string) func(string) ([]net.IP, error) {
	return func(string) ([]net.IP, error) {
		out := make([]net.IP, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

// --- Internal-host allowlist (AUDITLOOP_INTERNAL_ALLOW_HOSTS) ---
//
// The exact-host soft-relax lets ONE in-cluster name resolve into a soft
// (private/loopback/CGNAT) range, while HARD ranges (link-local/metadata/
// multicast/unspecified) stay blocked for that host too, and the same-domain
// AllowedHosts gate still applies. These are the security invariants.

// hostResolver resolves per-host (unlike staticResolver, which ignores the host)
// so a test can point two different names at the SAME private IP.
func hostResolver(m map[string]string) func(string) ([]net.IP, error) {
	return func(host string) ([]net.IP, error) {
		if s, ok := m[host]; ok {
			if ip := net.ParseIP(s); ip != nil {
				return []net.IP{ip}, nil
			}
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
}

func TestIsHardBlocked(t *testing.T) {
	// Every reason blockedIP can emit must map deterministically; SOFT = bypassable.
	soft := map[string]bool{"private": true, "loopback": true, "carrier-grade-nat": true}
	hard := []string{"link-local", "link-local/metadata", "multicast", "unspecified", "invalid ip", "some-future-range"}
	for r := range soft {
		if isHardBlocked(r) {
			t.Errorf("reason %q should be SOFT (bypassable), got HARD", r)
		}
	}
	for _, r := range hard {
		if !isHardBlocked(r) {
			t.Errorf("reason %q should be HARD (never bypassable), got SOFT", r)
		}
	}
}

// (1) An allowlisted host (also in AllowedHosts) resolving into a private range passes.
func TestInternalAllowlistSoftPrivatePasses(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "192.168.10.20", "172.16.5.5"} {
		g := GuardConfig{
			AllowedHosts:       []string{"cluster.internal"},
			InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
			Resolve:            staticResolver(ip),
		}
		if err := g.CheckURL("http://cluster.internal/health"); err != nil {
			t.Errorf("allowlisted host resolving to %s should PASS, got %v", ip, err)
		}
	}
}

// (2) The SAME private IP via a DIFFERENT (non-allowlisted) host is still blocked —
// exact-host matching, so a rebind of another name to the same IP is refused.
func TestInternalAllowlistExactHostOnly(t *testing.T) {
	resolve := hostResolver(map[string]string{
		"cluster.internal": "10.1.2.3",
		"evil.internal":    "10.1.2.3", // same private IP, different name
	})
	g := GuardConfig{
		AllowedHosts:       []string{"cluster.internal", "evil.internal"},
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
		Resolve:            resolve,
	}
	if err := g.CheckURL("http://cluster.internal/"); err != nil {
		t.Errorf("allowlisted host should pass: %v", err)
	}
	if err := g.CheckURL("http://evil.internal/"); err == nil {
		t.Error("non-allowlisted host on the same private IP must be BLOCKED")
	}
	// A subdomain of the allowlisted host is NOT exact-matched → still blocked.
	resolve2 := hostResolver(map[string]string{"sub.cluster.internal": "10.1.2.3"})
	g2 := GuardConfig{
		AllowedHosts:       []string{"cluster.internal"}, // subdomain passes hostAllowed
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
		Resolve:            resolve2,
	}
	if err := g2.CheckURL("http://sub.cluster.internal/"); err == nil {
		t.Error("a subdomain of the allowlisted host is NOT exact-matched → must be blocked")
	}
}

// (3) HARD ranges are NEVER bypassable, even for an allowlisted host.
func TestInternalAllowlistHardNeverBypassable(t *testing.T) {
	cases := []struct{ name, ip string }{
		{"metadata", "169.254.169.254"},
		{"link-local", "169.254.10.20"},
		{"multicast", "224.0.0.1"},
		{"unspecified", "0.0.0.0"},
	}
	for _, c := range cases {
		g := GuardConfig{
			AllowedHosts:       []string{"cluster.internal"},
			InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
			Resolve:            staticResolver(c.ip),
		}
		if err := g.CheckURL("http://cluster.internal/"); err == nil {
			t.Errorf("%s (%s) must STAY blocked even for an allowlisted host", c.name, c.ip)
		}
	}
}

// (4) Allowlisted but NOT in AllowedHosts → blocked by the same-domain gate; the
// allowlist does not bypass hostAllowed.
func TestInternalAllowlistDoesNotBypassSameDomain(t *testing.T) {
	g := GuardConfig{
		AllowedHosts:       []string{"other.example.com"}, // cluster.internal NOT allowed
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
		Resolve:            staticResolver("10.1.2.3"),
	}
	err := g.CheckURL("http://cluster.internal/")
	if err == nil {
		t.Fatal("host not in AllowedHosts must be blocked despite the internal allowlist")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("expected same-domain allowlist refusal, got %v", err)
	}
}

// (5) CGNAT is soft: allowlisted passes, non-allowlisted blocked.
func TestInternalAllowlistCGNATSoft(t *testing.T) {
	allowed := GuardConfig{
		AllowedHosts:       []string{"cluster.internal"},
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
		Resolve:            staticResolver("100.64.1.2"),
	}
	if err := allowed.CheckURL("http://cluster.internal/"); err != nil {
		t.Errorf("CGNAT allowlisted should pass: %v", err)
	}
	notAllowed := GuardConfig{
		AllowedHosts: []string{"cluster.internal"},
		Resolve:      staticResolver("100.64.1.2"),
	}
	if err := notAllowed.CheckURL("http://cluster.internal/"); err == nil {
		t.Error("CGNAT with an EMPTY internal allowlist must be blocked")
	}
}

// (6) Empty allowlist → every existing case behaves identically (no regression):
// every private/hard IP for an allowlisted-in-AllowedHosts host is still blocked.
func TestInternalAllowlistEmptyNoRegression(t *testing.T) {
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1", "169.254.169.254", "100.64.0.1", "224.0.0.1", "0.0.0.0"} {
		g := GuardConfig{
			AllowedHosts:       []string{"example.com"},
			InternalAllowHosts: InternalAllowSet(nil), // empty
			Resolve:            staticResolver(ip),
		}
		if err := g.CheckURL("https://example.com/"); err == nil {
			t.Errorf("empty allowlist: %s must still be blocked", ip)
		}
	}
	// A public IP still passes (sanity).
	g := GuardConfig{AllowedHosts: []string{"example.com"}, Resolve: staticResolver("8.8.8.8")}
	if err := g.CheckURL("https://example.com/"); err != nil {
		t.Errorf("public IP should pass: %v", err)
	}
}

func TestInternalAllowSetNormalizes(t *testing.T) {
	m := InternalAllowSet([]string{" Cluster.Internal ", "", "  ", "b.local"})
	if !m["cluster.internal"] || !m["b.local"] {
		t.Errorf("expected lowercased/trimmed entries, got %v", m)
	}
	if len(m) != 2 {
		t.Errorf("blanks must be dropped, got %v", m)
	}
	if InternalAllowSet(nil) != nil || InternalAllowSet([]string{"", "  "}) != nil {
		t.Error("empty/all-blank input must yield nil (fully-guarded default)")
	}
}
