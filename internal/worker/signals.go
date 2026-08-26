package worker

import (
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/report"
	"github.com/ZacxDev/auditloop/internal/signals"
)

// The perf/layout finding thresholds live in internal/signals (the single source
// of truth, shared with the plugin-push ingest path). These worker adapters map a
// crawler.PageResult's raw fields onto the pure report.Perf / report.LayoutSmells
// value types signals consumes — so a native crawl and an external push produce
// identical findings from identical measurements. The detail types are aliased so
// existing worker tests keep referencing perfDetail/layoutDetail unchanged.
type (
	perfDetail   = signals.PerfDetail
	layoutDetail = signals.LayoutDetail
)

// perfFindings emits a report.Finding per web-vital that breaches its "good"
// threshold (plus a heavy-page finding), delegating to signals.PerfFindings.
func perfFindings(pr crawler.PageResult) []report.Finding {
	return signals.PerfFindings(report.Perf{
		LCPMs: pr.LCPMs, CLS: pr.CLS, TBTMs: pr.TBTMs,
		WeightBytes: pr.WeightBytes, ReqCount: pr.ReqCount,
	})
}

// layoutFindings emits a report.Finding per DOM layout smell, delegating to
// signals.LayoutFindings (mobile gates the mobile-only smells).
func layoutFindings(pr crawler.PageResult, mobile bool) []report.Finding {
	ls := pr.Layout
	return signals.LayoutFindings(report.LayoutSmells{
		HorizontalOverflow: ls.HorizontalOverflow, ScrollWidth: ls.ScrollWidth, InnerWidth: ls.InnerWidth,
		SmallTapTargets: ls.SmallTapTargets, SmallText: ls.SmallText, MissingViewportMeta: ls.MissingViewportMeta,
		ImagesNoDims: ls.ImagesNoDims, Examples: ls.Examples,
	}, mobile)
}

// sevForNetwork maps a network error to a severity by status class and origin. A
// 5xx anywhere is serious; a first-party 4xx (a broken link/asset on the site
// itself, esp. 404) is serious; a third-party 4xx is minor; a failed load
// (net::ERR_*) is serious first-party / minor third-party.
func sevForNetwork(ne crawler.NetworkError) string {
	switch {
	case ne.Status >= 500:
		return "serious"
	case ne.Status >= 400:
		if ne.FirstPart {
			return "serious"
		}
		return "minor"
	default:
		// No HTTP status → a failed load (net::ERR_*, DNS, connection reset).
		if ne.FirstPart {
			return "serious"
		}
		return "minor"
	}
}
