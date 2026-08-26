package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	// Ensure a clean env for the keys we assert on.
	for _, k := range []string{"PORT", "AUDITLOOP_ROLE", "DATABASE_DRIVER", "S3_ENDPOINT", "CRAWL_MAX_PAGES", "CRAWL_MAX_DEPTH", "DEV_MODE", "S3_BUCKET"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.Port != "8112" {
		t.Errorf("Port = %q, want 8112", c.Port)
	}
	if c.Role != RoleAll {
		t.Errorf("Role = %q, want all", c.Role)
	}
	if c.DatabaseDriver != "sqlite" {
		t.Errorf("driver = %q, want sqlite", c.DatabaseDriver)
	}
	if c.CrawlMaxPages != 50 || c.CrawlMaxDepth != 3 {
		t.Errorf("crawl caps = %d/%d, want 50/3", c.CrawlMaxPages, c.CrawlMaxDepth)
	}
	if c.S3Bucket != "audit-artifacts" {
		t.Errorf("bucket = %q", c.S3Bucket)
	}
	if !c.StorageIsLocal() {
		t.Error("expected local storage when S3_ENDPOINT unset")
	}
	if c.DevMode {
		t.Error("DevMode should default false")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PORT", "9000")
	t.Setenv("AUDITLOOP_ROLE", "Worker")
	t.Setenv("CRAWL_MAX_PAGES", "7")
	t.Setenv("CRAWL_MAX_DEPTH", "2")
	t.Setenv("DEV_MODE", "true")
	t.Setenv("S3_ENDPOINT", "minio:9000")
	t.Setenv("S3_USE_PATH_STYLE", "false")
	c := Load()
	if c.Port != "9000" {
		t.Errorf("Port = %q", c.Port)
	}
	if c.Role != RoleWorker || c.RunsWeb() || !c.RunsWorker() {
		t.Errorf("role parse wrong: %q web=%v worker=%v", c.Role, c.RunsWeb(), c.RunsWorker())
	}
	if c.CrawlMaxPages != 7 || c.CrawlMaxDepth != 2 {
		t.Errorf("caps = %d/%d", c.CrawlMaxPages, c.CrawlMaxDepth)
	}
	if !c.DevMode {
		t.Error("DevMode should be true")
	}
	if c.StorageIsLocal() {
		t.Error("expected S3 storage when endpoint set")
	}
	if c.S3UsePathStyle {
		t.Error("path style should be false")
	}
}

func TestInternalAllowHostsParse(t *testing.T) {
	// Default: empty (fully-guarded SSRF behavior).
	t.Setenv("AUDITLOOP_INTERNAL_ALLOW_HOSTS", "")
	if got := Load().InternalAllowHosts; len(got) != 0 {
		t.Errorf("default should be empty, got %v", got)
	}
	// Comma-separated, trims blanks, lowercases.
	t.Setenv("AUDITLOOP_INTERNAL_ALLOW_HOSTS", " Cluster.Internal , svc.default.svc , ,  ")
	got := Load().InternalAllowHosts
	want := []string{"cluster.internal", "svc.default.svc"}
	if len(got) != len(want) {
		t.Fatalf("parsed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parsed[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// All-blank → nil.
	t.Setenv("AUDITLOOP_INTERNAL_ALLOW_HOSTS", " , ,  ")
	if got := Load().InternalAllowHosts; len(got) != 0 {
		t.Errorf("all-blank should yield empty, got %v", got)
	}
}

func TestRoleAll(t *testing.T) {
	c := AppConfig{Role: RoleAll}
	if !c.RunsWeb() || !c.RunsWorker() {
		t.Error("RoleAll should run both")
	}
}

func TestOpenRouterConfigDefaults(t *testing.T) {
	for _, k := range []string{"OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "AUDITLOOP_LLM_MODELS", "AUDITLOOP_LLM_MAX_TOKENS", "AUDITLOOP_LLM_SYNTH_MAX_TOKENS", "AUDITLOOP_LLM_EVAL_MAX_TOKENS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.OpenRouterEnabled() {
		t.Error("OpenRouterEnabled should be false without a key")
	}
	if c.OpenRouterBaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base url = %q", c.OpenRouterBaseURL)
	}
	if c.LLMMaxTokens != 1024 {
		t.Errorf("max tokens = %d, want 1024", c.LLMMaxTokens)
	}
	// The synthesis pass has its own, larger default budget (its ranked JSON
	// overflows the per-page cap and truncates).
	if c.LLMSynthMaxTokens != 3000 {
		t.Errorf("synth max tokens = %d, want default 3000", c.LLMSynthMaxTokens)
	}
	// The per-page eval gen/verify calls get a middle-tier budget (verbose verdicts
	// overflow the 1024 notes cap and truncate), smaller than the synthesis budget.
	if c.LLMEvalMaxTokens != 2000 {
		t.Errorf("eval max tokens = %d, want default 2000", c.LLMEvalMaxTokens)
	}
	models := c.Models()
	if len(models) != 2 || models[0] != "anthropic/claude-haiku-4.5" {
		t.Errorf("default models wrong: %v", models)
	}
	// The first model is the default-checked one.
	if !c.ModelAllowed("anthropic/claude-sonnet-4.6") {
		t.Error("sonnet should be allowed by default")
	}
	if c.ModelAllowed("evil/backdoor") {
		t.Error("arbitrary model must not be allowed")
	}
}

func TestOpenRouterConfigOverrides(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-test")
	t.Setenv("OPENROUTER_BASE_URL", "http://fake.local/v1")
	t.Setenv("AUDITLOOP_LLM_MODELS", " a/model , b/model ,")
	t.Setenv("AUDITLOOP_LLM_MAX_TOKENS", "512")
	t.Setenv("AUDITLOOP_LLM_SYNTH_MAX_TOKENS", "4096")
	t.Setenv("AUDITLOOP_LLM_EVAL_MAX_TOKENS", "1800")
	c := Load()
	if !c.OpenRouterEnabled() {
		t.Error("OpenRouterEnabled should be true with a key")
	}
	if c.OpenRouterBaseURL != "http://fake.local/v1" {
		t.Errorf("base url = %q", c.OpenRouterBaseURL)
	}
	if c.LLMMaxTokens != 512 {
		t.Errorf("max tokens = %d", c.LLMMaxTokens)
	}
	if c.LLMSynthMaxTokens != 4096 {
		t.Errorf("synth max tokens = %d, want 4096", c.LLMSynthMaxTokens)
	}
	if c.LLMEvalMaxTokens != 1800 {
		t.Errorf("eval max tokens = %d, want 1800", c.LLMEvalMaxTokens)
	}
	models := c.Models()
	if len(models) != 2 || models[0] != "a/model" || models[1] != "b/model" {
		t.Errorf("parsed models = %v, want [a/model b/model]", models)
	}
	if c.ModelAllowed("anthropic/claude-haiku-4.5") {
		t.Error("default model must not be allowed once the allowlist is overridden")
	}
}
