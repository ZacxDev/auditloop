package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capturedRequest is what the fake OpenRouter server parsed from the request.
type capturedRequest struct {
	Model        string
	MaxTokens    int
	Auth         string
	Referer      string
	Title        string
	UsageInclude bool
	TextParts    []string
	Images       [][]byte // decoded image bytes, in order
}

// fakeOpenRouter stands up an httptest server that records the request and, on
// success, returns a canned completion. status<300 → success. When withUsage is
// true the success body carries a usage object (cost + tokens); otherwise it omits
// usage entirely (mimicking a provider that doesn't report cost).
func fakeOpenRouter(t *testing.T, status int, completion string, captured *capturedRequest) *httptest.Server {
	return fakeOpenRouterUsage(t, status, completion, captured, true)
}

func fakeOpenRouterUsage(t *testing.T, status int, completion string, captured *capturedRequest, withUsage bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Usage     struct {
				Include bool `json:"include"`
			} `json:"usage"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if captured != nil {
			captured.Model = req.Model
			captured.MaxTokens = req.MaxTokens
			captured.Auth = r.Header.Get("Authorization")
			captured.Referer = r.Header.Get("HTTP-Referer")
			captured.Title = r.Header.Get("X-Title")
			captured.UsageInclude = req.Usage.Include
			for _, m := range req.Messages {
				for _, part := range m.Content {
					switch part.Type {
					case "text":
						captured.TextParts = append(captured.TextParts, part.Text)
					case "image_url":
						if part.ImageURL != nil {
							if i := strings.Index(part.ImageURL.URL, ","); i >= 0 {
								raw, err := base64.StdEncoding.DecodeString(part.ImageURL.URL[i+1:])
								if err == nil {
									captured.Images = append(captured.Images, raw)
								}
							}
						}
					}
				}
			}
		}
		if status >= 300 {
			http.Error(w, "boom", status)
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": completion}},
			},
		}
		if withUsage {
			resp["usage"] = map[string]any{
				"prompt_tokens":     1200,
				"completion_tokens": 340,
				"total_tokens":      1540,
				"cost":              0.000033,
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestDraftSendsBothImagesAndModel(t *testing.T) {
	var cap capturedRequest
	srv := fakeOpenRouter(t, 200, "## UX notes\n- Looks fine.", &cap)
	defer srv.Close()

	c := New(srv.URL, "test-key", 777)
	// A desktop shot that exceeds the cap (must be downscaled) + a small mobile shot.
	desktop := makePNG(t, 400, 5000)
	mobile := makePNG(t, 390, 800)

	out, usage, err := c.Draft(context.Background(), "anthropic/claude-haiku-4.5",
		"system prompt", "user prompt about the page",
		[]Image{{Label: "desktop", PNG: desktop}, {Label: "mobile", PNG: mobile}})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if !strings.Contains(out, "UX notes") {
		t.Errorf("completion not parsed: %q", out)
	}
	// The request opted into usage accounting.
	if !cap.UsageInclude {
		t.Error(`request must carry "usage":{"include":true}`)
	}
	// The usage object was parsed back off the response.
	if usage.CostUSD != 0.000033 {
		t.Errorf("usage cost = %v, want 0.000033", usage.CostUSD)
	}
	if usage.PromptTokens != 1200 || usage.CompletionTokens != 340 {
		t.Errorf("usage tokens = %d/%d, want 1200/340", usage.PromptTokens, usage.CompletionTokens)
	}

	if cap.Model != "anthropic/claude-haiku-4.5" {
		t.Errorf("model = %q", cap.Model)
	}
	if cap.MaxTokens != 777 {
		t.Errorf("max_tokens = %d, want 777", cap.MaxTokens)
	}
	if cap.Auth != "Bearer test-key" {
		t.Errorf("auth header = %q", cap.Auth)
	}
	if cap.Referer == "" || cap.Title == "" {
		t.Errorf("expected HTTP-Referer + X-Title headers, got %q / %q", cap.Referer, cap.Title)
	}
	// BOTH images were sent.
	if len(cap.Images) != 2 {
		t.Fatalf("expected 2 image parts, got %d", len(cap.Images))
	}
	// The user prompt text made it through.
	joined := strings.Join(cap.TextParts, "\n")
	if !strings.Contains(joined, "user prompt about the page") {
		t.Errorf("user prompt missing from parts: %v", cap.TextParts)
	}
	// The oversized desktop image was downscaled to <= the cap.
	for i, raw := range cap.Images {
		cfg, _, err := image.DecodeConfig(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("image %d decode: %v", i, err)
		}
		if cfg.Width > MaxImageDim || cfg.Height > MaxImageDim {
			t.Errorf("sent image %d is %dx%d, exceeds cap %d", i, cfg.Width, cfg.Height, MaxImageDim)
		}
	}
	// Specifically: the 400x5000 desktop shot must be capped at 1568 tall.
	cfg0, _, _ := image.DecodeConfig(strings.NewReader(string(cap.Images[0])))
	if cfg0.Height != MaxImageDim {
		t.Errorf("desktop shot height = %d, want %d (downscaled)", cfg0.Height, MaxImageDim)
	}
}

func TestDraftServerErrorDegrades(t *testing.T) {
	srv := fakeOpenRouter(t, 500, "", nil)
	defer srv.Close()
	c := New(srv.URL, "k", 100)
	_, _, err := c.Draft(context.Background(), "m", "s", "u", []Image{{Label: "desktop", PNG: makePNG(t, 100, 100)}})
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestDraftNoKey(t *testing.T) {
	c := New("http://unused", "", 100)
	if _, _, err := c.Draft(context.Background(), "m", "s", "u", nil); err == nil {
		t.Error("expected error without an API key")
	}
}

func TestDraftMaxTokensDefault(t *testing.T) {
	var cap capturedRequest
	srv := fakeOpenRouter(t, 200, "ok", &cap)
	defer srv.Close()
	c := New(srv.URL, "k", 0) // 0 → default
	if _, _, err := c.Draft(context.Background(), "m", "", "u", nil); err != nil {
		t.Fatal(err)
	}
	if cap.MaxTokens != DefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", cap.MaxTokens, DefaultMaxTokens)
	}
}

// TestDraftMaxTokensOverride verifies WithMaxTokens overrides the client's default
// cap for a single call (the request body carries the larger budget) without
// changing the client default.
func TestDraftMaxTokensOverride(t *testing.T) {
	var cap capturedRequest
	srv := fakeOpenRouter(t, 200, "ok", &cap)
	defer srv.Close()
	c := New(srv.URL, "k", 1024) // client default 1024
	if _, _, err := c.Draft(context.Background(), "m", "", "u", nil, WithMaxTokens(3000)); err != nil {
		t.Fatal(err)
	}
	if cap.MaxTokens != 3000 {
		t.Errorf("max_tokens = %d, want the per-call override 3000", cap.MaxTokens)
	}
	// A second call WITHOUT the option falls back to the client default.
	cap = capturedRequest{}
	if _, _, err := c.Draft(context.Background(), "m", "", "u", nil); err != nil {
		t.Fatal(err)
	}
	if cap.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want the client default 1024 (override must not persist)", cap.MaxTokens)
	}
	// A non-positive override is ignored (keeps the client default).
	cap = capturedRequest{}
	if _, _, err := c.Draft(context.Background(), "m", "", "u", nil, WithMaxTokens(0)); err != nil {
		t.Fatal(err)
	}
	if cap.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024 (a 0 override is ignored)", cap.MaxTokens)
	}
}

// TestResolveMaxTokens is the small primitive fakes use to observe the requested budget.
func TestResolveMaxTokens(t *testing.T) {
	if got := ResolveMaxTokens(); got != 0 {
		t.Errorf("no opts → %d, want 0", got)
	}
	if got := ResolveMaxTokens(WithMaxTokens(3000)); got != 3000 {
		t.Errorf("override → %d, want 3000", got)
	}
	if got := ResolveMaxTokens(WithMaxTokens(-5)); got != 0 {
		t.Errorf("non-positive override → %d, want 0 (ignored)", got)
	}
}

// TestDraftNoUsageZeroValue verifies a response WITHOUT a usage object yields a
// zero-value Usage and still returns the completion text (no error).
func TestDraftNoUsageZeroValue(t *testing.T) {
	var cap capturedRequest
	srv := fakeOpenRouterUsage(t, 200, "## notes\n- ok", &cap, false)
	defer srv.Close()
	c := New(srv.URL, "k", 100)
	out, usage, err := c.Draft(context.Background(), "m", "", "u", nil)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if !strings.Contains(out, "notes") {
		t.Errorf("completion not returned: %q", out)
	}
	if usage != (Usage{}) {
		t.Errorf("usage should be zero when the response omits it, got %+v", usage)
	}
	// The request still opts in — the absence is the provider's, not ours.
	if !cap.UsageInclude {
		t.Error(`request must still carry "usage":{"include":true}`)
	}
}
