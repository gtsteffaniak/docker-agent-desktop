// Sessions are the dc-agent side of the computer-use contract: at most one
// live session may drive the computer at a time. A caller acquires a session,
// keeps it alive while it is working, and releases it when done. Mutating
// /v1 endpoints require the X-Dc-Session header of a live, unpaused session;
// reads never do. A session that is not kept alive expires and is closed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// defaultSessionLease is how long a session lives without a keepalive. The
// quantum-bots side keeps a session alive every 15 s, so 90 s tolerates
// several missed check-ins before the session is treated as abandoned.
const defaultSessionLease = 90 * time.Second

const sessionHeader = "X-Dc-Session"

// SessionHolder names whoever holds a session: an agent (with the run id) or
// a user driving the computer from the desktop.
type SessionHolder struct {
	Kind  string `json:"kind"` // "agent" or "user"
	Name  string `json:"name"`
	RunID string `json:"run_id,omitempty"`
}

// Session is one live lease on the computer.
type Session struct {
	ID         string        `json:"id"`
	Holder     SessionHolder `json:"holder"`
	AcquiredAt time.Time     `json:"acquired_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
	Paused     bool          `json:"paused"`
	ttl        time.Duration
}

// sessionStore holds the single live session, if any.
type sessionStore struct {
	mu  sync.Mutex
	cur *Session
}

// sessionLeaseTTL reads DC_SESSION_LEASE_MS, clamped to 10 s..10 min.
func sessionLeaseTTL() time.Duration {
	n, err := strconv.Atoi(envOr("DC_SESSION_LEASE_MS", "90000"))
	if err != nil || n < 10000 {
		return 10 * time.Second
	}
	if n > 600000 {
		return 10 * time.Minute
	}
	return time.Duration(n) * time.Millisecond
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ses_" + hex.EncodeToString(b[:])
}

// expireIfStale clears an expired session and records the expiry event.
// It returns the expired session, or nil when nothing expired.
func (a *Agent) expireIfStale() *Session {
	a.sessions.mu.Lock()
	cur := a.sessions.cur
	if cur == nil || time.Now().Before(cur.ExpiresAt) {
		a.sessions.mu.Unlock()
		return nil
	}
	a.sessions.cur = nil
	a.sessions.mu.Unlock()
	a.recordEvent("session.expired", cur.Holder.Name, "session "+cur.ID+" expired without keepalive")
	return cur
}

// liveSession returns a copy of the live session, or nil when none is held.
// Expired sessions are closed first.
func (a *Agent) liveSession() *Session {
	a.expireIfStale()
	a.sessions.mu.Lock()
	defer a.sessions.mu.Unlock()
	if a.sessions.cur == nil {
		return nil
	}
	cp := *a.sessions.cur
	return &cp
}

// acquireSession creates a session when none is live. On conflict it returns
// the current holder as the second value so the caller can report who holds it.
func (a *Agent) acquireSession(holder SessionHolder, ttl time.Duration) (*Session, *Session) {
	a.expireIfStale()
	now := time.Now()
	a.sessions.mu.Lock()
	if a.sessions.cur != nil {
		cp := *a.sessions.cur
		a.sessions.mu.Unlock()
		return nil, &cp
	}
	s := &Session{
		ID:         newSessionID(),
		Holder:     holder,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
		ttl:        ttl,
	}
	a.sessions.cur = s
	cp := *s
	a.sessions.mu.Unlock()
	a.recordEvent("session.acquired", holder.Name, "session "+s.ID+" acquired")
	return &cp, nil
}

// keepaliveSession extends a live session by its TTL. It errors when the id
// is unknown, held by someone else, or already expired.
func (a *Agent) keepaliveSession(id string) (*Session, error) {
	a.expireIfStale()
	now := time.Now()
	a.sessions.mu.Lock()
	cur := a.sessions.cur
	if cur == nil {
		a.sessions.mu.Unlock()
		return nil, errors.New("no session held")
	}
	if cur.ID != id {
		holder := cur.Holder
		a.sessions.mu.Unlock()
		return nil, &sessionHeldError{holder: holder}
	}
	cur.ExpiresAt = now.Add(cur.ttl)
	cp := *cur
	a.sessions.mu.Unlock()
	return &cp, nil
}

// sessionHeldError reports that another holder owns the live session.
type sessionHeldError struct {
	holder SessionHolder
}

func (e *sessionHeldError) Error() string {
	return "session held by " + e.holder.Name
}

// releaseSession ends the session with this id. It reports whether one was released.
func (a *Agent) releaseSession(id string) bool {
	a.sessions.mu.Lock()
	cur := a.sessions.cur
	if cur == nil || cur.ID != id {
		a.sessions.mu.Unlock()
		return false
	}
	a.sessions.cur = nil
	a.sessions.mu.Unlock()
	a.recordEvent("session.released", cur.Holder.Name, "session "+cur.ID+" released")
	return true
}

// pauseSession marks a live session paused (or resumes it). While paused, the
// session still holds the lease but mutating calls with it are refused.
func (a *Agent) pauseSession(id string, paused bool) (*Session, error) {
	a.expireIfStale()
	a.sessions.mu.Lock()
	cur := a.sessions.cur
	if cur == nil {
		a.sessions.mu.Unlock()
		return nil, errors.New("no session held")
	}
	if cur.ID != id {
		holder := cur.Holder
		a.sessions.mu.Unlock()
		return nil, &sessionHeldError{holder: holder}
	}
	cur.Paused = paused
	cp := *cur
	a.sessions.mu.Unlock()
	if paused {
		a.recordEvent("session.paused", cur.Holder.Name, "session "+cur.ID+" paused")
	} else {
		a.recordEvent("session.resumed", cur.Holder.Name, "session "+cur.ID+" resumed")
	}
	return &cp, nil
}

// sessionState returns a copy of the live session, or nil when none is held.
func (a *Agent) sessionState() *Session {
	return a.liveSession()
}

// sessionFromRequest resolves the session named by the request header. It
// returns the session, or nil plus an HTTP status and message when the caller
// may not act (no session held, held by someone else, or paused).
func (a *Agent) sessionFromRequest(r *http.Request) (*Session, int, string) {
	id := r.Header.Get(sessionHeader)
	s := a.sessionState()
	if s == nil {
		return nil, http.StatusConflict, "no session held"
	}
	if id == "" || id != s.ID {
		return nil, http.StatusConflict, "session held by " + s.Holder.Name
	}
	if s.Paused {
		return nil, http.StatusConflict, "session paused"
	}
	return s, 0, ""
}

// requireSession is the gate for mutating /v1 endpoints: the caller must
// present the live session's id, and the session must not be paused.
func (a *Agent) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, code, msg := a.sessionFromRequest(r); code != 0 {
			writeSessionError(w, code, msg, a.sessionState())
			return
		}
		next(w, r)
	}
}

// writeSessionError renders a 409 that names the current holder when there is one,
// so a waiting caller knows who to wait for (or interrupt).
func writeSessionError(w http.ResponseWriter, code int, msg string, holder *Session) {
	out := map[string]any{"error": msg}
	if holder != nil {
		out["holder"] = holder.Holder
		out["session_id"] = holder.ID
		out["expires_at"] = holder.ExpiresAt
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

// sessionHolderName returns the name of the session holder named by the request,
// or "user" when the request carries no live session (human-driven calls).
func (a *Agent) sessionHolderName(r *http.Request) string {
	if s, _, _ := a.sessionFromRequest(r); s != nil {
		return s.Holder.Name
	}
	return "user"
}

// reapSessions expires stale sessions every few seconds until ctx ends.
// Expiry is also applied lazily on every session read, so a missed tick never
// leaves a stale session live.
func (a *Agent) reapSessions(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.expireIfStale()
		}
	}
}

func (a *Agent) registerSessionRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /v1/session/acquire", auth(a.handleSessionAcquire))
	mux.HandleFunc("POST /v1/session/keepalive", auth(a.handleSessionKeepalive))
	mux.HandleFunc("POST /v1/session/release", auth(a.handleSessionRelease))
	mux.HandleFunc("POST /v1/session/pause", auth(a.handleSessionPause))
	mux.HandleFunc("POST /v1/session/resume", auth(a.handleSessionResume))
	mux.HandleFunc("GET /v1/session", auth(a.handleSessionGet))
}

func (a *Agent) handleSessionAcquire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Holder SessionHolder `json:"holder"`
		TTLMs  int           `json:"ttl_ms"`
	}
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	if body.Holder.Name == "" {
		http.Error(w, "holder.name is required", http.StatusBadRequest)
		return
	}
	if body.Holder.Kind != "agent" && body.Holder.Kind != "user" {
		http.Error(w, "holder.kind must be agent or user", http.StatusBadRequest)
		return
	}
	ttl := sessionLeaseTTL()
	if body.TTLMs != 0 {
		ttl = time.Duration(body.TTLMs) * time.Millisecond
		if ttl < 10*time.Second {
			ttl = 10 * time.Second
		}
		if ttl > 10*time.Minute {
			ttl = 10 * time.Minute
		}
	}
	s, held := a.acquireSession(body.Holder, ttl)
	if held != nil {
		writeSessionError(w, http.StatusConflict, "session held by "+held.Holder.Name, held)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, s)
}

func (a *Agent) handleSessionKeepalive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	s, err := a.keepaliveSession(body.SessionID)
	if err != nil {
		var held *sessionHeldError
		if errors.As(err, &held) {
			writeSessionError(w, http.StatusConflict, err.Error(), a.sessionState())
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, s)
}

func (a *Agent) handleSessionRelease(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	if !a.releaseSession(body.SessionID) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"released": true})
}

func (a *Agent) handleSessionPause(w http.ResponseWriter, r *http.Request) {
	a.handleSessionPauseResume(w, r, true)
}

func (a *Agent) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	a.handleSessionPauseResume(w, r, false)
}

func (a *Agent) handleSessionPauseResume(w http.ResponseWriter, r *http.Request, paused bool) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	s, err := a.pauseSession(body.SessionID, paused)
	if err != nil {
		var held *sessionHeldError
		if errors.As(err, &held) {
			writeSessionError(w, http.StatusConflict, err.Error(), a.sessionState())
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, s)
}

// handleSessionGet reports the live session (or null) plus what a waiting
// caller needs to decide whether to wait: browser state, viewers, apps running.
func (a *Agent) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	s := a.sessionState()
	sc := a.screencast()
	viewers := 0
	if sc != nil {
		viewers = sc.Viewers()
	}
	writeJSON(w, map[string]any{
		"session":         s,
		"browser_running": sc != nil,
		"viewers":         viewers,
		"apps_running":    a.runningAppIDs(),
		"uptime_s":        int64(time.Since(a.startedAt).Seconds()),
	})
}

// runningAppIDs lists the ids of apps with a live process.
func (a *Agent) runningAppIDs() []string {
	appProcs.mu.Lock()
	defer appProcs.mu.Unlock()
	out := []string{}
	for id, cmd := range appProcs.apps {
		if cmd.ProcessState == nil {
			out = append(out, id)
		}
	}
	return out
}
