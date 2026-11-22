// The activity log is the dad side of the computer's session history:
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
	a.handle(mux, "GET /v1/session/events", auth(a.handleSessionEvents))
}

// sessionEventsResponse is the GET /v1/session/events body.
type sessionEventsResponse struct {
	Events    []ActivityEvent `json:"events"`
	LatestSeq uint64          `json:"latest_seq"`
}

// handleSessionEvents returns activity-log events after ?since=N, up to ?limit= (default 100,
// max 500).
// @Summary Activity log events
// @Description Activity-log events with sequence number greater than ?since=, up to ?limit= (default 100, max 500). Poll with the latest_seq returned to follow the log.
// @Tags events
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param since query integer false "Only events with seq greater than this"
// @Param limit query integer false "Maximum events to return (default 100, max 500)"
// @Success 200 {object} sessionEventsResponse "Events and the latest sequence number"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/session/events [get]
func (a *Agent) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events := a.eventsSince(since, limit)
	a.events.mu.Lock()
	latest := a.events.latest
	a.events.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessionEventsResponse{Events: events, LatestSeq: latest})
}
