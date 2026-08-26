// Package llm is a minimal OpenRouter vision client used by the P3 UX-notes
// feature. It POSTs a single chat/completions request carrying a text prompt plus
// one or more screenshot images (as base64 data URLs) and returns the model's
// text reply.
//
// Design notes (mirrors how a sibling Go service keeps its OpenRouter client testable):
//   - The base URL is configurable (config.OpenRouterBaseURL, default
//     https://openrouter.ai/api/v1) so tests point it at an httptest fake.
//   - The API key is server-side only and is NEVER exposed to the browser.
//   - Every image is downscaled to <= MaxImageDim on its longest side before it
//     is sent (cost guard).
//   - max_tokens is capped; the whole call is context-cancellable; a per-call
//     failure is returned as an error so the caller can degrade (non-fatal).
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultMaxTokens caps the completion length when none is configured.
const DefaultMaxTokens = 1024

// DraftOption customizes a single Draft call, overriding the client default.
type DraftOption func(*draftOptions)

type draftOptions struct {
	// maxTokens, when > 0, overrides the client's default completion cap for this
	// one call. Used e.g. by the run-level eval SYNTHESIS pass, whose ranked
	// multi-item JSON output overflows the (small) per-page cap and truncates.
	maxTokens int
}

// WithMaxTokens overrides the client's default completion cap for a single Draft
// call. A non-positive value is ignored (keeps the client default). This lets one
// caller request a larger budget without inflating every other call.
func WithMaxTokens(n int) DraftOption {
	return func(o *draftOptions) {
		if n > 0 {
			o.maxTokens = n
		}
	}
}

// ResolveMaxTokens returns the per-call max_tokens an option set requests, or 0 if
// none is set. Exposed so callers/tests (e.g. a fake Drafter) can observe the
// budget a caller asked for.
func ResolveMaxTokens(opts ...DraftOption) int {
	var o draftOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o.maxTokens
}

// Client talks to an OpenRouter-compatible chat/completions endpoint.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	MaxTokens  int
	// MaxImageDim caps the longest side of each sent image (0 → MaxImageDim const).
	MaxImageDim int
	// Referer/Title populate the optional OpenRouter attribution headers.
	Referer string
	Title   string
}

// New builds a client with sensible defaults.
func New(baseURL, apiKey string, maxTokens int) *Client {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		HTTPClient:  &http.Client{Timeout: 120 * time.Second},
		MaxTokens:   maxTokens,
		MaxImageDim: MaxImageDim,
		Referer:     "https://auditloop.local",
		Title:       "auditloop",
	}
}

// Image is one screenshot to attach to the vision prompt.
type Image struct {
	// Label is a short human tag (e.g. "desktop"/"mobile") prepended as a text
	// part so the model knows which viewport each image is.
	Label string
	// PNG is the encoded screenshot bytes (PNG or JPEG; downscaled before send).
	PNG []byte
}

// --- request/response wire types (OpenAI/OpenRouter chat/completions shape) ---

type chatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []chatMessage `json:"messages"`
	// Usage asks OpenRouter to include exact per-call accounting (token counts and
	// the USD cost) in the response's `usage` object. Without this OpenRouter omits
	// `cost` entirely. See https://openrouter.ai/docs (usage accounting).
	Usage usageOption `json:"usage"`
}

// usageOption is the `{"include": true}` opt-in that turns on cost accounting.
type usageOption struct {
	Include bool `json:"include"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"` // data:image/png;base64,....
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	// Usage is present only when the request opted in ("usage":{"include":true}).
	// `cost` is the exact charge for the call in USD (e.g. 0.000033). Providers/paths
	// that don't report usage omit this object → zero-value Usage (never an error).
	Usage *struct {
		CostUSD          float64 `json:"cost"`
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		TotalTokens      int     `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Usage is the per-call OpenRouter accounting captured from the response. It is
// zero-valued when the provider/path did not report usage (never an error).
type Usage struct {
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
}

// Draft sends system+user prompts plus the given images to the model and returns
// the reply text plus the per-call Usage (token counts + USD cost) when OpenRouter
// reports it. Each image is downscaled to the client's MaxImageDim before sending.
// A non-2xx response or an empty completion returns an error so the caller degrades
// gracefully. When usage is absent the returned Usage is the zero value (no error).
func (c *Client) Draft(ctx context.Context, model, systemPrompt, userPrompt string, images []Image, opts ...DraftOption) (string, Usage, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", Usage{}, fmt.Errorf("llm: no API key configured")
	}
	if strings.TrimSpace(model) == "" {
		return "", Usage{}, fmt.Errorf("llm: no model specified")
	}

	// Build the user message: the prompt text, then each labeled image part.
	parts := []contentPart{{Type: "text", Text: userPrompt}}
	for _, im := range images {
		if len(im.PNG) == 0 {
			continue
		}
		scaled, w, h, err := downscale(im.PNG, c.maxDim())
		if err != nil {
			return "", Usage{}, err
		}
		label := im.Label
		if label == "" {
			label = "screenshot"
		}
		parts = append(parts, contentPart{Type: "text", Text: fmt.Sprintf("Screenshot — %s viewport (%d×%dpx):", label, w, h)})
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURL{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(scaled)},
		})
	}

	messages := []chatMessage{}
	if strings.TrimSpace(systemPrompt) != "" {
		messages = append(messages, chatMessage{Role: "system", Content: []contentPart{{Type: "text", Text: systemPrompt}}})
	}
	messages = append(messages, chatMessage{Role: "user", Content: parts})

	// Per-call budget: default to the client cap, but let a caller request more for
	// this one call (e.g. the eval synthesis pass).
	maxTok := c.maxTokens()
	if o := ResolveMaxTokens(opts...); o > 0 {
		maxTok = o
	}

	body, err := json.Marshal(chatRequest{Model: model, MaxTokens: maxTok, Messages: messages, Usage: usageOption{Include: true}})
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if c.Referer != "" {
		req.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		req.Header.Set("X-Title", c.Title)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", Usage{}, fmt.Errorf("llm: openrouter status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", Usage{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", Usage{}, fmt.Errorf("llm: openrouter error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", Usage{}, fmt.Errorf("llm: empty completion")
	}
	var usage Usage
	if parsed.Usage != nil {
		usage = Usage{
			CostUSD:          parsed.Usage.CostUSD,
			PromptTokens:     parsed.Usage.PromptTokens,
			CompletionTokens: parsed.Usage.CompletionTokens,
		}
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), usage, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) maxTokens() int {
	if c.MaxTokens <= 0 {
		return DefaultMaxTokens
	}
	return c.MaxTokens
}

func (c *Client) maxDim() int {
	if c.MaxImageDim <= 0 {
		return MaxImageDim
	}
	return c.MaxImageDim
}
