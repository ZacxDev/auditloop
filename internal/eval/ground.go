package eval

import (
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"
)

// SELECTOR GROUNDING — the second half of the deterministic DOM gate.
//
// dropContradicted (verify.go) drops a finding only when the digest POSITIVELY
// REFUTES it, and it can only do that through a UNIQUE CONCRETE anchor (#id /
// [name=…]). Measured on a real pushed run (civitai-manager-funnel, run 9d322473):
// 4 objective a11y claims in, 4 survived — every one cited a placeholder attribute,
// a class, or an invented pseudo-selector (`h3:contains('SDXL Portrait')`), so the
// gate had nothing to look up and the grounding was DECORATIVE: the digest reached
// the prompt, and nothing downstream depended on it.
//
// The evaluator reasons from PIXELS. It cannot know an element's real id, so left to
// itself it invents an anchor — and an invented anchor is unverifiable by
// construction. This file closes that: the prompt now tells the model the digest's
// selector list is the ONLY vocabulary it may cite for a MECHANICAL a11y claim
// (missing label / accessible name / keyboard operability), and groundSelectors
// ENFORCES it deterministically:
//
//	listed verbatim (or by its concrete anchor) → keep unchanged
//	not listed, but uniquely re-anchorable by the accessible name the finding quotes
//	                                       → REWRITE the selector to the digest's own
//	otherwise                              → DROP as ungrounded
//
// WHY DROPPING IS SOUND HERE (and why it is not "absence-inference", which
// dropContradicted rightly refuses): dropContradicted asks "is the CLAIM false?" —
// an absent selector can never answer that. This asks a different, answerable
// question: "is this claim CHECKABLE at all?" A mechanical a11y assertion about an
// element the authoritative DOM snapshot does not contain cannot be confirmed,
// refuted, or acted on by whoever reads the report — and mechanical a11y is ALREADY
// measured deterministically by axe on the same tree and shown separately. So the
// cost of dropping is a duplicate of a signal we already have; the benefit is that
// every surviving mechanical claim points at a real element.
//
// THE TRUNCATION GUARD IS LOAD-BEARING. The digest is CAPPED (≤40 interactive / ≤30
// form controls / ≤30 landmarks). On a page that hits a cap the list is a PREFIX of
// the page, so "not listed" stops meaning "not on the page" and dropping would
// silently discard real findings. When any list is at its cap the digest is treated
// as INCOMPLETE: re-anchoring still runs, dropping does NOT.
//
// SCOPE, precisely: only findings the existing narrow classifiers (claimsMissingLabel
// / claimsNotOperable) call MECHANICAL a11y claims are touched. Subjective/visible-UX
// findings — the evaluator's actual job — pass through with whatever selector the
// model wrote, exactly as before.
//
// ACCEPTED FALSE NEGATIVE (deliberate, documented): a mechanical a11y claim about an
// element the digest does not list — a non-interactive one (an <img> alt), or one
// hidden at capture — is dropped even if true. axe covers those classes
// deterministically. The alternative is the status quo: keeping unverifiable claims
// that measured ~20% precision.

// applyDOMGate is THE deterministic DOM/a11y gate as production runs it, and the one
// definition of its two stages' ORDER. groundSelectors must run FIRST: a finding it
// re-anchors onto the digest's own selector then gets the refutation check against the
// right element (a placeholder-cited input rewritten to `input#email` is exactly the
// one dropContradicted can now refute). Run the other way round, refutation sees the
// invented anchor, finds nothing, and the re-anchor is wasted.
//
// It exists as a function rather than two lines in the caller because the order is a
// LOAD-BEARING claim: with the sequence inlined in generator.go, tests that composed
// the stages themselves passed happily with the two swapped — they were pinning a copy
// of the pipeline, not the pipeline. Every caller and every test goes through here.
func applyDOMGate(pe report.PageEvaluation, d *report.A11yDigest) report.PageEvaluation {
	return dropContradicted(groundSelectors(pe, d), d)
}

// groundSelectors re-anchors or drops mechanical a11y findings whose selector is not
// one the digest actually lists. It is a no-op when the digest is empty/absent (the
// backward-compat guard: pre-0060 and digest-less pushed runs behave exactly as
// before) and never mutates the input's slices in place.
func groundSelectors(pe report.PageEvaluation, d *report.A11yDigest) report.PageEvaluation {
	if d.IsEmpty() {
		return pe
	}
	idx := buildGroundIndex(d)
	pe.Blockers = groundFindings(pe.Blockers, idx)
	pe.Frictions = groundFindings(pe.Frictions, idx)
	if pe.TopFix != nil {
		sel, keep := idx.ground(topFixAsFinding(*pe.TopFix))
		switch {
		case !keep:
			pe.TopFix = nil
		case sel != pe.TopFix.Selector:
			tf := *pe.TopFix
			tf.Selector = sel
			pe.TopFix = &tf
		}
	}
	return pe
}

func groundFindings(in []report.EvalFinding, idx groundIndex) []report.EvalFinding {
	if len(in) == 0 {
		return in
	}
	out := make([]report.EvalFinding, 0, len(in))
	for _, f := range in {
		sel, keep := idx.ground(f)
		if !keep {
			continue
		}
		f.Selector = sel
		out = append(out, f)
	}
	return out
}

// groundIndex is the digest's selector vocabulary plus the name→selector map used to
// re-anchor a finding that quoted an element's visible/accessible name instead.
type groundIndex struct {
	listed map[string]bool // normalised VERBATIM digest selectors
	keys   map[string]bool // concrete anchors (#id / [name=…]) the digest carries
	// byName maps an element's normalised accessible name to the digest's OWN,
	// UNMODIFIED selector string. It must be the raw one: an id / [name=…] match is
	// CASE-SENSITIVE in the DOM, so handing back a normalised (lowercased) copy would
	// emit `input#useremail` for a real `input#userEmail` — an anchor that matches
	// nothing, which is worse than the invented one it replaced.
	byName map[string]string
	// complete reports that absence from this index is INFORMATIVE. Two conditions,
	// and BOTH are load-bearing:
	//   (a) no list is at its cap — at a cap the digest is a PREFIX of the page;
	//   (b) the selector VOCABULARY is non-empty — a digest can be non-empty
	//       (A11yDigest.IsEmpty is false) while carrying ONLY landmarks, e.g. an
	//       error page, a confirmation screen, a page whose element queries matched
	//       nothing, or an externally-pushed digest with just one landmark (the push
	//       validator rejects only an ALL-empty digest). Without (b) such a page
	//       asserts "there are no interactive elements here" and the gate becomes a
	//       BLANKET DELETE of every mechanical a11y finding on it — the exact
	//       over-drop this design calls worse than the FP it fixes.
	complete bool
}

func buildGroundIndex(d *report.A11yDigest) groundIndex {
	idx := groundIndex{
		listed: map[string]bool{},
		keys:   map[string]bool{},
		byName: map[string]string{},
	}
	// A name seen on more than one element is AMBIGUOUS and must never re-anchor
	// (rewriting to the wrong element would move a finding onto an innocent one).
	// "" marks an ambiguous name so a later duplicate can't resurrect it.
	claim := func(name, selector string) {
		n := normalizeName(name)
		if n == "" || selector == "" {
			return
		}
		if prev, seen := idx.byName[n]; seen && prev != selector {
			idx.byName[n] = ""
			return
		}
		idx.byName[n] = selector
	}
	add := func(selector, name string) {
		s := normalizeSel(selector)
		if s == "" {
			return
		}
		idx.listed[s] = true
		for _, k := range concreteKeys(selector) {
			idx.keys[k] = true
		}
		claim(name, strings.TrimSpace(selector))
	}
	for _, e := range d.Interactive {
		add(e.Selector, e.AccessibleName)
	}
	for _, c := range d.FormControls {
		add(c.Selector, c.AccessibleName)
	}
	idx.complete = digestVocabularyComplete(d)
	return idx
}

// digestVocabularyComplete reports whether absence from this digest's selector
// vocabulary is INFORMATIVE — the single predicate behind BOTH the gate's licence to
// drop (groundIndex.complete) and the prompt's decision to emit selectorRule. They are
// one function on purpose: the prompt tells the model "a claim about an unlisted
// element is discarded automatically", so any page where the prompt says that and the
// gate does not enforce it is a page where the model self-censors for nothing. An
// earlier revision used two separate conditions and disagreed on every capped page.
//
// The landmark cap is deliberately part of it even though landmarks contribute no
// selectors: a landmark list at its cap means a LARGE page, where the element lists are
// the likeliest to have been truncated in ways the counts alone do not show. It errs
// toward not dropping, which is this design's standing direction.
//
// It is UNEXPORTED deliberately: both call sites are in this package, and a predicate
// whose meaning is "the gate is allowed to delete findings" is not one to hand out.
func digestVocabularyComplete(d *report.A11yDigest) bool {
	if d == nil {
		return false
	}
	if len(d.Interactive) >= report.MaxA11yInteractive ||
		len(d.FormControls) >= report.MaxA11yFormControls ||
		len(d.Landmarks) >= report.MaxA11yLandmarks {
		return false
	}
	for _, e := range d.Interactive {
		if normalizeSel(e.Selector) != "" {
			return true
		}
	}
	for _, c := range d.FormControls {
		if normalizeSel(c.Selector) != "" {
			return true
		}
	}
	return false
}

// ground returns the selector to keep for f and whether f survives. A finding that
// is not a mechanical a11y claim is returned untouched.
func (idx groundIndex) ground(f report.EvalFinding) (string, bool) {
	if !claimsMissingLabel(f) && !claimsNotOperable(f) {
		return f.Selector, true
	}
	if idx.isListed(f.Selector) {
		return f.Selector, true
	}
	if sel, ok := idx.reanchor(f.Selector); ok {
		metrics.EvalFindingsGated.WithLabelValues("reanchored").Inc()
		return sel, true
	}
	if !idx.complete {
		// The digest is capped-out, so "not listed" does not mean "not on the page".
		return f.Selector, true
	}
	metrics.EvalFindingsGated.WithLabelValues("ungrounded").Inc()
	return "", false
}

// isListed reports whether the selector names an element the digest carries — either
// verbatim (modulo case/whitespace) or via a concrete anchor the digest indexes, so
// `#subscribe-q` matches a digest entry written `input#subscribe-q`. An EMPTY
// selector is never listed: an unanchored mechanical a11y claim is exactly the
// unverifiable, pixel-derived class this gate exists to remove.
func (idx groundIndex) isListed(selector string) bool {
	s := normalizeSel(selector)
	if s == "" {
		return false
	}
	if idx.listed[s] {
		return true
	}
	for _, k := range concreteKeys(selector) {
		if idx.keys[k] {
			return true
		}
	}
	return false
}

// reanchor tries to map an invented selector onto a real digest selector using the
// TEXT the model quoted inside it — `input[placeholder='Email address']`,
// `h3:contains('SDXL Portrait')`, `button[aria-label="Close"]`. It rewrites ONLY on
// an UNAMBIGUOUS hit (a name carried by exactly one digest element); anything else
// leaves the finding to the drop rule. This preserves a TRUE finding whose only sin
// was the anchor — without it, a genuinely unlabelled input cited by its placeholder
// would be discarded along with the false ones.
func (idx groundIndex) reanchor(selector string) (string, bool) {
	for _, lit := range quotedLiterals(selector) {
		n := normalizeName(lit)
		if n == "" {
			continue
		}
		if sel := idx.byName[n]; sel != "" {
			return sel, true
		}
		// A model quoting a long name routinely truncates it ("Search by name, tag,
		// ..."), so fall back to a PREFIX match — but only when exactly one digest
		// name is involved. An ambiguous name (value "") anywhere in the match set
		// aborts: re-anchoring onto the wrong element is worse than not re-anchoring.
		//
		// A prefix needs MUCH more evidence than an exact match: "Promo" is a prefix
		// of "Promotional discount code for returning buyers" and of any number of
		// unrelated names, so a short prefix silently walks a finding onto a stranger.
		// A genuinely truncated quote is long by construction, hence minPrefixMatch.
		//
		// The evidence is the SHARED prefix, i.e. the SHORTER of the two strings — so
		// BOTH must clear the bar. Measuring only the model's literal leaves the mirror
		// image of the same bug open in the other direction: a digest element merely
		// named "Save" (4 runes) would absorb a finding quoting "Save these changes and
		// publish now", which is precisely the misattribution this bar exists to stop.
		if utf8.RuneCountInString(n) < minPrefixMatch {
			continue
		}
		var hit string
		for name, sel := range idx.byName {
			// Below the bar this name is not a candidate at all — so it neither matches
			// NOR aborts. That is a deliberate behaviour change: a short AMBIGUOUS name
			// ("Save" on two elements) used to veto a long unique prefix match, but a
			// 26-rune quote cannot be a truncation of a 4-rune name, so the veto was
			// noise. Only candidates that clear the evidence bar get a vote.
			if utf8.RuneCountInString(name) < minPrefixMatch {
				continue
			}
			if !strings.HasPrefix(name, n) && !strings.HasPrefix(n, name) {
				continue
			}
			if sel == "" || (hit != "" && hit != sel) {
				hit = ""
				break
			}
			hit = sel
		}
		if hit != "" {
			return hit, true
		}
	}
	return "", false
}

// nameBearingAttrs are the attributes whose VALUE can plausibly be the element's
// accessible name — the only ones a quoted literal may be matched against.
//
// Harvesting EVERY quoted literal is unsound: `input[type="submit"]` re-anchored onto
// an unrelated `button#newsletter-submit` purely because that button's accessible name
// is "Submit". The value of type / class / id / name / href / role / data-* is an
// IDENTIFIER, not a name, and matching it against a name index moves a finding onto an
// innocent element — the precise hazard the ambiguity check cannot see, because it only
// notices two DIGEST elements sharing a name, never that the literal was never a name.
var nameBearingAttrs = map[string]bool{
	"placeholder":      true,
	"aria-label":       true,
	"title":            true,
	"alt":              true,
	"value":            true,
	"label":            true,
	"aria-placeholder": true,
}

// quotedLiterals extracts the human-readable TEXT a selector carries: the value of a
// NAME-BEARING attribute (`[placeholder="Email address"]`) and the argument of a text
// pseudo-class (`:contains('SDXL Portrait')`). Identifier-valued attributes are
// deliberately skipped — see nameBearingAttrs.
func quotedLiterals(selector string) []string {
	var out []string
	// Attribute selectors: scan `[` … `]` and keep only name-bearing attributes.
	rest := selector
	for {
		i := strings.IndexByte(rest, '[')
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		j := strings.IndexByte(rest, ']')
		if j < 0 {
			break
		}
		attr := rest[:j]
		rest = rest[j+1:]
		eq := strings.IndexByte(attr, '=')
		if eq < 0 {
			continue
		}
		// `[aria-label*="Close"]` / `[title^='Save']` — strip the match operator.
		name := strings.ToLower(strings.TrimSpace(strings.TrimRight(attr[:eq], "*^$~|")))
		if !nameBearingAttrs[name] {
			continue
		}
		if v := strings.TrimSpace(attr[eq+1:]); v != "" {
			out = append(out, strings.Trim(v, `"'`))
		}
	}
	// Text pseudo-classes, the other place a model puts visible text.
	for _, fn := range []string{":contains(", ":has-text(", ":text("} {
		s := selector
		for {
			i := strings.Index(s, fn)
			if i < 0 {
				break
			}
			s = s[i+len(fn):]
			j := strings.IndexByte(s, ')')
			if j < 0 {
				break
			}
			out = append(out, strings.Trim(strings.TrimSpace(s[:j]), `"'`))
			s = s[j+1:]
		}
	}
	return dedupe(out)
}

// normalizeSel is the comparison form for a whole selector string: lowercased,
// whitespace-collapsed, with quote style normalised so `[name="q"]` and `[name='q']`
// compare equal.
func normalizeSel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, `'`, `"`)
	return strings.Join(strings.Fields(s), " ")
}

// normalizeName is the comparison form for an accessible name / quoted literal:
// lowercased, whitespace-collapsed, with a trailing ellipsis or comma stripped (a
// model quoting a long placeholder routinely truncates it as "Search by name, tag,
// ..."). Names shorter than minNameMatch are ignored — a two-character literal is
// not evidence of identity. The length is counted in RUNES, not bytes: a 2-character
// CJK name is 6 bytes and would otherwise clear the bar it exists to set.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, " .,…")
	if utf8.RuneCountInString(s) < minNameMatch {
		return ""
	}
	return s
}

const (
	// minNameMatch is the shortest quoted literal allowed to re-anchor a finding on an
	// EXACT name match (an exact hit on a unique name is strong evidence on its own).
	minNameMatch = 4
	// minPrefixMatch is the far higher bar for a PREFIX match, which is weak evidence:
	// a short prefix matches unrelated names ("Promo" ⊂ "Promotional discount code for
	// returning buyers"). A genuinely model-truncated quote is long by construction.
	minPrefixMatch = 12
)
