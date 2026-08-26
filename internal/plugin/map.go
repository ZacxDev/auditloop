package plugin

import (
	"encoding/json"
	"strings"

	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/signals"
)

// PageKeys are the object-storage keys the caller assigned to a page's uploaded
// artifacts (the caller owns key naming + the actual upload; the mapper only
// records them). Axe/Network are "" when the page carried no such part.
type PageKeys struct {
	Screenshot string
	Axe        string
	Network    string
	// A11yDigest is the storage key of the page's VALIDATED + NORMALISED DOM/a11y
	// digest ("" when the push carried none). Setting it on the page row is the ENTIRE
	// wiring the persona evaluator needs: eval reads pages.a11y_digest_key, so a pushed
	// run flows through the exact same loadDigest → grounding-prompt → dropContradicted
	// path as a crawled one. "" ⇒ screenshot-only, gate never fires (legacy behaviour).
	A11yDigest string
}

// MappedPage is the DB-ready + report-ready result of mapping one validated
// PushPage. Findings have their PageID left blank — the caller sets it after the
// page row is inserted (the page id isn't known until then).
type MappedPage struct {
	Page     *db.Page
	Findings []*db.Finding
	Report   report.PageReport
}

// MapPage turns one validated PushPage into the row + findings + report entry the
// persistence layer expects. runID scopes the page; keys record where the
// caller uploaded the artifacts. env is the push's declared environment
// (lab|staging|prod|""): for a "lab" push the RAW perf columns + report.Perf are
// STILL persisted, but the misleading perf FINDINGS are suppressed (localhost lab
// perf is not field-representative). Layout/a11y/console/network findings are
// environment-independent and unaffected. It performs no I/O — pure and
// unit-testable.
func MapPage(runID string, pg PushPage, keys PageKeys, env string) MappedPage {
	page := &db.Page{
		RunID:                  runID,
		URL:                    pg.URL,
		Viewport:               pg.Viewport,
		ScreenshotKey:          keys.Screenshot,
		AxeKey:                 keys.Axe,
		A11yDigestKey:          keys.A11yDigest,
		AxeViolationCount:      pg.AxeViolations,
		ConsoleFirstPartyCount: pg.ConsoleFirstParty,
		ConsoleThirdPartyCount: pg.ConsoleThirdParty,
		NetworkFirstPartyCount: pg.NetworkFirstParty,
		NetworkThirdPartyCount: pg.NetworkThirdParty,
	}
	// Perf block (optional): mirror the crawler's perf columns onto the row + report.
	var repPerf *report.Perf
	if pg.Perf != nil {
		page.LCPMs = pg.Perf.LCPMs
		page.CLS = pg.Perf.CLS
		page.TBTMs = pg.Perf.TBTMs
		page.WeightBytes = pg.Perf.WeightBytes
		page.ReqCount = pg.Perf.ReqCount
		repPerf = &report.Perf{
			LCPMs:       pg.Perf.LCPMs,
			CLS:         pg.Perf.CLS,
			TBTMs:       pg.Perf.TBTMs,
			WeightBytes: pg.Perf.WeightBytes,
			ReqCount:    pg.Perf.ReqCount,
		}
	}
	// Layout block (optional): mirror the raw DOM smells onto the report.
	var repLayout *report.LayoutSmells
	if pg.Layout != nil {
		repLayout = &report.LayoutSmells{
			HorizontalOverflow:  pg.Layout.HorizontalOverflow,
			ScrollWidth:         pg.Layout.ScrollWidth,
			InnerWidth:          pg.Layout.InnerWidth,
			SmallTapTargets:     pg.Layout.SmallTapTargets,
			SmallText:           pg.Layout.SmallText,
			MissingViewportMeta: pg.Layout.MissingViewportMeta,
			ImagesNoDims:        pg.Layout.ImagesNoDims,
			Examples:            pg.Layout.Examples,
		}
	}

	findings := make([]*db.Finding, 0, len(pg.Findings))
	repFindings := make([]report.Finding, 0, len(pg.Findings))

	// Add appends a computed report.Finding (already-escaped JSON detail) to BOTH
	// the DB rows and the report page, keeping them in lockstep.
	add := func(f report.Finding) {
		findings = append(findings, &db.Finding{Type: f.Type, Severity: f.Severity, Detail: string(f.Detail)})
		repFindings = append(repFindings, f)
	}

	// Server-computed perf/layout findings are AUTHORITATIVE: the harness sends raw
	// measurement blocks, NOT perf/layout findings, and auditloop derives the
	// findings here via internal/signals (the SAME code the native crawl uses). If a
	// push nonetheless includes a hand-authored perf/layout finding alongside a
	// block, the BLOCK WINS — we drop the pushed finding of that type to avoid
	// double-emitting the same signal. Other finding types (a11y/console/network/
	// other) always pass through as pushed.
	for _, f := range pg.Findings {
		if (f.Type == "perf" && pg.Perf != nil) || (f.Type == "layout" && pg.Layout != nil) {
			continue
		}
		sev := f.Severity
		if sev == "" {
			sev = "info"
		}
		// Detail is stored as a JSON object so it round-trips through the same
		// findings table the crawl path uses.
		//
		// a11y findings are special-cased: the P2 diff (internal/worker/diff.go
		// runA11yRuleIDs) extracts each a11y finding's axe rule id from a TOP-LEVEL
		// "id" field of its stored detail (the shape the native crawl stores). If we
		// wrapped a pushed a11y detail as {"detail": "..."} like the others, it would
		// carry NO top-level "id" and the diff's new_a11y_rules would ALWAYS be empty
		// for pushed runs — silently making the CI a11y-regression gate a no-op for
		// every plugin push. a11yDetail therefore preserves/derives a top-level "id".
		//
		// Console/network/other findings keep the plain {"detail": "..."} wrapping.
		var detail json.RawMessage
		if f.Type == db.FindingA11y {
			detail = a11yDetail(f.Detail)
		} else {
			// json.Marshal escapes the free-text detail (no injection).
			detail, _ = json.Marshal(map[string]string{"detail": f.Detail})
		}
		add(report.Finding{Type: f.Type, Severity: sev, Detail: detail})
	}

	// Compute perf + layout findings server-side from the raw blocks (one source of
	// truth for the thresholds + mobile gating). mobile gates the mobile-only smells.
	//
	// Perf-honesty: a "lab" push measured perf on a hermetic localhost/DEV_MODE stack
	// (no CDN/latency/compression), so its LCP/TBT/weight/req are NOT
	// field-representative — we keep the raw perf columns + report.Perf (above) for
	// reference/trend but DO NOT emit the misleading perf findings. staging|prod|
	// unspecified are treated as field-representative → findings computed as usual.
	if repPerf != nil && env != EnvLab {
		for _, f := range signals.PerfFindings(*repPerf) {
			add(f)
		}
	}
	if repLayout != nil {
		for _, f := range signals.LayoutFindings(*repLayout, pg.Viewport == ViewportMobile) {
			add(f)
		}
	}

	rep := report.PageReport{
		URL:           pg.URL,
		Viewport:      pg.Viewport,
		Width:         WidthFor(pg.Viewport),
		ScreenshotKey: keys.Screenshot,
		AxeKey:        keys.Axe,
		NetworkKey:    keys.Network,
		A11yDigestKey: keys.A11yDigest,
		Console:       report.Origins{FirstParty: pg.ConsoleFirstParty, ThirdParty: pg.ConsoleThirdParty},
		Network:       report.Origins{FirstParty: pg.NetworkFirstParty, ThirdParty: pg.NetworkThirdParty},
		A11y:          report.A11y{ViolationCount: pg.AxeViolations},
		Findings:      repFindings,
		Perf:          repPerf,
		Layout:        repLayout,
	}

	return MappedPage{Page: page, Findings: findings, Report: rep}
}

// maxA11yRuleIDLen caps a rule id derived from a legacy free-text a11y detail so a
// pathological push can't store an unbounded id (the whole push is already
// size-capped upstream; this bounds the single derived field).
const maxA11yRuleIDLen = 128

// a11yDetail produces the stored JSON detail for a PUSHED a11y finding, guaranteeing
// a TOP-LEVEL "id" (the axe rule id) so the P2 diff's runA11yRuleIDs can recover it —
// the same contract the native crawl satisfies by storing the raw axe violation
// (which already has a top-level "id"). Two input shapes are handled:
//
//  1. A structured axe-violation JSON OBJECT that already carries a non-empty string
//     "id" (what a modern harness sends) → stored verbatim (it's already
//     the right shape; json.Marshal is not re-run so the harness's structure survives).
//  2. A legacy plain string (a legacy harness's format is "<rule-id> — <help text>") → the id is
//     the substring before the first " — " (or the whole trimmed string when absent),
//     and BOTH the id (for the diff) and the original text (for UI read-back) are stored
//     as {"id": ..., "detail": ...} (json.Marshal escapes both — no injection).
//
// TRANSITION NOTE: the FIRST diff after this ships compares a new run (rule ids now
// extracted) against a PRE-FIX baseline whose stored pushed-a11y findings have NO ids
// (empty rule set) — so every current rule looks "new" for that ONE diff per target.
// This is a one-time transient; the next run establishes a clean baseline. It is NOT a
// real regression and should not be mistaken for one.
func a11yDetail(pushed string) json.RawMessage {
	// Shape 1: already a structured object with a non-empty string "id" → keep as-is
	// (it's already the shape runA11yRuleIDs + the crawl path expect).
	if hasTopLevelStringID(pushed) {
		return json.RawMessage(pushed)
	}
	// Shape 2: legacy free-text "<rule-id> — <help text>" (or a bare id). Derive the id
	// (substring before the first " — ", else the whole trimmed string) and store BOTH
	// id (for the diff) and the original text (for UI read-back).
	id := strings.TrimSpace(pushed)
	if i := strings.Index(id, " — "); i >= 0 {
		id = strings.TrimSpace(id[:i])
	}
	if len(id) > maxA11yRuleIDLen {
		id = id[:maxA11yRuleIDLen]
	}
	detail, _ := json.Marshal(map[string]string{"id": id, "detail": pushed})
	return detail
}

// hasTopLevelStringID reports whether s is a JSON object with a non-empty string "id"
// field (the structured axe-violation shape the native crawl stores). A plain
// free-text string is not valid JSON here and returns false.
func hasTopLevelStringID(s string) bool {
	var meta struct {
		ID string `json:"id"`
	}
	return json.Unmarshal([]byte(s), &meta) == nil && meta.ID != ""
}
