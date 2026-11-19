package broadway

import (
	"encoding/binary"
	"testing"
)

// words decodes one encoded input message into its big-endian int32 fields.
func words(t *testing.T, msg []byte) []int32 {
	t.Helper()
	if len(msg)%4 != 0 {
		t.Fatalf("message length %d is not a multiple of 4", len(msg))
	}
	out := make([]int32, len(msg)/4)
	for i := range out {
		out[i] = int32(binary.BigEndian.Uint32(msg[i*4:]))
	}
	return out
}

func TestClickCarriesSurfaceAndWindowCoordinates(t *testing.T) {
	e := NewInputEncoder()
	msgs := e.Click(Pointer{Surface: 3, RootX: 110, RootY: 220, WinX: 10, WinY: 20})
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want enter+move+press+release", len(msgs))
	}
	wantOps := []int32{EventEnter, EventPointerMove, EventButtonPress, EventButtonRelease}
	for i, m := range msgs {
		w := words(t, m)
		if w[0] != wantOps[i] {
			t.Fatalf("message %d op = %c, want %c", i, rune(w[0]), rune(wantOps[i]))
		}
		// Layout: op, serial, time, mouseWin, eventWin, rootX, rootY, winX, winY, state, ...
		if w[3] != 3 || w[4] != 3 {
			t.Fatalf("message %d windows = %d/%d, want surface 3", i, w[3], w[4])
		}
		if w[5] != 110 || w[6] != 220 || w[7] != 10 || w[8] != 20 {
			t.Fatalf("message %d coords root=(%d,%d) win=(%d,%d)", i, w[5], w[6], w[7], w[8])
		}
	}
	if w := words(t, msgs[2]); w[len(w)-1] != 1 {
		t.Fatalf("press button = %d, want 1", w[len(w)-1])
	}
}

func TestSecondClickOnSameSurfaceSkipsEnter(t *testing.T) {
	e := NewInputEncoder()
	p := Pointer{Surface: 2, RootX: 5, RootY: 5, WinX: 5, WinY: 5}
	e.Click(p)
	if msgs := e.Click(p); len(msgs) != 3 || words(t, msgs[0])[0] != EventPointerMove {
		t.Fatalf("second click should start with a move, got %d messages", len(msgs))
	}
	other := Pointer{Surface: 4, RootX: 9, RootY: 9, WinX: 1, WinY: 1}
	if msgs := e.Click(other); len(msgs) != 4 || words(t, msgs[0])[0] != EventEnter {
		t.Fatal("moving to another surface should send a crossing enter")
	}
}

func opaquePixels(w, h int) []byte {
	b := make([]byte, w*h*4)
	for i := 3; i < len(b); i += 4 {
		b[i] = 255
	}
	return b
}

func TestPointerAtPicksTopmostOpaqueSurface(t *testing.T) {
	under := &Surface{ID: 1, X: 100, Y: 100, Width: 50, Height: 50, PixelsW: 50, PixelsH: 50, Visible: true, Pixels: opaquePixels(50, 50)}
	over := &Surface{ID: 2, X: 120, Y: 120, Width: 20, Height: 20, PixelsW: 20, PixelsH: 20, Visible: true, Pixels: opaquePixels(20, 20)}
	over.Pixels[(10*20+10)*4+3] = 0 // a transparent hole at window (10,10) = root (130,130)
	c := &Client{surfaces: map[uint16]*Surface{1: under, 2: over}}

	if p := c.pointerAt(125, 125); p.Surface != 2 || p.WinX != 5 || p.WinY != 5 {
		t.Fatalf("opaque overlap = %+v, want surface 2 at (5,5)", p)
	}
	if p := c.pointerAt(130, 130); p.Surface != 1 || p.WinX != 30 || p.WinY != 30 {
		t.Fatalf("transparent hole = %+v, want fall through to surface 1 at (30,30)", p)
	}
	if p := c.pointerAt(10, 10); p.Surface != 0 || p.WinX != 10 {
		t.Fatalf("bare desktop = %+v, want surface 0", p)
	}
	under.Visible = false
	if p := c.pointerAt(101, 101); p.Surface != 0 {
		t.Fatalf("hidden surface was hit: %+v", p)
	}
}

func TestConfigureNotifyLayout(t *testing.T) {
	e := NewInputEncoder()
	w := words(t, e.ConfigureNotify(7, -2, 1, 615, 511))
	// op, serial, time, id, x, y, w, h
	if len(w) != 8 || w[0] != EventConfigureNotify || w[3] != 7 || w[4] != -2 || w[5] != 1 || w[6] != 615 || w[7] != 511 {
		t.Fatalf("configure notify = %v", w)
	}
}

func TestApplyServerMessageTracesMoves(t *testing.T) {
	surfaces := map[uint16]*Surface{4: {ID: 4, X: 1, Y: 1, Width: 10, Height: 10, Visible: true}}
	// 'm' op: serial u32, id u16, flags u8 (1 = position, 2 = size), then x, y, w, h as 16-bit values.
	msg := []byte{'m', 1, 0, 0, 0, 4, 0, 3, 5, 0, 6, 0, 0x67, 0x02, 0xff, 0x01}
	var got [][3]int
	err := ApplyServerMessageTraced(msg, surfaces, func(op byte, _ uint32, id uint16, w, h int) {
		got = append(got, [3]int{int(op), int(id), w*1000 + h})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != [3]int{'m', 4, 615*1000 + 511} {
		t.Fatalf("trace = %v", got)
	}
	if s := surfaces[4]; s.X != 5 || s.Y != 6 || s.Width != 615 || s.Height != 511 {
		t.Fatalf("surface = %+v", s)
	}
}

func TestTypeTextKeysyms(t *testing.T) {
	e := NewInputEncoder()
	msgs := e.TypeText("aA\n!é€")
	// Each rune is a press and a release: 2 messages per rune, 6 runes.
	if len(msgs) != 12 {
		t.Fatalf("got %d messages, want 12", len(msgs))
	}
	check := func(i int, op, keysym, state int32) {
		t.Helper()
		w := words(t, msgs[i])
		if w[0] != op || w[3] != keysym || w[4] != state {
			t.Fatalf("message %d = op %c keysym 0x%x state 0x%x, want op %c keysym 0x%x state 0x%x",
				i, rune(w[0]), w[3], w[4], rune(op), keysym, state)
		}
	}
	check(0, EventKeyPress, 'a', 0)
	check(1, EventKeyRelease, 'a', 0)
	check(2, EventKeyPress, 'A', ModShift) // uppercase keysym with Shift, like the browser
	check(3, EventKeyRelease, 'A', ModShift)
	check(4, EventKeyPress, 0xff0d, 0) // newline is Return
	check(6, EventKeyPress, '!', 0)
	check(8, EventKeyPress, 0xe9, 0)        // é
	check(10, EventKeyPress, 0x010020ac, 0) // € as an X Unicode keysym
	if e.LastState != 0 {
		t.Fatalf("shift leaked into later keys: state 0x%x", e.LastState)
	}
}
