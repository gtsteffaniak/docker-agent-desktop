package broadway

import (
	"testing"
)

func TestClickButtonUsesGivenButton(t *testing.T) {
	e := NewInputEncoder()
	p := Pointer{Surface: 3, RootX: 110, RootY: 220, WinX: 10, WinY: 20}
	msgs := e.ClickButton(p, 3)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want enter+move+press+release", len(msgs))
	}
	if w := words(t, msgs[2]); w[0] != EventButtonPress || w[len(w)-1] != 3 {
		t.Fatalf("press = %v, want button 3", w)
	}
	if w := words(t, msgs[3]); w[0] != EventButtonRelease || w[len(w)-1] != 3 {
		t.Fatalf("release = %v, want button 3", w)
	}
}

func TestPressKeyCarriesModsThenRestoresState(t *testing.T) {
	e := NewInputEncoder()
	e.LastState = ModAlt
	msgs := e.PressKey(0xff0d, ModControl)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want press+release", len(msgs))
	}
	for i, m := range msgs {
		w := words(t, m)
		// Layout: op, serial, time, keysym, state.
		if w[3] != 0xff0d || w[4] != ModControl {
			t.Fatalf("message %d = %v, want keysym 0xff0d state ModControl", i, w)
		}
	}
	if words(t, msgs[0])[0] != EventKeyPress || words(t, msgs[1])[0] != EventKeyRelease {
		t.Fatal("want a key press followed by a release")
	}
	if e.LastState != ModAlt {
		t.Fatalf("state after PressKey = 0x%x, want the previous 0x%x", e.LastState, ModAlt)
	}
}

func TestScrollButtonPressCount(t *testing.T) {
	e := NewInputEncoder()
	p := Pointer{Surface: 0, RootX: 5, RootY: 5, WinX: 5, WinY: 5}
	msgs := e.ScrollButton(p, 5, 3)
	// move + 3 press/release pairs, no enter on the bare desktop.
	if len(msgs) != 7 {
		t.Fatalf("got %d messages, want 7", len(msgs))
	}
	if words(t, msgs[0])[0] != EventPointerMove {
		t.Fatal("scroll should start with a move")
	}
	for i := 0; i < 3; i++ {
		if w := words(t, msgs[1+i*2]); w[0] != EventButtonPress || w[len(w)-1] != 5 {
			t.Fatalf("press %d = %v, want button 5", i, w)
		}
		if w := words(t, msgs[2+i*2]); w[0] != EventButtonRelease || w[len(w)-1] != 5 {
			t.Fatalf("release %d = %v, want button 5", i, w)
		}
	}
}

func TestDragMovesInTenSteps(t *testing.T) {
	e := NewInputEncoder()
	from := Pointer{Surface: 2, RootX: 0, RootY: 0, WinX: 0, WinY: 0}
	to := Pointer{Surface: 2, RootX: 100, RootY: 50, WinX: 100, WinY: 50}
	msgs := e.Drag(from, to)
	// enter + move + press + 10 moves + release.
	if len(msgs) != 14 {
		t.Fatalf("got %d messages, want 14", len(msgs))
	}
	if w := words(t, msgs[2]); w[0] != EventButtonPress || w[len(w)-1] != 1 {
		t.Fatalf("press = %v", w)
	}
	for i := 1; i <= 10; i++ {
		w := words(t, msgs[2+i])
		if w[0] != EventPointerMove || w[5] != int32(i*10) || w[6] != int32(i*5) {
			t.Fatalf("move %d = %v, want root (%d,%d)", i, w, i*10, i*5)
		}
	}
	if w := words(t, msgs[13]); w[0] != EventButtonRelease || w[len(w)-1] != 1 {
		t.Fatalf("release = %v", w)
	}
}

func TestKeysymForName(t *testing.T) {
	cases := map[string]uint32{
		"Return": 0xff0d, "Enter": 0xff0d, "Tab": 0xff09,
		"Escape": 0xff1b, "Esc": 0xff1b,
		"BackSpace": 0xff08, "Delete": 0xffff, "Insert": 0xff63,
		"ArrowLeft": 0xff51, "ArrowUp": 0xff52, "ArrowRight": 0xff53, "ArrowDown": 0xff54,
		"Left": 0xff51, "Up": 0xff52, "Right": 0xff53, "Down": 0xff54,
		"Home": 0xff50, "End": 0xff57, "PageUp": 0xff55, "PageDown": 0xff56,
		"Space": 0x20, "F1": 0xffbe, "F12": 0xffc9,
		"a": 'a', "A": 'A', "7": '7', "!": '!',
	}
	for name, want := range cases {
		got, ok := KeysymForName(name)
		if !ok || got != want {
			t.Errorf("KeysymForName(%q) = 0x%x, %v; want 0x%x, true", name, got, ok, want)
		}
	}
	for _, bad := range []string{"", "F0", "F13", "Shift", "Control", "Enter\n", "ab"} {
		if ks, ok := KeysymForName(bad); ok {
			t.Errorf("KeysymForName(%q) = 0x%x, want false", bad, ks)
		}
	}
}

// TestClientInputNeedsConnection checks the new Client input methods fail cleanly with no
// broadwayd connection, and that ClickButton validates its button first.
func TestClientInputNeedsConnection(t *testing.T) {
	c := &Client{enc: NewInputEncoder(), surfaces: map[uint16]*Surface{}}
	for name, fn := range map[string]func() error{
		"ClickButton": func() error { return c.ClickButton(1, 2, 1) },
		"Key":         func() error { return c.Key(0xff0d, 0) },
		"ScrollAt":    func() error { return c.ScrollAt(1, 2, 0, 120) },
		"Drag":        func() error { return c.Drag(1, 2, 3, 4) },
	} {
		if err := fn(); err != ErrNotConnected {
			t.Errorf("%s without a connection = %v, want ErrNotConnected", name, err)
		}
	}
	if err := c.ClickButton(1, 2, 4); err == nil || err == ErrNotConnected {
		t.Errorf("ClickButton with button 4 = %v, want a validation error", err)
	}
	if err := c.ScrollAt(1, 2, 0, 0); err != nil {
		t.Errorf("ScrollAt with dy 0 = %v, want nil (nothing to send)", err)
	}
}
