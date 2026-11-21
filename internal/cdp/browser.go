package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// NavState is what the browser window needs to draw its toolbar.
type NavState struct {
	URL          string `json:"url"`
	Title        string `json:"title"`
	CanGoBack    bool   `json:"can_back"`
	CanGoForward bool   `json:"can_forward"`
	// Seq increases whenever any other field changes, so callers can detect updates cheaply.
	Seq uint64 `json:"seq"`
}

type navEntry struct {
	ID    int    `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// navPollInterval is the safety poll. Navigation events do most of the work.
const navPollInterval = time.Second

// Nav returns the latest navigation state.
func (s *Screencast) Nav() NavState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nav
}

// RefreshNav reads the navigation history from Chromium and updates the cached state.
func (s *Screencast) RefreshNav(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	raw, err := s.Call(cctx, "Page.getNavigationHistory", nil)
	if err != nil {
		return err
	}
	var h struct {
		CurrentIndex int        `json:"currentIndex"`
		Entries      []navEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return err
	}
	s.setHistory(h.CurrentIndex, h.Entries)
	return nil
}

func (s *Screencast) refreshNavQuiet() { _ = s.RefreshNav(context.Background()) }

func (s *Screencast) pollNav(life context.Context) {
	t := time.NewTicker(navPollInterval)
	defer t.Stop()
	for {
		select {
		case <-life.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			s.refreshNavQuiet()
		}
	}
}

// setHistory records the history and updates the nav state. Seq only advances on a real change.
func (s *Screencast) setHistory(idx int, entries []navEntry) {
	if idx < 0 || idx >= len(entries) {
		return
	}
	cur := entries[idx]
	next := NavState{
		URL:          cur.URL,
		Title:        cur.Title,
		CanGoBack:    idx > 0,
		CanGoForward: idx < len(entries)-1,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = entries
	s.idx = idx
	next.Seq = s.nav.Seq
	if next != s.nav {
		next.Seq++
		s.nav = next
		// Wake everyone waiting in WaitNav. A new channel replaces the closed one.
		if s.navNotify != nil {
			close(s.navNotify)
		}
		s.navNotify = make(chan struct{})
	}
}

// WaitNav blocks until the navigation state has changed past since, then returns it. A client
// passes back the Seq it last sent, so it is told about each change once.
func (s *Screencast) WaitNav(ctx context.Context, since uint64) (NavState, error) {
	for {
		s.mu.Lock()
		st := s.nav
		ch := s.navNotify
		s.mu.Unlock()
		if st.Seq > since {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return NavState{}, ctx.Err()
		case <-s.done:
			return NavState{}, ErrClosed
		case <-ch:
		}
	}
}

// isMainFrameNavigation reports whether a Page.frameNavigated event is for the top frame.
// Subframes (ads, embeds) carry a parentId and must not change the address bar.
func isMainFrameNavigation(params json.RawMessage) bool {
	var p struct {
		Frame struct {
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return false
	}
	return p.Frame.ParentID == ""
}

// Navigate loads url in the page. Chromium reports DNS and connection failures through the
// result's errorText while still showing its own error page, so that is returned as an error.
func (s *Screencast) Navigate(ctx context.Context, url string) error {
	raw, err := s.Call(ctx, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if json.Unmarshal(raw, &r) == nil && r.ErrorText != "" {
		return fmt.Errorf("navigate: %s", r.ErrorText)
	}
	return nil
}

// GoBack moves one step back through the real navigation history.
func (s *Screencast) GoBack(ctx context.Context) error { return s.goHistory(ctx, -1) }

// GoForward moves one step forward through the real navigation history.
func (s *Screencast) GoForward(ctx context.Context) error { return s.goHistory(ctx, 1) }

func (s *Screencast) goHistory(ctx context.Context, delta int) error {
	if err := s.RefreshNav(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	idx := s.idx + delta
	var entry navEntry
	ok := idx >= 0 && idx < len(s.entries)
	if ok {
		entry = s.entries[idx]
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	_, err := s.Call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": entry.ID})
	return err
}

// Reload reloads the current page.
func (s *Screencast) Reload(ctx context.Context) error {
	_, err := s.Call(ctx, "Page.reload", nil)
	return err
}

// SetViewport sizes the page to the browser window in CSS pixels. scale is the device pixel
// ratio of the screen showing it, so the frames are sharp on high-density displays.
func (s *Screencast) SetViewport(ctx context.Context, w, h int, scale float64) error {
	w = clampInt(w, 200, 4096)
	h = clampInt(h, 150, 4096)
	if scale < 1 {
		scale = 1
	}
	if scale > 3 {
		scale = 3
	}
	_, err := s.Call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": scale, "mobile": false,
	})
	return err
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Modifier bits as CDP defines them.
const (
	ModAlt   = 1
	ModCtrl  = 2
	ModMeta  = 4
	ModShift = 8
)

// MouseEvent is one pointer event from the browser window.
type MouseEvent struct {
	Action  string  `json:"action"` // press, release or move
	X       float64 `json:"x"`      // page coordinates
	Y       float64 `json:"y"`
	Button  int     `json:"button"`  // 1 left, 2 middle, 3 right (GDK numbering)
	Buttons int     `json:"buttons"` // bitmask of buttons held: 1 left, 2 right, 4 middle
	Clicks  int     `json:"clicks"`
	Mods    int     `json:"mods"`
}

// MouseParams builds the Input.dispatchMouseEvent parameters, or false for an unknown action.
func MouseParams(ev MouseEvent) (map[string]any, bool) {
	var typ string
	switch ev.Action {
	case "press":
		typ = "mousePressed"
	case "release":
		typ = "mouseReleased"
	case "move":
		typ = "mouseMoved"
	default:
		return nil, false
	}
	button := "none"
	switch ev.Button {
	case 1:
		button = "left"
	case 2:
		button = "middle"
	case 3:
		button = "right"
	}
	if ev.Action == "move" {
		// A move reports the held button so drags and text selection work.
		switch {
		case ev.Buttons&1 != 0:
			button = "left"
		case ev.Buttons&4 != 0:
			button = "middle"
		case ev.Buttons&2 != 0:
			button = "right"
		default:
			button = "none"
		}
	}
	clicks := ev.Clicks
	if ev.Action != "move" && clicks < 1 {
		clicks = 1
	}
	return map[string]any{
		"type": typ, "x": ev.X, "y": ev.Y, "button": button,
		"buttons": ev.Buttons, "clickCount": clicks, "modifiers": ev.Mods,
	}, true
}

// Mouse sends a pointer event to the page.
func (s *Screencast) Mouse(ctx context.Context, ev MouseEvent) error {
	p, ok := MouseParams(ev)
	if !ok {
		return fmt.Errorf("unknown mouse action %q", ev.Action)
	}
	_, err := s.Call(ctx, "Input.dispatchMouseEvent", p)
	return err
}

// Scroll sends a mouse wheel event at (x, y).
func (s *Screencast) Scroll(ctx context.Context, x, y, dx, dy float64, mods int) error {
	_, err := s.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy, "modifiers": mods,
	})
	return err
}

// KeyEvent is one key event from the browser window. Key uses DOM key names (Enter,
// Backspace, ArrowLeft, a). Text is the character the key produces, empty for non-text keys.
type KeyEvent struct {
	Action string `json:"action"` // down or up
	Key    string `json:"key"`
	Code   string `json:"code"`
	Text   string `json:"text"`
	Mods   int    `json:"mods"`
}

// virtualKeys maps DOM key names to Windows virtual key codes, which Chromium needs before it
// will act on keys such as Backspace and the arrows.
var virtualKeys = map[string]int{
	"Backspace": 8, "Tab": 9, "Enter": 13, "Escape": 27, " ": 32,
	"PageUp": 33, "PageDown": 34, "End": 35, "Home": 36,
	"ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40,
	"Insert": 45, "Delete": 46,
}

// VirtualKey returns the Windows virtual key code for a DOM key name, or 0 when unknown.
func VirtualKey(key string) int {
	if v, ok := virtualKeys[key]; ok {
		return v
	}
	if len(key) == 1 {
		c := key[0]
		switch {
		case c >= 'a' && c <= 'z':
			return int(c - 'a' + 'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return int(c)
		}
	}
	return 0
}

// KeyParams builds the Input.dispatchKeyEvent parameters, or false for an unknown action.
func KeyParams(ev KeyEvent) (map[string]any, bool) {
	text := ev.Text
	if ev.Key == "Enter" && text == "" {
		text = "\r"
	}
	vk := VirtualKey(ev.Key)
	p := map[string]any{
		"modifiers":             ev.Mods,
		"key":                   ev.Key,
		"code":                  ev.Code,
		"windowsVirtualKeyCode": vk,
		"nativeVirtualKeyCode":  vk,
	}
	switch ev.Action {
	case "down":
		if text != "" && ev.Mods&(ModCtrl|ModAlt|ModMeta) == 0 {
			p["type"] = "keyDown"
			p["text"] = text
			p["unmodifiedText"] = text
		} else {
			p["type"] = "rawKeyDown"
		}
	case "up":
		p["type"] = "keyUp"
	default:
		return nil, false
	}
	return p, true
}

// Key sends a key event to the page.
func (s *Screencast) Key(ctx context.Context, ev KeyEvent) error {
	p, ok := KeyParams(ev)
	if !ok {
		return fmt.Errorf("unknown key action %q", ev.Action)
	}
	_, err := s.Call(ctx, "Input.dispatchKeyEvent", p)
	return err
}

// InsertText types a string into the focused field, which is how paste works.
func (s *Screencast) InsertText(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	_, err := s.Call(ctx, "Input.insertText", map[string]any{"text": text})
	return err
}

// Evaluate runs a JavaScript expression in the page and returns the RemoteObject it produced
// (the result.result field of the Runtime.evaluate reply). Callers read the value out of it;
// an expression that evaluates to undefined has no value field. A JavaScript exception is
// returned as an error.
func (s *Screencast) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	raw, err := s.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		// ExceptionDetails is present when the expression threw.
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception,omitempty"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if d := r.ExceptionDetails; d != nil {
		msg := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			msg = d.Exception.Description
		}
		return nil, fmt.Errorf("evaluate: %s", msg)
	}
	return r.Result, nil
}

// PageText returns the page's visible text, or "" when the page has no body.
func (s *Screencast) PageText(ctx context.Context) (string, error) {
	raw, err := s.Evaluate(ctx, `document.body ? document.body.innerText : ""`)
	if err != nil {
		return "", err
	}
	var r struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Value, nil
}
