package crawler

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/ZacxDev/auditloop/internal/recipe"
)

// staticResolverL returns a fixed IP for any host (login preflight tests).
func staticResolverL(ip string) func(string) ([]net.IP, error) {
	return func(string) ([]net.IP, error) { return []net.IP{net.ParseIP(ip)}, nil }
}

// runLoginPreflightOnly exercises just the goto-URL guard portion of runLogin by
// giving it a context that is already cancelled — so if the preflight passes,
// the first chromedp action fails fast (not a login guard rejection). We assert
// on the *ErrLoginFailed reason to distinguish a guard rejection.
func TestLoginPreflightRejectsOffDomain(t *testing.T) {
	guard := GuardConfig{
		AllowedHosts: []string{"gated.test"},
		Resolve:      staticResolverL("93.184.216.34"),
	}
	lc := &LoginConfig{
		Steps: []recipe.Step{
			{Type: recipe.StepGoto, URL: "https://evil.other.test/login"}, // foreign domain
			{Type: recipe.StepWaitFor, Selector: "nav"},
		},
		Credentials: map[string]string{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ensure no real browser work happens
	err := runLogin(ctx, lc, guard)
	assertLoginRejected(t, err, "refused")
}

func TestLoginPreflightRejectsPrivateIP(t *testing.T) {
	guard := GuardConfig{
		AllowedHosts: []string{"10.0.0.5"}, // in allowlist, but resolves private → guard blocks
		Resolve:      staticResolverL("10.0.0.5"),
	}
	lc := &LoginConfig{
		Steps: []recipe.Step{
			{Type: recipe.StepGoto, URL: "http://10.0.0.5/login"},
			{Type: recipe.StepWaitFor, Selector: "nav"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runLogin(ctx, lc, guard)
	assertLoginRejected(t, err, "refused")
}

func TestLoginPreflightRejectsMetadataURL(t *testing.T) {
	guard := GuardConfig{AllowedHosts: []string{"169.254.169.254"}}
	lc := &LoginConfig{
		Steps: []recipe.Step{
			{Type: recipe.StepGoto, URL: "http://169.254.169.254/latest/meta-data/"},
			{Type: recipe.StepWaitFor, Selector: "nav"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runLogin(ctx, lc, guard)
	assertLoginRejected(t, err, "refused")
}

func assertLoginRejected(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a login rejection error")
	}
	lf, ok := err.(*ErrLoginFailed)
	if !ok {
		t.Fatalf("expected *ErrLoginFailed, got %T: %v", err, err)
	}
	if !strings.Contains(lf.Reason, wantSubstr) {
		t.Fatalf("reason = %q, want it to contain %q", lf.Reason, wantSubstr)
	}
}

func TestRunLoginNilIsNoop(t *testing.T) {
	if err := runLogin(context.Background(), nil, GuardConfig{}); err != nil {
		t.Fatalf("nil login should be a no-op, got %v", err)
	}
	if err := runLogin(context.Background(), &LoginConfig{}, GuardConfig{}); err != nil {
		t.Fatalf("empty login should be a no-op, got %v", err)
	}
}

// Guards the ErrLoginFailed message never surfaces the timeout-based flake as a
// panic; just a smoke check the type formats.
func TestErrLoginFailedString(t *testing.T) {
	e := &ErrLoginFailed{Reason: "boom"}
	if !strings.Contains(e.Error(), "boom") {
		t.Fatalf("Error() = %q", e.Error())
	}
}
