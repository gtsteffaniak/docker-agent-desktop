package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gtsteffaniak/docker-agent-desktop/internal/cdp"
)

// The browser window talks to the agent over one WebSocket. The agent pushes frames (binary)
// and navigation state (JSON, only when it changes). The client sends commands and input (JSON).
// Commands with an id get an ack. Input and size messages do not.
//
// Client to agent:  {"type":"navigate","id":1,"url":"..."}   back, forward, reload
//                   {"type":"pointer","action":"press","x":..,"y":..,"button":1,"buttons":1,"clicks":1,"mods":0}
//                   {"type":"key","action":"down","key":"a","code":"KeyA","text":"a","mods":0}
//                   {"type":"scroll","x":..,"y":..,"dx":0,"dy":100,"mods":0}
//                   {"type":"paste","text":"..."}   {"type":"size","w":900,"h":500,"scale":2}
// Agent to client:  binary JPEG frame
//                   {"type":"state","running":true,"url":"...","title":"...","can_back":true,...}
//                   {"type":"ack","id":1,"ok":true,"error":""}

// errBrowserNotRunning is returned for input and history commands before Chromium has started.
var errBrowserNotRunning = errors.New("browser is not running")

// wsCommand is every message the client can send. Fields are used by type.
type wsCommand struct {
	Type    string  `json:"type"`
	ID      int64   `json:"id,omitempty"`
	URL     string  `json:"url,omitempty"`
	W       int     `json:"w,omitempty"`
	H       int     `json:"h,omitempty"`
	Scale   float64 `json:"scale,omitempty"`
	Action  string  `json:"action,omitempty"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	DX      float64 `json:"dx,omitempty"`
	DY      float64 `json:"dy,omitempty"`
	Button  int     `json:"button,omitempty"`
	Buttons int     `json:"buttons,omitempty"`
	Clicks  int     `json:"clicks,omitempty"`
	Key     string  `json:"key,omitempty"`
	Code    string  `json:"code,omitempty"`
	Text    string  `json:"text,omitempty"`
	Mods    int     `json:"mods,omitempty"`
}

type wsAck struct {
	Type  string `json:"type"`
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type wsStateMsg struct {
	Type string `json:"type"`
	browserStateOut
}

// wsSocket serializes writes. Frames, state and acks come from different goroutines.
type wsSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *wsSocket) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.conn.Write(wctx, typ, data)
}

func (s *wsSocket) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(ctx, websocket.MessageText, data)
}

// handleBrowserWS serves the browser window's socket until the client disconnects.
func (a *Agent) handleBrowserWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	a.touch()
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sock := &wsSocket{conn: conn}
	go a.pumpFrames(ctx, sock)
	go a.pumpState(ctx, sock)
	a.readCommands(ctx, sock)
}

// pumpFrames sends each new screencast frame. A slow client gets the newest frame, not a queue.
// It holds a viewer subscription on the current screencast, so Chromium only encodes frames while
// at least one window is open, and moves the subscription when Chromium restarts.
func (a *Agent) pumpFrames(ctx context.Context, s *wsSocket) {
	var cur *cdp.Screencast
	var sent uint64
	release := func() {}
	defer func() { release() }()
	for ctx.Err() == nil {
		sc := a.screencast()
		if sc == nil {
			if cur != nil {
				release()
				release, cur = func() {}, nil
			}
			pause(ctx, 250*time.Millisecond)
			continue
		}
		if sc != cur {
			release()
			cur = sc
			sent, release = sc.Subscribe()
		}
		frame, seq, err := sc.WaitNewer(ctx, sent)
		if err != nil {
			pause(ctx, 250*time.Millisecond) // Chromium went away; look for the next one
			continue
		}
		if err := s.write(ctx, websocket.MessageBinary, frame); err != nil {
			return
		}
		sent = seq
	}
}

// pumpState tells the client whether Chromium is running, and sends the navigation state each
// time it changes. Nothing is sent while the state is steady.
func (a *Agent) pumpState(ctx context.Context, s *wsSocket) {
	var cur *cdp.Screencast
	var last uint64
	announced := false
	for ctx.Err() == nil {
		sc := a.screencast()
		if sc == nil {
			if !announced || cur != nil {
				cur = nil
				announced = true
				if err := s.writeJSON(ctx, wsStateMsg{Type: "state", browserStateOut: browserStateOut{}}); err != nil {
					return
				}
			}
			pause(ctx, 250*time.Millisecond)
			continue
		}
		if sc != cur {
			cur, last, announced = sc, 0, false
		}
		st, err := sc.WaitNav(ctx, last)
		if err != nil {
			pause(ctx, 250*time.Millisecond)
			continue
		}
		msg := wsStateMsg{Type: "state", browserStateOut: browserStateOut{NavState: st, Running: true}}
		if err := s.writeJSON(ctx, msg); err != nil {
			return
		}
		last, announced = st.Seq, true
	}
}

// readCommands handles client messages in order. Input is ordered by the socket itself, so the
// client needs no queue of its own.
func (a *Agent) readCommands(ctx context.Context, s *wsSocket) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd wsCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
		err = a.runCommand(cctx, cmd)
		cancel()
		if cmd.ID != 0 {
			ack := wsAck{Type: "ack", ID: cmd.ID, OK: err == nil}
			if err != nil {
				ack.Error = err.Error()
			}
			if werr := s.writeJSON(ctx, ack); werr != nil {
				return
			}
		} else if err != nil {
			a.log.Debug("browser input failed", "type", cmd.Type, "err", err)
		}
	}
}

// runCommand performs one client command. Navigation starts Chromium if it is not running yet.
func (a *Agent) runCommand(ctx context.Context, cmd wsCommand) error {
	a.touch()
	if cmd.Type == "navigate" {
		sc, err := a.ensureBrowser(ctx)
		if err != nil {
			return err
		}
		target, err := NormalizeURL(cmd.URL)
		if err != nil {
			return err
		}
		return sc.Navigate(ctx, target)
	}
	sc := a.screencast()
	if sc == nil {
		return errBrowserNotRunning
	}
	switch cmd.Type {
	case "back":
		return sc.GoBack(ctx)
	case "forward":
		return sc.GoForward(ctx)
	case "reload":
		return sc.Reload(ctx)
	case "size":
		return sc.SetViewport(ctx, cmd.W, cmd.H, cmd.Scale)
	case "pointer":
		return sc.Mouse(ctx, cdp.MouseEvent{Action: cmd.Action, X: cmd.X, Y: cmd.Y, Button: cmd.Button, Buttons: cmd.Buttons, Clicks: cmd.Clicks, Mods: cmd.Mods})
	case "key":
		return sc.Key(ctx, cdp.KeyEvent{Action: cmd.Action, Key: cmd.Key, Code: cmd.Code, Text: cmd.Text, Mods: cmd.Mods})
	case "scroll":
		return sc.Scroll(ctx, cmd.X, cmd.Y, cmd.DX, cmd.DY, cmd.Mods)
	case "paste":
		return sc.InsertText(ctx, cmd.Text)
	}
	return fmt.Errorf("unknown message type %q", cmd.Type)
}

// pause sleeps unless the context ends first.
func pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
