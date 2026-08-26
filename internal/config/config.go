// Package config loads runtime configuration from environment variables.
//
// It mirrors the shape of a sibling Go service's config.AppConfig, trimmed to what
// auditloop needs: server + role, database, Supabase auth, S3/MinIO object
// storage, and crawl caps.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Role selects which half of the single binary runs.
type Role string

const (
	RoleWeb    Role = "web"    // HTTP/API server only
	RoleWorker Role = "worker" // background crawl worker only
	RoleAll    Role = "all"    // both (default for local/dev)
)

// AppConfig is the fully-resolved runtime configuration.
type AppConfig struct {
	// Server / role
	Port    string
	BaseURL string
	Role    Role

	// Database (sqlite by default; postgres when DatabaseDriver=="postgres")
	DatabaseDriver string
	DatabasePath   string // sqlite file path
	DatabaseURL    string // postgres DSN

	// Supabase auth (HS256 JWT verification)
	SupabaseURL       string
	SupabaseAnonKey   string
	SupabaseJWTSecret string

	// Object storage (S3 / MinIO). When S3Endpoint is empty the app uses the
	// local-filesystem storage backend (S3Local) instead — used for hermetic
	// tests and zero-dependency local dev.
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3Region       string
	S3UsePathStyle bool
	S3UseSSL       bool
	S3Local        string // filesystem root used when S3Endpoint == ""

	// Crawl caps
	CrawlMaxPages int
	CrawlMaxDepth int
	// CrawlAllowLoopback is a dev/test-only flag that lets the crawler reach
	// loopback addresses (so a hermetic e2e can crawl a local fixture). NEVER set
	// in production — it weakens the SSRF guard. Other private ranges stay blocked.
	CrawlAllowLoopback bool
	// InternalAllowHosts is a set of EXACT-match hostnames whose resolution into a
	// private/loopback/CGNAT range is tolerated (in-cluster dev targets ONLY). It
	// NEVER relaxes link-local/metadata/multicast/unspecified. Empty in every
	// normal deployment. From AUDITLOOP_INTERNAL_ALLOW_HOSTS (comma-separated).
	InternalAllowHosts []string

	// Chromium executable path override (chromedp ExecPath). Empty → resolve
	// chromium/chrome from PATH.
	ChromiumPath string

	// --- P3: vision-LLM UX notes (OpenRouter) ---
	// OpenRouterAPIKey is the server-side API key. It is NEVER sent to the browser.
	// The whole notes feature is gated on this being non-empty (OpenRouterEnabled).
	OpenRouterAPIKey string
	// OpenRouterBaseURL is the OpenRouter API base (default
	// https://openrouter.ai/api/v1). Overridable so tests point it at an httptest
	// fake server.
	OpenRouterBaseURL string
	// LLMModels is the curated allowlist of vision model ids. The FIRST is the
	// default-checked one in the picker. Only these ids are accepted server-side —
	// an arbitrary user-supplied model id is rejected.
	LLMModels []string
	// LLMMaxTokens caps the completion length per notes/per-page eval call (cost
	// guard). A per-page verdict fits comfortably in this.
	LLMMaxTokens int
	// LLMSynthMaxTokens is the LARGER completion budget for the run-level eval
	// SYNTHESIS call only — its ranked ≤8-item JSON (each with title, rationale,
	// affected_urls/personas) overflows LLMMaxTokens and truncates mid-JSON. Applied
	// per-call via llm.WithMaxTokens, so it does NOT inflate the per-page calls.
	LLMSynthMaxTokens int
	// LLMEvalMaxTokens is the completion budget for the per-page persona-walkthrough
	// GENERATION + VERIFICATION calls. A verbose verdict (several blockers/frictions,
	// each with issue/selector/evidence, + a top_fix) overflows the 1024 notes cap and
	// truncates mid-JSON → ParseEvaluation fails ("unexpected EOF") and the cell's
	// findings are lost. Richer than a notes draft but far smaller than the run-level
	// synthesis, so it gets its own middle tier (three tiers: notes 1024 < eval 2000 <
	// synth 3000). Applied per-call via llm.WithMaxTokens; the P3 notes calls keep 1024.
	LLMEvalMaxTokens int
	// LLMDriveMaxTokens is the SMALL completion budget for the goal-directed
	// walkthrough DRIVER's per-turn planner call (Phase 3). Each turn asks for
	// exactly ONE action JSON object (a handful of fields), so the cap is tight —
	// far below the per-page eval verdict. Applied per-call via llm.WithMaxTokens.
	LLMDriveMaxTokens int

	// --- P4: login recipes (authenticated crawls) ---
	// EncryptionKey is the server-side AES-256 key (AUDITLOOP_ENCRYPTION_KEY),
	// accepted as hex (64 chars) or base64 decoding to 32 bytes. It encrypts
	// login-recipe credentials at rest and NEVER reaches the browser. The login-
	// recipe feature is gated on this being present+valid (LoginRecipesEnabled).
	EncryptionKey string

	// DevMode bypasses auth (injects a fixed dev user). Never enable in prod.
	DevMode bool
}

// DefaultLLMModels is the curated vision-model allowlist when AUDITLOOP_LLM_MODELS
// is unset. The first entry is the default-checked model in the picker.
var DefaultLLMModels = []string{
	"anthropic/claude-haiku-4.5",
	"anthropic/claude-sonnet-4.6",
}

// DefaultDevUser is the synthetic identity injected under DEV_MODE.
const DefaultDevUser = "00000000-0000-0000-0000-000000000001"

// Load resolves configuration from the environment, applying sensible defaults.
func Load() AppConfig {
	c := AppConfig{
		Port:               env("PORT", "8112"),
		BaseURL:            env("BASE_URL", "http://localhost:8112"),
		Role:               parseRole(env("AUDITLOOP_ROLE", "all")),
		DatabaseDriver:     env("DATABASE_DRIVER", "sqlite"),
		DatabasePath:       env("DATABASE_PATH", "auditloop.db"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		SupabaseURL:        os.Getenv("SUPABASE_URL"),
		SupabaseAnonKey:    os.Getenv("SUPABASE_ANON_KEY"),
		SupabaseJWTSecret:  os.Getenv("SUPABASE_JWT_SECRET"),
		S3Endpoint:         os.Getenv("S3_ENDPOINT"),
		S3Bucket:           env("S3_BUCKET", "audit-artifacts"),
		S3AccessKey:        os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:        os.Getenv("S3_SECRET_KEY"),
		S3Region:           env("S3_REGION", "us-east-1"),
		S3UsePathStyle:     envBool("S3_USE_PATH_STYLE", true),
		S3UseSSL:           envBool("S3_USE_SSL", false),
		S3Local:            env("S3_LOCAL_DIR", "artifacts"),
		CrawlMaxPages:      envInt("CRAWL_MAX_PAGES", 50),
		CrawlMaxDepth:      envInt("CRAWL_MAX_DEPTH", 3),
		CrawlAllowLoopback: envBool("CRAWL_ALLOW_LOOPBACK", false),
		InternalAllowHosts: parseHostList(os.Getenv("AUDITLOOP_INTERNAL_ALLOW_HOSTS")),
		ChromiumPath:       os.Getenv("AUDITLOOP_CHROMIUM"),
		OpenRouterAPIKey:   os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterBaseURL:  env("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		LLMModels:          parseModels(os.Getenv("AUDITLOOP_LLM_MODELS")),
		LLMMaxTokens:       envInt("AUDITLOOP_LLM_MAX_TOKENS", 1024),
		LLMSynthMaxTokens:  envInt("AUDITLOOP_LLM_SYNTH_MAX_TOKENS", 3000),
		LLMEvalMaxTokens:   envInt("AUDITLOOP_LLM_EVAL_MAX_TOKENS", 2000),
		LLMDriveMaxTokens:  envInt("AUDITLOOP_LLM_DRIVE_MAX_TOKENS", 256),
		EncryptionKey:      os.Getenv("AUDITLOOP_ENCRYPTION_KEY"),
		DevMode:            envBool("DEV_MODE", false),
	}
	return c
}

// OpenRouterEnabled reports whether the vision-LLM notes feature is available
// (true iff a server-side OpenRouter API key is configured).
func (c AppConfig) OpenRouterEnabled() bool { return strings.TrimSpace(c.OpenRouterAPIKey) != "" }

// LoginRecipesEnabled reports whether the P4 authenticated-crawl feature is
// available (true iff a server-side encryption key is configured). Whether the
// key actually parses to 32 bytes is validated when the cipher is built at
// startup; a present-but-invalid key logs and disables the feature there.
func (c AppConfig) LoginRecipesEnabled() bool { return strings.TrimSpace(c.EncryptionKey) != "" }

// Models returns the curated vision-model allowlist. The first entry is the
// default-checked one in the picker. Never empty.
func (c AppConfig) Models() []string {
	if len(c.LLMModels) == 0 {
		return DefaultLLMModels
	}
	return c.LLMModels
}

// ModelAllowed reports whether id is in the curated allowlist (server-side guard
// against arbitrary user-supplied model ids).
func (c AppConfig) ModelAllowed(id string) bool {
	for _, m := range c.Models() {
		if m == id {
			return true
		}
	}
	return false
}

// parseModels splits a comma-separated model list, trimming blanks. An empty or
// all-blank value yields nil (Load falls back to DefaultLLMModels).
func parseModels(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseHostList splits a comma-separated hostname list, trimming blanks and
// lowercasing each entry (hostnames are case-insensitive). An empty or all-blank
// value yields nil → an empty allowlist (current, fully-guarded behavior).
func parseHostList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// StorageIsLocal reports whether the filesystem storage backend is in use.
func (c AppConfig) StorageIsLocal() bool { return c.S3Endpoint == "" }

// RunsWeb / RunsWorker report which roles are active.
func (c AppConfig) RunsWeb() bool    { return c.Role == RoleWeb || c.Role == RoleAll }
func (c AppConfig) RunsWorker() bool { return c.Role == RoleWorker || c.Role == RoleAll }

func parseRole(s string) Role {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleWeb:
		return RoleWeb
	case RoleWorker:
		return RoleWorker
	default:
		return RoleAll
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
