package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/report"

	dto "github.com/prometheus/client_model/go"
)

// readWalkRegressions reads auditloop_walkthrough_regressions_total directly (no
// prometheus/testutil — it would pull a new module into go.sum for one assertion).
// It is a process-global counter, so tests read a DELTA.
func readWalkRegressions(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.WalkthroughRegressions.Write(&m); err != nil {
		t.Fatalf("read regressions counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// driveTraceInfra is a browser-stall trace exactly as crawler.Drive's session watchdog
// returns it: failed, NO steps, the stall hint, InfraFailed set.
func driveTraceInfra() *crawler.DriveTrace {
	return &crawler.DriveTrace{Outcome: "failed", Reason: crawler.StallHint, InfraFailed: true}
}

// TestGeneratorInfraFailedTraceIsNotARegression is the #45 end-to-end regression test
// through the real generator: a stalled browser (Outcome=failed, zero steps,
// InfraFailed) against a SUCCESSFUL baseline must NOT produce a regression, must not
// bump the regression metric, and must surface as infra_failed on the read-API.
func TestGeneratorInfraFailedTraceIsNotARegression(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(driveTraceInfra()))
	tgt := seedDriveTarget(t, app, true, true)

	base, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(base.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	before := readWalkRegressions(t)
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rw.Code)
	}
	wk := waitWalkthroughDone(t, app, tgt.ID)
	if wk.PrevWalkthroughID != base.ID {
		t.Fatalf("not baseline-linked: prev=%q want %q", wk.PrevWalkthroughID, base.ID)
	}
	if !wk.InfraFailed {
		t.Fatal("the persisted walkthrough is missing infra_failed — the trace's in-band signal was dropped")
	}

	var d report.WalkthroughDiff
	if wk.DiffJSON == "" {
		t.Fatal("no diff persisted at drive end")
	}
	if err := json.Unmarshal([]byte(wk.DiffJSON), &d); err != nil {
		t.Fatalf("diff_json: %v", err)
	}
	if d.IsRegression {
		t.Error("a browser stall was scored as a walkthrough REGRESSION (#45)")
	}
	if d.OutcomeCompared || !d.InfraFailed {
		t.Errorf("outcome_compared=%v infra_failed=%v, want false/true", d.OutcomeCompared, d.InfraFailed)
	}
	if after := readWalkRegressions(t); after != before {
		t.Errorf("auditloop_walkthrough_regressions_total moved %v→%v on an INFRA failure", before, after)
	}

	// Read-API: a CI consumer must be able to tell "could not run" from "regressed".
	arw := httptest.NewRecorder()
	app.handleAuditWalkthrough(arw, walkReq("GET", "/api/audit/walkthroughs/"+wk.ID, nil, map[string]string{"id": wk.ID}))
	if arw.Code != http.StatusOK {
		t.Fatalf("read-API = %d, want 200", arw.Code)
	}
	var got apiWalkthrough
	if err := json.Unmarshal(arw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Regression == nil {
		t.Fatal("regression block missing")
	}
	if got.Regression.IsRegression || got.Regression.OutcomeCompared || !got.Regression.InfraFailed {
		t.Fatalf("read-API block wrong: %+v", got.Regression)
	}
	// The raw JSON must carry both new keys (the CI gate reads them by name).
	if !json.Valid(arw.Body.Bytes()) || !containsKey(arw.Body.Bytes(), "outcome_compared") || !containsKey(arw.Body.Bytes(), "infra_failed") {
		t.Fatalf("read-API JSON missing outcome_compared/infra_failed: %s", arw.Body.String())
	}
}

// TestGeneratorProductFailedTraceIsStillARegression is the DISCRIMINATING CONTROL for
// the test above: the identical success→failed transition WITHOUT the infra flag must
// still be a regression and must still bump the metric. Without this, the infra test
// would pass against a blanket "never regress" bug.
func TestGeneratorProductFailedTraceIsStillARegression(t *testing.T) {
	trace := &crawler.DriveTrace{Outcome: "failed", Reason: "off-domain navigate refused"}
	app := walkTestApp(t, true, stubDrive(trace))
	tgt := seedDriveTarget(t, app, true, true)

	base, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(base.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	before := readWalkRegressions(t)
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rw.Code)
	}
	wk := waitWalkthroughDone(t, app, tgt.ID)
	if wk.InfraFailed {
		t.Fatal("a product-side driver failure must NOT be flagged infra_failed")
	}
	var d report.WalkthroughDiff
	if err := json.Unmarshal([]byte(wk.DiffJSON), &d); err != nil {
		t.Fatalf("diff_json %q: %v", wk.DiffJSON, err)
	}
	if !d.IsRegression || !d.OutcomeCompared {
		t.Fatalf("control: a genuine success→failed must stay a scored regression: %+v", d)
	}
	if after := readWalkRegressions(t); after <= before {
		t.Errorf("control: the regression metric did not bump (%v→%v)", before, after)
	}
}

// TestGeneratorInfraErrorIsFlagged covers the OUT-OF-BAND path: Drive returning
// (nil, err) wrapping crawler.ErrDriverInfra (browser start / render probe / enable
// interception) must flag the walkthrough too — with a plain-error control.
func TestGeneratorInfraErrorIsFlagged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantInfra bool
	}{
		{"a wrapped ErrDriverInfra flags the walkthrough", fmt.Errorf("driver: browser start: %w", crawler.ErrDriverInfra), true},
		{"a wrapped ErrBrowserStalled flags the walkthrough", fmt.Errorf("drive: %w", crawler.ErrBrowserStalled), true},
		{"a plain driver error does NOT (control)", fmt.Errorf("driver: no planner"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			derr := tc.err
			app := walkTestApp(t, true, func(ctx context.Context, opts crawler.DriveOptions) (*crawler.DriveTrace, error) {
				return nil, derr
			})
			tgt := seedDriveTarget(t, app, true, true)
			rw := httptest.NewRecorder()
			app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
			if rw.Code != http.StatusOK {
				t.Fatalf("start = %d, want 200", rw.Code)
			}
			wk := waitWalkthroughDone(t, app, tgt.ID)
			if wk.InfraFailed != tc.wantInfra {
				t.Fatalf("infra_failed = %v, want %v (reason %q)", wk.InfraFailed, tc.wantInfra, wk.Reason)
			}
		})
	}
}

// TestGeneratorPanicIsInfraFailed pins the handler's panic-recovery finisher: a panic
// inside OUR driver is not evidence about the audited product, so the walkthrough must
// be flagged infra and must not score as a regression against a successful baseline.
func TestGeneratorPanicIsInfraFailed(t *testing.T) {
	app := walkTestApp(t, true, func(ctx context.Context, opts crawler.DriveOptions) (*crawler.DriveTrace, error) {
		panic("boom: untrusted planner JSON blew up the driver")
	})
	tgt := seedDriveTarget(t, app, true, true)
	base, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(base.ID, db.WalkOutcomeSuccess, 0, "reached", false)

	before := readWalkRegressions(t)
	rw := httptest.NewRecorder()
	app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
	if rw.Code != http.StatusOK {
		t.Fatalf("start = %d, want 200", rw.Code)
	}
	wk := waitWalkthroughDone(t, app, tgt.ID)
	if !wk.InfraFailed {
		t.Fatalf("a panic in the driver must be recorded as an infra failure (#45); reason=%q", wk.Reason)
	}
	if after := readWalkRegressions(t); after != before {
		t.Errorf("the regression metric moved %v→%v on a driver PANIC", before, after)
	}
	// And it must not become the next walkthrough's baseline.
	next, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if next.PrevWalkthroughID != base.ID {
		t.Errorf("baseline = %q, want the last real walkthrough %q", next.PrevWalkthroughID, base.ID)
	}
}

// TestReadAPIInfraFailedWithoutADiff is finding #6: the out-of-band + config failure
// paths never reach RefreshWalkthroughDiff, and a target's FIRST walkthrough has no
// baseline — so there is NO `regression` block at all. A consumer must still be able to
// tell the driver could not run, which is why infra_failed is TOP-LEVEL.
func TestReadAPIInfraFailedWithoutADiff(t *testing.T) {
	t.Run("config failure with no diff", func(t *testing.T) {
		// driving_enabled ON so the route admits it, but NO audit config → the generator's
		// config-failure path: g.fail(..., infra=true), no diff computed.
		app := walkTestApp(t, true, stubDrive(nil))
		tgt := seedDriveTarget(t, app, true, true)
		// Remove the success condition so the generator fails setup rather than driving.
		if err := app.DB.SetTargetAuditConfig(&db.TargetAuditConfig{
			TargetID: tgt.ID, PrimaryJob: "sign up", Confirmed: true,
		}); err != nil {
			t.Fatal(err)
		}
		wk, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
		if _, err := app.DB.ClaimWalkthroughJob(wk.ID, 5); err != nil {
			t.Fatal(err)
		}
		if err := app.Walk.Run(context.Background(), wk.ID); err != nil {
			t.Fatalf("run: %v", err)
		}
		got := readWalkthroughAPI(t, app, wk.ID)
		if got.Regression != nil {
			t.Fatalf("expected no regression block (no baseline/diff), got %+v", got.Regression)
		}
		if !got.InfraFailed {
			t.Fatal("a setup failure with no diff must still report top-level infra_failed (#45)")
		}
	})

	t.Run("first walkthrough stall has no baseline", func(t *testing.T) {
		app := walkTestApp(t, true, stubDrive(driveTraceInfra()))
		tgt := seedDriveTarget(t, app, true, true)
		rw := httptest.NewRecorder()
		app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
		if rw.Code != http.StatusOK {
			t.Fatalf("start = %d, want 200", rw.Code)
		}
		wk := waitWalkthroughDone(t, app, tgt.ID)
		if wk.PrevWalkthroughID != "" {
			t.Fatalf("expected no baseline on a first walkthrough, got %q", wk.PrevWalkthroughID)
		}
		got := readWalkthroughAPI(t, app, wk.ID)
		if got.Regression != nil {
			t.Fatalf("expected no regression block on a first walkthrough, got %+v", got.Regression)
		}
		if !got.InfraFailed {
			t.Fatal("a first-walkthrough stall must still report top-level infra_failed (#45)")
		}
	})

	// CONTROL: a walkthrough that genuinely ran reports infra_failed=false, so the field
	// is a real signal and not a constant.
	t.Run("a real walkthrough is not flagged", func(t *testing.T) {
		app := walkTestApp(t, true, stubDrive(&crawler.DriveTrace{Outcome: "success"}))
		tgt := seedDriveTarget(t, app, true, true)
		rw := httptest.NewRecorder()
		app.handleStartWalkthrough(rw, walkReq("POST", "/api/targets/"+tgt.ID+"/walkthrough", url.Values{}, map[string]string{"id": tgt.ID}))
		wk := waitWalkthroughDone(t, app, tgt.ID)
		if got := readWalkthroughAPI(t, app, wk.ID); got.InfraFailed {
			t.Fatal("control: a walkthrough that ran must report infra_failed=false")
		}
	})
}

// TestApplyDiffCompatBackfillsOnlyPre45Blobs covers the back-fill that keeps every
// walkthrough diff persisted BEFORE migration 0064 from being misread. Those blobs have
// no "outcome_compared" key, so they decode to false — which would badge every
// historical walkthrough "Could not run" and hand the new CI predicate a false infra
// verdict. Key ABSENCE is the discriminator, so the two directions must be tested
// separately: absent ⇒ back-fill, present ⇒ never touch.
func TestApplyDiffCompatBackfillsOnlyPre45Blobs(t *testing.T) {
	t.Run("pre-#45 blob (key absent) is treated as compared", func(t *testing.T) {
		// A real pre-#45 shape: a scored regression, no #45 fields at all.
		raw := `{"prev_walkthrough_id":"prev","prev_outcome":"success","outcome":"stuck",` +
			`"outcome_changed":true,"is_regression":true,"new_task_blockers":[],"blockers_compared":false}`
		var d report.WalkthroughDiff
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		if d.OutcomeCompared {
			t.Fatal("precondition: a pre-#45 blob must decode to OutcomeCompared=false")
		}
		applyDiffCompat(raw, &d)
		if !d.OutcomeCompared {
			t.Error("a pre-#45 diff must be back-filled as COMPARED — it was scored before infra existed")
		}
		if d.InfraFailed {
			t.Error("a pre-#45 diff must not be reported as an infra failure")
		}
		// The real verdict must survive the back-fill untouched.
		if !d.IsRegression {
			t.Error("the historical regression verdict was lost")
		}
	})

	t.Run("post-#45 infra blob is left untouched", func(t *testing.T) {
		// The blob a real infra failure writes today: the keys are PRESENT and false/true.
		raw := `{"prev_walkthrough_id":"prev","prev_outcome":"success","outcome":"failed",` +
			`"outcome_changed":true,"is_regression":false,"new_task_blockers":[],` +
			`"blockers_compared":false,"outcome_compared":false,"infra_failed":true}`
		var d report.WalkthroughDiff
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		applyDiffCompat(raw, &d)
		if d.OutcomeCompared {
			t.Error("a post-#45 blob's explicit outcome_compared=false was overwritten — the back-fill must key on ABSENCE, not on the value")
		}
		if !d.InfraFailed {
			t.Error("a post-#45 blob's explicit infra_failed=true was cleared")
		}
	})

	t.Run("post-#45 compared blob is left untouched", func(t *testing.T) {
		raw := `{"prev_walkthrough_id":"prev","outcome":"success","is_regression":false,` +
			`"new_task_blockers":[],"blockers_compared":true,"outcome_compared":true,"infra_failed":false}`
		var d report.WalkthroughDiff
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		applyDiffCompat(raw, &d)
		if !d.OutcomeCompared || d.InfraFailed {
			t.Errorf("a normal post-#45 blob was mutated: compared=%v infra=%v", d.OutcomeCompared, d.InfraFailed)
		}
	})

	t.Run("malformed JSON mutates nothing", func(t *testing.T) {
		d := report.WalkthroughDiff{OutcomeCompared: false, InfraFailed: true}
		applyDiffCompat(`{not json`, &d)
		if d.OutcomeCompared || !d.InfraFailed {
			t.Errorf("malformed raw JSON must leave the value untouched, got compared=%v infra=%v", d.OutcomeCompared, d.InfraFailed)
		}
	})
}

// TestLegacyDiffRendersAsComparedNotInfra is the USER-VISIBLE half of the back-fill: a
// walkthrough carrying a pre-#45 diff blob must still render its real verdict, not the
// neutral "Could not run" card, and must not report a false infra verdict to a CI gate.
func TestLegacyDiffRendersAsComparedNotInfra(t *testing.T) {
	app := walkTestApp(t, true, stubDrive(nil))
	tgt := seedDriveTarget(t, app, true, true)
	base, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(base.ID, db.WalkOutcomeSuccess, 0, "reached", false)
	wk, _ := app.DB.CreateWalkthrough(tgt.ID, "", "sign up", true)
	_ = app.DB.FinishWalkthrough(wk.ID, db.WalkOutcomeStuck, 2, "budget exhausted", false)
	// A blob in the PRE-#45 shape (no outcome_compared / infra_failed keys).
	legacy := `{"prev_walkthrough_id":"` + base.ID + `","prev_outcome":"success","outcome":"stuck",` +
		`"outcome_changed":true,"is_regression":true,"new_task_blockers":[],"blockers_compared":false}`
	if err := app.DB.SetWalkthroughDiff(wk.ID, legacy); err != nil {
		t.Fatal(err)
	}
	fresh, err := app.DB.GetWalkthroughByID(wk.ID)
	if err != nil {
		t.Fatal(err)
	}

	vm := walkChangesVM(fresh)
	if !vm.OutcomeCompared {
		t.Error("a legacy diff must render as COMPARED, not as an unusable comparison")
	}
	if vm.InfraFailed {
		t.Error("a legacy diff must not render as an infra failure")
	}
	if !vm.IsRegression {
		t.Error("the legacy regression verdict was lost in the view model")
	}

	got := readWalkthroughAPI(t, app, wk.ID)
	if got.Regression == nil {
		t.Fatal("regression block missing")
	}
	if !got.Regression.OutcomeCompared || got.Regression.InfraFailed {
		t.Errorf("read-API misreports a legacy diff as infra: compared=%v infra=%v",
			got.Regression.OutcomeCompared, got.Regression.InfraFailed)
	}
	if !got.Regression.IsRegression {
		t.Error("read-API lost the legacy regression verdict")
	}
}

// readWalkthroughAPI fetches a walkthrough through the owner-scoped read-API.
func readWalkthroughAPI(t *testing.T, app *App, id string) apiWalkthrough {
	t.Helper()
	rw := httptest.NewRecorder()
	app.handleAuditWalkthrough(rw, walkReq("GET", "/api/audit/walkthroughs/"+id, nil, map[string]string{"id": id}))
	if rw.Code != http.StatusOK {
		t.Fatalf("read-API = %d, want 200", rw.Code)
	}
	var got apiWalkthrough
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The key must be present by NAME — a CI gate reads it off the JSON, not off Go.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rw.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw["infra_failed"]; !ok {
		t.Fatalf("read-API JSON has no top-level infra_failed key: %s", rw.Body.String())
	}
	return got
}

// containsKey reports whether a JSON object's top-level "regression" block declares key.
func containsKey(body []byte, key string) bool {
	var envelope struct {
		Regression map[string]json.RawMessage `json:"regression"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	_, ok := envelope.Regression[key]
	return ok
}
