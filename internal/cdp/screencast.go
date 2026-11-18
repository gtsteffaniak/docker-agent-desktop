// Package cdp implements a minimal Chromium DevTools client for Phase 0 spike tests.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Screencast captures JPEG frames from Page.startScreencast.
type Screencast struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next int64
	last []byte
}

// Connect dials a page DevTools WebSocket and starts screencast.
func Connect(ctx context.Context, wsURL string) (*Screencast, error) {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := d.DialContext(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("cdp dial: %w (status %s)", err, resp.Status)
		}
		return nil, err
	}
	s := &Screencast{conn: conn, next: 1}
	go s.readLoop()
	if err := s.call(ctx, "Page.enable", nil); err != nil {
		return nil, err
	}
	if err := s.call(ctx, "Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       70,
		"maxWidth":      1280,
		"maxHeight":     720,
		"everyNthFrame": 1,
	}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Screencast) readLoop() {
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Data      string `json:"data"`
				SessionID int    `json:"sessionId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Method != "Page.screencastFrame" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(msg.Params.Data)
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.last = raw
		s.mu.Unlock()
		_ = s.call(context.Background(), "Page.screencastFrameAck", map[string]any{
			"sessionId": msg.Params.SessionID,
		})
	}
}

func (s *Screencast) call(ctx context.Context, method string, params any) error {
	id := atomic.AddInt64(&s.next, 1)
	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	data, _ := json.Marshal(payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("closed")
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

// WaitJPEG blocks until a frame arrives or ctx is done.
func (s *Screencast) WaitJPEG(ctx context.Context, poll time.Duration) ([]byte, error) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		s.mu.Lock()
		frame := append([]byte(nil), s.last...)
		s.mu.Unlock()
		if len(frame) > 0 {
			return frame, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
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
