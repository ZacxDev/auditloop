package db

import (
	"sync"
	"testing"
)

func TestWalkthroughLifecycle(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)

	wk, err := d.CreateWalkthrough(tgt.ID, "", "sign up", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wk.Status != WalkIdle || !wk.DryRun {
		t.Fatalf("fresh walkthrough wrong: %+v", wk)
	}

	// Claim → driving, records the budget.
	won, err := d.ClaimWalkthroughJob(wk.ID, 20)
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.Status != WalkDriving || got.StepsTotal != 20 {
		t.Fatalf("after claim: %+v", got)
	}

	// Progress + cost accumulate.
	_ = d.UpdateWalkthroughProgress(wk.ID, 3)
	_ = d.AddWalkthroughCost(wk.ID, 0.01, 100, 50)
	_ = d.AddWalkthroughCost(wk.ID, 0.02, 200, 60)
	got, _ = d.GetWalkthroughByID(wk.ID)
	if got.StepsDone != 3 || got.PromptTokens != 300 || got.CompletionTokens != 110 {
		t.Fatalf("progress/cost wrong: %+v", got)
	}
	if got.CostUSD < 0.0299 || got.CostUSD > 0.0301 {
		t.Fatalf("cost = %v want ~0.03", got.CostUSD)
	}

	// Steps.
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{WalkthroughID: wk.ID, Idx: 0, ActionJSON: `{"type":"click"}`, URL: "https://t.test/", Outcome: "ok"}); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertWalkthroughStep(&WalkthroughStep{WalkthroughID: wk.ID, Idx: 1, ActionJSON: `{"type":"finish"}`, URL: "https://t.test/done", Outcome: "success"}); err != nil {
		t.Fatalf("insert step: %v", err)
	}
	steps, err := d.ListWalkthroughSteps(wk.ID)
	if err != nil || len(steps) != 2 || steps[0].Idx != 0 || steps[1].Outcome != "success" {
		t.Fatalf("list steps: %v %+v", err, steps)
	}

	// Finish → done + outcome.
	if err := d.FinishWalkthrough(wk.ID, WalkOutcomeSuccess, 2, "reached", false); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = d.GetWalkthroughByID(wk.ID)
	if got.Status != WalkDone || got.Outcome != WalkOutcomeSuccess || got.StuckStep != 2 || got.Reason != "reached" {
		t.Fatalf("after finish: %+v", got)
	}
}

func TestClaimWalkthroughAtomicityAndCostReset(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	wk, _ := d.CreateWalkthrough(tgt.ID, "", "goal", true)

	// First claim wins; a concurrent second claim while driving loses.
	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if won, _ := d.ClaimWalkthroughJob(wk.ID, 20); won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winning claim, got %d", wins)
	}

	// Accumulate cost, finish, then a fresh claim RESETS the cost + outcome.
	_ = d.AddWalkthroughCost(wk.ID, 0.05, 500, 300)
	_ = d.FinishWalkthrough(wk.ID, WalkOutcomeStuck, 5, "budget", false)
	if won, _ := d.ClaimWalkthroughJob(wk.ID, 10); !won {
		t.Fatal("re-claim of a finished job should win")
	}
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.CostUSD != 0 || got.PromptTokens != 0 || got.Outcome != "" || got.StuckStep != 0 || got.StepsTotal != 10 {
		t.Fatalf("re-claim did not reset: %+v", got)
	}
}

func TestGetWalkthroughOwnerScoped(t *testing.T) {
	d := testDB(t)
	ta, _ := d.CreateTarget("user-a", "A", "https://a.test", nil)
	wk, _ := d.CreateWalkthrough(ta.ID, "", "goal", true)

	if _, err := d.GetWalkthrough("user-a", wk.ID); err != nil {
		t.Fatalf("owner should read own walkthrough: %v", err)
	}
	// A different user must NOT see it (owner-scoped via target→user join).
	if _, err := d.GetWalkthrough("user-b", wk.ID); err != ErrNotFound {
		t.Fatalf("cross-user get = %v, want ErrNotFound", err)
	}

	// Latest-for-target is owner-scoped too.
	if got, err := d.LatestWalkthroughForTarget("user-a", ta.ID); err != nil || got == nil || got.ID != wk.ID {
		t.Fatalf("latest for owner: %v %+v", err, got)
	}
	if got, err := d.LatestWalkthroughForTarget("user-b", ta.ID); err != nil || got != nil {
		t.Fatalf("latest for non-owner should be nil: %v %+v", err, got)
	}
}

func TestMarkDrivingWalkthroughsFailed(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	wk, _ := d.CreateWalkthrough(tgt.ID, "", "goal", true)
	if _, err := d.ClaimWalkthroughJob(wk.ID, 20); err != nil {
		t.Fatal(err)
	}
	n, err := d.MarkDrivingWalkthroughsFailed()
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	got, _ := d.GetWalkthroughByID(wk.ID)
	if got.Status != WalkFailed || got.Outcome != WalkOutcomeFailed || got.Reason == "" {
		t.Fatalf("swept walkthrough: %+v", got)
	}
}

func TestDrivingConfigScanAndSet(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	// Defaults: both off.
	if tgt.DrivingEnabled || tgt.AllowRealSubmit {
		t.Fatalf("fresh target should have driving off: %+v", tgt)
	}
	if err := d.SetDrivingConfig("u", tgt.ID, true, true); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := d.GetTarget("u", tgt.ID)
	if !got.DrivingEnabled || !got.AllowRealSubmit {
		t.Fatalf("driving flags not persisted: %+v", got)
	}
	// Ownership-scoped: a foreign user can't flip them.
	if err := d.SetDrivingConfig("other", tgt.ID, false, false); err != ErrNotFound {
		t.Fatalf("cross-user set = %v, want ErrNotFound", err)
	}
}

func TestAuditConfigSuccessAssertionPersists(t *testing.T) {
	d := testDB(t)
	tgt, _ := d.CreateTarget("u", "T", "https://t.test", nil)
	cfg := &TargetAuditConfig{
		TargetID:           tgt.ID,
		PrimaryJob:         "sign up",
		SuccessSelector:    "#done",
		SuccessURLContains: "/welcome",
		SuccessTimeoutMs:   8000,
		Confirmed:          true,
	}
	if err := d.SetTargetAuditConfig(cfg); err != nil {
		t.Fatalf("set config: %v", err)
	}
	got, found, err := d.GetTargetAuditConfig("u", tgt.ID)
	if err != nil || !found {
		t.Fatalf("get config: %v found=%v", err, found)
	}
	if got.SuccessSelector != "#done" || got.SuccessURLContains != "/welcome" || got.SuccessTimeoutMs != 8000 {
		t.Fatalf("success assertion not persisted: %+v", got)
	}
	// Cross-user isolation.
	if _, found, _ := d.GetTargetAuditConfig("other", tgt.ID); found {
		t.Fatal("foreign user should not read the config")
	}
}
