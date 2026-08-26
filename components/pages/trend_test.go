package pages

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/auditloop/internal/db"

	g "maragu.dev/gomponents"
)

func renderNode(t *testing.T, n g.Node) string {
	t.Helper()
	var b strings.Builder
	if err := n.Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestFindingTrendEmpty(t *testing.T) {
	// 0 completed runs → renders nothing (no chart, no note).
	var b strings.Builder
	if err := FindingTrend(nil).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("0-run trend should render empty, got %q", b.String())
	}
}

func TestFindingTrendSingleRunNote(t *testing.T) {
	// 1 completed run → subtle note, NO svg.
	var b strings.Builder
	pts := []db.TrendPoint{{RunID: "r1", At: time.Unix(1000, 0), A11y: 3}}
	if err := FindingTrend(pts).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "<svg") {
		t.Errorf("single-run trend should not draw an svg: %s", out)
	}
	if !strings.Contains(out, "2 or more completed runs") {
		t.Errorf("single-run trend should show the note, got %s", out)
	}
}

func TestFindingTrendMultiRunSVG(t *testing.T) {
	pts := []db.TrendPoint{
		{RunID: "r1", At: time.Unix(1000, 0), A11y: 2, Console: 1},
		{RunID: "r2", At: time.Unix(2000, 0), A11y: 5, Console: 2, Layout: 1},
		{RunID: "r3", At: time.Unix(3000, 0), A11y: 12, Console: 0, Layout: 0},
	}
	out := renderNode(t, FindingTrend(pts))

	if !strings.Contains(out, "<svg") || !strings.Contains(out, "<polyline") {
		t.Fatalf("multi-run trend must render an svg with polylines: %s", out)
	}
	if strings.Count(out, "viewBox") != 1 {
		t.Errorf("expected exactly one viewBox, got %d", strings.Count(out, "viewBox"))
	}
	// a11y summary: latest 12, up 10 over 3 runs.
	if !strings.Contains(out, "▲10") || !strings.Contains(out, "over 3 runs") {
		t.Errorf("expected a11y delta ▲10 over 3 runs, got %s", out)
	}
	// a11y is always drawn; layout became nonzero so it's drawn too; both labels present.
	for _, lbl := range []string{"a11y", "layout", "console"} {
		if !strings.Contains(out, ">"+lbl+"<") {
			t.Errorf("expected series label %q in output: %s", lbl, out)
		}
	}
	// network is flat-zero everywhere → NOT drawn.
	if strings.Contains(out, ">network<") {
		t.Errorf("flat-zero network series should be omitted: %s", out)
	}
}

// determinism: the same input renders byte-identical output.
func TestFindingTrendDeterministic(t *testing.T) {
	pts := []db.TrendPoint{
		{RunID: "r1", At: time.Unix(1000, 0), A11y: 2},
		{RunID: "r2", At: time.Unix(2000, 0), A11y: 4},
	}
	a := renderNode(t, FindingTrend(pts))
	b := renderNode(t, FindingTrend(pts))
	if a != b {
		t.Errorf("render not deterministic:\n%s\n---\n%s", a, b)
	}
}
