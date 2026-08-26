package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZacxDev/auditloop/internal/llm"
)

// Persona-walkthrough Phase 2: goal INFERENCE. A SYNCHRONOUS single LLM call
// (NOT the async job machinery) that reads a completed crawl's landing screenshot
// + a compact URL digest and returns a DRAFT audit config the owner then confirms.
// It reuses the same OpenRouter client + first curated model as the Phase-1 pass.

// InferSystemPrompt instructs the model to return ONLY a structured JSON draft of
// the target's audit goals. The audiences list is constrained to the curated
// persona ids (documented inline; filtered server-side against the allowlist).
const InferSystemPrompt = `You are a product analyst. Given a screenshot of a website's landing page plus the
list of pages discovered in a crawl, infer this product's audit configuration: what the product is, the single
primary task a visitor is most likely trying to complete, the main call-to-action, and which visitor personas
are most relevant to evaluate it for.

Choose "audiences" ONLY from this fixed set of persona ids (use the ids verbatim; pick the 1-4 most relevant):
- first-time-nontechnical  (a first-time, non-technical visitor)
- returning-power-user     (a frequent expert user)
- skeptical-evaluator      (a comparison-shopping evaluator deciding whether to commit)
- accessibility-constrained (a user with low vision / keyboard-only / screen-reader / motor constraints)

Respond with ONLY a single JSON object, no prose, no markdown fences, matching EXACTLY this shape:
  {"product_summary":"one line: what this product is/does",
   "primary_job":"the main task a visitor is trying to complete (e.g. sign up and create a first project)",
   "primary_cta":"the main call-to-action label (e.g. Sign up)",
   "audiences":["first-time-nontechnical","skeptical-evaluator"]}
Base every field on what is VISIBLE in the screenshot and the page URLs. Do not invent features not shown.`

// InferredConfig is the structured draft the inference pass returns. Audiences is
// filtered to the curated persona ids by ParseInferredConfig.
type InferredConfig struct {
	ProductSummary string   `json:"product_summary"`
	PrimaryJob     string   `json:"primary_job"`
	PrimaryCTA     string   `json:"primary_cta"`
	Audiences      []string `json:"audiences"`
}

// InferUserPrompt builds the grounding user prompt: the landing URL + a compact
// digest of the crawl's page URLs (deduped, capped so a huge crawl stays bounded).
func InferUserPrompt(landingURL string, urls []string) string {
	var b strings.Builder
	if landingURL != "" {
		fmt.Fprintf(&b, "Landing page URL: %s\n\n", landingURL)
	}
	b.WriteString("Pages discovered in the crawl (the flow to reason about):\n")
	const maxURLs = 60
	shown := urls
	if len(shown) > maxURLs {
		shown = shown[:maxURLs]
	}
	for _, u := range shown {
		fmt.Fprintf(&b, "- %s\n", u)
	}
	if len(urls) > maxURLs {
		fmt.Fprintf(&b, "- …and %d more\n", len(urls)-maxURLs)
	}
	b.WriteString("\nThe first screenshot is the landing page. Return the JSON config now.")
	return b.String()
}

// ParseInferredConfig parses an inference reply into a validated draft. It is
// lenient about surrounding prose/fences (extractJSON, shared with ParseEvaluation)
// and FILTERS audiences to the curated persona allowlist (unknown ids dropped,
// order + de-dup preserved). A body with no JSON object is an error (never a
// panic) so the caller degrades cleanly.
func ParseInferredConfig(reply string) (InferredConfig, error) {
	raw, err := extractJSON(reply)
	if err != nil {
		return InferredConfig{}, err
	}
	var ic InferredConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&ic); err != nil {
		return InferredConfig{}, fmt.Errorf("decode inferred config: %w", err)
	}
	ic.ProductSummary = strings.TrimSpace(ic.ProductSummary)
	ic.PrimaryJob = strings.TrimSpace(ic.PrimaryJob)
	ic.PrimaryCTA = strings.TrimSpace(ic.PrimaryCTA)
	ic.Audiences = filterPersonas(ic.Audiences)
	return ic, nil
}

// filterPersonas keeps only curated persona ids, de-duplicated + order-preserving.
func filterPersonas(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] || !PersonaAllowed(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// InferConfig runs the synchronous single-call inference for a completed run: it
// loads the run's pages, sends the LANDING page's (first page, desktop-preferred)
// screenshot plus a URL digest to the model, and returns the parsed draft config.
// It reuses the Generator's LLM client, model, store, and eval-tier completion
// budget (a config draft is small). A no-page run or an unparseable reply returns
// a clean error (the handler surfaces it) — never a panic.
func (g *Generator) InferConfig(ctx context.Context, runID string) (InferredConfig, error) {
	pages, err := g.DB.ListPages(runID)
	if err != nil {
		return InferredConfig{}, err
	}
	if len(pages) == 0 {
		return InferredConfig{}, fmt.Errorf("run has no captured pages to infer from")
	}

	// Unique URLs in flow (creation/id) order + the landing = the first URL.
	var urls []string
	seen := map[string]bool{}
	landingURL := ""
	landingShot := ""
	for _, p := range pages {
		if !seen[p.URL] {
			seen[p.URL] = true
			urls = append(urls, p.URL)
		}
		if landingURL == "" {
			landingURL = p.URL
		}
		// Prefer the landing URL's DESKTOP screenshot; fall back to any of its shots.
		if p.URL == landingURL && p.ScreenshotKey != "" {
			if p.Viewport == "desktop" || landingShot == "" {
				landingShot = p.ScreenshotKey
			}
		}
	}

	var images []llm.Image
	if landingShot != "" {
		if b, ferr := g.fetch(ctx, landingShot); ferr == nil {
			images = append(images, llm.Image{Label: "desktop", PNG: b})
		}
	}

	genMax := g.GenMaxTokens
	if genMax <= 0 {
		genMax = DefaultEvalMaxTokens
	}
	text, _, err := g.LLM.Draft(ctx, g.Model, InferSystemPrompt, InferUserPrompt(landingURL, urls), images, llm.WithMaxTokens(genMax))
	if err != nil {
		return InferredConfig{}, err
	}
	return ParseInferredConfig(text)
}
