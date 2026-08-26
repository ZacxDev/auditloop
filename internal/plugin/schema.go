package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MaxPages caps the number of pages a single push may carry (abuse guard).
const MaxPages = 200

// Viewports allowed on a pushed page (mirrors the crawler's two viewports).
const (
	ViewportMobile  = "mobile"
	ViewportDesktop = "desktop"
)

// Environments a push may declare. The value is a PERF-HONESTY signal: a "lab"
// push measured perf on a hermetic localhost/DEV_MODE stack (no CDN, no network
// latency, no compression), so its LCP/TBT/weight/req numbers are NOT
// field-representative — auditloop keeps the raw numbers but suppresses the
// misleading perf FINDINGS for lab pushes (see MapPage). staging|prod are treated
// as field-representative (perf findings computed as usual). Absent/"" ==
// unspecified → current behavior (perf findings computed).
const (
	EnvLab     = "lab"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// validEnvironments is the closed set accepted on PushPayload.Environment.
var validEnvironments = map[string]bool{EnvLab: true, EnvStaging: true, EnvProd: true}

// Default capture widths per viewport (mirrors the crawler: mobile 390px,
// desktop 1440px). Recorded on the page row for parity with crawled runs.
const (
	widthMobile  = 390
	widthDesktop = 1440
)

// findingTypes is the closed set of accepted finding types. "perf" + "layout"
// mirror the deterministic perf/layout-smell findings the native crawler emits
// (internal/worker/signals.go) so a pushed funnel carries the same signals.
var findingTypes = map[string]bool{
	"a11y": true, "console": true, "network": true, "perf": true, "layout": true, "other": true,
}

// PushPayload is the top-level plugin-push body (the JSON `metadata` multipart
// part). It is an ingestion-oriented mirror of report.PageReport: the harness's
// shim maps its own audit output to this shape.
type PushPayload struct {
	// Label is an optional human label for the pushed run (e.g. "signup funnel
	// #42"). Surfaced in report.json.
	Label string `json:"label,omitempty"`
	// Environment is the OPTIONAL declared environment the capture ran in
	// (lab|staging|prod). It is a PERF-HONESTY signal: a "lab" push measured perf on
	// a hermetic localhost/DEV_MODE stack, so its perf numbers are not
	// field-representative and auditloop suppresses the misleading perf FINDINGS for
	// it (raw perf columns are still kept). Absent → unspecified (perf findings
	// computed as usual — backward-compatible). Validated against validEnvironments.
	Environment string `json:"environment,omitempty"`
	// Pages is the set of captured views. Each page's `url` is the STABLE page
	// identity used for P2 diff matching across pushes (see PushPage.URL).
	Pages []PushPage `json:"pages"`
}

// PushPage is one captured view. `url` + `viewport` form the identity the P2 diff
// engine matches on across pushes — the shim MUST emit a consistent `url` per
// logical view (a real URL, or a stable view label like "signup-step-3") so
// regressions track the same view run to run.
type PushPage struct {
	URL      string `json:"url"`
	Viewport string `json:"viewport"` // mobile|desktop
	// Screenshot is the filename (in the multipart) of this view's PNG/JPEG. Required.
	Screenshot string `json:"screenshot"`
	// Axe / Network are optional filenames of raw axe-core / network-log JSON.
	Axe     string `json:"axe,omitempty"`
	Network string `json:"network,omitempty"`
	// A11yDigest is the OPTIONAL filename of this view's bounded DOM/accessibility
	// digest — the same JSON shape the native crawl captures (report.A11yDigest:
	// interactive elements + form-control label associations + a landmark/heading
	// skeleton). Supplying it routes the pushed run through the SAME deterministic,
	// no-LLM grounding gate the crawl + driven paths get (eval.dropContradicted), which
	// drops persona a11y findings the DOM positively REFUTES (an sr-only <label for>, an
	// <a> styled as a card). Omitted → the pushed run stays screenshot-only and the gate
	// never fires: byte-for-byte the pre-digest behaviour.
	//
	// The content is UNTRUSTED (producer-authored) and, unlike the crawl path's
	// self-generated digest, it drives a gate that DROPS findings — so it is validated +
	// normalised at ingest (NormalizeA11yDigest): a malformed one REJECTS the push (400),
	// and `has_label` is derived from `label_source` rather than trusted.
	A11yDigest string `json:"a11y_digest,omitempty"`
	// Roll-up counts (parity with the crawl summary).
	AxeViolations     int `json:"axe_violations"`
	ConsoleFirstParty int `json:"console_first_party"`
	ConsoleThirdParty int `json:"console_third_party"`
	NetworkFirstParty int `json:"network_first_party"`
	NetworkThirdParty int `json:"network_third_party"`
	// Findings are normalized issues attached to the view.
	Findings []PushFinding `json:"findings,omitempty"`
	// Perf is the OPTIONAL deterministic web-vitals / page-weight block, mirroring
	// the crawler's perf columns. Omitted → the page persists with zero perf (a
	// pre-perf push still decodes). See PushPerf.
	Perf *PushPerf `json:"perf,omitempty"`
	// Layout is the OPTIONAL RAW DOM layout-smell measurement block, mirroring
	// crawler.LayoutSmells / report.LayoutSmells exactly (same snake_case keys). The
	// harness sends RAW COUNTS/FLAGS ONLY — auditloop computes the layout FINDINGS
	// server-side (internal/signals) so the smell→finding thresholds + mobile gating
	// live in ONE place, never re-implemented in the harness. Omitted → no layout
	// findings (a pre-layout push still decodes). See PushLayout.
	Layout *PushLayout `json:"layout,omitempty"`
}

// PushPerf is the OPTIONAL per-page deterministic perf capture. Field shape mirrors
// the crawler's persisted perf columns AND report.Perf JSON, so a harness emits the
// SAME numbers the native crawler records. LCP/TBT are milliseconds, CLS is unitless.
// tbt_ms is a LAB PROXY (headless, no field input) — an approximation of main-thread
// blocking, not a real field Total Blocking Time.
type PushPerf struct {
	LCPMs       int64   `json:"lcp_ms,omitempty"`
	CLS         float64 `json:"cls,omitempty"`
	TBTMs       int64   `json:"tbt_ms,omitempty"` // lab proxy (headless, no field input)
	WeightBytes int64   `json:"weight_bytes,omitempty"`
	ReqCount    int     `json:"req_count,omitempty"`
}

// PushLayout is the OPTIONAL per-page RAW DOM layout-smell measurement block. Its
// JSON mirrors crawler.LayoutSmells / report.LayoutSmells EXACTLY (same snake_case
// keys, including `examples`), so a harness emits the SAME raw numbers the native
// crawler records. auditloop turns these into layout findings server-side via
// internal/signals (thresholds + mobile-only gating shared with the crawl path) —
// the harness MUST NOT compute layout findings itself.
//
// `examples` is attacker-controlled DOM-derived selector text: it is stored as
// ESCAPED JSON on the finding detail and rendered escaped in the UI (no XSS).
type PushLayout struct {
	HorizontalOverflow  bool                `json:"horizontal_overflow,omitempty"`
	ScrollWidth         int                 `json:"scroll_width,omitempty"`
	InnerWidth          int                 `json:"inner_width,omitempty"`
	SmallTapTargets     int                 `json:"small_tap_targets,omitempty"`
	SmallText           int                 `json:"small_text,omitempty"`
	MissingViewportMeta bool                `json:"missing_viewport_meta,omitempty"`
	ImagesNoDims        int                 `json:"images_no_dims,omitempty"`
	Examples            map[string][]string `json:"examples,omitempty"`
}

// PushFinding is one normalized issue on a pushed page.
type PushFinding struct {
	Type     string `json:"type"`     // a11y|console|network|perf|layout|other
	Severity string `json:"severity"` // free text (critical|serious|moderate|minor|info)
	Detail   string `json:"detail"`   // human-readable detail (rendered escaped)
}

// fileRefs is the ONE list of multipart filenames a page can reference. Both
// ReferencedFiles (uploader path-containment + orphan detection) and Validate
// (refs↔parts integrity) walk it, so a new artifact ref can never be added to one
// without the other.
func (pg PushPage) fileRefs() []string {
	return []string{pg.Screenshot, pg.Axe, pg.Network, pg.A11yDigest}
}

// Parse decodes a push payload with DisallowUnknownFields, so an unexpected JSON
// key (a typo, an injected field) is REJECTED rather than silently ignored.
func Parse(r io.Reader) (*PushPayload, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var p PushPayload
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid metadata JSON: %w", err)
	}
	// Reject trailing garbage after the JSON object.
	if dec.More() {
		return nil, fmt.Errorf("invalid metadata JSON: unexpected trailing data")
	}
	return &p, nil
}

// ReferencedFiles returns the set of multipart filenames the payload references
// (screenshots + optional axe/network/a11y-digest parts).
func (p *PushPayload) ReferencedFiles() map[string]bool {
	out := map[string]bool{}
	for _, pg := range p.Pages {
		for _, f := range pg.fileRefs() {
			if f != "" {
				out[f] = true
			}
		}
	}
	return out
}

// Validate checks the payload against the provided set of multipart filenames.
// It enforces: at least one page and no more than MaxPages; a non-empty stable
// url + a known viewport + a screenshot ref per page; a known finding type per
// finding; and strict multipart integrity — every referenced filename has a
// matching part AND every provided part is referenced (no orphan uploads).
//
// It returns a credential-free, user-facing error on the first violation.
func (p *PushPayload) Validate(provided map[string]bool) error {
	if p.Environment != "" && !validEnvironments[p.Environment] {
		return fmt.Errorf("environment must be %q, %q or %q, got %q", EnvLab, EnvStaging, EnvProd, p.Environment)
	}
	if len(p.Pages) == 0 {
		return fmt.Errorf("no pages in payload")
	}
	if len(p.Pages) > MaxPages {
		return fmt.Errorf("too many pages: %d (max %d)", len(p.Pages), MaxPages)
	}
	seenRefs := map[string]bool{}
	for i, pg := range p.Pages {
		if strings.TrimSpace(pg.URL) == "" {
			return fmt.Errorf("page %d: url (stable page identity) is required", i)
		}
		if pg.Viewport != ViewportMobile && pg.Viewport != ViewportDesktop {
			return fmt.Errorf("page %d: viewport must be %q or %q, got %q", i, ViewportMobile, ViewportDesktop, pg.Viewport)
		}
		if strings.TrimSpace(pg.Screenshot) == "" {
			return fmt.Errorf("page %d: screenshot filename is required", i)
		}
		for _, ref := range pg.fileRefs() {
			if ref == "" {
				continue
			}
			if !provided[ref] {
				return fmt.Errorf("page %d references file %q but no matching part was uploaded", i, ref)
			}
			seenRefs[ref] = true
		}
		for j, f := range pg.Findings {
			if !findingTypes[f.Type] {
				return fmt.Errorf("page %d finding %d: unknown type %q", i, j, f.Type)
			}
		}
		// Perf values must be non-negative when present (light validation — the
		// harness owns the exact numbers; we only reject obvious garbage).
		if pg.Perf != nil {
			p := pg.Perf
			if p.LCPMs < 0 || p.CLS < 0 || p.TBTMs < 0 || p.WeightBytes < 0 || p.ReqCount < 0 {
				return fmt.Errorf("page %d: perf values must be non-negative", i)
			}
		}
		// Layout counts must be non-negative when present (light validation — the
		// harness owns the raw measurements; we only reject obvious garbage, and
		// don't over-constrain the untrusted `examples` map, which is stored escaped).
		if pg.Layout != nil {
			l := pg.Layout
			if l.ScrollWidth < 0 || l.InnerWidth < 0 || l.SmallTapTargets < 0 || l.SmallText < 0 || l.ImagesNoDims < 0 {
				return fmt.Errorf("page %d: layout values must be non-negative", i)
			}
		}
	}
	// No orphan parts: every uploaded file must be referenced by the metadata.
	for name := range provided {
		if !seenRefs[name] {
			return fmt.Errorf("uploaded file %q is not referenced by any page", name)
		}
	}
	return nil
}

// WidthFor returns the recorded capture width for a viewport.
func WidthFor(viewport string) int {
	switch viewport {
	case ViewportDesktop:
		return widthDesktop
	default:
		return widthMobile
	}
}
