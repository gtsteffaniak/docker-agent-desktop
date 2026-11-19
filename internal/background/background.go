// Package background parses the desktop wallpaper setting and renders it.
//
// The same setting drives two views that must match: the portal page (CSS) and API
// screenshots (pixels). Only a strict subset of CSS is accepted so the value can be
// emitted into a page safely: a plain #rrggbb colour, or a linear-gradient of hex stops.
package background

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Default is the purple-to-black wallpaper used by Quantum Bots.
const Default = "linear-gradient(135deg, #2b0a4a 0%, #1a0533 28%, #0b0018 62%, #000000 100%)"

// fallback is the flat colour used when the background is "none".
var fallback = color.RGBA{0x2e, 0x34, 0x40, 0xff}

type stop struct {
	pos float64 // 0..1
	c   color.RGBA
}

// Spec is a parsed background. The zero value is not usable; call Parse.
type Spec struct {
	none  bool
	angle float64 // degrees, CSS convention (0 = up, 90 = right)
	stops []stop

	mu     sync.Mutex
	cached *image.RGBA
}

// Parse accepts "none", "#rgb", "#rrggbb", or "linear-gradient([<n>deg,] <stop>, ...)"
// where each stop is a hex colour with an optional percentage.
func Parse(s string) (*Spec, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Parse(Default)
	case strings.EqualFold(s, "none"):
		return &Spec{none: true}, nil
	case strings.HasPrefix(s, "#"):
		c, err := parseHex(s)
		if err != nil {
			return nil, err
		}
		return &Spec{angle: 180, stops: []stop{{0, c}, {1, c}}}, nil
	}
	const prefix = "linear-gradient("
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, ")") {
		return nil, fmt.Errorf("background: want none, #rrggbb or linear-gradient(...), got %q", s)
	}
	parts := strings.Split(s[len(prefix):len(s)-1], ",")
	spec := &Spec{angle: 180}
	if first := strings.TrimSpace(parts[0]); strings.HasSuffix(first, "deg") {
		a, err := strconv.ParseFloat(strings.TrimSuffix(first, "deg"), 64)
		if err != nil || math.IsNaN(a) || math.IsInf(a, 0) {
			return nil, fmt.Errorf("background: bad angle %q", first)
		}
		spec.angle = a
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return nil, fmt.Errorf("background: a gradient needs at least two colours")
	}
	explicit := make([]bool, len(parts))
	for i, p := range parts {
		fields := strings.Fields(p)
		if len(fields) == 0 || len(fields) > 2 {
			return nil, fmt.Errorf("background: bad stop %q", strings.TrimSpace(p))
		}
		c, err := parseHex(fields[0])
		if err != nil {
			return nil, err
		}
		st := stop{c: c}
		if len(fields) == 2 {
			if !strings.HasSuffix(fields[1], "%") {
				return nil, fmt.Errorf("background: stop position %q must be a percentage", fields[1])
			}
			v, err := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
			if err != nil || v < 0 || v > 100 {
				return nil, fmt.Errorf("background: stop position %q out of range", fields[1])
			}
			st.pos = v / 100
			explicit[i] = true
		}
		spec.stops = append(spec.stops, st)
	}
	spec.fillPositions(explicit)
	return spec, nil
}

// fillPositions spaces unpositioned stops evenly and keeps positions non-decreasing, as CSS does.
func (s *Spec) fillPositions(explicit []bool) {
	n := len(s.stops)
	if !explicit[0] {
		s.stops[0].pos = 0
		explicit[0] = true
	}
	if !explicit[n-1] {
		s.stops[n-1].pos = 1
		explicit[n-1] = true
	}
	for i := 1; i < n-1; i++ {
		if explicit[i] {
			continue
		}
		j := i
		for !explicit[j] {
			j++
		}
		prev := s.stops[i-1].pos
		step := (s.stops[j].pos - prev) / float64(j-i+1)
		for k := i; k < j; k++ {
			s.stops[k].pos = prev + step*float64(k-i+1)
		}
		i = j
	}
	for i := 1; i < n; i++ {
		s.stops[i].pos = math.Max(s.stops[i].pos, s.stops[i-1].pos)
	}
}

func parseHex(s string) (color.RGBA, error) {
	h := strings.TrimPrefix(s, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return color.RGBA{}, fmt.Errorf("background: bad colour %q", s)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("background: bad colour %q", s)
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}, nil
}

// None reports whether the wallpaper is disabled.
func (s *Spec) None() bool { return s.none }

// CSS returns a canonical CSS value rebuilt from the parsed stops, never the raw input,
// so it is safe to place in a style element. It is empty for "none".
func (s *Spec) CSS() string {
	if s.none {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "linear-gradient(%gdeg", s.angle)
	for _, st := range s.stops {
		// Round to two decimals so float noise such as 28.000000000000004 never reaches the CSS.
		fmt.Fprintf(&b, ", #%02x%02x%02x %s%%", st.c.R, st.c.G, st.c.B,
			strconv.FormatFloat(math.Round(st.pos*10000)/100, 'f', -1, 64))
	}
	b.WriteString(")")
	return b.String()
}

// Image renders the wallpaper at w by h. The most recent size is cached, so repeated
// screenshots do not recompute every pixel. Callers must not modify the result.
func (s *Spec) Image(w, h int) *image.RGBA {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.cached.Rect.Dx() == w && s.cached.Rect.Dy() == h {
		return s.cached
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if s.none || w <= 0 || h <= 0 {
		fill(img, fallback)
		s.cached = img
		return img
	}
	// CSS gradient line: direction (sin a, -cos a) in screen space (y down), length |W sin a| + |H cos a|.
	rad := s.angle * math.Pi / 180
	dx, dy := math.Sin(rad), -math.Cos(rad)
	length := math.Abs(float64(w)*dx) + math.Abs(float64(h)*dy)
	cx, cy := float64(w)/2, float64(h)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := 0.5
			if length > 0 {
				t = 0.5 + ((float64(x)+0.5-cx)*dx+(float64(y)+0.5-cy)*dy)/length
			}
			img.SetRGBA(x, y, s.colorAt(t))
		}
	}
	s.cached = img
	return img
}

func (s *Spec) colorAt(t float64) color.RGBA {
	st := s.stops
	if t <= st[0].pos {
		return st[0].c
	}
	for i := 1; i < len(st); i++ {
		if t <= st[i].pos {
			span := st[i].pos - st[i-1].pos
			if span <= 0 {
				return st[i].c
			}
			return lerp(st[i-1].c, st[i].c, (t-st[i-1].pos)/span)
		}
	}
	return st[len(st)-1].c
}

func lerp(a, b color.RGBA, f float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*f)) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}

func fill(img *image.RGBA, c color.RGBA) {
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
}
