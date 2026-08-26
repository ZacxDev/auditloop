package crawler

import "testing"

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"https://x.test/a#frag": "https://x.test/a",
		" https://x.test/b ":    "https://x.test/b",
		"ftp://x.test/c":        "",
		"not a url":             "",
		"":                      "",
	}
	for in, want := range cases {
		if got := canonicalURL(in); got != want {
			t.Errorf("canonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostAllowedIn(t *testing.T) {
	hosts := []string{"x.test"}
	if !hostAllowedIn("https://x.test/a", hosts) {
		t.Error("x.test should be allowed")
	}
	if !hostAllowedIn("https://www.x.test/a", hosts) {
		t.Error("subdomain should be allowed")
	}
	if hostAllowedIn("https://y.test/a", hosts) {
		t.Error("y.test should not be allowed")
	}
}

func TestAllowLoopbackExceptionOnlyLoopback(t *testing.T) {
	// AllowLoopback permits 127.0.0.1 but must STILL block other private ranges.
	g := GuardConfig{AllowedHosts: []string{"127.0.0.1", "10.0.0.5"}, AllowLoopback: true}
	if err := g.CheckURL("http://127.0.0.1:8080/"); err != nil {
		t.Errorf("loopback should be allowed under AllowLoopback: %v", err)
	}
	if err := g.CheckURL("http://10.0.0.5/"); err == nil {
		t.Error("private (non-loopback) must stay blocked even under AllowLoopback")
	}
}
