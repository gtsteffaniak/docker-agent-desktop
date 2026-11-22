package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable clock for the session store.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClockAgent(t *testing.T) (*Agent, *fakeClock) {
	t.Helper()
	a := newSessionAgent(t)
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	a.sessions.now = clk.Now
	return a, clk
}

func countEvents(a *Agent, typ string) int {
	n := 0
	for _, ev := range a.eventsSince(0, maxActivityEvents) {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func TestClampLeaseTTL(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, minSessionLease},
		{time.Millisecond, minSessionLease},
		{9999 * time.Millisecond, minSessionLease},
		{10 * time.Second, 10 * time.Second},
		{90 * time.Second, 90 * time.Second},
		{10 * time.Minute, 10 * time.Minute},
		{11 * time.Minute, maxSessionLease},
		{-time.Second, minSessionLease},
	}
	for _, c := range cases {
		if got := clampLeaseTTL(c.in); got != c.want {
			t.Errorf("clampLeaseTTL(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSessionLeaseTTLFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultSessionLease},
		{"not-a-number", defaultSessionLease},
		{"5000", minSessionLease},
		{"30000", 30 * time.Second},
		{"700000", maxSessionLease},
		{"-1", minSessionLease},
	}
	for _, c := range cases {
		t.Setenv("DAD_SESSION_LEASE_MS", c.env)
		if got := sessionLeaseTTL(); got != c.want {
			t.Errorf("DAD_SESSION_LEASE_MS=%q: got %v, want %v", c.env, got, c.want)
		}
	}
}

func TestRequestedLeaseTTL(t *testing.T) {
	t.Setenv("DAD_SESSION_LEASE_MS", "45000")
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{0, 45 * time.Second},
		{1, minSessionLease},
		{20000, 20 * time.Second},
		{999999999, maxSessionLease},
	}
	for _, c := range cases {
		if got := requestedLeaseTTL(c.ms); got != c.want {
			t.Errorf("requestedLeaseTTL(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}

// A keepalive that arrives at or after the expiry instant must fail, even when
// no reaper tick or read has expired the session yet.
func TestKeepaliveCannotReviveExpiredLease(t *testing.T) {
	a, clk := newClockAgent(t)
	s, held := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot"}, 10*time.Second)
	if held != nil {
		t.Fatal("acquire failed")
	}
	clk.Advance(9 * time.Second)
	if _, err := a.keepaliveSession(s.ID); err != nil {
		t.Fatalf("keepalive inside the lease: %v", err)
	}
	clk.Advance(10 * time.Second) // exactly at the extended expiry
	if _, err := a.keepaliveSession(s.ID); !errors.Is(err, errNoSession) {
		t.Fatalf("keepalive at expiry = %v, want errNoSession", err)
	}
	if got := countEvents(a, "session.expired"); got != 1 {
		t.Fatalf("session.expired events = %d, want 1", got)
	}
}

func TestReleasePauseOfExpiredLeaseFail(t *testing.T) {
	a, clk := newClockAgent(t)
	s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot"}, 10*time.Second)
	clk.Advance(10 * time.Second)
	if _, err := a.pauseSession(s.ID, true); !errors.Is(err, errNoSession) {
		t.Fatalf("pause of expired session = %v, want errNoSession", err)
	}
	s2, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot2"}, 10*time.Second)
	clk.Advance(11 * time.Second)
	if a.releaseSession(s2.ID) {
		t.Fatal("release of an expired session reported released")
	}
	if got := countEvents(a, "session.released"); got != 0 {
		t.Fatalf("session.released events = %d, want 0", got)
	}
	if got := countEvents(a, "session.expired"); got != 2 {
		t.Fatalf("session.expired events = %d, want 2", got)
	}
}

func TestReaperExpiresWithoutReads(t *testing.T) {
	old := sessionReapEvery
	sessionReapEvery = 5 * time.Millisecond
	t.Cleanup(func() { sessionReapEvery = old })
	a, clk := newClockAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.reapSessions(ctx)
	a.acquireSession(SessionHolder{Kind: "agent", Name: "bot"}, 10*time.Second)
	clk.Advance(10 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.sessions.mu.Lock()
		cur := a.sessions.cur
		a.sessions.mu.Unlock()
		if cur == nil {
			if got := countEvents(a, "session.expired"); got != 1 {
				t.Fatalf("session.expired events = %d, want 1", got)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reaper never expired the session")
}

func TestConcurrentAcquireHasOneWinner(t *testing.T) {
	a := newSessionAgent(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot"}, time.Minute); s != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("acquire winners = %d, want 1", wins.Load())
	}
}

// TestSessionLeaseRace hammers every lease operation and the reaper at once
// with a short real TTL. Run with -race; it also checks that each session
// expires or is released at most once.
func TestSessionLeaseRace(t *testing.T) {
	old := sessionReapEvery
	sessionReapEvery = time.Millisecond
	t.Cleanup(func() { sessionReapEvery = old })
	a := newSessionAgent(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.reapSessions(ctx)

	var wg sync.WaitGroup
	stop := time.Now().Add(300 * time.Millisecond)
	worker := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				fn()
			}
		}()
	}
	// The minimum real TTL is 10 s, so drive expiry by acquiring with a store
	// entry whose ExpiresAt is pushed into the past by a writer.
	worker(func() { a.acquireSession(SessionHolder{Kind: "agent", Name: "w"}, time.Minute) })
	worker(func() {
		if s := a.liveSession(); s != nil {
			_, _ = a.keepaliveSession(s.ID)
		}
	})
	worker(func() {
		if s := a.liveSession(); s != nil {
			_, _ = a.pauseSession(s.ID, true)
			_, _ = a.pauseSession(s.ID, false)
		}
	})
	worker(func() {
		if s := a.liveSession(); s != nil {
			a.releaseSession(s.ID)
		}
	})
	worker(func() {
		a.sessions.mu.Lock()
		if a.sessions.cur != nil {
			a.sessions.cur.ExpiresAt = time.Now().Add(-time.Millisecond)
		}
		a.sessions.mu.Unlock()
	})
	worker(func() {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if s := a.liveSession(); s != nil {
			req.Header.Set(sessionHeader, s.ID)
		}
		a.sessionFromRequest(req)
	})
	wg.Wait()

	ended := map[string]int{}
	for _, ev := range a.eventsSince(0, maxActivityEvents) {
		if ev.Type == "session.expired" || ev.Type == "session.released" {
			id := strings.Fields(ev.Detail)[1]
			ended[id]++
		}
	}
	for id, n := range ended {
		if n != 1 {
			t.Fatalf("session %s ended %d times", id, n)
		}
	}
}

// The 409 must name the holder but never carry its session id, or any
// contender could act under the holder's lease.
func TestSessionConflictDoesNotLeakID(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	id := acquire(t, srv, "bot-a")
	code, body := postJSON(t, srv.URL+"/v1/session/acquire", `{"holder":{"kind":"agent","name":"bot-b"}}`, nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, id) {
		t.Fatalf("409 body leaks the session id: %s", body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if h, _ := out["holder"].(map[string]any); h["name"] != "bot-a" {
		t.Fatalf("holder = %v", out["holder"])
	}
	// The gate's 409 for a wrong session id must not leak it either.
	code, body = postJSON(t, srv.URL+"/v1/session/keepalive", `{"session_id":"ses_wrong"}`, nil)
	if code != http.StatusConflict || strings.Contains(body, id) {
		t.Fatalf("keepalive with wrong id: %d %s", code, body)
	}
}
