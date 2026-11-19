package broadway

import (
	"encoding/binary"
	"fmt"
)

// Surface is one Broadway drawable on the shared display.
type Surface struct {
	// ID is the Broadway surface identifier assigned by broadwayd.
	ID uint16
	// X and Y are root coordinates of the surface origin.
	X, Y int
	// Width and Height are the surface dimensions in pixels.
	Width  int
	Height int
	// Visible is true after the server has shown the surface.
	Visible bool
	// Pixels holds the latest RGBA framebuffer for this surface.
	Pixels []byte
	// PixelsW and PixelsH are the dimensions of Pixels. They lag Width and Height after a resize
	// until the app sends a buffer at the new size, so decoding must use these, not Width/Height.
	PixelsW, PixelsH int
}

type commandReader struct {
	data []byte
	pos  int
}

func newCommandReader(data []byte) *commandReader {
	return &commandReader{data: data}
}

func (r *commandReader) remaining() int {
	return len(r.data) - r.pos
}

func (r *commandReader) getU8() (byte, error) {
	if r.remaining() < 1 {
		return 0, fmt.Errorf("short read u8")
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *commandReader) getU16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, fmt.Errorf("short read u16")
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *commandReader) getI16() (int16, error) {
	u, err := r.getU16()
	return int16(u), err
}

func (r *commandReader) getU32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, fmt.Errorf("short read u32")
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *commandReader) getBool() (bool, error) {
	b, err := r.getU8()
	return b != 0, err
}

func (r *commandReader) getBytes(n int) ([]byte, error) {
	if r.remaining() < n {
		return nil, fmt.Errorf("short read %d bytes", n)
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

// OpTracer receives one call per surface operation while a message is applied. It is used for
// debug logging and may be nil. w and h are set only for operations that carry a size.
type OpTracer func(op byte, serial uint32, id uint16, w, h int)

// ApplyServerMessage parses one binary WebSocket message from broadwayd into surface updates.
func ApplyServerMessage(data []byte, surfaces map[uint16]*Surface) error {
	return ApplyServerMessageTraced(data, surfaces, nil)
}

// ApplyServerMessageTraced is ApplyServerMessage with an optional per-operation tracer.
func ApplyServerMessageTraced(data []byte, surfaces map[uint16]*Surface, trace OpTracer) error {
	if trace == nil {
		trace = func(byte, uint32, uint16, int, int) {}
	}
	r := newCommandReader(data)
	for r.remaining() > 0 {
		op, err := r.getU8()
		if err != nil {
			return err
		}
		serial, err := r.getU32()
		if err != nil {
			return err
		}
		switch op {
		case 'D':
			return fmt.Errorf("broadway disconnected")
		case 's':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			x, _ := r.getI16()
			y, _ := r.getI16()
			w, _ := r.getU16()
			h, _ := r.getU16()
			_, _ = r.getBool()
			trace(op, serial, id, int(w), int(h))
			surfaces[id] = &Surface{ID: id, X: int(x), Y: int(y), Width: int(w), Height: int(h)}
		case 'S':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			trace(op, serial, id, 0, 0)
			if s := surfaces[id]; s != nil {
				s.Visible = true
			}
		case 'H':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			trace(op, serial, id, 0, 0)
			if s := surfaces[id]; s != nil {
				s.Visible = false
			}
		case 'd':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			trace(op, serial, id, 0, 0)
			delete(surfaces, id)
		case 'm':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			flags, err := r.getU8()
			if err != nil {
				return err
			}
			s := surfaces[id]
			if s == nil {
				s = &Surface{ID: id}
				surfaces[id] = s
			}
			if flags&1 != 0 {
				x, _ := r.getI16()
				y, _ := r.getI16()
				s.X, s.Y = int(x), int(y)
			}
			if flags&2 != 0 {
				w, _ := r.getU16()
				h, _ := r.getU16()
				s.Width, s.Height = int(w), int(h)
			}
			trace(op, serial, id, s.Width, s.Height)
		case 'r', 'R', 'p', 'g', 'u', 'k':
			// Skip ops we do not need for framebuffer decode.
			switch op {
			case 'r', 'R', 'p':
				if _, err := r.getU16(); err != nil {
					return err
				}
				if op == 'p' {
					if _, err := r.getU16(); err != nil {
						return err
					}
				}
			case 'g':
				if _, err := r.getU16(); err != nil {
					return err
				}
				if _, err := r.getBool(); err != nil {
					return err
				}
			case 'k':
				if _, err := r.getU16(); err != nil {
					return err
				}
			}
		case 'b':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			w, err := r.getU16()
			if err != nil {
				return err
			}
			h, err := r.getU16()
			if err != nil {
				return err
			}
			sz, err := r.getU32()
			if err != nil {
				return err
			}
			rest, err := r.getBytes(int(sz))
			if err != nil {
				return err
			}
			trace(op, serial, id, int(w), int(h))
			s := surfaces[id]
			if s == nil {
				s = &Surface{ID: id, Width: int(w), Height: int(h), Visible: true}
				surfaces[id] = s
			}
			inflated, err := inflateRaw(rest)
			if err != nil {
				return fmt.Errorf("inflate surface %d: %w", id, err)
			}
			// Diff against the previous buffer using that buffer's own dimensions.
			pix, err := decodeBuffer(s.Pixels, s.PixelsW, s.PixelsH, int(w), int(h), inflated)
			if err != nil {
				return err
			}
			s.Width, s.Height = int(w), int(h)
			s.Pixels, s.PixelsW, s.PixelsH = pix, int(w), int(h)
			s.Visible = true
		case 'i':
			id, err := r.getU16()
			if err != nil {
				return err
			}
			x, _ := r.getU16()
			y, _ := r.getU16()
			w, _ := r.getU16()
			h, _ := r.getU16()
			n := int(w) * int(h) * 4
			if n > r.remaining() {
				return fmt.Errorf("put_rgb %d needs %d bytes", id, n)
			}
			rgb, err := r.getBytes(n)
			if err != nil {
				return err
			}
			s := surfaces[id]
			if s == nil {
				s = &Surface{ID: id, Visible: true}
				surfaces[id] = s
			}
			s.X, s.Y = int(x), int(y)
			s.Width, s.Height = int(w), int(h)
			s.Pixels = make([]byte, len(rgb))
			copy(s.Pixels, rgb)
			s.PixelsW, s.PixelsH = int(w), int(h)
			s.Visible = true
		default:
			return fmt.Errorf("unknown broadway op %q at %d", op, r.pos-1)
		}
	}
	return nil
}
