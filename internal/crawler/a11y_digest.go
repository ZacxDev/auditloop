package crawler

import _ "embed"

// a11yDigestSource is the vendored crawl-path A11y/DOM digest script (see
// a11y-digest.js). It is evaluated on the SAME default tab as axe (one extra
// Evaluate, no new tab) and returns a bounded JSON string of the page's semantic
// structure (interactive elements + form-control label associations + a landmark/
// heading skeleton). Capture is best-effort/non-fatal — a failure leaves the page's
// digest empty and the persona evaluator degrades to screenshot-only.
//
//go:embed a11y-digest.js
var a11yDigestSource string
