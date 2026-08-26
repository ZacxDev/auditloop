package diff

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// solidPNG builds a w×h opaque PNG filled with c.
func solidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return encode(t, img)
}

// pngWithChanges clones a solid base and repaints the first n pixels (row-major)
// with alt.
func pngWithChanges(t *testing.T, w, h int, base, alt color.RGBA, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	painted := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if painted < n {
				img.Set(x, y, alt)
				painted++
			} else {
				img.Set(x, y, base)
			}
		}
	}
	return encode(t, img)
}

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

var (
	white = color.RGBA{255, 255, 255, 255}
	black = color.RGBA{0, 0, 0, 255}
)

func TestCompareIdentical(t *testing.T) {
	img := solidPNG(t, 20, 10, white)
	res, err := Compare(img, img)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if res.ChangedPixels != 0 || res.DiffPct != 0 {
		t.Errorf("identical images: changed=%d pct=%.4f, want 0/0", res.ChangedPixels, res.DiffPct)
	}
	if res.SizeChanged {
		t.Error("identical images should not report SizeChanged")
	}
	if res.TotalPixels != 200 {
		t.Errorf("total pixels = %d, want 200", res.TotalPixels)
	}
	// The diff PNG must decode and match the current dimensions.
	assertDecodes(t, res.DiffPNG, 20, 10)
}

func TestCompareKnownChange(t *testing.T) {
	const w, h, n = 10, 10, 25 // 25 of 100 pixels → 25%
	base := solidPNG(t, w, h, white)
	cur := pngWithChanges(t, w, h, white, black, n)
	res, err := Compare(base, cur)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if res.ChangedPixels != n {
		t.Errorf("changed pixels = %d, want %d", res.ChangedPixels, n)
	}
	if res.TotalPixels != w*h {
		t.Errorf("total = %d, want %d", res.TotalPixels, w*h)
	}
	if res.DiffPct != 25.0 {
		t.Errorf("diff pct = %.4f, want 25.0000", res.DiffPct)
	}
	if res.SizeChanged {
		t.Error("same-size images should not report SizeChanged")
	}
	assertDecodes(t, res.DiffPNG, w, h)
}

func TestCompareToleranceIgnoresNoise(t *testing.T) {
	// A uniform per-channel shift of 10 (< DefaultTolerance 16) must read as 0%.
	base := solidPNG(t, 8, 8, color.RGBA{100, 100, 100, 255})
	noisy := solidPNG(t, 8, 8, color.RGBA{110, 110, 110, 255})
	res, err := Compare(base, noisy)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if res.ChangedPixels != 0 {
		t.Errorf("sub-tolerance noise flagged %d changed pixels, want 0", res.ChangedPixels)
	}
	// A shift of 40 (> tolerance) must flag every pixel.
	big := solidPNG(t, 8, 8, color.RGBA{140, 140, 140, 255})
	res2, _ := Compare(base, big)
	if res2.ChangedPixels != 64 {
		t.Errorf("supra-tolerance change flagged %d pixels, want 64", res2.ChangedPixels)
	}
}

func TestCompareDifferentDimensions(t *testing.T) {
	// Baseline 10×10 all white; current 10×20 all white (page grew taller). The
	// overlap (10×10) matches; the extra 10×10 rows are new → changed. Union is
	// 10×20=200, changed=100 → 50%.
	base := solidPNG(t, 10, 10, white)
	cur := solidPNG(t, 10, 20, white)
	res, err := Compare(base, cur)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !res.SizeChanged {
		t.Error("expected SizeChanged for differing dimensions")
	}
	if res.TotalPixels != 200 {
		t.Errorf("total = %d, want 200 (union)", res.TotalPixels)
	}
	if res.ChangedPixels != 100 {
		t.Errorf("changed = %d, want 100 (the new rows)", res.ChangedPixels)
	}
	if res.DiffPct != 50.0 {
		t.Errorf("pct = %.4f, want 50", res.DiffPct)
	}
	// Diff image is sized to the current image.
	assertDecodes(t, res.DiffPNG, 10, 20)
}

func TestCompareShrunk(t *testing.T) {
	// Baseline taller than current (page shrank): overlap matches, baseline-only
	// rows count as changed. Union 10×20=200, changed=100 → 50%.
	base := solidPNG(t, 10, 20, white)
	cur := solidPNG(t, 10, 10, white)
	res, err := Compare(base, cur)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !res.SizeChanged || res.TotalPixels != 200 || res.ChangedPixels != 100 || res.DiffPct != 50 {
		t.Errorf("shrunk diff wrong: %+v", res)
	}
	assertDecodes(t, res.DiffPNG, 10, 10)
}

func TestCompareOverCapSkipsVisualization(t *testing.T) {
	// A 10×10 current image with a tiny cap (4 px) simulates an over-cap capture
	// WITHOUT allocating a huge image: the visualization is skipped (TooLarge=true,
	// DiffPNG=nil) but diff_pct is still computed from the counting loop.
	base := solidPNG(t, 10, 10, white)
	cur := pngWithChanges(t, 10, 10, white, black, 50) // 50/100 pixels changed → 50%
	res, err := compareTolCap(base, cur, DefaultTolerance, 4)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !res.TooLarge {
		t.Error("expected TooLarge for a capture over the visualization cap")
	}
	if res.DiffPNG != nil {
		t.Errorf("over-cap compare must not allocate/return a diff image, got %d bytes", len(res.DiffPNG))
	}
	if res.DiffPct != 50.0 {
		t.Errorf("diff pct = %.4f, want 50 (still computed even when over cap)", res.DiffPct)
	}
	if res.ChangedPixels != 50 {
		t.Errorf("changed pixels = %d, want 50", res.ChangedPixels)
	}
	// Under-cap: the visualization is produced as normal.
	ok, err := compareTolCap(base, cur, DefaultTolerance, 1000)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if ok.TooLarge || ok.DiffPNG == nil {
		t.Errorf("under-cap compare should visualize: TooLarge=%v pngLen=%d", ok.TooLarge, len(ok.DiffPNG))
	}
	// The default public Compare (24 MP cap) visualizes a normal small image.
	def, _ := Compare(base, cur)
	if def.TooLarge || def.DiffPNG == nil {
		t.Errorf("default Compare should visualize a small image: TooLarge=%v", def.TooLarge)
	}
}

func TestCompareRejectsNonPNG(t *testing.T) {
	if _, err := Compare([]byte("not a png"), solidPNG(t, 2, 2, white)); err == nil {
		t.Error("expected an error decoding a non-PNG baseline")
	}
	if _, err := Compare(solidPNG(t, 2, 2, white), []byte("nope")); err == nil {
		t.Error("expected an error decoding a non-PNG current")
	}
}

func assertDecodes(t *testing.T, b []byte, w, h int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("diff PNG did not decode: %v", err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		t.Errorf("diff PNG dims = %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), w, h)
	}
}
