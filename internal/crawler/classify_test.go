package crawler

import "testing"

func TestSameOrigin(t *testing.T) {
	base := "https://shop.example.com/products"
	cases := []struct {
		candidate string
		same      bool
	}{
		{"https://shop.example.com/api/cart", true},
		{"https://shop.example.com:443/x", true}, // default port equivalence
		{"http://shop.example.com/x", false},     // scheme differs
		{"https://cdn.example.com/lib.js", false},
		{"https://google-analytics.com/g", false},
		{"https://shop.example.com:8443/x", false}, // explicit non-default port
		{"/relative/path", true},
		{"data:image/png;base64,AAAA", true},
		{"blob:https://shop.example.com/uuid", true},
		{"javascript:void(0)", true},
	}
	for _, c := range cases {
		if got := SameOrigin(base, c.candidate); got != c.same {
			t.Errorf("SameOrigin(%q) = %v, want %v", c.candidate, got, c.same)
		}
	}
}

func TestClassify(t *testing.T) {
	base := "https://app.test"
	urls := []string{
		"https://app.test/main.js",
		"https://app.test/api/data",
		"https://cdn.jsdelivr.net/x.js",
		"https://www.google-analytics.com/collect",
		"data:text/css,body{}",
	}
	oc := Classify(base, urls)
	if oc.FirstParty != 3 { // two app.test + one data:
		t.Errorf("first party = %d, want 3", oc.FirstParty)
	}
	if oc.ThirdParty != 2 {
		t.Errorf("third party = %d, want 2", oc.ThirdParty)
	}
}
