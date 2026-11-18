package broadway

import (
	"bytes"
	"compress/flate"
	"fmt"
	"image"
	"image/color"
	"image/draw"
)

// decodeBuffer expands a Broadway PUT_BUFFER payload into RGBA pixels (port of broadway.js decodeBuffer).
func decodeBuffer(old []byte, oldW, oldH int, w, h int, data []byte) ([]byte, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d", w, h)
	}
	out := make([]byte, w*h*4)
	if old != nil && len(old) == oldW*oldH*4 {
		copyRect(old, 0, 0, oldW, out, 0, 0, w, oldW, oldH)
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
			out[dest+3] = 0
			dest += 4
		case 0x10:
			if src+3 > len(data) {
				return nil, fmt.Errorf("truncated delta0")
			}
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			dest += lenPix * 4
		case 0x20:
			if src+7 > len(data) {
				return nil, fmt.Errorf("truncated block ref")
			}
			blockID := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			destX := int(alpha)<<8 | int(r)
			destY := int(g)<<8 | int(b)
			blockStride := (oldW + 31) / 32
			srcY := (blockID / blockStride) * 32
			srcX := (blockID % blockStride) * 32
			if old == nil {
				break
			}
			copyRect(old, srcX, srcY, oldW, out, destX, destY, w, 32, 32)
		case 0x30:
			if src+3 > len(data) {
				return nil, fmt.Errorf("truncated color run header")
			}
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			if src+4 > len(data) {
				return nil, fmt.Errorf("truncated color run")
			}
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			for i := 0; i < lenPix; i++ {
				out[dest] = r
				out[dest+1] = g
				out[dest+2] = b
				out[dest+3] = alpha
				dest += 4
			}
		case 0x40:
			if src+3 > len(data) {
				return nil, fmt.Errorf("truncated delta run header")
			}
			lenPix := (int(r&0x0f) << 16) | (int(g) << 8) | int(b)
			if src+4 > len(data) {
				return nil, fmt.Errorf("truncated delta run")
			}
			b = data[src]
			g = data[src+1]
			r = data[src+2]
			alpha = data[src+3]
			src += 4
			for i := 0; i < lenPix; i++ {
				out[dest] = byte((int(out[dest]) + int(r)) & 0xff)
				out[dest+1] = byte((int(out[dest+1]) + int(g)) & 0xff)
				out[dest+2] = byte((int(out[dest+2]) + int(b)) & 0xff)
				out[dest+3] = byte((int(out[dest+3]) + int(alpha)) & 0xff)
				dest += 4
			}
		default:
			return nil, fmt.Errorf("unknown buffer command 0x%x", cmd)
		}
	}
	return out, nil
}

func copyRect(src []byte, srcX, srcY, srcStride int, dst []byte, dstX, dstY, dstStride, w, h int) {
	for row := 0; row < h; row++ {
		sy := srcY + row
		dy := dstY + row
		if sy < 0 || dy < 0 {
			continue
		}
		for col := 0; col < w; col++ {
			sx := srcX + col
			dx := dstX + col
			sp := (sy*srcStride + sx) * 4
			dp := (dy*dstStride + dx) * 4
			if sp+3 >= len(src) || dp+3 >= len(dst) {
				continue
			}
			copy(dst[dp:dp+4], src[sp:sp+4])
		}
	}
}

// inflateRaw applies raw DEFLATE (zlib.RawInflate as used by Broadway).
func inflateRaw(compressed []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(compressed))
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ComposeScreenshot renders all visible surfaces into one RGBA image sized width×height.
func ComposeScreenshot(surfaces map[uint16]*Surface, width, height int) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{0x2e, 0x34, 0x40, 255}}, image.Point{}, draw.Src)
	for _, s := range surfaces {
		if !s.Visible || len(s.Pixels) == 0 {
			continue
		}
		sub := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))
		if len(s.Pixels) >= s.Width*s.Height*4 {
			copy(sub.Pix, s.Pixels[:s.Width*s.Height*4])
		}
		dst := image.Rect(s.X, s.Y, s.X+s.Width, s.Y+s.Height)
		draw.Draw(canvas, dst, sub, image.Point{}, draw.Over)
	}
	return canvas
}
