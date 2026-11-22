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
// The "session" type binds a session id to the socket (v1 stream only);
// it is never a browser command.
type wsCommand struct {
	Type      string  `json:"type"`
	ID        int64   `json:"id,omitempty"`
	URL       string  `json:"url,omitempty"`
	W         int     `json:"w,omitempty"`
	H         int     `json:"h,omitempty"`
	Scale     float64 `json:"scale,omitempty"`
	Action    string  `json:"action,omitempty"`
	X         float64 `json:"x,omitempty"`
	Y         float64 `json:"y,omitempty"`
	DX        float64 `json:"dx,omitempty"`
	DY        float64 `json:"dy,omitempty"`
	Button    int     `json:"button,omitempty"`
	Buttons   int     `json:"buttons,omitempty"`
	Clicks    int     `json:"clicks,omitempty"`
	Key       string  `json:"key,omitempty"`
	Code      string  `json:"code,omitempty"`
	Text      string  `json:"text,omitempty"`
	Mods      int     `json:"mods,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
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

// handleBrowserStream is the /v1 browser socket: frames and navigation state
// pumped down, commands and input read in order. A WebSocket cannot carry
// the X-Dad-Session header, so the session travels differently: a
// ?session=<id> query parameter on connect, or a
// {"type":"session","session_id":"..."} message that (re)binds the socket
// mid-connection. Frames and state flow without a session (reads); every
// control message is refused while no live, unpaused session is bound. A
// ?session= that names no live session is a 409 at connect time, so an API
// client learns immediately instead of on its first command.
//
// Down: binary JPEG frames and {"type":"state", ...} navigation state. Up:
// {"type":"navigate","url":...}, {"type":"back"|"forward"|"reload"},
// {"type":"pointer"|"key"|"scroll"|"paste", ...} and {"type":"size","w","h","scale"}.
// Control messages with an "id" get an {"type":"ack","id","ok","error"} reply.
// @Summary Browser live stream
// @Description The live browser socket: binary JPEG frames and navigation-state messages pumped down, navigate/pointer/key/scroll/paste/size commands read in order. A WebSocket cannot carry the X-Dad-Session header, so the session travels as ?session=<id> at connect, or a {"type":"session","session_id":"..."} message that (re)binds the socket mid-connection. Frames and state flow without a session (reads); every control message is refused while no live, unpaused session is bound. A ?session= naming no live session is a 409 at connect time. Control messages carrying an "id" get an {"type":"ack","id","ok","error"} reply.
// @Tags browser
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param session query string false "Session id to bind at connect (control messages need a live, unpaused session bound)"
// @Success 101 "Switched to the WebSocket protocol"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "?session= names no live session"
// @Security ApiKeyAuth
// @Router /v1/browser/stream [get]
func (a *Agent) handleBrowserStream(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	if sid != "" {
		if _, err := a.sessionByID(sid); err != nil {
			var held *sessionHeldError
			switch {
			case errors.Is(err, errNoSession):
				writeSessionError(w, http.StatusConflict, "no session held", a.sessionState())
			case errors.As(err, &held):
				writeSessionError(w, http.StatusConflict, err.Error(), a.sessionState())
			default:
				writeSessionError(w, http.StatusConflict, err.Error(), a.sessionState())
			}
			return
		}
	}
	a.serveBrowserSocket(w, r, sid)
}

// serveBrowserSocket is the socket core: frames and state pumped down,
// commands read in order. bound seeds the session bound to the socket (from
// ?session=, empty otherwise). Control messages are gated: the gate
// requires the bound session to be the live, unpaused session — even when
// nothing is bound yet, so an unbound socket refuses control until a
// {"type":"session"} message binds it.
func (a *Agent) serveBrowserSocket(w http.ResponseWriter, r *http.Request, bound string) {
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
	b := bound
	a.readCommands(ctx, sock, &b)
}

// streamGate returns the control-message gate for a stream socket. bound is
// the session currently bound to the socket; the gate re-checks it against
// the live lease on every message, so an expired, released or paused
// session stops working mid-connection, and a takeover by another holder is
// refused until the socket rebinds.
func streamGate(a *Agent, bound *string) func(wsCommand) error {
	return func(cmd wsCommand) error {
		if cmd.Type == "session" {
			return nil
		}
		if *bound == "" {
			return errNoSession
		}
		_, err := a.sessionByID(*bound)
		return err
	}
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
// client needs no queue of its own. bound is the session currently bound to the socket; the
// gate re-checks it against the live lease for every control message, so an expired, released
// or paused session stops working mid-connection. A gated failure is an ack with ok:false
// when the message carries an id, and a dropped message otherwise. A "session" message binds
// (or rebinds) the socket to a session id, validated against the live lease; it is the only
// message the gate never sees. bound is only touched in this goroutine, so no lock is needed.
func (a *Agent) readCommands(ctx context.Context, s *wsSocket, bound *string) {
	gate := streamGate(a, bound)
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd wsCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}
		if cmd.Type == "session" {
			a.bindSocketSession(ctx, s, cmd, bound)
			continue
		}
		if gerr := gate(cmd); gerr != nil {
			if cmd.ID != 0 {
				ack := wsAck{Type: "ack", ID: cmd.ID, OK: false, Error: gerr.Error()}
				if werr := s.writeJSON(ctx, ack); werr != nil {
					return
				}
			} else {
				a.log.Debug("browser control refused: no live session bound", "type", cmd.Type, "err", gerr)
			}
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

// bindSocketSession handles a {"type":"session"} message: it validates the
// session id against the live lease and, when valid, rebinds the socket's
// gate to it by writing *bound, which the gate closure reads on the next
// control message. A message without an id, or naming no live session, is
// an ack with ok:false and leaves the previous binding (if any) in place.
func (a *Agent) bindSocketSession(ctx context.Context, s *wsSocket, cmd wsCommand, bound *string) {
	ack := wsAck{Type: "ack", ID: cmd.ID, OK: false}
	if _, err := a.sessionByID(cmd.SessionID); err != nil {
		ack.Error = err.Error()
	} else {
		*bound = cmd.SessionID
		ack.OK = true
	}
	if cmd.ID != 0 {
		if werr := s.writeJSON(ctx, ack); werr != nil {
			return
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
