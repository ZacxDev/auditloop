package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/report"

	"github.com/gorilla/mux"
	g "maragu.dev/gomponents"
)

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	render(w, pages.Login(a.pageCtx(r, "Sign in")))
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// dashboardCardShots caps how many screenshot thumbnails a project card carousel
// shows (keeps the per-target page fetch bounded).
const dashboardCardShots = 5

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	targets, err := a.DB.ListTargets(uid)
	if err != nil {
		http.Error(w, "failed to load targets", http.StatusInternalServerError)
		return
	}
	apiKeys, err := a.DB.ListAPIKeys(uid)
	if err != nil {
		http.Error(w, "failed to load api keys", http.StatusInternalServerError)
		return
	}
	cards := make([]pages.DashboardCardVM, 0, len(targets))
	for _, t := range targets {
		cards = append(cards, a.buildDashboardCard(t))
	}
	render(w, pages.Dashboard(a.pageCtx(r, "Targets"), cards, apiKeys))
}

// buildDashboardCard assembles the per-target project-card VM: the latest done run's
// favicon + a few screenshot thumbnails + cheap summary stats. Owner-scoped
// throughout (the run belongs to the target's user). All artifact URLs go through the
// authed proxy. A missing run/favicon degrades gracefully (monogram + "No runs yet").
func (a *App) buildDashboardCard(t *db.Target) pages.DashboardCardVM {
	vm := pages.DashboardCardVM{Target: t}
	run, err := a.DB.LatestDoneRunForTargetOwned(t.UserID, t.ID)
	if err != nil || run == nil {
		return vm
	}
	vm.HasRun = true
	vm.Status = run.Status
	vm.RunDate = run.CreatedAt.Format("Jan 2, 2006")
	if run.FaviconKey != "" {
		vm.FaviconURL = artifactURL(run.FaviconKey)
	}

	var summary report.Summary
	if err := json.Unmarshal([]byte(run.SummaryJSON), &summary); err == nil {
		vm.Pages = summary.PagesCrawled
		vm.A11yViolations = summary.A11yViolations
	}
	if run.DiffJSON != "" {
		var diff report.Diff
		if err := json.Unmarshal([]byte(run.DiffJSON), &diff); err == nil {
			vm.Regressions = diff.PagesChanged
		}
	}

	if pageRows, err := a.DB.ListPages(run.ID); err == nil {
		vm.Shots = dashboardShots(pageRows)
	}
	return vm
}

// dashboardShots picks up to dashboardCardShots screenshot thumbnails for the card
// carousel, one per URL (preferring the desktop viewport), in page order.
func dashboardShots(pageRows []*db.Page) []pages.CardShot {
	byURL := map[string]*db.Page{}
	var order []string
	for _, p := range pageRows {
		if p.ScreenshotKey == "" {
			continue
		}
		cur, seen := byURL[p.URL]
		if !seen {
			order = append(order, p.URL)
			byURL[p.URL] = p
			continue
		}
		// Prefer the desktop capture as the representative thumbnail.
		if cur.Viewport != "desktop" && p.Viewport == "desktop" {
			byURL[p.URL] = p
		}
	}
	shots := make([]pages.CardShot, 0, dashboardCardShots)
	for _, u := range order {
		if len(shots) >= dashboardCardShots {
			break
		}
		p := byURL[u]
		shots = append(shots, pages.CardShot{
			URL: artifactURL(p.ScreenshotKey),
			Alt: "Screenshot of " + p.URL,
		})
	}
	return shots
}

func (a *App) handleTargetView(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	runs, err := a.DB.ListRuns(uid, id)
	if err != nil {
		http.Error(w, "failed to load runs", http.StatusInternalServerError)
		return
	}
	// Findings-over-time trend across the target's completed runs (owner-scoped).
	trend, err := a.DB.TargetFindingTrend(uid, id)
	if err != nil {
		http.Error(w, "failed to load trend", http.StatusInternalServerError)
		return
	}
	var pluginVM *pages.PluginVM
	var auditCfgVM *pages.AuditConfigVM
	var walkVM *pages.WalkthroughVM
	if t.AuthMode == db.AuthPlugin {
		pluginVM = a.pluginVM(t, "") // token never re-rendered (only on create/rotate)
	} else {
		auditCfgVM = a.auditConfigVM(uid, t)
		walkVM = a.walkthroughVM(uid, t)
	}
	render(w, pages.TargetView(a.pageCtx(r, t.Name), t, runs, trend, targetOverviewVM(runs), a.authVM(t), pluginVM, auditCfgVM, walkVM))
}

// targetOverviewVM derives the target overview status strip from the most recent
// run (runs are newest-first). Stats come from that run's persisted summary when it
// parses; a queued/running run (empty summary) degrades to just the status + date.
func targetOverviewVM(runs []*db.Run) *pages.TargetOverviewVM {
	if len(runs) == 0 {
		return &pages.TargetOverviewVM{}
	}
	last := runs[0]
	ov := &pages.TargetOverviewVM{
		HasRun:     true,
		LastStatus: last.Status,
		LastRunAt:  last.CreatedAt,
		LastRunID:  last.ID,
	}
	if last.SummaryJSON != "" {
		var s report.Summary
		if err := json.Unmarshal([]byte(last.SummaryJSON), &s); err == nil {
			ov.HasStats = true
			ov.Pages = s.PagesCrawled
			ov.A11y = s.A11yViolations
			ov.Console = s.ConsoleFirst
			ov.Network = s.NetworkFirst
		}
	}
	return ov
}

func (a *App) handleRunView(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	t, err := a.DB.GetTarget(uid, run.TargetID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	groups := a.runGroups(r, run)
	render(w, pages.RunView(a.pageCtx(r, "Run"), run, t, groups, a.runDiff(r, run), a.notesVM(run), a.evalVM(run)))
}

// handleRunStatus returns the run-body fragment (htmx poll target).
func (a *App) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	run, err := a.DB.GetRun(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	t, err := a.DB.GetTarget(uid, run.TargetID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	groups := a.runGroups(r, run)
	render(w, pages.RunBody(run, t, groups, a.runDiff(r, run), a.notesVM(run), a.evalVM(run)))
}

// runDiff builds the P2 "changes since previous run" view model: it parses the
// run's persisted diff summary and presigns each diff-image URL. Returns nil
// unless the run is done (the diff only exists once the run completes).
func (a *App) runDiff(r *http.Request, run *db.Run) *pages.DiffVM {
	if run.Status != db.RunDone {
		return nil
	}
	vm := &pages.DiffVM{PrevRunID: run.PrevRunID}
	if run.DiffJSON == "" {
		return vm // has a baseline flag (or first run) but no computed diff
	}
	var d report.Diff
	if err := json.Unmarshal([]byte(run.DiffJSON), &d); err != nil {
		return vm
	}
	vm.Diff = &d
	for _, cp := range d.ChangedPages {
		shot := pages.ChangedShotVM{
			URL: cp.URL, Viewport: cp.Viewport, DiffPct: cp.DiffPct,
			SizeChanged: cp.SizeChanged, NotCompared: cp.NotCompared, Regression: cp.IsRegression(),
		}
		shot.DiffURL = artifactURL(cp.DiffKey)
		vm.ChangedShots = append(vm.ChangedShots, shot)
	}
	return vm
}

// runGroups loads a run's pages + findings and builds presigned screenshot URLs.
func (a *App) runGroups(r *http.Request, run *db.Run) []pages.PageGroup {
	pageRows, err := a.DB.ListPages(run.ID)
	if err != nil {
		return nil
	}
	byURL := map[string]*pages.PageGroup{}
	var order []string
	for _, p := range pageRows {
		grp, ok := byURL[p.URL]
		if !ok {
			byURL[p.URL] = &pages.PageGroup{URL: p.URL}
			grp = byURL[p.URL]
			order = append(order, p.URL)
		}
		var findings []*db.Finding
		if f, err := a.DB.ListFindings(p.ID); err == nil {
			findings = f
		}
		grp.Viewports = append(grp.Viewports, pages.PageVM{Page: p, ScreenshotURL: artifactURL(p.ScreenshotKey), Findings: findings})
	}
	out := make([]pages.PageGroup, 0, len(order))
	for _, u := range order {
		out = append(out, *byURL[u])
	}
	return out
}

func render(w http.ResponseWriter, node g.Node) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = node.Render(w)
}
