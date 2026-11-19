package broadway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// maxMessageBytes bounds one broadwayd frame. Full-screen PUT_BUFFER updates are a few MB at most.
const maxMessageBytes = 16 << 20

// writeTimeout bounds one input or close write.
const writeTimeout = 5 * time.Second

// ErrNotConnected is returned when an operation needs a live broadwayd connection.
var ErrNotConnected = errors.New("broadway: not connected")

// Client is the sole Broadway WebSocket client for one broadwayd display.
// It is safe for concurrent use. Connect and Run manage the connection lifecycle.
type Client struct {
	url      string
	log      *slog.Logger
	screenW  int
	screenH  int
	backdrop Backdrop

	encMu sync.Mutex // serializes encoder state and the writes that depend on it
	enc   *InputEncoder

	mu       sync.Mutex // guards the fields below and surfaces
	serial   uint32     // serial of the latest server message
	conn     *websocket.Conn
	done     chan struct{} // closed when the current connection's read loop exits
	surfaces map[uint16]*Surface

	ready     chan struct{}
	readyOnce sync.Once
}

// Backdrop supplies the wallpaper drawn behind surfaces in screenshots.
type Backdrop interface {
	// Image returns the wallpaper at w by h. Callers must not modify it.
	Image(w, h int) *image.RGBA
}

// Config holds connection parameters for a Broadway client.
type Config struct {
	// Backdrop is the screenshot wallpaper. Nil draws a flat colour.
	Backdrop Backdrop
	// HostPort is the broadwayd listen address (e.g. "127.0.0.1:8081").
	HostPort string
	// Width and Height are the client viewport in pixels.
	Width  int
	Height int
}

// NewClient prepares a client. Call Connect or Run to dial broadwayd.
// A nil logger uses slog.Default.
func NewClient(cfg Config, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		url:      fmt.Sprintf("ws://%s/socket", cfg.HostPort),
		log:      log.With("component", "broadway"),
		enc:      NewInputEncoder(),
		surfaces: make(map[uint16]*Surface),
		screenW:  cfg.Width,
		screenH:  cfg.Height,
		backdrop: cfg.Backdrop,
		ready:    make(chan struct{}),
	}
}

// Connect dials broadwayd once and starts the read loop. It replaces surface state
// because broadwayd resends the full scene on every new connection.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := Dial(ctx, c.url)
	if err != nil {
		return err
	}

	done := make(chan struct{})
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.CloseNow()
	}
	c.conn = conn
	c.done = done
	c.surfaces = make(map[uint16]*Surface)
	c.mu.Unlock()

	go c.readLoop(conn, done)

	// Lock the encoder while the screen-size message is written so no input
	// interleaves ahead of it.
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.write(conn, c.enc.SetScreenSize(int32(c.screenW), int32(c.screenH)))
}

// Run keeps a connection to broadwayd open until ctx ends. It reconnects with
// exponential backoff (200 ms to 5 s) after a failed dial or a dropped connection.
func (c *Client) Run(ctx context.Context) error {
	const minBackoff, maxBackoff = 200 * time.Millisecond, 5 * time.Second
	backoff := minBackoff
	for {
		if err := c.Connect(ctx); err != nil {
			c.log.Warn("connect failed", "url", c.url, "err", err, "retry_in", backoff)
		} else {
			c.log.Info("connected", "url", c.url)
			backoff = minBackoff
			select {
			case <-ctx.Done():
				_ = c.Close()
				return ctx.Err()
			case <-c.Done():
				c.log.Warn("disconnected, reconnecting", "retry_in", backoff)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// Done returns a channel closed when the current connection ends.
// Before the first successful Connect it returns a channel that never closes.
func (c *Client) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		return make(chan struct{})
	}
	return c.done
}

// Connected reports whether a connection is currently open.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		typ, data, err := conn.Read(context.Background())
		if err != nil {
			if websocket.CloseStatus(err) == -1 {
				c.log.Warn("read failed", "err", err)
			}
			c.mu.Lock()
			if c.conn == conn {
				c.conn = nil
			}
			c.mu.Unlock()
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		c.mu.Lock()
		debug := c.log.Enabled(context.Background(), slog.LevelDebug)
		var moved []uint16
		trace := func(op byte, serial uint32, id uint16, w, h int) {
			c.serial = serial
			if op == 'm' && !slices.Contains(moved, id) {
				moved = append(moved, id)
			}
			if debug {
				c.log.Debug("op", "op", string(rune(op)), "surface", id, "w", w, "h", h)
			}
		}
		if err := ApplyServerMessageTraced(data, c.surfaces, trace); err != nil {
			c.log.Warn("dropped malformed broadway message", "err", err, "bytes", len(data))
		}
		type geom struct {
			id         uint32
			x, y, w, h int32
		}
		var confirms []geom
		for _, id := range moved {
			if s := c.surfaces[id]; s != nil {
				confirms = append(confirms, geom{uint32(id), int32(s.X), int32(s.Y), int32(s.Width), int32(s.Height)})
			}
		}
		c.mu.Unlock()
		// The browser confirms every move or resize, and GDK waits for it before redrawing.
		if len(confirms) > 0 {
			err := c.send(func(e *InputEncoder) [][]byte {
				msgs := make([][]byte, 0, len(confirms))
				for _, g := range confirms {
					msgs = append(msgs, e.ConfigureNotify(g.id, g.x, g.y, g.w, g.h))
				}
				return msgs
			})
			if err != nil {
				c.log.Debug("configure notify not sent", "err", err)
			}
		}
		c.readyOnce.Do(func() { close(c.ready) })
	}
}

// write sends one binary message with a timeout. Callers hold encMu when ordering matters.
func (c *Client) write(conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageBinary, msg)
}

// send encodes with build and writes the result in order. The encoder lock is held across
// both steps, so serial numbers match wire order. Lock order is encMu, then mu.
func (c *Client) send(build func(*InputEncoder) [][]byte) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	c.mu.Lock()
	c.enc.SetServerSerial(c.serial)
	c.mu.Unlock()
	return c.writeLocked(build(c.enc))
}

// sendAll writes already-encoded messages in order.
func (c *Client) sendAll(msgs [][]byte) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.writeLocked(msgs)
}

// writeLocked writes msgs to the current connection. The caller holds encMu.
func (c *Client) writeLocked(msgs [][]byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	for _, m := range msgs {
		if err := c.write(conn, m); err != nil {
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
	var bg *image.RGBA
	if c.backdrop != nil {
		bg = c.backdrop.Image(c.screenW, c.screenH)
	}
	img := ComposeScreenshot(c.surfaces, c.screenW, c.screenH, bg)
	c.mu.Unlock()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ClickAt clicks at root coordinates. It finds the surface under the point, because broadwayd
// expects the target surface and window-relative coordinates, as the browser client sends them.
// A point over no surface lands on the bare desktop (surface 0).
func (c *Client) ClickAt(x, y int32) error {
	p := c.pointerAt(x, y)
	return c.send(func(e *InputEncoder) [][]byte { return e.Click(p) })
}

// pointerAt resolves root coordinates to the topmost surface that is visible and opaque there.
// Surfaces stack in ID order, matching ComposeScreenshot, so higher IDs win. Fully transparent
// pixels (such as a dock's shadow margin) let the click fall through to what is underneath.
func (c *Client) pointerAt(x, y int32) Pointer {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := slices.Sorted(maps.Keys(c.surfaces))
	for i := len(ids) - 1; i >= 0; i-- {
		s := c.surfaces[ids[i]]
		wx, wy := int(x)-s.X, int(y)-s.Y
		if !s.Visible || wx < 0 || wy < 0 || wx >= s.Width || wy >= s.Height {
			continue
		}
		// The buffer can lag the surface size right after a resize, so index it by its own width.
		if pw := s.PixelsW; pw > 0 && wx < pw {
			if off := (wy*pw + wx) * 4; off+3 < len(s.Pixels) && s.Pixels[off+3] == 0 {
				continue
			}
		}
		return Pointer{Surface: uint32(s.ID), RootX: x, RootY: y, WinX: int32(wx), WinY: int32(wy)}
	}
	return Pointer{RootX: x, RootY: y, WinX: x, WinY: y}
}

// TypeText injects keystrokes.
func (c *Client) TypeText(text string) error {
	return c.send(func(e *InputEncoder) [][]byte { return e.TypeText(text) })
}

// SendRaw writes one already-encoded input message (used by the portal proxy).
func (c *Client) SendRaw(msg []byte) error {
	return c.sendAll([][]byte{msg})
}

// Close ends the current WebSocket session. It is safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close(websocket.StatusNormalClosure, "")
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
