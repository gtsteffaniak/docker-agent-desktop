package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// streamClient is a minimal /v1/browser/stream client for tests: it reads
// messages, collects acks by id, and records the latest binary frame.
type streamClient struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	acks map[int64]wsAck
}

func dialStream(t *testing.T, url string) *streamClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial %s: %v (status %d)", url, err, resp.StatusCode)
		}
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return &streamClient{t: t, conn: conn, ctx: ctx, acks: map[int64]wsAck{}}
}

func (c *streamClient) send(t *testing.T, cmd wsCommand) {
	t.Helper()
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	if err := c.conn.Write(wctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write %+v: %v", cmd, err)
	}
}

// waitAck waits for the ack with this id, skipping frames and state messages.
func (c *streamClient) waitAck(t *testing.T, id int64) wsAck {
	t.Helper()
	if ack, ok := c.acks[id]; ok {
		return ack
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithTimeout(c.ctx, 500*time.Millisecond)
		typ, data, err := c.conn.Read(rctx)
		cancel()
		if err != nil {
			continue
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &msg); err != nil || msg.Type != "ack" {
			continue
		}
		var ack wsAck
		if err := json.Unmarshal(data, &ack); err != nil {
			t.Fatal(err)
		}
		c.acks[ack.ID] = ack
		if ack.ID == id {
			return ack
		}
	}
	t.Fatalf("timed out waiting for ack %d (got %+v)", id, c.acks)
	return wsAck{}
}

func TestBrowserStreamGate(t *testing.T) {
	_, sc := connectFake(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	a.cdp = sc
	srv := httptest.NewServer(http.HandlerFunc(a.handleBrowserStream))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c := dialStream(t, wsURL)

	// No session bound: control is refused with a clear ack.
	c.send(t, wsCommand{Type: "size", ID: 1, W: 800, H: 600})
	if ack := c.waitAck(t, 1); ack.OK || ack.Error != "no session held" {
		t.Fatalf("size without session: %+v, want ok:false no session held", ack)
	}

	// Binding a bogus id is refused and leaves the socket unbound.
	c.send(t, wsCommand{Type: "session", ID: 2, SessionID: "ses_wrong"})
	if ack := c.waitAck(t, 2); ack.OK {
		t.Fatalf("bind bogus id: %+v, want ok:false", ack)
	}
	c.send(t, wsCommand{Type: "size", ID: 3, W: 800, H: 600})
	if ack := c.waitAck(t, 3); ack.OK {
		t.Fatalf("size after failed bind: %+v, want ok:false", ack)
	}

	// Bind the live session: control works.
	s, held := a.acquireSession(SessionHolder{Kind: "agent", Name: "stream-tester"}, time.Minute)
	if held != nil {
		t.Fatal("acquire failed")
	}
	c.send(t, wsCommand{Type: "session", ID: 4, SessionID: s.ID})
	if ack := c.waitAck(t, 4); !ack.OK {
		t.Fatalf("bind live session: %+v, want ok:true", ack)
	}
	c.send(t, wsCommand{Type: "size", ID: 5, W: 800, H: 600})
	if ack := c.waitAck(t, 5); !ack.OK {
		t.Fatalf("size with session: %+v, want ok:true", ack)
	}

	// Paused: control is refused until resumed.
	if _, err := a.pauseSession(s.ID, true); err != nil {
		t.Fatal(err)
	}
	c.send(t, wsCommand{Type: "size", ID: 6, W: 800, H: 600})
	if ack := c.waitAck(t, 6); ack.OK || ack.Error != "session paused" {
		t.Fatalf("size while paused: %+v, want ok:false session paused", ack)
	}
	if _, err := a.pauseSession(s.ID, false); err != nil {
		t.Fatal(err)
	}
	c.send(t, wsCommand{Type: "size", ID: 7, W: 800, H: 600})
	if ack := c.waitAck(t, 7); !ack.OK {
		t.Fatalf("size after resume: %+v, want ok:true", ack)
	}

	// Released: control is refused again, and the gate re-checks every message.
	if !a.releaseSession(s.ID) {
		t.Fatal("release failed")
	}
	c.send(t, wsCommand{Type: "size", ID: 8, W: 800, H: 600})
	if ack := c.waitAck(t, 8); ack.OK || ack.Error != "no session held" {
		t.Fatalf("size after release: %+v, want ok:false no session held", ack)
	}
}

func TestBrowserStreamSessionQuery(t *testing.T) {
	_, sc := connectFake(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	a.cdp = sc
	srv := httptest.NewServer(http.HandlerFunc(a.handleBrowserStream))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// A ?session= naming no live session is a 409 at connect time.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL+"?session=ses_wrong", nil)
	if err == nil {
		t.Fatal("dial with bogus ?session=: want an error")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("dial with bogus ?session=: status %v, want 409", resp)
	}

	// A ?session= naming the live session binds at connect: control works at once.
	s, held := a.acquireSession(SessionHolder{Kind: "agent", Name: "stream-tester"}, time.Minute)
	if held != nil {
		t.Fatal("acquire failed")
	}
	c := dialStream(t, wsURL+"?session="+s.ID)
	c.send(t, wsCommand{Type: "size", ID: 1, W: 800, H: 600})
	if ack := c.waitAck(t, 1); !ack.OK {
		t.Fatalf("size with ?session=: %+v, want ok:true", ack)
	}
}

func TestV1BrowserFrameRoute(t *testing.T) {
	_, sc := connectFake(t)
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	a.cdp = sc
	mux := http.NewServeMux()
	a.registerBrowserV1Routes(mux, a.auth)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/browser/frame?format=png", nil)
	req.Header.Set("X-Dad-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/browser/frame: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("content type %q, want image/png", got)
	}
}
