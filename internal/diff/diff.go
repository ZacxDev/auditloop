// Package diff computes regression diffs between two audit runs: a pure-Go
// per-pixel visual diff of PNG screenshots (stdlib image only, no dependency)
// plus small deterministic set-delta helpers used for page-set, a11y-rule, and
// console/network deltas. Everything here is pure and deterministic so it is
// cheaply and exhaustively unit-tested.
package diff

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// DefaultTolerance is the per-channel color-distance below which two pixels are
// considered equal. It absorbs trivial anti-alias / PNG-encoding noise so a
// re-render of an unchanged page reports ~0% rather than a sea of false diffs.
const DefaultTolerance = 16

// MaxVisualizationPixels caps the current-image area (width × height) for which
// we allocate the red-tinted diff visualization. Crawler screenshots are
// FULL-PAGE, so a long/tall page can be tens of megapixels; a naive
// image.NewRGBA(w, h) for the visualization would allocate w×h×4 bytes (a 24 MP
// page ≈ 96 MB) on top of the two decoded source images. Above this cap we skip
// the visualization allocation (and set Result.TooLarge) — diff_pct is still
// computed from a cheap counting loop over the already-decoded pixels. 24 MP is
// generously above a normal full-page capture (e.g. 1280 × ~18 700 px).
const MaxVisualizationPixels = 24_000_000

// Result is the outcome of a visual diff between a baseline and a current PNG.
type Result struct {
	// DiffPct is the fraction of changed pixels as a 0–100 percentage, measured
	// over the UNION bounding box (so a page that grew/shrank counts the
	// non-overlapping area as changed). NOTE: a raw full-page diff_pct is only a
	// trustworthy visual-regression signal when SizeChanged is false — when the
	// two captures differ in height the rows below the change misalign and inflate
	// diff_pct toward 100%. Callers must gate the regression signal on
	// !SizeChanged (see report.ChangedPage.IsRegression / worker.runDiff).
	DiffPct float64
	// ChangedPixels is the number of pixels that differ (overlap mismatches plus
	// the entire non-overlapping area).
	ChangedPixels int
	// TotalPixels is the union area (max width × max height) — the denominator.
	TotalPixels int
	// SizeChanged is true when the two images have different dimensions (a strong
	// signal on its own: page height/width shifted).
	SizeChanged bool
	// TooLarge is true when the current image exceeded MaxVisualizationPixels, so
	// the diff visualization was skipped to bound memory. DiffPct is still valid;
	// DiffPNG is nil.
	TooLarge bool
	// DiffPNG is a visualization: the current image dimmed, with changed pixels
	// tinted red. Decodable PNG bytes, or nil when TooLarge.
	DiffPNG []byte
}

// Compare diffs two PNG-encoded screenshots using DefaultTolerance.
func Compare(baseline, current []byte) (Result, error) {
	return CompareTol(baseline, current, DefaultTolerance)
}

// CompareTol diffs two PNG-encoded screenshots with an explicit per-channel
// tolerance. Different dimensions are handled gracefully: the overlapping region
// is compared pixel-by-pixel and the non-overlapping area is counted as changed
// (never panics on a size mismatch).
func CompareTol(baseline, current []byte, tol uint8) (Result, error) {
	return compareTolCap(baseline, current, tol, MaxVisualizationPixels)
}

// compareTolCap is CompareTol with an explicit visualization pixel cap (injected
// so the cap behavior is unit-testable without allocating a huge image).
func compareTolCap(baseline, current []byte, tol uint8, maxViz int) (Result, error) {
	base, err := decodePNG(baseline)
	if err != nil {
		return Result{}, err
	}
	cur, err := decodePNG(current)
	if err != nil {
		return Result{}, err
	}

	bb := base.Bounds()
	cb := cur.Bounds()
	bw, bh := bb.Dx(), bb.Dy()
	cw, ch := cb.Dx(), cb.Dy()

	overlapW := min(bw, cw)
	overlapH := min(bh, ch)
	unionW := max(bw, cw)
	unionH := max(bh, ch)

	res := Result{
		TotalPixels: unionW * unionH,
		SizeChanged: bw != cw || bh != ch,
	}

	// The diff visualization is sized to the CURRENT image (what the UI shows):
	// dim the current pixels, then tint changed ones red. Skip the allocation
	// entirely for oversized captures — diff_pct is still counted below.
	genViz := maxViz <= 0 || cw*ch <= maxViz
	res.TooLarge = !genViz
	var out *image.RGBA
	if genViz {
		out = image.NewRGBA(image.Rect(0, 0, cw, ch))
	}

	// Count mismatches within the overlap; draw the whole current image (when
	// generating the visualization).
	overlapMismatches := 0
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			cr, cg, cb8 := rgb8(cur.At(cb.Min.X+x, cb.Min.Y+y))
			changed := false
			if x < overlapW && y < overlapH {
				br, bg, bb8 := rgb8(base.At(bb.Min.X+x, bb.Min.Y+y))
				if channelDelta(cr, br) > tol || channelDelta(cg, bg) > tol || channelDelta(cb8, bb8) > tol {
					changed = true
					overlapMismatches++
				}
			} else {
				// Present in current but outside the baseline overlap: new area.
				changed = true
			}
			if genViz {
				if changed {
					out.Set(x, y, tintRed(cr, cg, cb8))
				} else {
					out.Set(x, y, dim(cr, cg, cb8))
				}
			}
		}
	}
	// Changed = overlap mismatches + the entire non-overlapping area (pixels that
	// exist in exactly one image — either the current grew past the baseline, or
	// the baseline had rows/cols the current no longer does).
	nonOverlap := unionW*unionH - overlapW*overlapH
	res.ChangedPixels = overlapMismatches + nonOverlap

	if res.TotalPixels > 0 {
		res.DiffPct = 100 * float64(res.ChangedPixels) / float64(res.TotalPixels)
	}

	if genViz {
		var buf bytes.Buffer
		if err := png.Encode(&buf, out); err != nil {
			return Result{}, err
		}
		res.DiffPNG = buf.Bytes()
	}
	return res, nil
}

// (min/max are Go 1.21+ builtins.)

func decodePNG(b []byte) (image.Image, error) {
	return png.Decode(bytes.NewReader(b))
}

// rgb8 returns the 8-bit R,G,B channels of a color (alpha ignored — screenshots
// are opaque).
func rgb8(c color.Color) (uint8, uint8, uint8) {
	r, g, b, _ := c.RGBA() // 16-bit per channel
	return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)
}

func channelDelta(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

// dim darkens a base pixel so the red overlay reads clearly.
func dim(r, g, b uint8) color.RGBA {
	return color.RGBA{R: r / 3, G: g / 3, B: b / 3, A: 255}
}

// tintRed marks a changed pixel: strong red, keeping a little of the original
// luminance so structure remains visible.
func tintRed(r, g, b uint8) color.RGBA {
	_ = r
	return color.RGBA{R: 255, G: g / 4, B: b / 4, A: 255}
}
