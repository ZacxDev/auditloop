package llm

import (
	"bytes"
	"fmt"
	"image"
	"image/png"

	// Registered so DecodeImage can read JPEG screenshots too, not just PNG.
	_ "image/jpeg"

	xdraw "golang.org/x/image/draw"
)

// MaxImageDim is the longest-side pixel cap applied to every screenshot before it
// is sent to the vision model. Full-page captures can be many thousands of pixels
// tall; sending them verbatim is wasteful (and the model down-samples anyway), so
// we cap the longest side. Anthropic's vision guidance recommends ~1568px.
const MaxImageDim = 1568

// downscale decodes an encoded image (PNG/JPEG), and if its longest side exceeds
// maxDim, resamples it down preserving aspect ratio. It always returns PNG bytes
// plus the final width/height. An image already within the cap is re-encoded
// unchanged in dimensions (so callers get consistent PNG output).
func downscale(src []byte, maxDim int) (out []byte, w, h int, err error) {
	if maxDim <= 0 {
		maxDim = MaxImageDim
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("llm: decode image: %w", err)
	}
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, 0, 0, fmt.Errorf("llm: empty image bounds")
	}

	nw, nh := scaledDims(sw, sh, maxDim)
	if nw == sw && nh == sh {
		// Within the cap already — re-encode as PNG unchanged.
		buf, encErr := encodePNG(img)
		return buf, sw, sh, encErr
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// CatmullRom is a good-quality resampler; screenshots are downscaled rarely
	// (once per capture per pass), so quality over speed is fine.
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	buf, encErr := encodePNG(dst)
	return buf, nw, nh, encErr
}

// scaledDims returns the width/height after clamping the longest side to maxDim,
// preserving aspect ratio. When both sides are already within the cap it returns
// the input dimensions unchanged.
func scaledDims(w, h, maxDim int) (int, int) {
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= maxDim {
		return w, h
	}
	ratio := float64(maxDim) / float64(longest)
	nw := int(float64(w) * ratio)
	nh := int(float64(h) * ratio)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return nw, nh
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("llm: encode png: %w", err)
	}
	return buf.Bytes(), nil
}
