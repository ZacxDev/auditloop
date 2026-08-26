package worker

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/report"
)

// perfSev returns the severity of the perf finding for the named metric ("" if absent).
func perfSev(findings []report.Finding, metric string) (string, bool) {
	for _, f := range findings {
		if f.Type != "perf" {
			continue
		}
		var d perfDetail
		if json.Unmarshal(f.Detail, &d) == nil && d.Metric == metric {
			return f.Severity, true
		}
	}
	return "", false
}

func TestPerfFindingThresholds(t *testing.T) {
	cases := []struct {
		name     string
		pr       crawler.PageResult
		metric   string
		wantSev  string
		wantNone bool
	}{
		{name: "LCP good", pr: crawler.PageResult{LCPMs: 2000}, metric: "LCP", wantNone: true},
		{name: "LCP at good boundary", pr: crawler.PageResult{LCPMs: 2500}, metric: "LCP", wantNone: true},
		{name: "LCP needs-improvement", pr: crawler.PageResult{LCPMs: 3000}, metric: "LCP", wantSev: "moderate"},
		{name: "LCP poor", pr: crawler.PageResult{LCPMs: 5000}, metric: "LCP", wantSev: "serious"},
		{name: "CLS good", pr: crawler.PageResult{CLS: 0.05}, metric: "CLS", wantNone: true},
		{name: "CLS needs-improvement", pr: crawler.PageResult{CLS: 0.2}, metric: "CLS", wantSev: "moderate"},
		{name: "CLS poor", pr: crawler.PageResult{CLS: 0.4}, metric: "CLS", wantSev: "serious"},
		{name: "TBT good", pr: crawler.PageResult{TBTMs: 100}, metric: "TBT", wantNone: true},
		{name: "TBT needs-improvement", pr: crawler.PageResult{TBTMs: 400}, metric: "TBT", wantSev: "moderate"},
		{name: "TBT poor", pr: crawler.PageResult{TBTMs: 800}, metric: "TBT", wantSev: "serious"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sev, ok := perfSev(perfFindings(tc.pr), tc.metric)
			if tc.wantNone {
				if ok {
					t.Errorf("expected NO %s finding, got severity %q", tc.metric, sev)
				}
				return
			}
			if !ok {
				t.Fatalf("expected a %s finding, got none", tc.metric)
			}
			if sev != tc.wantSev {
				t.Errorf("%s severity = %q, want %q", tc.metric, sev, tc.wantSev)
			}
		})
	}
}

func TestPerfHeavyPageFinding(t *testing.T) {
	// Under both budgets → no page-weight finding.
	if fs := perfFindings(crawler.PageResult{WeightBytes: 1 << 20, ReqCount: 20}); len(fs) != 0 {
		t.Errorf("light page should emit no perf finding, got %d", len(fs))
	}
	// Over the weight budget → one minor finding.
	fs := perfFindings(crawler.PageResult{WeightBytes: 5 << 20, ReqCount: 10})
	if len(fs) != 1 || fs[0].Severity != "minor" {
		t.Fatalf("heavy page should emit one minor perf finding, got %+v", fs)
	}
	// Over the request budget → one minor finding.
	fs = perfFindings(crawler.PageResult{ReqCount: 150})
	if len(fs) != 1 || fs[0].Severity != "minor" {
		t.Fatalf("request-heavy page should emit one minor perf finding, got %+v", fs)
	}
}

func smellSet(fs []report.Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		var d layoutDetail
		if json.Unmarshal(f.Detail, &d) == nil {
			out[d.Smell] = true
		}
	}
	return out
}

func TestLayoutFindingEmission(t *testing.T) {
	full := crawler.PageResult{Layout: crawler.LayoutSmells{
		HorizontalOverflow: true, ScrollWidth: 500, InnerWidth: 390,
		SmallTapTargets: 3, SmallText: 2, MissingViewportMeta: true, ImagesNoDims: 4,
		Examples: map[string][]string{"small_tap_targets": {"a.btn (20x20)"}},
	}}

	// Mobile: all five smells fire.
	mob := layoutFindings(full, true)
	got := smellSet(mob)
	for _, want := range []string{"horizontal-overflow", "small-tap-targets", "small-text", "missing-viewport-meta", "images-without-dimensions"} {
		if !got[want] {
			t.Errorf("mobile: expected %q layout finding, got %v", want, got)
		}
	}

	// Desktop: the mobile-only smells (overflow + tap targets) must NOT fire; the
	// viewport-agnostic ones still do.
	desk := smellSet(layoutFindings(full, false))
	if desk["horizontal-overflow"] {
		t.Error("desktop: horizontal-overflow should not fire (mobile-only)")
	}
	if desk["small-tap-targets"] {
		t.Error("desktop: small-tap-targets should not fire (mobile-only)")
	}
	if !desk["missing-viewport-meta"] || !desk["images-without-dimensions"] || !desk["small-text"] {
		t.Errorf("desktop: viewport-agnostic smells should still fire, got %v", desk)
	}

	// A clean page emits nothing.
	if fs := layoutFindings(crawler.PageResult{}, true); len(fs) != 0 {
		t.Errorf("clean page should emit no layout findings, got %d", len(fs))
	}

	// Example selectors flow into the detail (escaped JSON).
	for _, f := range mob {
		var d layoutDetail
		_ = json.Unmarshal(f.Detail, &d)
		if d.Smell == "small-tap-targets" && (len(d.Examples) == 0 || d.Examples[0] != "a.btn (20x20)") {
			t.Errorf("small-tap-targets finding should carry example selectors, got %+v", d)
		}
	}
}

func TestSevForNetwork(t *testing.T) {
	cases := []struct {
		name string
		ne   crawler.NetworkError
		want string
	}{
		{"5xx first-party", crawler.NetworkError{Status: 500, FirstPart: true}, "serious"},
		{"5xx third-party", crawler.NetworkError{Status: 503, FirstPart: false}, "serious"},
		{"first-party 404", crawler.NetworkError{Status: 404, FirstPart: true}, "serious"},
		{"first-party 403", crawler.NetworkError{Status: 403, FirstPart: true}, "serious"},
		{"third-party 404", crawler.NetworkError{Status: 404, FirstPart: false}, "minor"},
		{"failed load first-party", crawler.NetworkError{Reason: "net::ERR_NAME_NOT_RESOLVED", FirstPart: true}, "serious"},
		{"failed load third-party", crawler.NetworkError{Reason: "net::ERR_FAILED", FirstPart: false}, "minor"},
	}
	for _, tc := range cases {
		if got := sevForNetwork(tc.ne); got != tc.want {
			t.Errorf("%s: sevForNetwork = %q, want %q", tc.name, got, tc.want)
		}
	}
}
