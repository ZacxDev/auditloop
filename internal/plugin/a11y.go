package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/ZacxDev/auditloop/internal/report"
)

// PUSHED DOM/A11Y DIGEST — validation + normalisation of an UNTRUSTED digest.
//
// The crawl + driven paths capture their a11y digest with OUR OWN chromedp
// (crawler/a11y-digest.js), so the eval read side can take its shape on faith. A
// PLUGIN-PUSHED digest is different in kind: it is authored by an external harness
// (every external producer, including auditloop's own self-audit). That matters
// because the digest drives eval.dropContradicted, a gate that DROPS findings — so a
// malformed, buggy, or hostile digest does not merely add noise, it can SUPPRESS REAL
// PROBLEMS. That is the more dangerous failure mode, so the rules below deliberately
// err toward refuting LESS.
//
// TRUST BOUNDARY (honest): a digest can only be pushed with a target's own push token,
// so a target owner lying in a digest only misleads THEIR OWN audit — there is no
// cross-tenant impact. The realistic risk is not malice but a BUGGY producer whose
// wrong digest silently hides findings from its own owner.
//
// The split, deliberately:
//
//   - FAIL-CLOSED ON VALIDATION (reject the whole push, 400, nothing persisted):
//     unparseable JSON, an unknown JSON field, an unknown label_source value, an
//     element list over the crawl-path cap, a payload over the byte cap, a missing
//     selector, or a digest that decodes to nothing. A silently-ignored digest is
//     indistinguishable from a working one — this repo has already been bitten twice by
//     exactly that on the producer side, so a broken digest must be LOUD at ingest.
//
//   - FAIL-OPEN ON THE GATE (accept, but refute nothing off the questionable fact):
//     has_label is NEVER trusted as pushed — it is DERIVED from label_source, so a
//     producer that omits label_source gets "unknown → refute nothing" rather than an
//     unearned contradiction. Over-long strings are truncated rather than rejected (a
//     truncated selector stops matching a concrete key, which again refutes less).
//
// The returned bytes are the RE-SERIALISED canonical digest — what gets stored is a
// digest auditloop produced from validated fields, never the producer's raw bytes.

// NormalizeA11yDigest validates an externally-supplied DOM/a11y digest against the
// SAME contract the crawl path produces and returns canonical, re-serialised bytes to
// store. A violation returns a credential-free error the push handler turns into a 400.
//
// A caller must only invoke this when the push actually referenced a digest file: an
// ABSENT digest is not an error, it is the (byte-for-byte unchanged) legacy behaviour.
func NormalizeA11yDigest(raw []byte) ([]byte, error) {
	if len(raw) > report.MaxA11yDigestBytes {
		return nil, fmt.Errorf("a11y digest is %d bytes, over the %d-byte cap", len(raw), report.MaxA11yDigestBytes)
	}
	// DisallowUnknownFields (recursively) — an unexpected key is a producer bug or an
	// injected field, and is rejected rather than silently dropped. Same rule as Parse.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d report.A11yDigest
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("invalid a11y digest JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid a11y digest JSON: unexpected trailing data")
	}

	// Element caps: a pushed digest may never exceed what the crawl path itself would
	// have captured (the gate's soundness argument assumes a bounded, capped list).
	if n := len(d.Interactive); n > report.MaxA11yInteractive {
		return nil, fmt.Errorf("a11y digest has %d interactive elements (max %d)", n, report.MaxA11yInteractive)
	}
	if n := len(d.FormControls); n > report.MaxA11yFormControls {
		return nil, fmt.Errorf("a11y digest has %d form controls (max %d)", n, report.MaxA11yFormControls)
	}
	if n := len(d.Landmarks); n > report.MaxA11yLandmarks {
		return nil, fmt.Errorf("a11y digest has %d landmarks (max %d)", n, report.MaxA11yLandmarks)
	}

	// progByAnchor tracks each concrete ANCHOR's PROGRAMMATIC-LABEL verdict so a
	// duplicate carrying a CONFLICTING verdict can be rejected. The read-side gate
	// OR-merges facts onto a shared key, which is sound for a self-generated digest (a
	// `#id` is unique by construction) but for producer input would let ONE buggy
	// duplicate license a drop — the only place in the design where "most permissive
	// wins" instead of "ambiguity → refute nothing". Rejecting the conflict at ingest
	// keeps the gate itself untouched.
	//
	// It compares the PROGRAMMATIC-NESS, not the raw string, deliberately: a faithful
	// port of a11y-digest.js legitimately reports the SAME <input> as "text-content" in
	// `interactive` and "none" in `form_controls` (the script collapses a stray text/
	// value source differently per list). Both are non-programmatic, so that is not a
	// conflict and must not 400.
	//
	// It keys on report.ConcreteKeys — the EXACT normalisation the gate indexes under —
	// NOT the raw selector string. Keying on the raw string let `"input#e"` and `"#e"`
	// pass as "two different selectors" and then collapse onto the same `#e` key in the
	// gate, where the OR-merge resolved their conflicting label facts permissively and
	// licensed the very drop this check exists to prevent. A selector with no concrete
	// key is inert for the gate (never indexed, never refutes) and is skipped.
	progByAnchor := map[string]bool{}
	checkProg := func(what string, i int, selector string, prog bool) error {
		for _, k := range report.ConcreteKeys(selector) {
			if prev, seen := progByAnchor[k]; seen && prev != prog {
				return fmt.Errorf("a11y digest %s %d: anchor %s (from selector %q) appears twice with conflicting label_source (programmatic vs not)", what, i, k, selector)
			}
			progByAnchor[k] = prog
		}
		return nil
	}

	for i := range d.Interactive {
		e := &d.Interactive[i]
		if strings.TrimSpace(e.Selector) == "" {
			return nil, fmt.Errorf("a11y digest interactive %d: selector is required", i)
		}
		if !report.KnownLabelSource(e.LabelSource) {
			return nil, fmt.Errorf("a11y digest interactive %d: unknown label_source %q", i, e.LabelSource)
		}
		e.Tag = strings.ToLower(trunc(e.Tag, report.MaxA11yTokenLen))
		if e.Tag != "" && !validTag(e.Tag) {
			return nil, fmt.Errorf("a11y digest interactive %d: invalid tag %q", i, e.Tag)
		}
		e.Role = trunc(e.Role, report.MaxA11yTokenLen)
		e.Type = trunc(e.Type, report.MaxA11yTokenLen)
		e.Selector = trunc(e.Selector, report.MaxA11ySelectorLen)
		e.AccessibleName = trunc(e.AccessibleName, report.MaxA11yNameLen)
		e.LabelSource = strings.ToLower(strings.TrimSpace(e.LabelSource))
		if err := checkProg("interactive", i, e.Selector, report.IsProgrammaticLabelSource(e.LabelSource)); err != nil {
			return nil, err
		}
	}

	for i := range d.FormControls {
		c := &d.FormControls[i]
		if strings.TrimSpace(c.Selector) == "" {
			return nil, fmt.Errorf("a11y digest form_control %d: selector is required", i)
		}
		if !report.KnownLabelSource(c.LabelSource) {
			return nil, fmt.Errorf("a11y digest form_control %d: unknown label_source %q", i, c.LabelSource)
		}
		c.Selector = trunc(c.Selector, report.MaxA11ySelectorLen)
		c.AccessibleName = trunc(c.AccessibleName, report.MaxA11yNameLen)
		c.LabelSource = strings.ToLower(strings.TrimSpace(c.LabelSource))
		if err := checkProg("form_control", i, c.Selector, report.IsProgrammaticLabelSource(c.LabelSource)); err != nil {
			return nil, err
		}
		// has_label is the single most dangerous field in the whole digest: it is what
		// the deterministic gate refutes a "no label" finding off. We do NOT take the
		// producer's word for it — we DERIVE it structurally from label_source (the #36
		// lesson, applied to untrusted input). A producer that sets has_label:true but
		// omits/downgrades label_source therefore refutes NOTHING, instead of silently
		// suppressing a real missing-label finding.
		c.HasLabel = report.IsProgrammaticLabelSource(c.LabelSource)
	}

	for i := range d.Landmarks {
		l := &d.Landmarks[i]
		l.Tag = strings.ToLower(trunc(l.Tag, report.MaxA11yTokenLen))
		if l.Tag != "" && !validTag(l.Tag) {
			return nil, fmt.Errorf("a11y digest landmark %d: invalid tag %q", i, l.Tag)
		}
		l.Role = trunc(l.Role, report.MaxA11yTokenLen)
		l.Text = trunc(l.Text, report.MaxA11yTextLen)
	}

	// A digest that carries no semantic facts would be stored, loaded, and then no-op —
	// i.e. indistinguishable from not sending one at all. Reject it LOUDLY so a producer
	// whose capture silently returned nothing finds out at ingest.
	if d.IsEmpty() {
		return nil, fmt.Errorf("a11y digest is empty (no interactive elements, form controls or landmarks)")
	}

	out, err := json.Marshal(&d)
	if err != nil {
		return nil, fmt.Errorf("could not re-serialise a11y digest: %w", err)
	}
	return out, nil
}

// trunc strips control characters, then bounds a string on a rune boundary so the
// result stays valid UTF-8.
//
// The control-character strip is a SECURITY step, not cosmetics. These strings are
// producer-authored and are rendered into the persona prompt inside a block the prompt
// itself declares AUTHORITATIVE. An embedded newline would let ~200 bytes × up to 70
// elements of attacker-shaped text forge its own instruction lines — a way to steer the
// evaluator that BYPASSES the deterministic gate entirely, which is strictly worse than
// the drop-power this design is otherwise organised around. (The prompt now also %q's
// these fields; this is the belt to that suspenders, and it keeps what we STORE clean.)
func trunc(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' ' // keep word boundaries rather than gluing tokens together
		}
		if isLineBreaking(r) {
			return ' '
		}
		if unicode.IsControl(r) || isBidiOverride(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	b := []rune(s)
	for len(string(b)) > n {
		b = b[:len(b)-1]
	}
	return strings.TrimSpace(string(b))
}

// isLineBreaking reports whether r is a Unicode LINE/PARAGRAPH separator (U+2028 /
// U+2029, categories Zl/Zp). `unicode.IsControl` is category Cc ONLY and does not cover
// these, yet many renderers treat them as newlines — so the control-character strip
// would otherwise be one layer thinner than its own doc claims.
func isLineBreaking(r rune) bool { return r == 0x2028 || r == 0x2029 }

// isBidiOverride reports whether r is a bidirectional embedding/override/isolate
// (category Cf, U+202A–U+202E and U+2066–U+2069). These can visually REORDER stored
// text — e.g. make a selector render as something other than what it matches — so they
// are dropped from what we store. Other Cf codepoints (ZWJ/ZWNJ, U+200C/U+200D) are
// deliberately KEPT: they are legitimate in accessible names (emoji sequences, Indic
// scripts) and carry no reordering or line-forging power.
func isBidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// htmlTagName matches a valid HTML element name TOKEN: an ASCII letter followed by
// letters/digits/hyphens. That is the ENTIRE tag rule — there is deliberately NO
// closed allowlist of element names.
//
// An earlier revision added one and it was WRONG: `a11y-digest.js`'s own queries
// include `[role=button]`, `[role=link]` and `[contenteditable=true]`, which match ANY
// element, so everyday markup like `<i class="fa" role="button">` or
// `<address role="contentinfo">` is NORMAL output — and the landmark/heading sweep
// yields `figure`, `code`, `blockquote`, `dl`, `tr`, `dialog`, `picture`, `video`,
// `svg`/`g`/`path`, … A closed list therefore 400s a FAITHFUL producer and discards the
// whole multi-page push over one `<i role="button">`. It also never had security value:
// any allowlist must CONTAIN `a` and `button`, so it could not stop a producer claiming
// one — what closes that hole is the eval gate requiring the digest's own
// `focusable && !disabled` to agree (internal/eval/verify.go).
//
// The regex still rejects everything the check was actually aimed at — "a b", "<a>",
// "a\nrole=link", "1div" — with zero false rejections.
var htmlTagName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validTag reports whether tag is a well-formed element-name token.
func validTag(tag string) bool { return htmlTagName.MatchString(tag) }
