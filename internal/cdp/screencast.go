// Package cdp is a minimal Chromium DevTools client: it runs Page.startScreencast
// and keeps the latest JPEG frame.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ErrClosed is returned after the DevTools connection has ended.
var ErrClosed = errors.New("cdp: connection closed")

// callTimeout bounds one request/response round trip.
const callTimeout = 10 * time.Second

// Screencast captures JPEG frames from Page.startScreencast.
type Screencast struct {
	conn    *websocket.Conn
	cancel  context.CancelFunc // ends the connection's lifetime context
	nextID  atomic.Int64
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan result

	mu        sync.Mutex
	last      []byte
	seq       uint64        // increments with every stored frame
	nav       NavState      // latest navigation state
	navNotify chan struct{} // closed and replaced whenever nav changes
	entries   []navEntry    // history from the last Page.getNavigationHistory
	idx       int           // index of the current entry in entries
	frames    chan struct{} // signaled (non-blocking) when a new frame arrives
	done      chan struct{} // closed when the read loop exits
}

type result struct {
	raw json.RawMessage
	err error
}

// Connect dials a page DevTools WebSocket and starts a screencast.
// The connection lives until Close or until Chromium drops it; ctx only bounds setup.
func Connect(ctx context.Context, wsURL string) (*Screencast, error) {
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("cdp dial: %w (status %s)", err, resp.Status)
		}
		return nil, err
	}
	conn.SetReadLimit(16 << 20)

	life, cancel := context.WithCancel(context.Background())
	s := &Screencast{
		conn:      conn,
		cancel:    cancel,
		pending:   make(map[int64]chan result),
		frames:    make(chan struct{}, 1),
		navNotify: make(chan struct{}),
		done:      make(chan struct{}),
	}
	go s.readLoop(life)

	setup := func(method string, params any) error {
		cctx, c := context.WithTimeout(ctx, callTimeout)
		defer c()
		_, err := s.Call(cctx, method, params)
		return err
	}
	if err := setup("Page.enable", nil); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := setup("Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       90,
		"maxWidth":      2560,
		"maxHeight":     1600,
		"everyNthFrame": 1,
	}); err != nil {
		_ = s.Close()
		return nil, err
	}
	// Seed the navigation state, then keep it fresh. Events trigger most refreshes; the poll
	// catches title changes and anything an event missed.
	_ = s.RefreshNav(ctx)
	go s.pollNav(life)
	return s, nil
}

type message struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Params json.RawMessage `json:"params"`
}

func (s *Screencast) readLoop(life context.Context) {
	defer close(s.done)
	defer s.failPending()
	for {
		_, data, err := s.conn.Read(life)
		if err != nil {
			return
		}
		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			s.resolve(msg)
			continue
		}
		switch msg.Method {
		case "Page.screencastFrame":
			s.onFrame(msg.Params)
		case "Page.frameNavigated":
			if isMainFrameNavigation(msg.Params) {
				go s.refreshNavQuiet()
			}
		case "Page.navigatedWithinDocument":
			go s.refreshNavQuiet()
		}
	}
}

// onFrame stores a screencast frame and acknowledges it so Chromium keeps sending.
func (s *Screencast) onFrame(params json.RawMessage) {
	var p struct {
		Data      string `json:"data"`
		SessionID int    `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.last = raw
	s.seq++
	s.mu.Unlock()
	select {
	case s.frames <- struct{}{}:
	default:
	}
	// Fire-and-forget acknowledgement.
	go func(sid int) {
		_, _ = s.Call(context.Background(), "Page.screencastFrameAck", map[string]any{"sessionId": sid})
	}(p.SessionID)
}

// Frame returns a copy of the latest frame and its sequence number. The sequence is 0 until
// the first frame arrives, so a caller that passes its last seen value can skip repeats.
func (s *Screencast) Frame() ([]byte, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.last...), s.seq
}

func (s *Screencast) resolve(msg message) {
	s.pendingMu.Lock()
	ch, ok := s.pending[msg.ID]
	delete(s.pending, msg.ID)
	s.pendingMu.Unlock()
	if !ok {
		return
	}
	if msg.Error != nil {
		ch <- result{err: fmt.Errorf("cdp: %s", msg.Error.Message)}
		return
	}
	ch <- result{raw: msg.Result}
}

// failPending wakes every waiting caller when the connection ends.
func (s *Screencast) failPending() {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, ch := range s.pending {
		ch <- result{err: ErrClosed}
		delete(s.pending, id)
	}
}

// Call sends one DevTools command and waits for its response.
func (s *Screencast) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)
	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ch := make(chan result, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()

	s.writeMu.Lock()
	err = s.conn.Write(ctx, websocket.MessageText, data)
	s.writeMu.Unlock()
	if err != nil {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-ctx.Done():
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// WaitJPEG returns the latest frame, waiting for one if none has arrived.
// It returns ErrClosed if the connection ends first, so callers never get a stale frame.
func (s *Screencast) WaitJPEG(ctx context.Context) ([]byte, error) {
	for {
		s.mu.Lock()
		frame := s.last
		s.mu.Unlock()
		if len(frame) > 0 {
			return append([]byte(nil), frame...), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.done:
			return nil, ErrClosed
		case <-s.frames:
		}
	}
}

// Done is closed when the DevTools connection ends.
func (s *Screencast) Done() <-chan struct{} { return s.done }

// Close stops the screencast connection. It is safe to call more than once.
func (s *Screencast) Close() error {
	s.cancel()
	return s.conn.CloseNow()
}

// DiscoverWS resolves a page target DevTools websocket URL from an HTTP debug port.
func DiscoverWS(ctx context.Context, hostPort string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hostPort+"/json/list", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		URL                  string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", fmt.Errorf("no page target on %s", hostPort)
}

// WaitNewer blocks until a frame newer than since is available, then returns it with its
// sequence number. Callers pass the last sequence they sent, so a slow client skips intermediate
// frames instead of queueing them.
func (s *Screencast) WaitNewer(ctx context.Context, since uint64) ([]byte, uint64, error) {
	for {
		frame, seq := s.Frame()
		if seq > since && len(frame) > 0 {
			return frame, seq, nil
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-s.done:
			return nil, 0, ErrClosed
		case <-s.frames:
		}
	}
}
