// Sessions are the dad side of the computer-use contract: at most one
// live session may drive the computer at a time. A caller acquires a session,
// keeps it alive while it is working, and releases it when done. Mutating
// /v1 endpoints require the X-Dad-Session header of a live, unpaused session;
// reads never do. A session that is not kept alive expires and is closed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// defaultSessionLease is how long a session lives without a keepalive. A
// client that keeps its session alive every 15 s can miss several check-ins
// before the session is treated as abandoned.
const defaultSessionLease = 90 * time.Second

const sessionHeader = "X-Dad-Session"

// sessionReapEvery is how often reapSessions looks for an expired lease. It is
// a variable so tests can run the reaper quickly.
var sessionReapEvery = 5 * time.Second

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

// sessionStore holds the single live session, if any. Every read and change
// of cur, including the expiry check, happens under mu, so a keepalive can
// never revive a session the reaper (or a lazy read) has already expired.
type sessionStore struct {
	mu  sync.Mutex
	cur *Session
	// now is the clock; nil means time.Now. Tests replace it to drive expiry
	// deterministically.
	now func() time.Time
}

// clock returns the store's current time.
func (s *sessionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// minSessionLease and maxSessionLease bound every lease TTL, whether it comes
// from DAD_SESSION_LEASE_MS or a per-acquire ttl_ms.
const (
	minSessionLease = 10 * time.Second
	maxSessionLease = 10 * time.Minute
)

// clampLeaseTTL clamps a lease TTL to minSessionLease..maxSessionLease.
func clampLeaseTTL(d time.Duration) time.Duration {
	return min(max(d, minSessionLease), maxSessionLease)
}

// sessionLeaseTTL reads DAD_SESSION_LEASE_MS, clamped to 10 s..10 min. An
// unset or unparsable value means the 90 s default.
func sessionLeaseTTL() time.Duration {
	n, err := strconv.Atoi(os.Getenv("DAD_SESSION_LEASE_MS"))
	if err != nil {
		return defaultSessionLease
	}
	return clampLeaseTTL(time.Duration(n) * time.Millisecond)
}

// requestedLeaseTTL is the TTL for an acquire: the environment default when
// ttlMs is 0, otherwise ttlMs clamped like the default.
func requestedLeaseTTL(ttlMs int) time.Duration {
	if ttlMs == 0 {
		return sessionLeaseTTL()
	}
	return clampLeaseTTL(time.Duration(ttlMs) * time.Millisecond)
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ses_" + hex.EncodeToString(b[:])
}

// expireLocked clears cur when its lease has run out and returns the expired
// session, or nil. The caller must hold a.sessions.mu and must record the
// expiry with a.noteExpired after unlocking.
func (a *Agent) expireLocked() *Session {
	cur := a.sessions.cur
	if cur == nil || a.sessions.clock().Before(cur.ExpiresAt) {
		return nil
	}
	a.sessions.cur = nil
	return cur
}

// noteExpired records a session.expired event for s when s is not nil.
func (a *Agent) noteExpired(s *Session) {
	if s != nil {
		a.recordEvent("session.expired", s.Holder.Name, "session "+s.ID+" expired without keepalive")
	}
}

// expireIfStale clears an expired session and records the expiry event.
// It returns the expired session, or nil when nothing expired.
func (a *Agent) expireIfStale() *Session {
	a.sessions.mu.Lock()
	expired := a.expireLocked()
	a.sessions.mu.Unlock()
	a.noteExpired(expired)
	return expired
}

// liveSession returns a copy of the live session, or nil when none is held.
// Expired sessions are closed first.
func (a *Agent) liveSession() *Session {
	a.sessions.mu.Lock()
	expired := a.expireLocked()
	var cp *Session
	if a.sessions.cur != nil {
		c := *a.sessions.cur
		cp = &c
	}
	a.sessions.mu.Unlock()
	a.noteExpired(expired)
	return cp
}

// acquireSession creates a session when none is live. On conflict it returns
// the current holder as the second value so the caller can report who holds it.
func (a *Agent) acquireSession(holder SessionHolder, ttl time.Duration) (*Session, *Session) {
	a.sessions.mu.Lock()
	expired := a.expireLocked()
	if a.sessions.cur != nil {
		cp := *a.sessions.cur
		a.sessions.mu.Unlock()
		a.noteExpired(expired)
		return nil, &cp
	}
	now := a.sessions.clock()
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
	a.noteExpired(expired)
	a.recordEvent("session.acquired", holder.Name, "session "+s.ID+" acquired")
	return &cp, nil
}

// errNoSession reports that no live session exists (none was acquired, or the
// last one expired or was released).
var errNoSession = errors.New("no session held")

// ownedLocked returns the live session when it has this id. It expires a
// stale session first; the expired session, if any, is returned so the
// caller can record it after unlocking. The caller must hold a.sessions.mu.
func (a *Agent) ownedLocked(id string) (cur, expired *Session, err error) {
	expired = a.expireLocked()
	cur = a.sessions.cur
	if cur == nil {
		return nil, expired, errNoSession
	}
	if cur.ID != id {
		return nil, expired, &sessionHeldError{holder: cur.Holder}
	}
	return cur, expired, nil
}

// keepaliveSession extends a live session by its TTL. It errors when the id
// is unknown, held by someone else, or already expired. The expiry check and
// the extension happen under one lock, so an expired lease is never revived.
func (a *Agent) keepaliveSession(id string) (*Session, error) {
	a.sessions.mu.Lock()
	cur, expired, err := a.ownedLocked(id)
	if err != nil {
		a.sessions.mu.Unlock()
		a.noteExpired(expired)
		return nil, err
	}
	cur.ExpiresAt = a.sessions.clock().Add(cur.ttl)
	cp := *cur
	a.sessions.mu.Unlock()
	return &cp, nil
}

// sessionHeldError reports that another holder owns the live session.
type sessionHeldError struct {
	holder SessionHolder
}

// Error names the holder, matching the /v1 409 message.
func (e *sessionHeldError) Error() string {
	return "session held by " + e.holder.Name
}

// releaseSession ends the session with this id. It reports whether one was
// released; a session whose lease already ran out is expired, not released.
func (a *Agent) releaseSession(id string) bool {
	a.sessions.mu.Lock()
	cur, expired, err := a.ownedLocked(id)
	if err != nil {
		a.sessions.mu.Unlock()
		a.noteExpired(expired)
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
	a.sessions.mu.Lock()
	cur, expired, err := a.ownedLocked(id)
	if err != nil {
		a.sessions.mu.Unlock()
		a.noteExpired(expired)
		return nil, err
	}
	cur.Paused = paused
	cp := *cur
	a.sessions.mu.Unlock()
	if paused {
		a.recordEvent("session.paused", cp.Holder.Name, "session "+cp.ID+" paused")
	} else {
		a.recordEvent("session.resumed", cp.Holder.Name, "session "+cp.ID+" resumed")
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
	if id == "" {
		id = r.Header.Get(legacySessionHeader)
	}
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
// so a waiting caller knows who to wait for (or interrupt). It never includes the
// holder's session id: the id is what authorizes mutating calls, so handing it to
// a contender would let any caller act under someone else's lease.
func writeSessionError(w http.ResponseWriter, code int, msg string, holder *Session) {
	out := map[string]any{"error": msg}
	if holder != nil {
		out["holder"] = holder.Holder
		out["expires_at"] = holder.ExpiresAt
		out["paused"] = holder.Paused
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
	t := time.NewTicker(sessionReapEvery)
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
	s, held := a.acquireSession(body.Holder, requestedLeaseTTL(body.TTLMs))
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
	// A tracked app is running until its reaper removes it from the map.
	for id := range appProcs.apps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
