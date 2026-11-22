package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/cdp"
)

// fakePage is a tiny DevTools page target: it answers every command, sends one frame when the
// screencast starts, and returns a marker image for Page.captureScreenshot.
type fakePage struct {
	mu      sync.Mutex
	methods []string
}

func (f *fakePage) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (f *fakePage) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, msg.Method)
		f.mu.Unlock()
		result := map[string]any{}
		if msg.Method == "Page.captureScreenshot" {
			var p struct{ Format string }
			_ = json.Unmarshal(msg.Params, &p)
			result["data"] = base64.StdEncoding.EncodeToString([]byte("shot-" + p.Format))
		}
		out, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
		if conn.Write(r.Context(), websocket.MessageText, out) != nil {
			return
		}
		if msg.Method == "Page.startScreencast" {
			ev, _ := json.Marshal(map[string]any{"method": "Page.screencastFrame",
				"params": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("frame")), "sessionId": 1}})
			_ = conn.Write(r.Context(), websocket.MessageText, ev)
		}
	}
}

func connectFake(t *testing.T) (*fakePage, *cdp.Screencast) {
	t.Helper()
	f := &fakePage{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sc, err := cdp.Connect(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return f, sc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestBrowserWSMovesSubscriptionOnRestart checks that an open browser window holds exactly one
// viewer on the current screencast, follows Chromium to a new one, and lets go when it closes.
func TestBrowserWSMovesSubscriptionOnRestart(t *testing.T) {
	_, sc1 := connectFake(t)
	_, sc2 := connectFake(t)
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	a.cdp = sc1
	srv := httptest.NewServer(http.HandlerFunc(a.handleBrowserStream))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	readFrame := func() string {
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if typ == websocket.MessageBinary {
				return string(data)
			}
		}
	}
	if got := readFrame(); got != "frame" {
		t.Fatalf("first frame %q", got)
	}
	if n := sc1.Viewers(); n != 1 {
		t.Fatalf("viewers on the first screencast = %d", n)
	}

	// Chromium restarts, in the order startChromiumOnce does it: detach, close, attach anew.
	a.mu.Lock()
	a.cdp = nil
	a.mu.Unlock()
	_ = sc1.Close()
	waitFor(t, "release on detach", func() bool { return sc1.Viewers() == 0 })
	a.mu.Lock()
	a.cdp = sc2
	a.mu.Unlock()
	if got := readFrame(); got != "frame" {
		t.Fatalf("frame after restart %q", got)
	}
	waitFor(t, "subscription to move", func() bool { return sc1.Viewers() == 0 && sc2.Viewers() == 1 })

	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "viewer release", func() bool { return sc2.Viewers() == 0 })
}

func TestBrowserFrameCaptureFormats(t *testing.T) {
	f, sc := connectFake(t)
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	cases := []struct {
		query, ctype, body string
		code               int
	}{
		{"", "image/jpeg", "shot-jpeg", 200},
		{"?format=jpeg", "image/jpeg", "shot-jpeg", 200},
		{"?format=png", "image/png", "shot-png", 200},
		{"?format=gif", "", "", 400},
		{"?since=0&format=png", "", "", 400},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		a.handleBrowserFrame(rec, httptest.NewRequest(http.MethodGet, "/v1/browser/frame"+c.query, nil))
		if rec.Code != c.code {
			t.Fatalf("%s: code %d", c.query, rec.Code)
		}
		if c.code == 200 && (rec.Header().Get("Content-Type") != c.ctype || rec.Body.String() != c.body) {
			t.Fatalf("%s: %s %q", c.query, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	if f.count("Page.startScreencast") != 0 {
		t.Fatal("one-off screenshots must not start the screencast")
	}
}

// TestBrowserFramePolling checks the ?since contract the e2e script relies on: the first poll
// starts the stream and returns its frame, and an immediate repeat returns 204.
func TestBrowserFramePolling(t *testing.T) {
	f, sc := connectFake(t)
	a := &Agent{log: slog.New(slog.DiscardHandler)}
	a.cdp = sc
	rec := httptest.NewRecorder()
	a.handleBrowserFrame(rec, httptest.NewRequest(http.MethodGet, "/v1/browser/frame?since=0", nil))
	if rec.Code != 200 || rec.Body.String() != "frame" || rec.Header().Get("X-Frame-Seq") != "1" {
		t.Fatalf("first poll: %d %q seq %s", rec.Code, rec.Body.String(), rec.Header().Get("X-Frame-Seq"))
	}
	rec = httptest.NewRecorder()
	a.handleBrowserFrame(rec, httptest.NewRequest(http.MethodGet, "/v1/browser/frame?since=1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeat poll: %d", rec.Code)
	}
	if n := f.count("Page.startScreencast"); n != 1 {
		t.Fatalf("startScreencast sent %d times", n)
	}
}
