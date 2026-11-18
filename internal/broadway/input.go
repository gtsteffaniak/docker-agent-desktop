package broadway

import (
	"encoding/binary"
	"time"
)

// InputEncoder builds Broadway binary input messages (same layout as broadway.js sendInput).
type InputEncoder struct {
	// LastState carries GDK modifier masks for the next event.
	serial    int32
	timeBase  int64
	seenTime  bool
	LastState uint32
	LastX     int32
	LastY     int32
	MouseWin  uint32
	EventWin  uint32
}

// NewInputEncoder returns an encoder with an initial timestamp base.
func NewInputEncoder() *InputEncoder {
	return &InputEncoder{serial: 1, timeBase: time.Now().UnixMilli()}
}

func (e *InputEncoder) nextTime() int64 {
	now := time.Now().UnixMilli()
	if !e.seenTime {
		e.seenTime = true
		return now - e.timeBase
	}
	return now - e.timeBase
}

func (e *InputEncoder) encode(cmd rune, args ...int32) []byte {
	full := make([]int32, 0, 3+len(args))
	full = append(full, int32(cmd), e.serial, int32(e.nextTime()))
	full = append(full, args...)
	e.serial++

	buf := make([]byte, 4*len(full))
	for i, v := range full {
		binary.BigEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return buf
}

// SetScreenSize notifies broadwayd of the client viewport (required after connect).
func (e *InputEncoder) SetScreenSize(width, height int32) []byte {
	return e.encode(EventScreenSize, width, height)
}

// PointerMove sends a pointer motion event at root coordinates.
func (e *InputEncoder) PointerMove(rootX, rootY int32, surfaceID uint32) []byte {
	winX, winY := rootX, rootY
	e.LastX, e.LastY = rootX, rootY
	e.EventWin = surfaceID
	return e.encode(EventPointerMove,
		int32(e.MouseWin), int32(surfaceID),
		rootX, rootY, winX, winY, int32(e.LastState))
}

// Enter sends a crossing-enter event for a surface.
func (e *InputEncoder) Enter(surfaceID uint32, rootX, rootY int32) []byte {
	e.MouseWin = surfaceID
	e.EventWin = surfaceID
	return e.encode(EventEnter,
		int32(e.MouseWin), int32(surfaceID),
		rootX, rootY, rootX, rootY, int32(e.LastState), CrossingNormal)
}

// ButtonPress sends a button press (button 1 = left).
func (e *InputEncoder) ButtonPress(surfaceID uint32, rootX, rootY int32, button uint32) []byte {
	return e.encode(EventButtonPress,
		int32(e.MouseWin), int32(surfaceID),
		rootX, rootY, rootX, rootY, int32(e.LastState), int32(button))
}

// ButtonRelease sends a button release.
func (e *InputEncoder) ButtonRelease(surfaceID uint32, rootX, rootY int32, button uint32) []byte {
	return e.encode(EventButtonRelease,
		int32(e.MouseWin), int32(surfaceID),
		rootX, rootY, rootX, rootY, int32(e.LastState), int32(button))
}

// KeyPress sends a keysym press (X11 keysym values).
func (e *InputEncoder) KeyPress(keysym uint32) []byte {
	return e.encode(EventKeyPress, int32(keysym), int32(e.LastState))
}

// KeyRelease sends a keysym release.
func (e *InputEncoder) KeyRelease(keysym uint32) []byte {
	return e.encode(EventKeyRelease, int32(keysym), int32(e.LastState))
}

// Click performs move, enter, press, and release at root coordinates on a surface.
func (e *InputEncoder) Click(surfaceID uint32, rootX, rootY int32) [][]byte {
	return [][]byte{
		e.PointerMove(rootX, rootY, surfaceID),
		e.ButtonPress(surfaceID, rootX, rootY, 1),
		e.ButtonRelease(surfaceID, rootX, rootY, 1),
	}
}

// TypeText sends key press/release pairs for ASCII runes (lowercase keysyms).
func (e *InputEncoder) TypeText(text string) [][]byte {
	var out [][]byte
	for _, r := range text {
		ks := uint32(r)
		if r >= 'a' && r <= 'z' {
			ks = uint32(r)
		}
		if r >= 'A' && r <= 'Z' {
			e.LastState |= ModShift
			ks = uint32(r + 32)
		}
		out = append(out, e.KeyPress(ks))
		out = append(out, e.KeyRelease(ks))
		e.LastState &^= ModShift
	}
	return out
}
