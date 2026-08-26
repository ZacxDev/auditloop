package eval

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZacxDev/auditloop/internal/report"
)

// validComprehension is the closed set for the comprehension verdict.
var validComprehension = map[string]bool{"clear": true, "unclear": true, "blocked": true}
var validImpact = map[string]bool{"high": true, "medium": true, "low": true}

// extractJSON pulls the first top-level JSON object out of a model reply,
// tolerating markdown code fences and surrounding prose (models sometimes wrap
// JSON despite the instruction). Returns an error if no object is found.
func extractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	// Strip a leading ```json / ``` fence and any trailing fence.
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		s = strings.TrimSpace(rest)
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return "", fmt.Errorf("no JSON object found")
	}
	return s[start : end+1], nil
}

// ParseEvaluation parses a generation-pass reply into a validated PageEvaluation.
// It is lenient about surrounding prose (extractJSON) but strict about the schema:
// an unparseable body or a comprehension outside the closed set is an error (the
// caller stores it as a per-cell error and continues — never panics).
//
// A per-page verdict is a SINGLE JSON object (not an array), so the array-prefix
// salvage ParseSynthesis uses does not apply — a truncated single object is
// genuinely unparseable. The real fix for truncation is the larger per-page
// completion budget (config.AUDITLOOP_LLM_EVAL_MAX_TOKENS); this parser just
// degrades cleanly (error, no panic) if a body still arrives malformed.
func ParseEvaluation(reply string) (report.PageEvaluation, error) {
	raw, err := extractJSON(reply)
	if err != nil {
		return report.PageEvaluation{}, err
	}
	var pe report.PageEvaluation
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&pe); err != nil {
		return report.PageEvaluation{}, fmt.Errorf("decode evaluation: %w", err)
	}
	pe.Comprehension = strings.ToLower(strings.TrimSpace(pe.Comprehension))
	if !validComprehension[pe.Comprehension] {
		return report.PageEvaluation{}, fmt.Errorf("invalid comprehension %q (want clear|unclear|blocked)", pe.Comprehension)
	}
	pe.Blockers = sanitizeFindings(pe.Blockers)
	pe.Frictions = sanitizeFindings(pe.Frictions)
	pe.TopFix = sanitizeTopFix(pe.TopFix)
	return pe, nil
}

// sanitizeFindings drops empty-issue findings and trims fields (defensive; the
// stored JSON is rendered escaped regardless).
func sanitizeFindings(in []report.EvalFinding) []report.EvalFinding {
	var out []report.EvalFinding
	for _, f := range in {
		f.Issue = strings.TrimSpace(f.Issue)
		f.Selector = strings.TrimSpace(f.Selector)
		f.Evidence = strings.TrimSpace(f.Evidence)
		if f.Issue == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func sanitizeTopFix(t *report.EvalTopFix) *report.EvalTopFix {
	if t == nil {
		return nil
	}
	t.Change = strings.TrimSpace(t.Change)
	t.Selector = strings.TrimSpace(t.Selector)
	t.Rationale = strings.TrimSpace(t.Rationale)
	t.Impact = strings.ToLower(strings.TrimSpace(t.Impact))
	if !validImpact[t.Impact] {
		t.Impact = "medium"
	}
	if t.Change == "" {
		return nil
	}
	return t
}

// applyVerification takes a draft evaluation and the verification-pass reply and
// returns the evaluation with ONLY the verified findings kept (each marked
// Verified=true). If the verify reply can't be parsed, it degrades to the draft
// unchanged (findings left with Verified=false) — a failed verify must not lose
// the draft. comprehension/top_fix come from the verify pass when present.
func applyVerification(draft report.PageEvaluation, verifyReply string) report.PageEvaluation {
	v, err := ParseEvaluation(verifyReply)
	if err != nil {
		return draft // degrade: keep the unverified draft
	}
	for i := range v.Blockers {
		v.Blockers[i].Verified = true
	}
	for i := range v.Frictions {
		v.Frictions[i].Verified = true
	}
	if v.TopFix == nil {
		v.TopFix = draft.TopFix
	}
	return v
}

// ParseSynthesis parses the synthesis-pass reply into a capped, validated list of
// run-level improvements. The real fix for truncation is the larger synthesis token
// budget (config.AUDITLOOP_LLM_SYNTH_MAX_TOKENS); this is defense-in-depth: if the
// body is STILL truncated/malformed it salvages the valid PREFIX of completed
// improvement objects rather than losing the whole story. When nothing can be
// salvaged it returns the decode error (so the caller LOGS it — the failure stays
// honest and non-fatal, never a silent fake success).
func ParseSynthesis(reply string) ([]report.EvalSynthItem, error) {
	raw, err := extractJSON(reply)
	if err != nil {
		// No complete JSON object (e.g. truncated before any closing brace): try to
		// salvage completed items straight from the improvements array.
		if items := normalizeSynthItems(salvageSynthItems(reply)); len(items) > 0 {
			return items, nil
		}
		return nil, err
	}
	var wrap struct {
		Improvements []report.EvalSynthItem `json:"improvements"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		if items := normalizeSynthItems(salvageSynthItems(reply)); len(items) > 0 {
			return items, nil
		}
		return nil, fmt.Errorf("decode synthesis: %w", err)
	}
	return normalizeSynthItems(wrap.Improvements), nil
}

// normalizeSynthItems trims, defaults the impact, drops title-less items, and caps
// the list at MaxSynthItems. Shared by the strict + salvage paths.
func normalizeSynthItems(in []report.EvalSynthItem) []report.EvalSynthItem {
	var out []report.EvalSynthItem
	for _, it := range in {
		it.Title = strings.TrimSpace(it.Title)
		if it.Title == "" {
			continue
		}
		it.Impact = strings.ToLower(strings.TrimSpace(it.Impact))
		if !validImpact[it.Impact] {
			it.Impact = "medium"
		}
		out = append(out, it)
		if len(out) >= MaxSynthItems {
			break
		}
	}
	return out
}

// salvageSynthItems recovers the valid PREFIX of completed improvement objects from
// a truncated/malformed synthesis body: it locates the "improvements" array and
// scans top-level objects with a brace/string-aware walk, decoding each COMPLETE
// object and stopping at the first incomplete (truncated) one. Best-effort — returns
// nil when it can't find the array or nothing completed.
func salvageSynthItems(s string) []report.EvalSynthItem {
	key := strings.Index(s, `"improvements"`)
	if key < 0 {
		return nil
	}
	open := strings.IndexByte(s[key:], '[')
	if open < 0 {
		return nil
	}
	body := s[key+open+1:]

	var out []report.EvalSynthItem
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					var it report.EvalSynthItem
					if err := json.Unmarshal([]byte(body[start:i+1]), &it); err == nil {
						out = append(out, it)
					}
					start = -1
				}
			}
		case ']':
			if depth == 0 {
				return out // array closed cleanly
			}
		}
	}
	return out // truncated before the array closed → whatever completed
}
