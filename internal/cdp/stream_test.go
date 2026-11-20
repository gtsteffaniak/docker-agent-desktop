package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome is a minimal DevTools page target. It answers every command, records the methods it
// saw, and while a screencast is running it sends a frame each time the test asks for one.
type fakeChrome struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	methods   []string
	acks      []int
	streaming bool
	session   int
	conn      *websocket.Conn
	// silentStart makes Page.startScreencast send no initial frame, like a page that is not
	// repainting, so the captureScreenshot fallback has to supply one.
	silentStart bool
	changed     chan struct{}
}

func newFakeChrome(t *testing.T) *fakeChrome {
	t.Helper()
	f := &fakeChrome{t: t, changed: make(chan struct{}, 64)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChrome) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeChrome) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
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
		result := map[string]any{}
		sendFrame := false
		f.mu.Lock()
		f.methods = append(f.methods, msg.Method)
		switch msg.Method {
		case "Page.startScreencast":
			f.streaming = true
			f.session++
			sendFrame = !f.silentStart
		case "Page.stopScreencast":
			f.streaming = false
		case "Page.screencastFrameAck":
			var p struct {
				SessionID int `json:"sessionId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.acks = append(f.acks, p.SessionID)
		case "Page.captureScreenshot":
			var p struct {
				Format string `json:"format"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			result["data"] = base64.StdEncoding.EncodeToString([]byte("shot-" + p.Format))
		case "Page.getNavigationHistory":
			result["currentIndex"] = 0
			result["entries"] = []map[string]any{{"id": 1, "url": "about:blank", "title": ""}}
		}
		f.mu.Unlock()
		out, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
		if conn.Write(r.Context(), websocket.MessageText, out) != nil {
			return
		}
		if sendFrame {
			f.frame("start")
		}
		select {
		case f.changed <- struct{}{}:
		default:
		}
	}
}

// frame sends one Page.screencastFrame if the screencast is running, and reports whether it did.
func (f *fakeChrome) frame(body string) bool {
	f.mu.Lock()
	conn, on, sid := f.conn, f.streaming, f.session
	f.mu.Unlock()
	if conn == nil || !on {
		return false
	}
	ev, _ := json.Marshal(map[string]any{
		"method": "Page.screencastFrame",
		"params": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(body)), "sessionId": sid},
	})
	return conn.Write(context.Background(), websocket.MessageText, ev) == nil
}

func (f *fakeChrome) count(method string) int {
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

func (f *fakeChrome) isStreaming() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streaming
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
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

// checkNoLeaks fails when goroutines outlive the test, in the spirit of goleak but stdlib only.
func checkNoLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= before {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines leaked: %d before, %d after\n%s", before, runtime.NumGoroutine(), buf[:n])
	})
}

func dial(t *testing.T, f *fakeChrome, linger time.Duration) *Screencast {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := connect(ctx, f.url(), linger)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNoScreencastWithoutViewers(t *testing.T) {
	f := newFakeChrome(t)
	s := dial(t, f, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if n := f.count("Page.startScreencast"); n != 0 {
		t.Fatalf("startScreencast sent %d times with no viewers", n)
	}
	if s.Streaming() {
		t.Fatal("Streaming() is true with no viewers")
	}
}

func TestSubscribeStartsAndLastReleaseStopsAfterLinger(t *testing.T) {
	checkNoLeaks(t)
	f := newFakeChrome(t)
	linger := 150 * time.Millisecond
	s := dial(t, f, linger)

	since1, release1 := s.Subscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	frame, seq, err := s.WaitNewer(ctx, since1)
	if err != nil || string(frame) != "start" || seq != 1 {
		t.Fatalf("first viewer: frame %q seq %d err %v", frame, seq, err)
	}
	eventually(t, "the frame ack", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.acks) == 1 && f.acks[0] == 1
	})

	// A second viewer gets the cached frame at once and does not restart the stream.
	since2, release2 := s.Subscribe()
	if since2 != 0 {
		t.Fatalf("second viewer since = %d, want 0 (cached frame is current)", since2)
	}
	if got, _, err := s.WaitNewer(ctx, since2); err != nil || string(got) != "start" {
		t.Fatalf("second viewer: %q %v", got, err)
	}
	if n := f.count("Page.startScreencast"); n != 1 {
		t.Fatalf("startScreencast sent %d times, want 1", n)
	}

	release1()
	release1() // idempotent
	time.Sleep(2 * linger)
	if !f.isStreaming() {
		t.Fatal("stream stopped while a viewer remained")
	}
	release2()
	if !s.Streaming() {
		t.Fatal("stream stopped before the linger")
	}
	eventually(t, "stopScreencast", func() bool { return f.count("Page.stopScreencast") == 1 })
	if s.Streaming() || s.Viewers() != 0 {
		t.Fatalf("after stop: streaming %v viewers %d", s.Streaming(), s.Viewers())
	}
}

func TestReconnectWithinLingerDoesNotThrash(t *testing.T) {
	f := newFakeChrome(t)
	linger := 300 * time.Millisecond
	s := dial(t, f, linger)
	_, release := s.Subscribe()
	eventually(t, "stream start", s.Streaming)
	for i := 0; i < 5; i++ {
		release()
		time.Sleep(linger / 5)
		var since uint64
		since, release = s.Subscribe()
		if since != 0 {
			t.Fatalf("reconnect %d: since %d, want the cached frame", i, since)
		}
	}
	release()
	eventually(t, "stopScreencast", func() bool { return f.count("Page.stopScreencast") == 1 })
	if n := f.count("Page.startScreencast"); n != 1 {
		t.Fatalf("startScreencast sent %d times, want 1", n)
	}
}

func TestRestartAfterStopWaitsForFreshFrame(t *testing.T) {
	f := newFakeChrome(t)
	s := dial(t, f, 10*time.Millisecond)
	since, release := s.Subscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := s.WaitNewer(ctx, since); err != nil {
		t.Fatal(err)
	}
	release()
	eventually(t, "stopScreencast", func() bool { return f.count("Page.stopScreencast") == 1 })

	// The cached frame may be stale now, so a new viewer must wait for the next one.
	since, release = s.Subscribe()
	defer release()
	if since != 1 {
		t.Fatalf("since after restart = %d, want 1", since)
	}
	_, seq, err := s.WaitNewer(ctx, since)
	if err != nil || seq != 2 {
		t.Fatalf("fresh frame: seq %d err %v", seq, err)
	}
	if n := f.count("Page.startScreencast"); n != 2 {
		t.Fatalf("startScreencast sent %d times, want 2", n)
	}
}

func TestSilentStartFallsBackToCapture(t *testing.T) {
	f := newFakeChrome(t)
	f.silentStart = true
	s := dial(t, f, time.Second)
	since, release := s.Subscribe()
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frame, _, err := s.WaitNewer(ctx, since)
	if err != nil || string(frame) != "shot-jpeg" {
		t.Fatalf("fallback frame %q err %v", frame, err)
	}
	// Later screencast frames still flow and are acknowledged.
	if !f.frame("next") {
		t.Fatal("fake is not streaming")
	}
	frame, _, err = s.WaitNewer(ctx, 1)
	if err != nil || string(frame) != "next" {
		t.Fatalf("next frame %q err %v", frame, err)
	}
}

func TestKeepLeasesTheStream(t *testing.T) {
	f := newFakeChrome(t)
	linger := 100 * time.Millisecond
	s := dial(t, f, linger)
	if base := s.Keep(); base != 0 {
		t.Fatalf("first Keep base = %d", base)
	}
	eventually(t, "stream start", s.Streaming)
	for i := 0; i < 4; i++ {
		time.Sleep(linger / 2)
		if base := s.Keep(); base != 0 {
			t.Fatalf("poll %d base = %d, want 0 while streaming", i, base)
		}
	}
	if f.count("Page.stopScreencast") != 0 {
		t.Fatal("stream stopped while polls kept it alive")
	}
	eventually(t, "stopScreencast", func() bool { return f.count("Page.stopScreencast") == 1 })
}

func TestCaptureFormats(t *testing.T) {
	f := newFakeChrome(t)
	s := dial(t, f, time.Second)
	ctx := context.Background()
	for _, tc := range []struct{ in, want string }{{"", "shot-jpeg"}, {"jpeg", "shot-jpeg"}, {"png", "shot-png"}} {
		img, err := s.Capture(ctx, tc.in)
		if err != nil || string(img) != tc.want {
			t.Fatalf("Capture(%q) = %q, %v", tc.in, img, err)
		}
	}
	if _, err := s.Capture(ctx, "gif"); err == nil {
		t.Fatal("gif should be rejected")
	}
	if f.count("Page.startScreencast") != 0 {
		t.Fatal("Capture must not start the screencast")
	}
}

func TestBrowserGoneEndsWaitersAndWorker(t *testing.T) {
	checkNoLeaks(t)
	f := newFakeChrome(t)
	s := dial(t, f, time.Hour)
	since, release := s.Subscribe()
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := s.WaitNewer(ctx, since); err != nil {
		t.Fatal(err)
	}
	// Chromium goes away while a viewer is connected.
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	_ = conn.CloseNow()
	if _, _, err := s.WaitNewer(ctx, 99); err != ErrClosed {
		t.Fatalf("WaitNewer after drop: %v, want ErrClosed", err)
	}
	select {
	case <-s.workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stream worker did not exit after the connection dropped")
	}
}

func TestManyWaitersAllWake(t *testing.T) {
	f := newFakeChrome(t)
	s := dial(t, f, time.Second)
	since, release := s.Subscribe()
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := s.WaitNewer(ctx, since); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.WaitNewer(ctx, 1)
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	f.frame("second")
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a waiter missed the frame: %v", err)
		}
	}
}
