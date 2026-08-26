package crawler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/plugin"
	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/chromedp/chromedp"
)

// ordinaryPageFixture is deliberately BORING markup — the kind any real site emits —
// chosen to exercise the element names a11y-digest.js actually yields once you remember
// its queries include `[role=button]`, `[role=link]` and `[contenteditable=true]`, which
// match ANY element:
//
//   - <i class="fa" role="button">      → tag "i"      (everyday icon-font markup)
//   - <address role="contentinfo">      → tag "address"
//   - <span role="link">                → tag "span"
//   - <div contenteditable="true">      → tag "div"
//   - <figure>/<figcaption>/<code>/<blockquote>/<dialog> → landmark/heading sweep
//
// An earlier revision of the pushed-digest validator carried a closed allowlist of
// "known interactive tags"; this page rejected with `invalid tag "figure"`, which would
// have 400'd — and therefore DISCARDED — a whole multi-page push from a faithful
// producer over one icon button.
func ordinaryPageFixture() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Ordinary</title></head>
<body>
<h1>Product detail</h1>
<nav aria-label="Breadcrumb"><a href="/">Home</a></nav>
<i class="fa fa-star" role="button" tabindex="0" aria-label="Favourite this item">&#9733;</i>
<span id="more-link" role="link" tabindex="0">Read more</span>
<div id="notes" contenteditable="true" aria-label="Your notes">Notes here</div>
<figure><img src="/x.png" alt="A product photo"><figcaption>Product photo</figcaption></figure>
<blockquote>A quote.</blockquote>
<code>npm install</code>
<dialog open><p>Hi</p></dialog>
<address role="contentinfo">1 Example Street</address>
<form><label for="qty">Quantity</label><input id="qty" name="qty" type="number"></form>
</body></html>`))
	})
	return httptest.NewServer(mux)
}

// TestRealA11yDigestPassesPushValidation (chromium-gated) is the regression test for the
// allowlist defect: it captures a GENUINE digest by running the vendored a11y-digest.js
// in a real browser against ordinary markup, then feeds those EXACT bytes through the
// pushed-digest validator. A faithful producer — one that ports a11y-digest.js verbatim,
// which is exactly what the contract tells producers to do — must be ACCEPTED.
//
// This closes the loop the unit tests could not: they used hand-written fixtures, so
// they only ever contained tags the author happened to think of.
func TestRealA11yDigestPassesPushValidation(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := ordinaryPageFixture()
	defer fx.Close()
	tabCtx, cleanup := newDriverTab(t, chromium)
	defer cleanup()

	if err := navigate(tabCtx, fx.URL+"/", 20*time.Second); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	var raw string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(a11yDigestSource, &raw)); err != nil {
		t.Fatalf("evaluate a11y-digest.js: %v", err)
	}

	// Sanity: the fixture really did produce the tags that used to be rejected —
	// otherwise this test would pass vacuously on a page that never exercised them.
	var captured report.A11yDigest
	if err := json.Unmarshal([]byte(raw), &captured); err != nil {
		t.Fatalf("captured digest is not valid JSON: %v", err)
	}
	tags := map[string]bool{}
	for _, e := range captured.Interactive {
		tags[strings.ToLower(e.Tag)] = true
	}
	for _, l := range captured.Landmarks {
		tags[strings.ToLower(l.Tag)] = true
	}
	sawUnusual := false
	for _, want := range []string{"i", "span", "address", "figure", "code", "blockquote", "dialog", "figcaption"} {
		if tags[want] {
			sawUnusual = true
			t.Logf("captured digest contains tag %q (previously rejected by the allowlist)", want)
		}
	}
	if !sawUnusual {
		t.Fatalf("fixture produced no unusual tags — the regression would not be exercised; captured tags: %v", tags)
	}

	// THE ASSERTION: the real capture is accepted by the push validator.
	norm, err := plugin.NormalizeA11yDigest([]byte(raw))
	if err != nil {
		t.Fatalf("a GENUINE a11y-digest.js capture was REJECTED by the push validator: %v\n\ndigest:\n%s", err, raw)
	}

	// And the normalised result still carries the facts the gate needs.
	var out report.A11yDigest
	if err := json.Unmarshal(norm, &out); err != nil {
		t.Fatalf("normalised digest is not valid JSON: %v", err)
	}
	if out.IsEmpty() {
		t.Fatal("normalised digest is empty")
	}
	var sawLabelledQty bool
	for _, c := range out.FormControls {
		if strings.Contains(c.Selector, "qty") && c.HasLabel && c.LabelSource == "for" {
			sawLabelledQty = true
		}
	}
	if !sawLabelledQty {
		t.Errorf("normalisation lost the <label for> association: %+v", out.FormControls)
	}
}
