package broadway

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"maps"
	"slices"
)

// Limits that stop a hostile or buggy broadwayd from forcing huge allocations.
const (
	maxSurfaceDim    = 16384
	maxSurfacePixels = 1 << 24 // 16 Mpx ≈ 64 MiB RGBA
	maxInflatedBytes = 64 << 20
)

// ErrCorrupt is returned when a PUT_BUFFER payload violates the Broadway buffer format.
var ErrCorrupt = errors.New("broadway: corrupt buffer")

// decodeBuffer expands a Broadway PUT_BUFFER payload into RGBA pixels (port of broadway.js decodeBuffer).
// Every write is bounds-checked against the w×h output, so malformed input returns an error
// instead of panicking.
func decodeBuffer(old []byte, oldW, oldH int, w, h int, data []byte) ([]byte, error) {
	if w <= 0 || h <= 0 || w > maxSurfaceDim || h > maxSurfaceDim || w*h > maxSurfacePixels {
		return nil, fmt.Errorf("invalid dimensions %dx%d", w, h)
	}
	total := w * h * 4
	out := make([]byte, total)
	if old != nil && len(old) == oldW*oldH*4 {
		copyRect(old, 0, 0, oldW, out, 0, 0, w, min(oldW, w), min(oldH, h))
	}
	// need reports whether n more output bytes fit at dest.
	need := func(dest, n int) error {
		if dest < 0 || n < 0 || dest+n > total {
			return fmt.Errorf("%w: output overflow at %d", ErrCorrupt, dest)
		}
		return nil
	}
	src := 0
	dest := 0
	for src < len(data) {
		if src+4 > len(data) {
			break
		}
		b := data[src]
		g := data[src+1]
		r := data[src+2]
		alpha := data[src+3]
		src += 4
		if alpha != 0 {
			if err := need(dest, 4); err != nil {
				return nil, err
			}
			out[dest] = r
			out[dest+1] = g
			out[dest+2] = b
			out[dest+3] = alpha
			dest += 4
			continue
		}
		cmd := r & 0xf0
		switch cmd {
		case 0x00:
			if err := need(dest, 4); err != nil {
				return nil, err
			}
			out[dest+3] = 0
			dest += 4
		case 0x10:
			// Skip run: the length is in the word just read, so no further bytes are needed.
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			if err := need(dest, lenPix*4); err != nil {
				return nil, err
			}
			dest += lenPix * 4
		case 0x20:
			if src+4 > len(data) {
				return nil, fmt.Errorf("%w: truncated block ref", ErrCorrupt)
			}
			blockID := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			destX := int(alpha)<<8 | int(r)
			destY := int(g)<<8 | int(b)
			if old == nil || oldW <= 0 {
				continue
			}
			blockStride := (oldW + 31) / 32
			srcY := (blockID / blockStride) * 32
			srcX := (blockID % blockStride) * 32
			copyRect(old, srcX, srcY, oldW, out, destX, destY, w, 32, 32)
		case 0x30:
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			if src+4 > len(data) {
				return nil, fmt.Errorf("%w: truncated color run", ErrCorrupt)
			}
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			if err := need(dest, lenPix*4); err != nil {
				return nil, err
			}
			for i := 0; i < lenPix; i++ {
				out[dest] = r
				out[dest+1] = g
				out[dest+2] = b
				out[dest+3] = alpha
				dest += 4
			}
		case 0x40:
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			if src+4 > len(data) {
				return nil, fmt.Errorf("%w: truncated delta run", ErrCorrupt)
			}
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			if err := need(dest, lenPix*4); err != nil {
				return nil, err
			}
			for i := 0; i < lenPix; i++ {
				out[dest] = byte((int(out[dest]) + int(r)) & 0xff)
				out[dest+1] = byte((int(out[dest+1]) + int(g)) & 0xff)
				out[dest+2] = byte((int(out[dest+2]) + int(b)) & 0xff)
				out[dest+3] = byte((int(out[dest+3]) + int(alpha)) & 0xff)
				dest += 4
			}
		default:
			return nil, fmt.Errorf("%w: unknown buffer command 0x%x", ErrCorrupt, cmd)
		}
	}
	return out, nil
}

// copyRect copies a w×h block between two RGBA buffers, clipping at every edge.
func copyRect(src []byte, srcX, srcY, srcStride int, dst []byte, dstX, dstY, dstStride, w, h int) {
	srcRows := len(src) / (4 * srcStride)
	dstRows := len(dst) / (4 * dstStride)
	for row := 0; row < h; row++ {
		sy := srcY + row
		dy := dstY + row
		if sy < 0 || dy < 0 || sy >= srcRows || dy >= dstRows {
			continue
		}
		for col := 0; col < w; col++ {
			sx := srcX + col
			dx := dstX + col
			if sx < 0 || dx < 0 || sx >= srcStride || dx >= dstStride {
				continue
			}
			sp := (sy*srcStride + sx) * 4
			dp := (dy*dstStride + dx) * 4
			copy(dst[dp:dp+4], src[sp:sp+4])
		}
	}
}

// inflateRaw applies raw DEFLATE (zlib.RawInflate as used by Broadway), capped at maxInflatedBytes.
func inflateRaw(compressed []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(compressed))
	defer r.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxInflatedBytes+1))
	if err != nil {
		return nil, err
	}
	if n > maxInflatedBytes {
		return nil, fmt.Errorf("%w: inflated payload exceeds %d bytes", ErrCorrupt, maxInflatedBytes)
	}
	return buf.Bytes(), nil
}

// flatBackground is the desktop colour used when no backdrop image is supplied.
var flatBackground = color.RGBA{0x2e, 0x34, 0x40, 255}

// ComposeScreenshot renders all visible surfaces into one RGBA image sized width×height,
// stacking them in ID order so the result is deterministic. bg, when non-nil, is drawn first
// as the wallpaper; otherwise the canvas is a flat colour.
func ComposeScreenshot(surfaces map[uint16]*Surface, width, height int, bg *image.RGBA) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	if bg != nil {
		draw.Draw(canvas, canvas.Bounds(), bg, bg.Bounds().Min, draw.Src)
	} else {
		draw.Draw(canvas, canvas.Bounds(), &image.Uniform{flatBackground}, image.Point{}, draw.Src)
	}
	for _, id := range slices.Sorted(maps.Keys(surfaces)) {
		s := surfaces[id]
		if !s.Visible || len(s.Pixels) == 0 {
			continue
		}
		// Draw the buffer at its own size: right after a resize it is still the old size.
		pw, ph := s.PixelsW, s.PixelsH
		if pw <= 0 || ph <= 0 {
			pw, ph = s.Width, s.Height
		}
		sub := image.NewRGBA(image.Rect(0, 0, pw, ph))
		if len(s.Pixels) >= pw*ph*4 {
			copyPremultiplied(sub.Pix, s.Pixels[:pw*ph*4])
		}
		dst := image.Rect(s.X, s.Y, s.X+pw, s.Y+ph)
		draw.Draw(canvas, dst, sub, image.Point{}, draw.Over)
	}
	return canvas
}

// copyPremultiplied copies RGBA pixels into dst and makes them valid premultiplied colour.
// Broadway leaves stale colour in fully transparent pixels, and delta runs can push a channel
// above alpha. A browser canvas ignores both, but image/draw would add that colour to the
// wallpaper, so zero transparent pixels and clamp every channel to its alpha.
func copyPremultiplied(dst, src []byte) {
	for i := 0; i+3 < len(src); i += 4 {
		a := src[i+3]
		if a == 0 {
			dst[i], dst[i+1], dst[i+2], dst[i+3] = 0, 0, 0, 0
			continue
		}
		dst[i], dst[i+1], dst[i+2], dst[i+3] = min(src[i], a), min(src[i+1], a), min(src[i+2], a), a
	}
}
