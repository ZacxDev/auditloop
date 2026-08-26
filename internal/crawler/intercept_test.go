package crawler

import (
	"net"
	"strings"
	"testing"
)

// TestInterceptorChecksNav exercises the runtime request-interception decision
// (checkNav) directly — no browser needed. It proves a redirect target that is a
// literal private/metadata IP, or an allowlisted name that resolves private, is
// blocked, while a public target is allowed.
func TestInterceptorChecksNav(t *testing.T) {
	resolve := func(host string) ([]net.IP, error) {
		switch host {
		case "public.example.com":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "rebind.example.com":
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		default:
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
	}
	ic := &interceptor{guard: GuardConfig{AllowedHosts: []string{"example.com"}, Resolve: resolve}}

	cases := []struct {
		name    string
		url     string
		blocked bool
	}{
		{"literal metadata IP", "http://169.254.169.254/latest/meta-data/", true},
		{"literal private IP", "http://10.1.2.3/", true},
		{"literal loopback IP", "http://127.0.0.1:8080/", true},
		{"dns-rebind to private", "https://rebind.example.com/", true},
		{"public host", "https://public.example.com/x", false},
		{"non-http scheme (data:)", "data:text/html,hi", false},
		{"about:blank", "about:blank", false},
	}
	for _, c := range cases {
		reason := ic.checkNav(c.url)
		got := reason != ""
		if got != c.blocked {
			t.Errorf("%s: checkNav(%q) blocked=%v want %v (reason=%q)", c.name, c.url, got, c.blocked, reason)
		}
	}
}

// TestInterceptorLoopbackException confirms the dev/test loopback escape hatch
// permits loopback but STILL blocks link-local/metadata (so the fixture's own
// loopback works while a malicious metadata redirect stays blocked).
func TestInterceptorLoopbackException(t *testing.T) {
	// The literal IPs are placed in AllowedHosts so the IP guard (not the
	// host-allowlist) is what blocks them — preserving this test's intent.
	ic := &interceptor{guard: GuardConfig{
		AllowedHosts:  []string{"127.0.0.1", "169.254.169.254", "10.0.0.1"},
		AllowLoopback: true,
	}}
	if r := ic.checkNav("http://127.0.0.1:9000/"); r != "" {
		t.Errorf("loopback should be allowed under AllowLoopback, got %q", r)
	}
	if r := ic.checkNav("http://169.254.169.254/latest/meta-data/"); r == "" {
		t.Error("metadata address must stay blocked even under AllowLoopback")
	}
	if r := ic.checkNav("http://10.0.0.1/"); r == "" {
		t.Error("private RFC1918 must stay blocked even under AllowLoopback")
	}
}

// TestInterceptorInternalAllowlist confirms the exact-host internal allowlist
// composes into the RUNTIME redirect-hop guard (checkNav → checkHostIP): the
// allowlisted internal name passes on a private IP, a non-allowlisted private
// host is still blocked, and a redirect to metadata is blocked even when the
// ORIGIN host was allowlisted (HARD ranges never bypassable).
func TestInterceptorInternalAllowlist(t *testing.T) {
	resolve := func(host string) ([]net.IP, error) {
		switch host {
		case "cluster.internal":
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		case "other.internal":
			return []net.IP{net.ParseIP("10.1.2.3")}, nil
		default:
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
	}
	ic := &interceptor{guard: GuardConfig{
		AllowedHosts:       []string{"cluster.internal", "other.internal"},
		Resolve:            resolve,
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
	}}
	// Exact-allowlisted internal host on a private IP → passes (empty reason).
	if r := ic.checkNav("http://cluster.internal/x"); r != "" {
		t.Errorf("allowlisted internal host should pass the redirect guard, got %q", r)
	}
	// A DIFFERENT (non-allowlisted) host on the same private IP → blocked.
	if r := ic.checkNav("http://other.internal/x"); r == "" {
		t.Error("non-allowlisted private host must be blocked by the redirect guard")
	}
	// A redirect hop to metadata is blocked even though cluster.internal is allowlisted
	// (the guard keys on the redirect TARGET's host, and metadata is HARD regardless).
	if r := ic.checkNav("http://169.254.169.254/latest/meta-data/"); r == "" {
		t.Error("redirect to metadata must stay blocked even with an allowlisted origin")
	}
}

// TestCheckNavHostAllowlist is the focused table for the click-triggered
// off-domain-navigation containment fix: the runtime guard (checkNav) now enforces
// the same-domain host-allowlist (hostAllowed) IN ADDITION to the IP-safety check,
// mirroring CheckURL. Before this, a CLICK that followed a *public* off-domain link
// (which never passes through the pre-nav CheckURL) was only IP-guarded and escaped
// same-origin containment (observed live: example.com → iana.org via a click).
func TestCheckNavHostAllowlist(t *testing.T) {
	// per-host resolver so different names can point at different IPs
	resolve := hostResolver(map[string]string{
		"example.com":         "93.184.216.34", // public
		"www.example.com":     "93.184.216.34", // public subdomain
		"iana.org":            "192.0.43.8",    // public, OFF-DOMAIN
		"private.example.com": "10.0.0.5",      // allowed host, resolves private
		"cluster.internal":    "10.1.2.3",      // internal-allow, resolves private
		"other.internal":      "10.1.2.3",      // NOT internal-allow, resolves private
	})
	ic := &interceptor{guard: GuardConfig{
		AllowedHosts:       []string{"example.com", "private.example.com", "cluster.internal"},
		InternalAllowHosts: InternalAllowSet([]string{"cluster.internal"}),
		Resolve:            resolve,
	}}

	cases := []struct {
		name    string
		url     string
		blocked bool
		want    string // substring the reason must contain when blocked
	}{
		{"same-domain public host", "https://example.com/page", false, ""},
		{"subdomain of allowed host", "https://www.example.com/x", false, ""},
		// THE GAP being closed: a public host NOT in the allowlist (a click-nav
		// off-domain) must now be BLOCKED, not merely IP-guarded through.
		{"off-domain public host", "https://iana.org/", true, "allowlist"},
		// Allowed host that resolves private → still blocked by the IP guard.
		{"allowed host resolves private", "https://private.example.com/", true, "private"},
		// InternalAllowHosts relaxation composes: allowlisted AND internal-allow,
		// private IP tolerated.
		{"internal-allow host in allowlist", "http://cluster.internal/x", false, ""},
		// Internal-allow host NOT in AllowedHosts → the same-domain gate wins.
		{"internal-allow host off-domain", "http://other.internal/x", true, "allowlist"},
		// Non-http(s) scheme / empty host → passed through (Chromium handles).
		{"data: scheme", "data:text/html,hi", false, ""},
		{"about:blank", "about:blank", false, ""},
	}
	for _, c := range cases {
		reason := ic.checkNav(c.url)
		got := reason != ""
		if got != c.blocked {
			t.Errorf("%s: checkNav(%q) blocked=%v want %v (reason=%q)", c.name, c.url, got, c.blocked, reason)
			continue
		}
		if c.blocked && !strings.Contains(reason, c.want) {
			t.Errorf("%s: reason=%q want substring %q", c.name, reason, c.want)
		}
	}
}

// TestInterceptorRecords verifies block bookkeeping used to suppress screenshots.
func TestInterceptorRecords(t *testing.T) {
	ic := &interceptor{guard: GuardConfig{}}
	if ic.blockedCount() != 0 {
		t.Fatal("expected no blocks initially")
	}
	if _, ok := ic.lastBlocked(); ok {
		t.Fatal("expected no last block initially")
	}
	ic.record("http://169.254.169.254/", "resolves to link-local/metadata (169.254.169.254)")
	if ic.blockedCount() != 1 {
		t.Fatalf("blockedCount=%d want 1", ic.blockedCount())
	}
	bn, ok := ic.lastBlocked()
	if !ok || !strings.Contains(bn.Reason, "metadata") {
		t.Fatalf("lastBlocked=%+v ok=%v", bn, ok)
	}
}

// TestCheckHostIPReusable confirms the extracted IP-only guard (no allowlist)
// used by the interceptor matches CheckURL's IP behavior.
func TestCheckHostIPReusable(t *testing.T) {
	g := GuardConfig{Resolve: staticResolver("10.0.0.9")}
	if err := g.checkHostIP("internal.example.com"); err == nil {
		t.Error("private-resolving host should be blocked by checkHostIP")
	}
	g2 := GuardConfig{}
	if err := g2.checkHostIP("169.254.169.254"); err == nil {
		t.Error("literal metadata IP should be blocked by checkHostIP")
	}
	g3 := GuardConfig{Resolve: staticResolver("8.8.8.8")}
	if err := g3.checkHostIP("public.example.com"); err != nil {
		t.Errorf("public host should pass checkHostIP: %v", err)
	}
}
