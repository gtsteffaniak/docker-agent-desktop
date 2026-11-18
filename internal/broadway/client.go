package broadway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client is the sole Broadway WebSocket client for one broadwayd display.
type Client struct {
	url      string
	conn     *websocket.Conn
	enc      *InputEncoder
	mu       sync.Mutex
	surfaces map[uint16]*Surface
	screenW  int
	screenH  int
	ready    chan struct{}
	once     sync.Once
}

// Config holds connection parameters for a Broadway client.
type Config struct {
	// HostPort is the broadwayd listen address (e.g. "127.0.0.1:8081").
	HostPort string
	// Width and Height are the client viewport in pixels.
	Width  int
	Height int
}

// NewClient dials broadwayd and prepares the encoder.
func NewClient(cfg Config) *Client {
	path := fmt.Sprintf("ws://%s/socket", cfg.HostPort)
	return &Client{
		url:      path,
		enc:      NewInputEncoder(),
		surfaces: make(map[uint16]*Surface),
		screenW:  cfg.Width,
		screenH:  cfg.Height,
		ready:    make(chan struct{}),
	}
}

// Connect opens the WebSocket and starts the reader goroutine.
func (c *Client) Connect(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"broadway"},
	}
	conn, _, err := dialer.DialContext(ctx, c.url, nil)
	if err != nil {
		return err
	}
	c.conn = conn
	go c.readLoop()
	if err := c.Send(c.enc.SetScreenSize(int32(c.screenW), int32(c.screenH))); err != nil {
		return err
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.mu.Lock()
		_ = ApplyServerMessage(data, c.surfaces)
		c.once.Do(func() { close(c.ready) })
		c.mu.Unlock()
	}
}

// Send writes one binary input message.
func (c *Client) Send(msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, msg)
}

// SendAll sends multiple input messages in order.
func (c *Client) SendAll(msgs [][]byte) error {
	for _, m := range msgs {
		if err := c.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// WaitReady blocks until at least one server frame was received or ctx ends.
func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitFrame blocks until surfaces contain pixels or ctx ends.
func (c *Client) WaitFrame(ctx context.Context, poll time.Duration) error {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		c.mu.Lock()
		ok := c.hasPixels()
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (c *Client) hasPixels() bool {
	for _, s := range c.surfaces {
		if s.Visible && len(s.Pixels) > 0 {
			return true
		}
	}
	return false
}

// Surfaces returns a snapshot copy of current surfaces.
func (c *Client) Surfaces() map[uint16]*Surface {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[uint16]*Surface, len(c.surfaces))
	for id, s := range c.surfaces {
		cp := *s
		if len(s.Pixels) > 0 {
			cp.Pixels = append([]byte(nil), s.Pixels...)
		}
		out[id] = &cp
	}
	return out
}

// ScreenshotPNG composites surfaces and returns PNG bytes.
func (c *Client) ScreenshotPNG() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	img := ComposeScreenshot(c.surfaces, c.screenW, c.screenH)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ClickAt sends pointer events at root coordinates (surface 0 lets server pick).
func (c *Client) ClickAt(x, y int32) error {
	id := uint32(0)
	msgs := c.enc.Click(id, x, y)
	return c.SendAll(msgs)
}

// TypeText injects keystrokes.
func (c *Client) TypeText(text string) error {
	return c.SendAll(c.enc.TypeText(text))
}

// Close ends the WebSocket session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Meta is debug metadata exported for tests.
type Meta struct {
	// SurfaceCount is the number of known surfaces.
	SurfaceCount int `json:"surface_count"`
	// Surfaces maps surface ID strings to placement and buffer metadata.
	Surfaces map[string]any `json:"surfaces"`
}

// DebugMeta returns JSON-friendly surface metadata.
func (c *Client) DebugMeta() Meta {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := Meta{Surfaces: make(map[string]any)}
	for id, s := range c.surfaces {
		key := fmt.Sprintf("%d", id)
		m.Surfaces[key] = map[string]any{
			"x": s.X, "y": s.Y, "w": s.Width, "h": s.Height,
			"visible": s.Visible, "bytes": len(s.Pixels),
		}
	}
	m.SurfaceCount = len(c.surfaces)
	return m
}

// MarshalDebug encodes DebugMeta as JSON.
func (c *Client) MarshalDebug() ([]byte, error) {
	return json.Marshal(c.DebugMeta())
}
