package broadway

import (
	"image"
	"image/color"
	"testing"
)

func TestComposeScreenshotUsesBackground(t *testing.T) {
	bg := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(bg.Pix); i += 4 {
		bg.Pix[i], bg.Pix[i+1], bg.Pix[i+2], bg.Pix[i+3] = 10, 20, 30, 255
	}
	out := ComposeScreenshot(map[uint16]*Surface{}, 4, 4, bg)
	if got := out.RGBAAt(1, 1); got != (color.RGBA{10, 20, 30, 255}) {
		t.Fatalf("pixel = %v, want background", got)
	}
}

func TestComposeScreenshotFlatWhenNoBackground(t *testing.T) {
	out := ComposeScreenshot(map[uint16]*Surface{}, 2, 2, nil)
	if out.RGBAAt(0, 0) != flatBackground {
		t.Fatalf("pixel = %v, want flat colour", out.RGBAAt(0, 0))
	}
}

func TestComposeScreenshotBlendsTranslucentSurfaceOverBackground(t *testing.T) {
	bg := image.NewRGBA(image.Rect(0, 0, 2, 1))
	for i := 0; i < len(bg.Pix); i += 4 {
		bg.Pix[i], bg.Pix[i+1], bg.Pix[i+2], bg.Pix[i+3] = 0, 0, 200, 255
	}
	// One fully transparent pixel and one opaque red pixel.
	pix := []byte{0, 0, 0, 0, 255, 0, 0, 255}
	surf := &Surface{ID: 1, Width: 2, Height: 1, Visible: true, Pixels: pix}
	out := ComposeScreenshot(map[uint16]*Surface{1: surf}, 2, 1, bg)
	if got := out.RGBAAt(0, 0); got.B != 200 {
		t.Fatalf("transparent pixel = %v, want background showing through", got)
	}
	if got := out.RGBAAt(1, 0); got.R != 255 || got.B != 0 {
		t.Fatalf("opaque pixel = %v, want red", got)
	}
}

func TestComposeScreenshotIgnoresColourInTransparentPixels(t *testing.T) {
	bg := image.NewRGBA(image.Rect(0, 0, 2, 1))
	for i := 0; i < len(bg.Pix); i += 4 {
		bg.Pix[i], bg.Pix[i+1], bg.Pix[i+2], bg.Pix[i+3] = 8, 0, 18, 255
	}
	// Stale green in a fully transparent pixel, and a channel above its alpha.
	pix := []byte{7, 255, 17, 0, 200, 10, 10, 100}
	surf := &Surface{ID: 1, Width: 2, Height: 1, Visible: true, Pixels: pix}
	out := ComposeScreenshot(map[uint16]*Surface{1: surf}, 2, 1, bg)
	if got := out.RGBAAt(0, 0); got != (color.RGBA{8, 0, 18, 255}) {
		t.Fatalf("transparent pixel changed the wallpaper: %v", got)
	}
	// Red was 200 with alpha 100. Clamped to 100, it blends to about 100 + 8*(155/255) = 105.
	if got := out.RGBAAt(1, 0); got.R < 100 || got.R > 110 || got.A != 255 {
		t.Fatalf("over-bright channel not clamped: %v", got)
	}
}
