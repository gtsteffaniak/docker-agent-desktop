package broadway

import (
	"encoding/binary"
	"time"
)

// InputEncoder builds Broadway binary input messages (same layout as broadway.js sendInput).
type InputEncoder struct {
	// LastState carries GDK modifier masks for the next event.
	// serverSerial is the serial of the latest message from broadwayd. The browser client stamps
	// its input with this value, not with a counter of its own, so the server accepts it.
	serverSerial uint32
	timeBase     int64
	LastState    uint32
	LastX        int32
	LastY        int32
	MouseWin     uint32
	EventWin     uint32
}

// NewInputEncoder returns an encoder with an initial timestamp base.
func NewInputEncoder() *InputEncoder {
	return &InputEncoder{timeBase: time.Now().UnixMilli()}
}

func (e *InputEncoder) nextTime() int64 {
	return time.Now().UnixMilli() - e.timeBase
}

func (e *InputEncoder) encode(cmd rune, args ...int32) []byte {
	full := make([]int32, 0, 3+len(args))
	full = append(full, int32(cmd), int32(e.serverSerial), int32(e.nextTime()))
	full = append(full, args...)

	buf := make([]byte, 4*len(full))
	for i, v := range full {
		binary.BigEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return buf
}

// SetServerSerial records the serial of the latest server message for the next input event.
func (e *InputEncoder) SetServerSerial(serial uint32) { e.serverSerial = serial }

// SetScreenSize notifies broadwayd of the client viewport (required after connect).
func (e *InputEncoder) SetScreenSize(width, height int32) []byte {
	return e.encode(EventScreenSize, width, height)
}

// Pointer describes where an event lands: the surface under the pointer, the position in root
// coordinates, and the same position relative to that surface's origin. Broadway expects both,
// exactly as broadway.js reports them. Surface 0 means no surface (the bare desktop).
type Pointer struct {
	Surface      uint32
	RootX, RootY int32
	WinX, WinY   int32
}

// ConfigureNotify reports a surface's final geometry, mirroring broadway.js sendConfigureNotify.
func (e *InputEncoder) ConfigureNotify(id uint32, x, y, w, h int32) []byte {
	return e.encode(EventConfigureNotify, int32(id), x, y, w, h)
}

// PointerMove sends a pointer motion event.
func (e *InputEncoder) PointerMove(p Pointer) []byte {
	e.LastX, e.LastY = p.RootX, p.RootY
	e.EventWin = p.Surface
	return e.encode(EventPointerMove,
		int32(e.MouseWin), int32(p.Surface),
		p.RootX, p.RootY, p.WinX, p.WinY, int32(e.LastState))
}

// Enter sends a crossing-enter event for a surface and records it as the surface under the pointer.
func (e *InputEncoder) Enter(p Pointer) []byte {
	e.MouseWin = p.Surface
	e.EventWin = p.Surface
	return e.encode(EventEnter,
		int32(e.MouseWin), int32(p.Surface),
		p.RootX, p.RootY, p.WinX, p.WinY, int32(e.LastState), CrossingNormal)
}

// ButtonPress sends a button press (button 1 = left).
func (e *InputEncoder) ButtonPress(p Pointer, button uint32) []byte {
	return e.encode(EventButtonPress,
		int32(e.MouseWin), int32(p.Surface),
		p.RootX, p.RootY, p.WinX, p.WinY, int32(e.LastState), int32(button))
}

// ButtonRelease sends a button release.
func (e *InputEncoder) ButtonRelease(p Pointer, button uint32) []byte {
	return e.encode(EventButtonRelease,
		int32(e.MouseWin), int32(p.Surface),
		p.RootX, p.RootY, p.WinX, p.WinY, int32(e.LastState), int32(button))
}

// KeyPress sends a keysym press (X11 keysym values).
func (e *InputEncoder) KeyPress(keysym uint32) []byte {
	return e.encode(EventKeyPress, int32(keysym), int32(e.LastState))
}

// KeyRelease sends a keysym release.
func (e *InputEncoder) KeyRelease(keysym uint32) []byte {
	return e.encode(EventKeyRelease, int32(keysym), int32(e.LastState))
}

// Click moves to p, crossing into its surface first if the pointer was elsewhere, then presses
// and releases the left button.
func (e *InputEncoder) Click(p Pointer) [][]byte {
	var out [][]byte
	if p.Surface != e.MouseWin {
		out = append(out, e.Enter(p))
	}
	return append(out,
		e.PointerMove(p),
		e.ButtonPress(p, 1),
		e.ButtonRelease(p, 1),
	)
}

// keysymFor maps a typed rune to the X keysym the browser client sends for it. Printable
// characters use their code point, control characters map to their named keys (Return, Tab,
// BackSpace, Escape), and characters beyond Latin-1 use the X Unicode keysym range.
func keysymFor(r rune) uint32 {
	switch r {
	case '\n', '\r':
		return 0xff0d // Return
	case '\t':
		return 0xff09 // Tab
	case '\b':
		return 0xff08 // BackSpace
	case 0x1b:
		return 0xff1b // Escape
	}
	if r > 0xff {
		return 0x01000000 | uint32(r)
	}
	return uint32(r)
}

// TypeText sends a press and release for each rune. Uppercase letters are sent as the uppercase
// keysym with the Shift modifier, as the browser client does, so GTK derives the same character
// and does not treat the key as a lowercase shortcut.
func (e *InputEncoder) TypeText(text string) [][]byte {
	var out [][]byte
	for _, r := range text {
		ks := keysymFor(r)
		shifted := r >= 'A' && r <= 'Z'
		if shifted {
			e.LastState |= ModShift
		}
		out = append(out, e.KeyPress(ks))
		out = append(out, e.KeyRelease(ks))
		if shifted {
			e.LastState &^= ModShift
		}
	}
	return out
}
