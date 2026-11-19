package background

import (
	"image/color"
	"strings"
	"testing"
)

func near(a, b color.RGBA, tol int) bool {
	d := func(x, y uint8) int {
		if x > y {
			return int(x - y)
		}
		return int(y - x)
	}
	return d(a.R, b.R) <= tol && d(a.G, b.G) <= tol && d(a.B, b.B) <= tol
}

func TestDefaultCornersMatchStops(t *testing.T) {
	s, err := Parse(Default)
	if err != nil {
		t.Fatal(err)
	}
	img := s.Image(400, 300)
	if got, want := img.RGBAAt(0, 0), (color.RGBA{0x2b, 0x0a, 0x4a, 0xff}); !near(got, want, 6) {
		t.Fatalf("top-left = %v, want about %v", got, want)
	}
	if got, want := img.RGBAAt(399, 299), (color.RGBA{0, 0, 0, 0xff}); !near(got, want, 6) {
		t.Fatalf("bottom-right = %v, want about %v", got, want)
	}
}

func TestEmptyUsesDefault(t *testing.T) {
	s, err := Parse("  ")
	if err != nil || s.CSS() == "" {
		t.Fatalf("err=%v css=%q", err, s.CSS())
	}
}

func TestSolidColourAndShortHex(t *testing.T) {
	s, err := Parse("#f00")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Image(4, 4).RGBAAt(2, 2); got != (color.RGBA{0xff, 0, 0, 0xff}) {
		t.Fatalf("pixel = %v", got)
	}
}

func TestNone(t *testing.T) {
	s, err := Parse("none")
	if err != nil || !s.None() || s.CSS() != "" {
		t.Fatalf("err=%v none=%v css=%q", err, s.None(), s.CSS())
	}
	if s.Image(2, 2).RGBAAt(0, 0) != fallback {
		t.Fatal("none should render the flat fallback colour")
	}
}

func TestUnpositionedStopsAreSpacedEvenly(t *testing.T) {
	s, err := Parse("linear-gradient(90deg, #000000, #808080, #ffffff)")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.stops[1].pos; got < 0.499 || got > 0.501 {
		t.Fatalf("middle stop at %v, want 0.5", got)
	}
}

func TestRejectsUnsafeOrMalformedInput(t *testing.T) {
	for _, in := range []string{
		"red",
		"url(http://x/y.png)",
		"linear-gradient(135deg, #000)",
		"linear-gradient(135deg, #000000, #ffffff);}</style><script>x</script>",
		"linear-gradient(135deg, #000000, expression(alert(1)))",
		"linear-gradient(135deg, #000000 150%, #ffffff)",
		"linear-gradient(135deg, #000000 10px, #ffffff)",
		"linear-gradient(NaNdeg, #000000, #ffffff)",
		"#12345",
		"#gggggg",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted, want error", in)
		}
	}
}

func TestCSSIsCanonicalAndContainsNoRawInput(t *testing.T) {
	s, err := Parse("linear-gradient(135DEG,#ABC,#000000 100%)")
	if err == nil {
		t.Fatalf("uppercase DEG should be rejected, got css %q", s.CSS())
	}
	s, err = Parse("linear-gradient( 135deg , #AABBCC , #000000 100% )")
	if err != nil {
		t.Fatal(err)
	}
	css := s.CSS()
	if !strings.HasPrefix(css, "linear-gradient(135deg, #aabbcc 0%") || strings.ContainsAny(css, "<>;{}") {
		t.Fatalf("css = %q", css)
	}
}

func TestImageCachesLastSize(t *testing.T) {
	s, _ := Parse(Default)
	if s.Image(10, 10) != s.Image(10, 10) {
		t.Fatal("same size should return the cached image")
	}
	if s.Image(10, 10) == s.Image(12, 10) {
		t.Fatal("different size should re-render")
	}
}

func TestDefaultCSSHasNoFloatNoise(t *testing.T) {
	s, err := Parse(Default)
	if err != nil {
		t.Fatal(err)
	}
	want := "linear-gradient(135deg, #2b0a4a 0%, #1a0533 28%, #0b0018 62%, #000000 100%)"
	if got := s.CSS(); got != want {
		t.Fatalf("CSS() = %q\nwant      %q", got, want)
	}
}
