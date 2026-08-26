package report

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &Report{
		Schema:      SchemaVersion,
		Tool:        "auditloop",
		ToolVersion: "test",
		RunID:       "run-1",
		TargetID:    "tgt-1",
		TargetName:  "Acme",
		BaseURL:     "https://acme.test",
		AuthMode:    "none",
		StartedAt:   now,
		FinishedAt:  now.Add(time.Minute),
		Status:      "done",
		Summary:     Summary{PagesCrawled: 2, A11yViolations: 1, URLsBlocked: 1},
		Pages: []PageReport{
			{
				URL: "https://acme.test/", Viewport: "mobile", Width: 390,
				ScreenshotKey: "acme/run-1/home/mobile.png",
				Console:       Origins{FirstParty: 1, ThirdParty: 2},
				A11y:          A11y{ViolationCount: 1, NodeCount: 1},
				Findings: []Finding{
					{Type: "a11y", Severity: "serious", Detail: json.RawMessage(`{"id":"image-alt"}`)},
				},
			},
		},
		Versions: ToolVersions{Auditloop: "test", AxeCore: "4.x"},
	}
	var buf bytes.Buffer
	if err := in.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Schema != SchemaVersion || out.RunID != "run-1" || out.Summary.PagesCrawled != 2 {
		t.Errorf("roundtrip mismatch: %+v", out.Summary)
	}
	if out.Summary.URLsBlocked != 1 {
		t.Errorf("blocked count lost: %d", out.Summary.URLsBlocked)
	}
	if len(out.Pages) != 1 || out.Pages[0].Findings[0].Type != "a11y" {
		t.Fatalf("pages lost: %+v", out.Pages)
	}
	// Detail must survive as raw JSON.
	var d map[string]string
	if err := json.Unmarshal(out.Pages[0].Findings[0].Detail, &d); err != nil {
		t.Fatalf("detail unmarshal: %v", err)
	}
	if d["id"] != "image-alt" {
		t.Errorf("detail id = %q", d["id"])
	}
}

func TestDiffBlockRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &Report{
		Schema: SchemaVersion, Tool: "auditloop", RunID: "run-2", Status: "done",
		Diff: &Diff{
			PrevRunID: "run-1", PrevRunAt: now,
			PagesAdded: []string{"https://a/new"}, PagesRemoved: []string{"https://a/old"},
			PagesChanged: 1, NewA11yRules: []string{"label"},
			A11yDelta: 2, ConsoleDelta: -1, NetworkDelta: 0,
			ChangedPages: []ChangedPage{
				{URL: "https://a/", Viewport: "mobile", DiffPct: 12.5, DiffKey: "a/run-2/home/mobile.diff.png"},
			},
		},
	}
	var buf bytes.Buffer
	if err := in.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Diff == nil {
		t.Fatal("diff block lost")
	}
	if out.Diff.PrevRunID != "run-1" || out.Diff.PagesChanged != 1 || out.Diff.NewA11yRules[0] != "label" {
		t.Errorf("diff roundtrip mismatch: %+v", out.Diff)
	}
	if len(out.Diff.ChangedPages) != 1 || !out.Diff.ChangedPages[0].IsRegression() {
		t.Errorf("changed pages mismatch: %+v", out.Diff.ChangedPages)
	}
}

func TestReportWithoutDiffIsBackwardCompatible(t *testing.T) {
	// A pre-P2 report.json (no "diff" key) must still decode, with Diff == nil.
	legacy := `{"schema":1,"tool":"auditloop","run_id":"r","status":"done","summary":{"pages_crawled":1},"pages":[],"versions":{"auditloop":"x"}}`
	out, err := Decode(bytes.NewReader([]byte(legacy)))
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if out.Diff != nil {
		t.Errorf("legacy report should have nil Diff, got %+v", out.Diff)
	}
	// And a report with no diff must not emit a "diff" key (omitempty).
	b, _ := (&Report{Schema: 1}).Marshal()
	if bytes.Contains(b, []byte(`"diff"`)) {
		t.Errorf("empty diff should be omitted, got %s", b)
	}
}

func TestMarshalStableKeys(t *testing.T) {
	r := &Report{Schema: 1, Tool: "auditloop"}
	b, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"schema": 1`)) || !bytes.Contains(b, []byte(`"tool": "auditloop"`)) {
		t.Errorf("unexpected marshal: %s", b)
	}
}
