package broadway

import (
	"encoding/binary"
	"strconv"
	"strings"
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
	return e.ClickButton(p, 1)
}

// ClickButton moves to p, crossing into its surface first if the pointer was elsewhere, then
// presses and releases the given button (1 left, 2 middle, 3 right).
func (e *InputEncoder) ClickButton(p Pointer, button uint32) [][]byte {
	var out [][]byte
	if p.Surface != e.MouseWin {
		out = append(out, e.Enter(p))
	}
	return append(out,
		e.PointerMove(p),
		e.ButtonPress(p, button),
		e.ButtonRelease(p, button),
	)
}

// PressKey sends a keysym press and release with the given modifier state, then restores the
// encoder's previous state.
func (e *InputEncoder) PressKey(keysym uint32, mods uint32) [][]byte {
	prev := e.LastState
	e.LastState = mods
	out := [][]byte{e.KeyPress(keysym), e.KeyRelease(keysym)}
	e.LastState = prev
	return out
}

// ScrollButton moves to p and then presses and releases button presses times. Button 4 scrolls
// up and button 5 scrolls down in X numbering; that is how the Broadway client reports wheel
// input, since there is no separate scroll event in the encoder.
func (e *InputEncoder) ScrollButton(p Pointer, button uint32, presses int) [][]byte {
	var out [][]byte
	if p.Surface != e.MouseWin {
		out = append(out, e.Enter(p))
	}
	out = append(out, e.PointerMove(p))
	for i := 0; i < presses; i++ {
		out = append(out, e.ButtonPress(p, button), e.ButtonRelease(p, button))
	}
	return out
}

// Drag presses the left button at from, moves to to in 10 steps, then releases.
func (e *InputEncoder) Drag(from, to Pointer) [][]byte {
	var out [][]byte
	if from.Surface != e.MouseWin {
		out = append(out, e.Enter(from))
	}
	out = append(out, e.PointerMove(from), e.ButtonPress(from, 1))
	for i := 1; i <= 10; i++ {
		p := Pointer{
			Surface: to.Surface,
			RootX:   from.RootX + (to.RootX-from.RootX)*int32(i)/10,
			RootY:   from.RootY + (to.RootY-from.RootY)*int32(i)/10,
			WinX:    from.WinX + (to.WinX-from.WinX)*int32(i)/10,
			WinY:    from.WinY + (to.WinY-from.WinY)*int32(i)/10,
		}
		out = append(out, e.PointerMove(p))
	}
	return append(out, e.ButtonRelease(to, 1))
}

// namedKeysyms maps key names to X keysyms. Single characters are handled by keysymFor.
var namedKeysyms = map[string]uint32{
	"Return": 0xff0d, "Enter": 0xff0d,
	"Tab": 0xff09, "Escape": 0xff1b, "Esc": 0xff1b,
	"BackSpace": 0xff08, "Delete": 0xffff, "Insert": 0xff63,
	"ArrowLeft": 0xff51, "Left": 0xff51,
	"ArrowUp": 0xff52, "Up": 0xff52,
	"ArrowRight": 0xff53, "Right": 0xff53,
	"ArrowDown": 0xff54, "Down": 0xff54,
	"Home": 0xff50, "End": 0xff57,
	"PageUp": 0xff55, "PageDown": 0xff56,
	"Space": 0x20,
}

// KeysymForName maps a key name to its X keysym: the named keys above, F1–F12, and any single
// character. It reports false for anything else.
func KeysymForName(name string) (uint32, bool) {
	if ks, ok := namedKeysyms[name]; ok {
		return ks, true
	}
	if strings.HasPrefix(name, "F") {
		if n, err := strconv.Atoi(name[1:]); err == nil && n >= 1 && n <= 12 {
			return 0xffbe + uint32(n-1), true
		}
	}
	if r := []rune(name); len(r) == 1 {
		return keysymFor(r[0]), true
	}
	return 0, false
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
