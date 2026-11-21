// The activity log is the dc-agent side of the computer's session history:
// an in-memory ring of what happened (sessions, commands, app and browser
// actions, health transitions). It is intentionally in-memory — the agent is
// stateless across restarts — and the client persists what it needs
// by polling GET /v1/session/events. Nothing here is a stub: every event type
// listed is recorded by a real call site.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxActivityEvents bounds the ring buffer. Oldest events drop first.
const maxActivityEvents = 500

// ActivityEvent is one recorded happening on the computer.
type ActivityEvent struct {
	Seq    uint64    `json:"seq"`
	Ts     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Actor  string    `json:"actor"`
	Detail string    `json:"detail"`
}

// eventLog is a mutex-guarded ring buffer with a monotonic sequence number.
type eventLog struct {
	mu     sync.Mutex
	buf    []ActivityEvent
	next   uint64
	latest uint64
}

// recordEvent appends an event. The detail should name the action and its
// outcome (e.g. "shell exec exit 0: ls /home/bot"), never secrets.
func (a *Agent) recordEvent(typ, actor, detail string) {
	a.events.mu.Lock()
	defer a.events.mu.Unlock()
	a.events.next++
	ev := ActivityEvent{Seq: a.events.next, Ts: time.Now().UTC(), Type: typ, Actor: actor, Detail: detail}
	a.events.buf = append(a.events.buf, ev)
	if len(a.events.buf) > maxActivityEvents {
		a.events.buf = a.events.buf[len(a.events.buf)-maxActivityEvents:]
	}
	a.events.latest = ev.Seq
}

// eventsSince returns up to limit events with Seq greater than since, in order.
func (a *Agent) eventsSince(since uint64, limit int) []ActivityEvent {
	a.events.mu.Lock()
	defer a.events.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	if limit > maxActivityEvents {
		limit = maxActivityEvents
	}
	out := []ActivityEvent{}
	for _, ev := range a.events.buf {
		if ev.Seq > since {
			out = append(out, ev)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (a *Agent) registerEventRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /v1/session/events", auth(a.handleSessionEvents))
}

func (a *Agent) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events := a.eventsSince(since, limit)
	a.events.mu.Lock()
	latest := a.events.latest
	a.events.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "latest_seq": latest})
}
