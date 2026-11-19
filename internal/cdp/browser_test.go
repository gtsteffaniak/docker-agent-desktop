package cdp

import (
	"encoding/json"
	"testing"
)

func TestSetHistoryTracksBackForward(t *testing.T) {
	s := &Screencast{}
	entries := []navEntry{
		{ID: 1, URL: "about:blank", Title: ""},
		{ID: 2, URL: "https://example.com/", Title: "Example"},
		{ID: 3, URL: "https://example.com/b", Title: "B"},
	}

	s.setHistory(0, entries)
	if st := s.Nav(); st.CanGoBack || !st.CanGoForward || st.URL != "about:blank" {
		t.Fatalf("first entry: %+v", st)
	}
	s.setHistory(1, entries)
	if st := s.Nav(); !st.CanGoBack || !st.CanGoForward || st.URL != "https://example.com/" || st.Title != "Example" {
		t.Fatalf("middle entry: %+v", st)
	}
	s.setHistory(2, entries)
	if st := s.Nav(); !st.CanGoBack || st.CanGoForward || st.URL != "https://example.com/b" {
		t.Fatalf("last entry: %+v", st)
	}
}

func TestSetHistorySeqOnlyAdvancesOnChange(t *testing.T) {
	s := &Screencast{}
	entries := []navEntry{{ID: 1, URL: "https://a.test/", Title: "A"}}
	s.setHistory(0, entries)
	first := s.Nav().Seq
	s.setHistory(0, entries) // the safety poll sees the same state
	if got := s.Nav().Seq; got != first {
		t.Fatalf("seq advanced without a change: %d -> %d", first, got)
	}
	entries[0].Title = "A, loaded"
	s.setHistory(0, entries)
	if got := s.Nav().Seq; got != first+1 {
		t.Fatalf("seq did not advance on a title change: %d -> %d", first, got)
	}
}

func TestSetHistoryIgnoresBadIndex(t *testing.T) {
	s := &Screencast{}
	s.setHistory(5, []navEntry{{ID: 1, URL: "https://a.test/"}})
	s.setHistory(-1, nil)
	if st := s.Nav(); st.URL != "" || st.Seq != 0 {
		t.Fatalf("state changed on a bad index: %+v", st)
	}
}

func TestIsMainFrameNavigation(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   bool
	}{
		{"main frame", `{"frame":{"id":"A","url":"https://a.test/"}}`, true},
		{"subframe", `{"frame":{"id":"B","parentId":"A","url":"https://ads.test/"}}`, false},
		{"garbage", `not json`, false},
	}
	for _, c := range cases {
		if got := isMainFrameNavigation(json.RawMessage(c.params)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVirtualKey(t *testing.T) {
	cases := map[string]int{
		"Backspace": 8, "Tab": 9, "Enter": 13, "Escape": 27, "ArrowLeft": 37, "ArrowDown": 40,
		"Delete": 46, "a": 'A', "Z": 'Z', "7": '7', "F13": 0, "": 0, "é": 0,
	}
	for key, want := range cases {
		if got := VirtualKey(key); got != want {
			t.Errorf("VirtualKey(%q) = %d, want %d", key, got, want)
		}
	}
}

func TestKeyParams(t *testing.T) {
	p, ok := KeyParams(KeyEvent{Action: "down", Key: "a", Code: "KeyA", Text: "a"})
	if !ok || p["type"] != "keyDown" || p["text"] != "a" || p["windowsVirtualKeyCode"] != int('A') {
		t.Fatalf("printable key: %v", p)
	}
	p, _ = KeyParams(KeyEvent{Action: "down", Key: "Backspace", Code: "Backspace"})
	if p["type"] != "rawKeyDown" || p["windowsVirtualKeyCode"] != 8 {
		t.Fatalf("backspace: %v", p)
	}
	p, _ = KeyParams(KeyEvent{Action: "down", Key: "Enter", Code: "Enter"})
	if p["type"] != "keyDown" || p["text"] != "\r" {
		t.Fatalf("enter should carry a carriage return: %v", p)
	}
	// Ctrl+A must not type an "a" into the page.
	p, _ = KeyParams(KeyEvent{Action: "down", Key: "a", Code: "KeyA", Text: "a", Mods: ModCtrl})
	if p["type"] != "rawKeyDown" || p["modifiers"] != ModCtrl {
		t.Fatalf("ctrl+a: %v", p)
	}
	p, _ = KeyParams(KeyEvent{Action: "up", Key: "a", Code: "KeyA"})
	if p["type"] != "keyUp" {
		t.Fatalf("key up: %v", p)
	}
	if _, ok := KeyParams(KeyEvent{Action: "sideways"}); ok {
		t.Fatal("unknown action should be rejected")
	}
}

func TestMouseParams(t *testing.T) {
	p, ok := MouseParams(MouseEvent{Action: "press", X: 10, Y: 20, Button: 1, Buttons: 1, Clicks: 2})
	if !ok || p["type"] != "mousePressed" || p["button"] != "left" || p["clickCount"] != 2 {
		t.Fatalf("press: %v", p)
	}
	p, _ = MouseParams(MouseEvent{Action: "press", Button: 3})
	if p["button"] != "right" || p["clickCount"] != 1 {
		t.Fatalf("right press defaults to one click: %v", p)
	}
	// A drag reports the held button on moves.
	p, _ = MouseParams(MouseEvent{Action: "move", Buttons: 1})
	if p["type"] != "mouseMoved" || p["button"] != "left" || p["clickCount"] != 0 {
		t.Fatalf("drag move: %v", p)
	}
	p, _ = MouseParams(MouseEvent{Action: "move"})
	if p["button"] != "none" {
		t.Fatalf("plain move: %v", p)
	}
	if _, ok := MouseParams(MouseEvent{Action: "wiggle"}); ok {
		t.Fatal("unknown action should be rejected")
	}
}

func TestClampInt(t *testing.T) {
	if clampInt(5, 10, 20) != 10 || clampInt(25, 10, 20) != 20 || clampInt(15, 10, 20) != 15 {
		t.Fatal("clampInt")
	}
}
