// Package signals computes the deterministic perf (web-vitals / page-weight) and
// DOM layout-smell FINDINGS from raw per-page measurements. It is the SINGLE
// source of truth for the thresholds that turn raw metrics into findings, shared
// by BOTH the native crawl worker (internal/worker) AND the plugin-push ingest
// path (internal/plugin) — so an external push harness sends RAW MEASUREMENTS
// only and never re-implements these thresholds in another language.
//
// It is pure and dependency-light: inputs are the pure report.Perf /
// report.LayoutSmells value types (no chromedp, no crawler import), and the
// web-vitals thresholds live in the report package (report.LCPGoodMs, …) so the
// finding logic and the UI's colour coding share one source of truth. A metric
// at/under Good emits nothing, between Good and Poor is a moderate finding, and
// above Poor is serious.
package signals

import (
	"encoding/json"
	"fmt"

	"github.com/ZacxDev/auditloop/internal/report"
)

// PerfDetail is the JSON detail stored on a perf finding.
type PerfDetail struct {
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	Threshold float64 `json:"threshold"`
	Rating    string  `json:"rating"` // needs-improvement|poor|info
	Note      string  `json:"note,omitempty"`
}

// PerfFindings emits a report.Finding per web-vital that breaches its "good"
// threshold, plus one informational finding when the page is heavy (weight or
// request count). Returns nil when everything is within budget.
func PerfFindings(p report.Perf) []report.Finding {
	var out []report.Finding
	add := func(d PerfDetail, sev string) {
		b, _ := json.Marshal(d)
		out = append(out, report.Finding{Type: "perf", Severity: sev, Detail: b})
	}

	if lcp := p.LCPMs; lcp > report.LCPGoodMs {
		sev, rating := "moderate", "needs-improvement"
		if lcp > report.LCPPoorMs {
			sev, rating = "serious", "poor"
		}
		add(PerfDetail{Metric: "LCP", Value: float64(lcp), Unit: "ms", Threshold: report.LCPGoodMs, Rating: rating,
			Note: "Largest Contentful Paint is slow — the main content takes too long to render."}, sev)
	}
	if cls := p.CLS; cls > report.CLSGood {
		sev, rating := "moderate", "needs-improvement"
		if cls > report.CLSPoor {
			sev, rating = "serious", "poor"
		}
		add(PerfDetail{Metric: "CLS", Value: cls, Unit: "", Threshold: report.CLSGood, Rating: rating,
			Note: "Cumulative Layout Shift is high — content moves as the page loads."}, sev)
	}
	if tbt := p.TBTMs; tbt > report.TBTGoodMs {
		sev, rating := "moderate", "needs-improvement"
		if tbt > report.TBTPoorMs {
			sev, rating = "serious", "poor"
		}
		add(PerfDetail{Metric: "TBT", Value: float64(tbt), Unit: "ms", Threshold: report.TBTGoodMs, Rating: rating,
			Note: "Total Blocking Time is high (LAB PROXY — captured headless with no field input; approximates main-thread blocking, not real field TBT)."}, sev)
	}
	// Heavy page → one informational finding covering whichever budget(s) breached.
	if p.WeightBytes > report.WeightWarnBytes || p.ReqCount > report.ReqCountWarn {
		note := ""
		if p.WeightBytes > report.WeightWarnBytes {
			note = fmt.Sprintf("Page transferred %.1f MB (> %d MB budget). ", float64(p.WeightBytes)/(1024*1024), report.WeightWarnBytes/(1024*1024))
		}
		if p.ReqCount > report.ReqCountWarn {
			note += fmt.Sprintf("Page made %d requests (> %d budget).", p.ReqCount, report.ReqCountWarn)
		}
		b, _ := json.Marshal(map[string]any{
			"metric": "page-weight", "weight_bytes": p.WeightBytes, "req_count": p.ReqCount,
			"weight_budget_bytes": report.WeightWarnBytes, "req_budget": report.ReqCountWarn, "note": note,
		})
		out = append(out, report.Finding{Type: "perf", Severity: "minor", Detail: b})
	}
	return out
}

// LayoutDetail is the JSON detail stored on a layout finding. Examples are DOM-
// derived selectors (untrusted-ish) — stored as escaped JSON, rendered escaped.
type LayoutDetail struct {
	Smell    string   `json:"smell"`
	Count    int      `json:"count,omitempty"`
	Examples []string `json:"examples,omitempty"`
	Note     string   `json:"note"`
}

// LayoutFindings emits a report.Finding per DOM layout smell present on the page.
// mobile gates the mobile-specific smells (horizontal overflow + small tap
// targets): those fire ONLY on the mobile viewport, where they are real UX defects
// (a desktop layout legitimately exceeds 390px). Missing viewport-meta and
// undimensioned images apply to any viewport.
func LayoutFindings(ls report.LayoutSmells, mobile bool) []report.Finding {
	var out []report.Finding
	add := func(d LayoutDetail, sev string) {
		b, _ := json.Marshal(d)
		out = append(out, report.Finding{Type: "layout", Severity: sev, Detail: b})
	}

	if mobile && ls.HorizontalOverflow {
		add(LayoutDetail{Smell: "horizontal-overflow", Examples: ls.Examples["horizontal_overflow"],
			Note: fmt.Sprintf("Page scrolls horizontally on mobile (scrollWidth %d > viewport %d).", ls.ScrollWidth, ls.InnerWidth)}, "moderate")
	}
	if mobile && ls.SmallTapTargets > 0 {
		add(LayoutDetail{Smell: "small-tap-targets", Count: ls.SmallTapTargets, Examples: ls.Examples["small_tap_targets"],
			Note: "Interactive elements smaller than 44x44px are hard to tap on mobile."}, "moderate")
	}
	if ls.SmallText > 0 {
		add(LayoutDetail{Smell: "small-text", Count: ls.SmallText, Examples: ls.Examples["small_text"],
			Note: "Text smaller than 12px is hard to read."}, "minor")
	}
	if ls.MissingViewportMeta {
		add(LayoutDetail{Smell: "missing-viewport-meta",
			Note: "No <meta name=viewport> — the page will not render responsively on mobile."}, "moderate")
	}
	if ls.ImagesNoDims > 0 {
		add(LayoutDetail{Smell: "images-without-dimensions", Count: ls.ImagesNoDims, Examples: ls.Examples["images_no_dims"],
			Note: "Images without width/height attributes cause layout shift (CLS) as they load."}, "minor")
	}
	return out
}
