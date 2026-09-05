package imageanalysis

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// encodePNG renders an RGBA image of the given size to PNG bytes.
func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestResizeImageNoOpWhenSmall(t *testing.T) {
	data := encodePNG(t, 100, 100)
	out, err := resizeImage(data, "image/png", 1600, 82)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if len(out) != len(data) {
		t.Fatalf("small image should be returned unchanged, want=%d got=%d", len(data), len(out))
	}
}

func TestResizeImageDownsizesLarge(t *testing.T) {
	data := encodePNG(t, 2000, 2000)
	out, err := resizeImage(data, "image/png", 1000, 82)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode resized: %v", err)
	}
	if cfg.Width > 1000 || cfg.Height > 1000 {
		t.Fatalf("resized image should fit within 1000px, got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width != 1000 || cfg.Height != 1000 {
		t.Fatalf("square image should scale to exactly 1000x1000, got %dx%d", cfg.Width, cfg.Height)
	}
}

func TestResizeImageZeroDimSkips(t *testing.T) {
	data := encodePNG(t, 3000, 3000)
	out, err := resizeImage(data, "image/png", 0, 82)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if len(out) != len(data) {
		t.Fatalf("max dim 0 should skip resize, want=%d got=%d", len(data), len(out))
	}
}
