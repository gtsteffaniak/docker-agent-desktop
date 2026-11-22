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

// sessionError is the 409 body when a session-gated call finds no live
// session or one held by someone else. The session id is deliberately never
// included: the id is what authorizes mutating calls, so handing it to a
// contender would let any caller act under someone else's lease.
type sessionError struct {
	Error     string         `json:"error"`
	Holder    *SessionHolder `json:"holder,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	Paused    bool           `json:"paused,omitempty"`
}

// sessionAcquireRequest is the body of POST /v1/session/acquire.
type sessionAcquireRequest struct {
	Holder SessionHolder `json:"holder"`
	TTLMs  int           `json:"ttl_ms,omitempty"`
}

// sessionIDRequest is the body of the session calls that name a session:
// keepalive, release, pause, resume.
type sessionIDRequest struct {
	SessionID string `json:"session_id"`
}

// sessionGetResponse is the reply to GET /v1/session.
type sessionGetResponse struct {
	Session        *Session `json:"session"`
	BrowserRunning bool     `json:"browser_running"`
	Viewers        int      `json:"viewers"`
	AppsRunning    []string `json:"apps_running"`
	UptimeS        int64    `json:"uptime_s"`
}

// sessionReleaseResponse is the reply to POST /v1/session/release.
type sessionReleaseResponse struct {
	Released bool `json:"released"`
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

// takeoverSession evicts the live session, if any, and installs a new one
// for holder. It returns the evicted session, or nil when none was held.
// The caller records the takeover with noteTakeover after unlocking.
func (a *Agent) takeoverSession(holder SessionHolder, ttl time.Duration) (evicted, s *Session) {
	a.sessions.mu.Lock()
	var evictedCp *Session
	if a.sessions.cur != nil {
		c := *a.sessions.cur
		evictedCp = &c
	}
	now := a.sessions.clock()
	s = &Session{
		ID:         newSessionID(),
		Holder:     holder,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
		ttl:        ttl,
	}
	a.sessions.cur = s
	sCp := *s
	a.sessions.mu.Unlock()
	if evictedCp != nil {
		a.recordEvent("session.takeover", holder.Name,
			"session "+sCp.ID+" took over from "+evictedCp.Holder.Name+" (session "+evictedCp.ID+")")
	} else {
		a.recordEvent("session.acquired", holder.Name, "session "+sCp.ID+" acquired")
	}
	return evictedCp, &sCp
}

// waitFree polls until no session is live or timeout elapses. It returns
// true when the lease became free. The caller's context ends the wait early.
func (a *Agent) waitFree(ctx context.Context, timeout time.Duration) bool {
	if a.sessionState() == nil {
		return true
	}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return a.sessionState() == nil
		case <-t.C:
			if a.sessionState() == nil {
				return true
			}
		}
	}
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

// sessionByID resolves a session id the way sessionFromRequest does for a
// header: the live session when the id matches it, or an error naming why
// the caller may not act (no session held, held by someone else, or paused).
// The browser stream uses it for the session bound to its socket.
func (a *Agent) sessionByID(id string) (*Session, error) {
	s := a.sessionState()
	if s == nil {
		return nil, errNoSession
	}
	if id == "" || id != s.ID {
		return nil, &sessionHeldError{holder: s.Holder}
	}
	if s.Paused {
		return nil, errors.New("session paused")
	}
	return s, nil
}

// sessionFromRequest resolves the session named by the request header. It
// returns the session, or nil plus an HTTP status and message when the caller
// may not act (no session held, held by someone else, or paused).
func (a *Agent) sessionFromRequest(r *http.Request) (*Session, int, string) {
	id := r.Header.Get(sessionHeader)
	if _, err := a.sessionByID(id); err != nil {
		var held *sessionHeldError
		switch {
		case errors.Is(err, errNoSession):
			return nil, http.StatusConflict, "no session held"
		case errors.As(err, &held):
			return nil, http.StatusConflict, "session held by " + held.holder.Name
		default:
			return nil, http.StatusConflict, err.Error()
		}
	}
	return a.sessionState(), 0, ""
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
	a.handle(mux, "POST /v1/session/acquire", auth(a.handleSessionAcquire))
	a.handle(mux, "POST /v1/session/keepalive", auth(a.handleSessionKeepalive))
	a.handle(mux, "POST /v1/session/release", auth(a.handleSessionRelease))
	a.handle(mux, "POST /v1/session/pause", auth(a.handleSessionPause))
	a.handle(mux, "POST /v1/session/resume", auth(a.handleSessionResume))
	a.handle(mux, "GET /v1/session", auth(a.handleSessionGet))
	a.handle(mux, "POST /v1/session/wait", auth(a.handleSessionWait))
	a.handle(mux, "POST /v1/session/takeover", auth(a.handleSessionTakeover))
}

// handleSessionAcquire acquires the single session lease. Exactly one holder at a time:
// a second acquire is a 409 naming the current holder.
// @Summary Acquire the session
// @Description Acquires the single session lease. Exactly one holder at a time: a second acquire is a 409 naming the current holder. The lease TTL defaults to 90 s (DAD_SESSION_LEASE_MS, clamped to 10 s..10 min); keep it alive with POST /v1/session/keepalive.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionAcquireRequest true "Holder and optional lease TTL"
// @Success 201 {object} Session "Lease acquired"
// @Failure 400 {string} string "holder.name missing or holder.kind not agent/user"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "Session already held"
// @Security ApiKeyAuth
// @Router /v1/session/acquire [post]
func (a *Agent) handleSessionAcquire(w http.ResponseWriter, r *http.Request) {
	var body sessionAcquireRequest
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

// handleSessionKeepalive extends the lease on a live session. A keepalive for an expired,
// released or taken-over session is a 409.
// @Summary Keep the session alive
// @Description Extends the lease on a live session. A keepalive for an expired, released or taken-over session is a 409: re-acquire instead.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionIDRequest true "Session id"
// @Success 200 {object} Session "Lease extended"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No such session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/session/keepalive [post]
func (a *Agent) handleSessionKeepalive(w http.ResponseWriter, r *http.Request) {
	var body sessionIDRequest
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

// handleSessionRelease releases a live session. Releasing a session that is not held is a 404.
// @Summary Release the session
// @Description Releases a live session. Releasing a session that is not held is a 404.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionIDRequest true "Session id"
// @Success 200 {object} sessionReleaseResponse "Released"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 404 {string} string "No such session"
// @Security ApiKeyAuth
// @Router /v1/session/release [post]
func (a *Agent) handleSessionRelease(w http.ResponseWriter, r *http.Request) {
	var body sessionIDRequest
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	if !a.releaseSession(body.SessionID) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	writeJSON(w, sessionReleaseResponse{Released: true})
}

func (a *Agent) handleSessionPause(w http.ResponseWriter, r *http.Request) {
	a.handleSessionPauseResume(w, r, true)
}

func (a *Agent) handleSessionResume(w http.ResponseWriter, r *http.Request) {
	a.handleSessionPauseResume(w, r, false)
}

// handleSessionPauseResume pauses or resumes the live session. While paused, control
// messages are refused but the lease is still held. A pause/resume for a session that is
// not the live one is a 409.
// @Summary Pause or resume the session
// @Description Pauses or resumes the live session. While paused, control messages are refused but the lease is still held. A pause/resume for a session that is not the live one is a 409.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionIDRequest true "Session id"
// @Success 200 {object} Session "Session paused or resumed"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Failure 409 {object} sessionError "No such session, or held by someone else"
// @Security ApiKeyAuth
// @Router /v1/session/pause [post]
// @Router /v1/session/resume [post]
func (a *Agent) handleSessionPauseResume(w http.ResponseWriter, r *http.Request, paused bool) {
	var body sessionIDRequest
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
// @Summary Current session state
// @Description The live session (or null) plus browser state, viewer count, running apps and uptime: what a waiting caller needs to decide whether to wait.
// @Tags session
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Success 200 {object} sessionGetResponse "Current state"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/session [get]
func (a *Agent) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	s := a.sessionState()
	sc := a.screencast()
	viewers := 0
	if sc != nil {
		viewers = sc.Viewers()
	}
	writeJSON(w, sessionGetResponse{
		Session:        s,
		BrowserRunning: sc != nil,
		Viewers:        viewers,
		AppsRunning:    a.runningAppIDs(),
		UptimeS:        int64(time.Since(a.startedAt).Seconds()),
	})
}

// sessionWaitRequest is the body of POST /v1/session/wait.
type sessionWaitRequest struct {
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// sessionWaitResponse is the reply to POST /v1/session/wait: whether the
// lease is free now, plus the same state GET /v1/session reports. A caller
// that sees free=true still has to acquire: another contender can take the
// lease between the wait returning and the acquire landing.
type sessionWaitResponse struct {
	Free           bool     `json:"free"`
	Session        *Session `json:"session"`
	BrowserRunning bool     `json:"browser_running"`
	Viewers        int      `json:"viewers"`
	AppsRunning    []string `json:"apps_running"`
	UptimeS        int64    `json:"uptime_s"`
	WaitedMs       int64    `json:"waited_ms"`
}

// sessionTakeoverRequest is the body of POST /v1/session/takeover. Force
// must be true: evicting another holder is deliberate, never accidental.
type sessionTakeoverRequest struct {
	Holder SessionHolder `json:"holder"`
	TTLMs  int           `json:"ttl_ms"`
	Force  bool          `json:"force"`
}

// sessionTakeoverResponse is the reply to POST /v1/session/takeover: the new
// session and the session it evicted, or null when none was held.
type sessionTakeoverResponse struct {
	Session *Session `json:"session"`
	Evicted *Session `json:"evicted"`
}

// clampWaitTimeout bounds a wait: 1 s..5 min, default 30 s when 0.
func clampWaitTimeout(ms int) time.Duration {
	if ms == 0 {
		return 30 * time.Second
	}
	return min(max(time.Duration(ms)*time.Millisecond, time.Second), 5*time.Minute)
}

// handleSessionWait long-polls until the lease is free or the timeout
// elapses. It never acquires: it only reports. A read, so no session is
// needed to call it.
// @Summary Wait for the session to free up
// @Description Long-polls until the lease is free or the timeout elapses (default 30 s, clamped to 1 s..5 min). It never acquires: it only reports. A caller that sees free=true still has to acquire — another contender can take the lease between the wait returning and the acquire landing. A read, so no session is needed to call it.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionWaitRequest false "Optional wait timeout"
// @Success 200 {object} sessionWaitResponse "Wait result"
// @Failure 400 {string} string "Unparsable body"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/session/wait [post]
func (a *Agent) handleSessionWait(w http.ResponseWriter, r *http.Request) {
	var body sessionWaitRequest
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	start := time.Now()
	free := a.waitFree(r.Context(), clampWaitTimeout(body.TimeoutMs))
	s := a.sessionState()
	sc := a.screencast()
	viewers := 0
	if sc != nil {
		viewers = sc.Viewers()
	}
	writeJSON(w, sessionWaitResponse{
		Free:           free,
		Session:        s,
		BrowserRunning: sc != nil,
		Viewers:        viewers,
		AppsRunning:    a.runningAppIDs(),
		UptimeS:        int64(time.Since(a.startedAt).Seconds()),
		WaitedMs:       time.Since(start).Milliseconds(),
	})
}

// handleSessionTakeover force-acquires the lease, evicting whoever holds it.
// The evicted holder is named in the activity log and in the response, so a
// takeover is always visible after the fact.
// @Summary Take over the session
// @Description Force-acquires the lease, evicting whoever holds it. Requires explicit "force": true — evicting another holder is deliberate, never accidental. The evicted holder is named in the activity log and in the response, so a takeover is always visible after the fact.
// @Tags session
// @Accept json
// @Produce json
// @Param X-Dad-Token header string false "Agent token (or the dad_token cookie)"
// @Param body body sessionTakeoverRequest true "New holder, with force=true"
// @Success 201 {object} sessionTakeoverResponse "New session and the evicted one (or null)"
// @Failure 400 {string} string "force is not true, or holder.name missing / holder.kind not agent/user"
// @Failure 401 {string} string "Missing or wrong token"
// @Security ApiKeyAuth
// @Router /v1/session/takeover [post]
func (a *Agent) handleSessionTakeover(w http.ResponseWriter, r *http.Request) {
	var body sessionTakeoverRequest
	if !decodeJSON(w, r, 4<<10, &body) {
		return
	}
	if !body.Force {
		http.Error(w, "takeover requires force=true", http.StatusBadRequest)
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
	evicted, s := a.takeoverSession(body.Holder, requestedLeaseTTL(body.TTLMs))
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sessionTakeoverResponse{Session: s, Evicted: evicted})
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
