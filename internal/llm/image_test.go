package llm

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// makePNG builds a solid-color PNG of the given size (test fixture).
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeDims(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestDownscaleCapsLongestSide(t *testing.T) {
	// A tall capture (300 × 4000) exceeds the cap on its height.
	src := makePNG(t, 300, 4000)
	out, w, h, err := downscale(src, MaxImageDim)
	if err != nil {
		t.Fatalf("downscale: %v", err)
	}
	if h != MaxImageDim {
		t.Errorf("longest side = %d, want %d", h, MaxImageDim)
	}
	// Aspect ratio preserved: 300/4000 == w/1568.
	wantW := 300 * MaxImageDim / 4000
	if w != wantW {
		t.Errorf("width = %d, want ~%d (aspect preserved)", w, wantW)
	}
	// The encoded bytes really carry the reduced dimensions.
	gw, gh := decodeDims(t, out)
	if gw != w || gh != h {
		t.Errorf("encoded dims = %dx%d, want %dx%d", gw, gh, w, h)
	}
	if gw > MaxImageDim || gh > MaxImageDim {
		t.Errorf("encoded image %dx%d exceeds cap %d", gw, gh, MaxImageDim)
	}
}

func TestDownscaleWideImage(t *testing.T) {
	src := makePNG(t, 5000, 400) // wide
	_, w, h, err := downscale(src, MaxImageDim)
	if err != nil {
		t.Fatal(err)
	}
	if w != MaxImageDim {
		t.Errorf("width = %d, want %d", w, MaxImageDim)
	}
	if h >= 400 || h <= 0 {
		t.Errorf("height = %d, want reduced <400", h)
	}
}

func TestDownscaleSmallImageUnchanged(t *testing.T) {
	src := makePNG(t, 800, 600)
	_, w, h, err := downscale(src, MaxImageDim)
	if err != nil {
		t.Fatal(err)
	}
	if w != 800 || h != 600 {
		t.Errorf("small image dims changed: %dx%d, want 800x600", w, h)
	}
}

func TestDownscaleBadInput(t *testing.T) {
	if _, _, _, err := downscale([]byte("not an image"), MaxImageDim); err == nil {
		t.Error("expected an error decoding garbage")
	}
}
