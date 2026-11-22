package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/broadway"
)

// fakeBroadway is a broadwayd stand-in: it accepts the client WebSocket and records every
// binary input message it receives.
type fakeBroadway struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (f *fakeBroadway) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		f.mu.Lock()
		f.msgs = append(f.msgs, append([]byte(nil), data...))
		f.mu.Unlock()
	}
}

// ops returns the Broadway op of each recorded message.
func (f *fakeBroadway) ops() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]int32, 0, len(f.msgs))
	for _, m := range f.msgs {
		if len(m) >= 4 {
			ops = append(ops, int32(binary.BigEndian.Uint32(m[0:4])))
		}
	}
	return ops
}

// words decodes one recorded message into its big-endian int32 fields.
func (f *fakeBroadway) words(t *testing.T, i int) []int32 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.msgs) {
		t.Fatalf("only %d messages recorded, want at least %d", len(f.msgs), i+1)
	}
	m := f.msgs[i]
	out := make([]int32, len(m)/4)
	for j := range out {
		out[j] = int32(binary.BigEndian.Uint32(m[j*4:]))
	}
	return out
}

// connectFakeBroadway dials a client at the fake. The first recorded message is the
// SetScreenSize sent on connect.
func connectFakeBroadway(t *testing.T) (*fakeBroadway, *broadway.Client) {
	t.Helper()
	f := &fakeBroadway{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := broadway.NewClient(broadway.Config{
		HostPort: strings.TrimPrefix(srv.URL, "http://"), Width: 1280, Height: 800,
	}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect broadway: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitFor(t, "the screen-size message", func() bool { return len(f.ops()) >= 1 })
	return f, c
}

// waitInput waits until the fake has recorded n messages past the initial screen-size one.
func waitInput(t *testing.T, f *fakeBroadway, n int) {
	t.Helper()
	waitFor(t, "the input messages", func() bool { return len(f.ops()) >= 1+n })
}

// newDisplayV1Agent mounts the /v1 display routes on a test server.
func newDisplayV1Agent(t *testing.T, a *Agent) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	a.registerDisplayV1Routes(mux, a.auth)
	a.registerSessionRoutes(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDisplayV1Click(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/click", `{"x":100,"y":200}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d, want 204", code)
	}
	waitInput(t, f, 3)
	ops := f.ops()[1:]
	if len(ops) != 3 || ops[0] != broadway.EventPointerMove || ops[1] != broadway.EventButtonPress || ops[2] != broadway.EventButtonRelease {
		t.Fatalf("ops = %v, want move+press+release", ops)
	}
	if w := f.words(t, 2); w[len(w)-1] != 1 {
		t.Fatalf("press button = %d, want 1 (left by default)", w[len(w)-1])
	}
	if w := f.words(t, 1); w[5] != 100 || w[6] != 200 {
		t.Fatalf("move = %v, want root (100,200)", w)
	}
}

func TestDisplayV1ClickRightButton(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/click", `{"x":1,"y":2,"button":3}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("click: %d, want 204", code)
	}
	waitInput(t, f, 3)
	if w := f.words(t, 2); w[len(w)-1] != 3 {
		t.Fatalf("press button = %d, want 3", w[len(w)-1])
	}
}

func TestDisplayV1ClickBadButton(t *testing.T) {
	_, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/click", `{"x":1,"y":2,"button":9}`, ses)
	if code != http.StatusBadRequest {
		t.Fatalf("button 9: %d, want 400", code)
	}
}

func TestDisplayV1Type(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/type", `{"text":"Hi"}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("type: %d, want 204", code)
	}
	waitInput(t, f, 4) // two runes, press+release each
	ops := f.ops()[1:]
	if len(ops) != 4 {
		t.Fatalf("ops = %v, want 4 key messages", ops)
	}
	// 'H' is sent as the uppercase keysym with Shift, 'i' plain.
	if w := f.words(t, 1); w[3] != 'H' || w[4] != broadway.ModShift {
		t.Fatalf("H = %v, want keysym H with Shift", w)
	}
	if w := f.words(t, 3); w[3] != 'i' || w[4] != 0 {
		t.Fatalf("i = %v, want keysym i with no mods", w)
	}
}

func TestDisplayV1Key(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/key", `{"key":"Return","mods":4}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("key: %d, want 204", code)
	}
	waitInput(t, f, 2)
	ops := f.ops()[1:]
	if len(ops) != 2 || ops[0] != broadway.EventKeyPress || ops[1] != broadway.EventKeyRelease {
		t.Fatalf("ops = %v, want press+release", ops)
	}
	if w := f.words(t, 1); w[3] != 0xff0d || w[4] != 4 {
		t.Fatalf("press = %v, want keysym Return with mods 4", w)
	}
}

func TestDisplayV1KeyUnknown(t *testing.T) {
	_, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/key", `{"key":"F13"}`, ses)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown key: %d, want 400", code)
	}
}

func TestDisplayV1Scroll(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	// dy 240 down is two presses of button 5.
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/scroll", `{"x":10,"y":20,"dy":240}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("scroll: %d, want 204", code)
	}
	waitInput(t, f, 5)
	ops := f.ops()[1:]
	if len(ops) != 5 || ops[0] != broadway.EventPointerMove {
		t.Fatalf("ops = %v, want move + 2 press/release pairs", ops)
	}
	for i := 1; i < 5; i += 2 {
		if w := f.words(t, 1+i); w[len(w)-1] != 5 {
			t.Fatalf("scroll press %d = %v, want button 5", i, w)
		}
	}
	// dy -120 up is one press of button 4.
	code, _ = doV1Session(t, srv, http.MethodPost, "/v1/display/scroll", `{"x":10,"y":20,"dy":-120}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("scroll up: %d, want 204", code)
	}
	waitInput(t, f, 8)
	if w := f.words(t, 7); w[0] != broadway.EventButtonPress || w[len(w)-1] != 4 {
		t.Fatalf("scroll up press = %v, want button 4", w)
	}
	// dy 0 sends nothing.
	code, _ = doV1Session(t, srv, http.MethodPost, "/v1/display/scroll", `{"x":10,"y":20,"dy":0}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("scroll 0: %d, want 204", code)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ops()); n != 9 {
		t.Fatalf("ops = %d, want 9 (a zero scroll sends nothing)", n)
	}
}

func TestDisplayV1Drag(t *testing.T) {
	f, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/drag", `{"x1":0,"y1":0,"x2":100,"y2":50}`, ses)
	if code != http.StatusNoContent {
		t.Fatalf("drag: %d, want 204", code)
	}
	waitInput(t, f, 13) // move + press + 10 moves + release, bare desktop
	ops := f.ops()[1:]
	if len(ops) != 13 || ops[0] != broadway.EventPointerMove || ops[1] != broadway.EventButtonPress || ops[12] != broadway.EventButtonRelease {
		t.Fatalf("ops = %v, want move+press+10 moves+release", ops)
	}
	if w := f.words(t, 12); w[5] != 100 || w[6] != 50 {
		t.Fatalf("last move = %v, want root (100,50)", w)
	}
}

func TestDisplayV1ClickNotConnected(t *testing.T) {
	c := broadway.NewClient(broadway.Config{HostPort: "127.0.0.1:1", Width: 1280, Height: 800}, slog.New(slog.DiscardHandler))
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	ses := acquireTestSession(t, srv)
	code, _ := doV1Session(t, srv, http.MethodPost, "/v1/display/click", `{"x":1,"y":2}`, ses)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("click with no broadwayd: %d, want 503", code)
	}
}

// TestDisplayV1RequiresSession checks display input refuses callers without a session.
func TestDisplayV1RequiresSession(t *testing.T) {
	_, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	code, body := doV1(t, srv, http.MethodPost, "/v1/display/click", `{"x":1,"y":2}`)
	if code != http.StatusConflict || !strings.Contains(body, "no session held") {
		t.Fatalf("click without a session: %d %s, want 409 no session held", code, body)
	}
}

func TestDisplayV1Aliases(t *testing.T) {
	_, c := connectFakeBroadway(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), broadway: c}
	srv := newDisplayV1Agent(t, a)
	code, body := doV1(t, srv, http.MethodGet, "/v1/display/screenshot", "")
	if code != 200 {
		t.Fatalf("screenshot: %d, want 200", code)
	}
	// The composited PNG starts with the PNG signature.
	if len(body) < 8 || body[:8] != "\x89PNG\r\n\x1a\n" {
		t.Fatal("screenshot is not a PNG")
	}
	code, body = doV1(t, srv, http.MethodGet, "/v1/display/meta", "")
	if code != 200 {
		t.Fatalf("meta: %d, want 200", code)
	}
	var meta struct {
		SurfaceCount int `json:"surface_count"`
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatal(err)
	}
}
