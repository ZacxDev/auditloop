package pages

import (
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/db"
)

func TestProjectCardFullyPopulated(t *testing.T) {
	vm := DashboardCardVM{
		Target:     &db.Target{ID: "t1", Name: "Acme Marketing", BaseURL: "https://acme.com", AuthMode: db.AuthNone},
		FaviconURL: "/artifacts/acme/r1/favicon.png",
		Shots: []CardShot{
			{URL: "/artifacts/acme/r1/home/desktop.png", Alt: "Screenshot of https://acme.com/"},
			{URL: "/artifacts/acme/r1/pricing/desktop.png", Alt: "Screenshot of https://acme.com/pricing"},
		},
		HasRun: true, Status: "done", RunDate: "Jul 19, 2026",
		Pages: 4, A11yViolations: 7, Regressions: 2,
	}
	out := renderNode(t, projectCard(vm))

	// Identity: name, favicon img, base URL, crawl badge.
	for _, want := range []string{"Acme Marketing", "/artifacts/acme/r1/favicon.png", "https://acme.com", "Crawl"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q:\n%s", want, out)
		}
	}
	// Carousel: focusable, labeled scroll region + alt text + prev/next controls.
	if !strings.Contains(out, `tabindex="0"`) {
		t.Error("carousel track must be keyboard-focusable (tabindex=0) for axe scrollable-region-focusable")
	}
	if !strings.Contains(out, `aria-label="Screenshots of Acme Marketing"`) {
		t.Error("carousel region must be labeled")
	}
	if !strings.Contains(out, `alt="Screenshot of https://acme.com/"`) {
		t.Error("thumbnails must carry real alt text")
	}
	if !strings.Contains(out, `aria-label="Scroll screenshots left"`) || !strings.Contains(out, `aria-label="Scroll screenshots right"`) {
		t.Error("prev/next controls must have accessible names")
	}
	// Summary stats + interactive card + entrance animation.
	for _, want := range []string{">4<", ">7<", ">2<", "regressions", "card-interactive", "motion-safe:animate-enter", "Run audit"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q:\n%s", want, out)
		}
	}
}

func TestProjectCardNoRunsMonogram(t *testing.T) {
	vm := DashboardCardVM{
		Target: &db.Target{ID: "t2", Name: "Beta Site", BaseURL: "https://beta.test", AuthMode: db.AuthNone},
	}
	out := renderNode(t, projectCard(vm))
	if !strings.Contains(out, "No runs yet") {
		t.Error("no-run card should show the empty carousel state")
	}
	if !strings.Contains(out, "Not yet audited") {
		t.Error("no-run card should show the not-audited hint")
	}
	// Monogram fallback (no favicon img).
	if strings.Contains(out, "favicon") {
		t.Error("no-favicon card should not render a favicon img")
	}
	if !strings.Contains(out, ">BS<") {
		t.Errorf("expected BS monogram, got:\n%s", out)
	}
	// Single control-less carousel: no prev/next when there are no shots.
	if strings.Contains(out, "Scroll screenshots") {
		t.Error("no-shots card should not render carousel controls")
	}
}

func TestProjectCardPluginPushOnly(t *testing.T) {
	vm := DashboardCardVM{
		Target: &db.Target{ID: "t3", Name: "Funnel", AuthMode: db.AuthPlugin},
	}
	out := renderNode(t, projectCard(vm))
	if !strings.Contains(out, "Plugin") {
		t.Error("plugin target should carry the Plugin badge")
	}
	if strings.Contains(out, "Run audit") {
		t.Error("plugin target is push-only — must NOT offer Run audit")
	}
	if !strings.Contains(out, "Push instructions") {
		t.Error("plugin target should link to push instructions")
	}
	if !strings.Contains(out, "Push-only plugin target") {
		t.Error("plugin target with no base URL should show the push-only subtitle")
	}
}

func TestSingleShotHasNoControls(t *testing.T) {
	vm := DashboardCardVM{
		Target: &db.Target{ID: "t4", Name: "Solo", AuthMode: db.AuthNone},
		Shots:  []CardShot{{URL: "/artifacts/x/r/home/desktop.png", Alt: "Screenshot of https://x/"}},
		HasRun: true, Status: "done", RunDate: "Jul 19, 2026", Pages: 1,
	}
	out := renderNode(t, projectCard(vm))
	// One thumbnail → the track renders but prev/next are pointless, so omitted.
	if strings.Contains(out, "Scroll screenshots") {
		t.Error("single-shot carousel should not render prev/next controls")
	}
	if !strings.Contains(out, `tabindex="0"`) {
		t.Error("even a single-shot track stays keyboard-focusable")
	}
}

func TestMonogram(t *testing.T) {
	cases := map[string]string{
		"Acme Marketing": "AM", // two words → first initials
		"acme":           "AC", // one word → first two letters
		"X":              "X",
		"":               "?",
		"  spaced  out ": "SO",
	}
	for in, want := range cases {
		if got := monogram(in); got != want {
			t.Errorf("monogram(%q) = %q, want %q", in, got, want)
		}
	}
}
