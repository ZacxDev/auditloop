package report

import "strings"

// MaxA11yDigestBytes bounds a stored/loaded DOM/a11y digest. The JS producer enforces
// element caps + truncation (~≤16 KiB), but its OUTPUT is UNTRUSTED (page-authored — a
// hostile page can override JSON.stringify to return an oversized payload, which the
// JS try/catch does not guard). Both the capture side (before storing) and the read side
// (loadDigest) treat an over-cap payload as an EMPTY digest → screenshot-only, no drop.
// 256 KiB is generous headroom over the enforced size (cf. the favicon 512 KiB cap).
const MaxA11yDigestBytes = 256 << 10 // 256 KiB

// Element caps for a DOM/a11y digest. These MIRROR the JS producer's hard caps
// (crawler/a11y-digest.js MAX_INTERACTIVE / MAX_FORM / MAX_LANDMARK) and are the
// CONTRACT an externally-supplied (plugin-pushed) digest is validated against, so a
// pushed digest can never be larger than one the crawl path would have produced.
const (
	MaxA11yInteractive  = 40
	MaxA11yFormControls = 30
	MaxA11yLandmarks    = 30
)

// Per-field string caps, mirroring the JS producer's slice() bounds. A pushed digest
// is TRUNCATED to these (never rejected): a truncated selector simply stops matching a
// concrete key, which makes the deterministic gate refute LESS — the safe direction.
const (
	MaxA11ySelectorLen = 200
	MaxA11yNameLen     = 120
	MaxA11yTextLen     = 80
	MaxA11yTokenLen    = 64 // tag / role / type
)

// Label-source values (the closed set the JS producer emits, and the ONE place the
// programmatic-vs-not distinction is defined). A PROGRAMMATIC source means a real,
// screen-reader-announced accessible name exists; placeholder / text-content / none
// do NOT. Consumers (internal/eval's deterministic verify, internal/plugin's pushed-
// digest validation) both key off this set rather than re-deriving it.
const (
	LabelSourceFor            = "for"
	LabelSourceAriaLabel      = "aria-label"
	LabelSourceAriaLabelledBy = "aria-labelledby"
	LabelSourceWrappingLabel  = "wrapping-label"
	LabelSourcePlaceholder    = "placeholder"
	LabelSourceTextContent    = "text-content"
	LabelSourceValue          = "value"
	LabelSourceNone           = "none"
)

// programmaticLabelSources are the sources that yield a real accessible name.
var programmaticLabelSources = map[string]bool{
	LabelSourceFor:            true,
	LabelSourceAriaLabel:      true,
	LabelSourceAriaLabelledBy: true,
	LabelSourceWrappingLabel:  true,
}

// nonProgrammaticLabelSources are the KNOWN sources that do NOT yield a real
// accessible name. Together with programmaticLabelSources they form the closed,
// accepted set (plus "" = unspecified).
var nonProgrammaticLabelSources = map[string]bool{
	LabelSourcePlaceholder: true,
	LabelSourceTextContent: true,
	LabelSourceValue:       true,
	LabelSourceNone:        true,
}

// IsProgrammaticLabelSource reports whether src is a TRUE programmatic label
// association — one a screen reader announces as the element's name (#36). This is
// the ONE definition; the deterministic verify gate refutes an "accessible name is
// missing" finding ONLY off a source in this set. Anything else (including an
// unknown or empty value) is "unknown → refute nothing".
func IsProgrammaticLabelSource(src string) bool {
	return programmaticLabelSources[normalizeLabelSource(src)]
}

// KnownLabelSource reports whether src is in the closed accepted set (either
// programmatic or a known non-programmatic value). "" (unspecified) is accepted —
// pre-#36 digests omit it. An UNKNOWN value is rejected by the pushed-digest
// validator rather than silently treated as non-programmatic.
func KnownLabelSource(src string) bool {
	s := normalizeLabelSource(src)
	return s == "" || programmaticLabelSources[s] || nonProgrammaticLabelSources[s]
}

func normalizeLabelSource(src string) string {
	return strings.ToLower(strings.TrimSpace(src))
}

// ConcreteKeys returns the UNIQUE concrete anchor keys for a selector — only its id
// (`#id`, matching a digest selector like `input#id`) and/or `[name=…]`. A bare tag or
// class selector yields NO key: it is not specific enough to soundly anchor a
// deterministic drop (two elements can share `a.card`).
//
// This is the ONE definition, deliberately. Two consumers must agree exactly:
//   - internal/eval's deterministic gate, which indexes digest facts under these keys
//     and OR-merges facts that land on the same key;
//   - internal/plugin's ingest validation, which rejects a selector appearing twice
//     with conflicting label facts.
//
// If the two normalised differently, a conflict could pass ingest as "two different
// selectors" (`input#e` vs `#e`) and then collapse onto one key in the gate, where the
// permissive merge would license exactly the drop ingest was meant to prevent.
func ConcreteKeys(selector string) []string {
	s := normalizeSelector(selector)
	if s == "" {
		return nil
	}
	var keys []string
	if id := extractID(s); id != "" {
		keys = append(keys, "#"+id)
	}
	if nm := extractName(s); nm != "" {
		keys = append(keys, "[name="+nm+"]")
	}
	return dedupeKeys(keys)
}

// normalizeSelector lowercases and trims a selector and drops descendant-combinator
// noise, keeping the LAST simple selector (the target element). Intentionally minimal:
// selectors are only ever compared by their id/name anchor.
func normalizeSelector(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if i := strings.LastIndexAny(s, " >+~"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// extractID pulls the id from "#id" or "tag#id" (stops at the next combinator/attr).
func extractID(s string) string {
	i := strings.IndexByte(s, '#')
	if i < 0 {
		return ""
	}
	id := s[i+1:]
	if j := strings.IndexAny(id, ".#[:"); j >= 0 {
		id = id[:j]
	}
	return strings.TrimSpace(id)
}

// extractName pulls x from [name="x"] / [name=x] / [name='x'].
func extractName(s string) string {
	i := strings.Index(s, "[name=")
	if i < 0 {
		return ""
	}
	rest := s[i+len("[name="):]
	j := strings.IndexByte(rest, ']')
	if j < 0 {
		return ""
	}
	val := strings.Trim(rest[:j], `"'`)
	return strings.TrimSpace(val)
}

func dedupeKeys(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// A11yDigest is the compacted DOM / accessibility-tree snapshot captured per page
// (crawl path, Phase 1) alongside axe. It carries the POSITIVE semantic facts a
// screenshot cannot reveal — resolved accessible names, label associations, element
// roles, focusability — so the persona evaluator stops re-deriving mechanical a11y
// from pixels (the meta-audit's objective false-positive cluster: an sr-only label,
// an <a> that looks like a card, an already-present aria name).
//
// It is stored as an artifact (…/{page_slug}/a11y.json, storage.A11yDigestKey) and
// referenced by pages.a11y_digest_key. All strings are UNTRUSTED (page-authored) →
// stored as escaped JSON, rendered escaped. The JS producer (crawler/a11y-digest.js)
// enforces the caps + truncation; this is the parse contract on the read side.
type A11yDigest struct {
	Interactive  []A11yInteractive `json:"interactive,omitempty"`
	FormControls []A11yFormControl `json:"form_controls,omitempty"`
	Landmarks    []A11yLandmark    `json:"landmarks,omitempty"`
}

// A11yInteractive is one visible interactive element (link/button/control/ARIA
// widget) with its computed accessible name and keyboard-operability.
//
// LabelSource records HOW the accessible name was derived (#36) — the same closed
// set as A11yFormControl.LabelSource, now exposed on EVERY interactive element (not
// just form controls) so the deterministic verify can SOUNDLY refute an
// accessible-name-absence finding on a button/link too. A value in the PROGRAMMATIC
// set (for | aria-label | aria-labelledby | wrapping-label) means a real,
// screen-reader-announced name exists; text-content / placeholder / none do NOT (an
// icon <button>★</button> reports "text-content", never a programmatic label). Empty
// on pre-Phase-2 digests (omitempty) → treated as non-programmatic, so the gate
// simply never refutes off it — backward compatible.
type A11yInteractive struct {
	Tag            string `json:"tag"`
	Role           string `json:"role,omitempty"`
	AccessibleName string `json:"accessible_name,omitempty"`
	Selector       string `json:"selector"`
	Type           string `json:"type,omitempty"`
	Disabled       bool   `json:"disabled,omitempty"`
	Focusable      bool   `json:"focusable"`
	LabelSource    string `json:"label_source,omitempty"` // for|aria-label|aria-labelledby|wrapping-label|placeholder|text-content|none
}

// A11yFormControl is one form control with its label association. HasLabel is true
// only for a PROGRAMMATIC label (for | aria-label | aria-labelledby | wrapping
// <label>) — a placeholder-only control is HasLabel=false, LabelSource="placeholder"
// (which is exactly the axe "label" violation case, so it is NOT treated as labelled).
type A11yFormControl struct {
	Selector       string `json:"selector"`
	AccessibleName string `json:"accessible_name,omitempty"`
	HasLabel       bool   `json:"has_label"`
	LabelSource    string `json:"label_source"` // for|aria-label|aria-labelledby|wrapping-label|placeholder|none
}

// A11yLandmark is one landmark region or heading in document order (structural
// scaffolding for the persona's comprehension/ordering reasoning).
type A11yLandmark struct {
	Tag  string `json:"tag"`
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
}

// IsEmpty reports whether the digest carries no semantic facts. Callers gate the
// authoritative-DOM behavior on a NON-empty digest (a pre-0060 or pushed run has no
// digest → the evaluator degrades to screenshot-only, and the deterministic verify
// never drops a finding).
func (d *A11yDigest) IsEmpty() bool {
	if d == nil {
		return true
	}
	return len(d.Interactive) == 0 && len(d.FormControls) == 0 && len(d.Landmarks) == 0
}
