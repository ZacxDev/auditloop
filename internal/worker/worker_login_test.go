package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/crypto"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/recipe"
)

// seedLoginTarget creates a target in auth_mode=login with an encrypted recipe.
func seedLoginTarget(t *testing.T, d *db.DB, c *crypto.Cipher) *db.Target {
	t.Helper()
	tgt, err := d.CreateTarget("u", "Gated", "https://gated.test", []string{"gated.test"})
	if err != nil {
		t.Fatal(err)
	}
	steps := recipe.GuidedForm{
		LoginURL:         "https://gated.test/login",
		UsernameSelector: "#email",
		PasswordSelector: "#password",
		SubmitSelector:   "button[type=submit]",
		SuccessSelector:  "nav.dash",
		SuccessTimeoutMs: 8000,
	}.Compile()
	stepsJSON, _ := recipe.MarshalSteps(steps)
	credBlob, _ := recipe.Credentials{Username: "alice@gated.test", Password: "hunter2"}.Marshal()
	enc, _ := c.EncryptToBase64(credBlob)
	if err := d.SetLoginRecipe(&db.LoginRecipe{
		TargetID: tgt.ID, LoginURL: steps[0].URL, StepsJSON: stepsJSON,
		SuccessSelector: "nav.dash", SuccessTimeoutMs: 8000, CredsEncrypted: enc,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetTargetByID(tgt.ID)
	return got
}

func newCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.New(make([]byte, crypto.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBuildLoginDecryptsCreds(t *testing.T) {
	d, st := setup(t)
	c := newCipher(t)
	tgt := seedLoginTarget(t, d, c)

	w := New(d, st, "", 50, 3)
	w.Cipher = c
	lc, err := w.buildLogin(tgt)
	if err != nil {
		t.Fatalf("buildLogin: %v", err)
	}
	if lc == nil {
		t.Fatal("expected a login config")
	}
	if lc.Credentials[recipe.RefUsername] != "alice@gated.test" || lc.Credentials[recipe.RefPassword] != "hunter2" {
		t.Fatalf("credentials not decrypted: %v", lc.Credentials)
	}
	if len(lc.Steps) != 5 || lc.Steps[0].Type != recipe.StepGoto {
		t.Fatalf("steps not parsed: %+v", lc.Steps)
	}
}

func TestBuildLoginMissingCipher(t *testing.T) {
	d, st := setup(t)
	c := newCipher(t)
	tgt := seedLoginTarget(t, d, c)

	w := New(d, st, "", 50, 3)
	w.Cipher = nil // key not configured
	if _, err := w.buildLogin(tgt); err == nil || !strings.Contains(err.Error(), "encryption key") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestBuildLoginNoneModeIsNil(t *testing.T) {
	d, st := setup(t)
	tgt, _ := d.CreateTarget("u", "Public", "https://pub.test", []string{"pub.test"})
	w := New(d, st, "", 50, 3)
	w.Cipher = newCipher(t)
	lc, err := w.buildLogin(tgt)
	if err != nil || lc != nil {
		t.Fatalf("non-login target should yield (nil,nil), got (%v,%v)", lc, err)
	}
}

// TestProcessRunLoginRunsBeforeCrawl asserts the worker passes the resolved
// (decrypted) login config into the crawler BEFORE crawling.
func TestProcessRunLoginRunsBeforeCrawl(t *testing.T) {
	d, st := setup(t)
	c := newCipher(t)
	tgt := seedLoginTarget(t, d, c)
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()

	var gotLogin *crawler.LoginConfig
	w := New(d, st, "", 50, 3)
	w.Cipher = c
	w.Crawl = func(_ context.Context, opts crawler.Options) (*crawler.Result, error) {
		gotLogin = opts.Login
		return fakeCrawl(context.Background(), opts)
	}
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process: %v", err)
	}
	if gotLogin == nil {
		t.Fatal("crawler was not given a login config")
	}
	if gotLogin.Credentials[recipe.RefPassword] != "hunter2" {
		t.Fatalf("login creds not substituted: %v", gotLogin.Credentials)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.Status != db.RunDone {
		t.Fatalf("run status = %q", got.Status)
	}
}

// TestProcessRunLoginFailureFailsRun asserts a login failure fails the run with
// the recipe hint and skips crawling (no pages persisted).
func TestProcessRunLoginFailureFailsRun(t *testing.T) {
	d, st := setup(t)
	c := newCipher(t)
	tgt := seedLoginTarget(t, d, c)
	run, _ := d.CreateRun("u", tgt.ID)
	claimed, _ := d.ClaimNextQueuedRun()

	w := New(d, st, "", 50, 3)
	w.Cipher = c
	w.Crawl = func(_ context.Context, _ crawler.Options) (*crawler.Result, error) {
		return nil, &crawler.ErrLoginFailed{Reason: "success element \"nav.dash\" not found in time"}
	}
	if err := w.ProcessRun(context.Background(), claimed); err != nil {
		t.Fatalf("process returned err (should record failure, not error): %v", err)
	}
	got, _ := d.GetRun("u", run.ID)
	if got.Status != db.RunFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "login recipe failed") {
		t.Fatalf("run error = %q, want login-failed message", got.Error)
	}
	// Crawl was skipped → no pages.
	pages, _ := d.ListPages(run.ID)
	if len(pages) != 0 {
		t.Fatalf("expected no pages on login failure, got %d", len(pages))
	}
	// Credential value must not leak into the persisted error.
	if strings.Contains(got.Error, "hunter2") {
		t.Fatal("credential value leaked into run error")
	}
}
