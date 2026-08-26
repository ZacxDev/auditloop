package crawler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/chromedp/chromedp"
)

// a11yDigestFixture serves a page exercising every digest classification: an sr-only
// <label for>-associated input (label present but visually hidden — invisible to a
// screenshot), an aria-labelled button, an <a> styled as a card (focusable link), and
// a NEGATIVE control (an input with neither label nor placeholder).
func a11yDigestFixture() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Digest</title>
<style>.sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0,0,0,0)}</style></head>
<body>
<h1>Create your campaign</h1>
<form>
  <label for="campaign-name-input" class="sr-only">Campaign name</label>
  <input id="campaign-name-input" name="campaign_name" type="text">
  <input id="orphan-field" name="orphan" type="text">
  <button id="save-btn" type="button" aria-label="Save campaign">Save</button>
</form>
<a id="client-card-1" class="client-card" href="/clients/acme">Open client Acme</a>
<button id="star-btn" type="button">★</button>
</body></html>`))
	})
	return httptest.NewServer(mux)
}

// TestBuildA11yDigest (chromium-gated) runs the vendored a11y-digest.js against a
// loopback fixture and asserts the semantic facts a screenshot cannot reveal: an
// sr-only <label for>-associated input resolves an accessible name via labelSource
// "for" (hasLabel=true), an aria-labelled button carries its aria name, an <a> card is
// tag=a + focusable, and an unlabelled input is honestly hasLabel=false/source=none.
func TestBuildA11yDigest(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := a11yDigestFixture()
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
	var d report.A11yDigest
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("digest is not valid JSON: %v\n%s", err, raw)
	}
	if d.IsEmpty() {
		t.Fatalf("digest is empty:\n%s", raw)
	}

	// The <a> card → tag=a, focusable=true (kills the "not keyboard-operable" FP #1).
	var card *report.A11yInteractive
	for i := range d.Interactive {
		if d.Interactive[i].Selector == "a#client-card-1" {
			card = &d.Interactive[i]
		}
	}
	if card == nil {
		t.Fatalf("digest missing the <a> card: %+v", d.Interactive)
	}
	if card.Tag != "a" || !card.Focusable {
		t.Errorf("card should be tag=a, focusable=true; got %+v", card)
	}
	if card.AccessibleName != "Open client Acme" {
		t.Errorf("card accessible name = %q, want \"Open client Acme\"", card.AccessibleName)
	}

	// The aria-labelled button → its aria name (kills the "add aria-label" FP #13).
	var btn *report.A11yInteractive
	for i := range d.Interactive {
		if d.Interactive[i].Selector == "button#save-btn" {
			btn = &d.Interactive[i]
		}
	}
	if btn == nil || btn.AccessibleName != "Save campaign" {
		t.Errorf("button should resolve aria-label \"Save campaign\"; got %+v", btn)
	}

	// Form controls: the sr-only <label for> input resolves a name via source "for"
	// (kills the "no <label>" FP #11); the orphan input is unlabelled.
	fc := map[string]report.A11yFormControl{}
	for _, c := range d.FormControls {
		fc[c.Selector] = c
	}
	named, ok := fc["input#campaign-name-input"]
	if !ok {
		t.Fatalf("digest missing the labelled input; form controls: %+v", d.FormControls)
	}
	if !named.HasLabel || named.LabelSource != "for" || named.AccessibleName != "Campaign name" {
		t.Errorf("sr-only labelled input should be hasLabel=true, labelSource=for, name=\"Campaign name\"; got %+v", named)
	}
	// Negative control: the orphan input has neither label nor placeholder.
	orphan, ok := fc["input#orphan-field"]
	if !ok {
		t.Fatalf("digest missing the orphan input; form controls: %+v", d.FormControls)
	}
	if orphan.HasLabel || orphan.LabelSource != "none" {
		t.Errorf("orphan input should be hasLabel=false, labelSource=none; got %+v", orphan)
	}
}

// TestBuildA11yDigestInteractiveLabelSource (chromium-gated, #36) asserts the
// label_source field now emitted on EVERY interactive element, distinguishing a real
// programmatic label from an element's own text content. This is the structural signal
// the deterministic verify keys on to soundly refute (or NOT refute) a name-absence
// finding on a button/link.
func TestBuildA11yDigestInteractiveLabelSource(t *testing.T) {
	chromium := resolveChromiumT(t)
	fx := a11yDigestFixture()
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
	var d report.A11yDigest
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("digest is not valid JSON: %v\n%s", err, raw)
	}
	bySel := map[string]report.A11yInteractive{}
	for _, e := range d.Interactive {
		bySel[e.Selector] = e
	}

	// An aria-labelled button → PROGRAMMATIC source "aria-label" (a real name → refutable).
	if e := bySel["button#save-btn"]; e.LabelSource != "aria-label" {
		t.Errorf("aria-labelled button label_source = %q, want aria-label; %+v", e.LabelSource, e)
	}
	// An <a> whose name is its link text → "text-content" (NOT programmatic — a link is
	// still keyboard-operable, but its NAME is not a programmatic label).
	if e := bySel["a#client-card-1"]; e.LabelSource != "text-content" {
		t.Errorf("<a> card label_source = %q, want text-content; %+v", e.LabelSource, e)
	}
	// An icon <button>★</button> → "text-content", NOT a programmatic label. This is the
	// exact case the keyword heuristic could not tell from a real label (#36).
	if e := bySel["button#star-btn"]; e.LabelSource != "text-content" {
		t.Errorf("icon button label_source = %q, want text-content (NOT a programmatic label); %+v", e.LabelSource, e)
	}
	// Cross-check the sr-only <label for> input surfaces "for" on the interactive list too
	// (inputs appear in BOTH lists; the interactive-list source must agree with the JS
	// producer's programmatic set).
	if e := bySel["input#campaign-name-input"]; e.LabelSource != "for" {
		t.Errorf("sr-only labelled input interactive label_source = %q, want for; %+v", e.LabelSource, e)
	}
}
