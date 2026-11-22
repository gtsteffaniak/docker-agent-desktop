package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newSessionAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
}

func sessionServer(t *testing.T, a *Agent) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	a.registerSessionRoutes(mux, a.auth)
	a.registerEventRoutes(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dad-Token", testToken)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func acquire(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	code, body := postJSON(t, srv.URL+"/v1/session/acquire",
		`{"holder":{"kind":"agent","name":"`+name+`","run_id":"run-1"}}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("acquire status = %d, body %s", code, body)
	}
	var s Session
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID == "" || s.Holder.Name != name {
		t.Fatalf("bad session: %+v", s)
	}
	return s.ID
}

func TestSessionAcquireConflictNamesHolder(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	acquire(t, srv, "bot-a")

	code, body := postJSON(t, srv.URL+"/v1/session/acquire",
		`{"holder":{"kind":"agent","name":"bot-b"}}`, nil)
	if code != http.StatusConflict {
		t.Fatalf("second acquire status = %d, want 409", code)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	holder, _ := out["holder"].(map[string]any)
	if holder["name"] != "bot-a" {
		t.Fatalf("holder = %v, want bot-a", out["holder"])
	}
}

func TestSessionKeepaliveRelease(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	id := acquire(t, srv, "bot-a")

	code, body := postJSON(t, srv.URL+"/v1/session/keepalive", `{"session_id":"`+id+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("keepalive status = %d, body %s", code, body)
	}
	var s Session
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID != id {
		t.Fatalf("keepalive returned wrong session: %+v", s)
	}

	code, _ = postJSON(t, srv.URL+"/v1/session/release", `{"session_id":"`+id+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("release status = %d", code)
	}
	if got := a.sessionState(); got != nil {
		t.Fatalf("session still held after release: %+v", got)
	}
	code, _ = postJSON(t, srv.URL+"/v1/session/keepalive", `{"session_id":"`+id+`"}`, nil)
	if code != http.StatusConflict {
		t.Fatalf("keepalive after release status = %d, want 409", code)
	}
}

func TestSessionPauseBlocksMutations(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	id := acquire(t, srv, "bot-a")

	code, _ := postJSON(t, srv.URL+"/v1/session/pause", `{"session_id":"`+id+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("pause status = %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/session", nil)
	req.Header.Set("X-Dad-Token", testToken)
	req.Header.Set(sessionHeader, id)
	if _, code, _ := a.sessionFromRequest(req); code == 0 {
		t.Fatal("paused session passed the gate")
	}
	code, _ = postJSON(t, srv.URL+"/v1/session/resume", `{"session_id":"`+id+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("resume status = %d", code)
	}
	if _, code, _ := a.sessionFromRequest(req); code != 0 {
		t.Fatalf("resumed session refused: %d", code)
	}
}

func TestRequireSessionGate(t *testing.T) {
	a := newSessionAgent(t)
	ok := a.requireSession(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// No session held at all.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	ok.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no session status = %d, want 409", rec.Code)
	}

	id := func() string {
		s, held := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot-a"}, time.Minute)
		if held != nil || s == nil {
			t.Fatal("acquire failed")
		}
		return s.ID
	}()

	// Wrong id names the holder.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(sessionHeader, "ses_wrong")
	ok.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "bot-a") {
		t.Fatalf("wrong id status = %d, body %s", rec.Code, rec.Body.String())
	}

	// Right id passes.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	req.Header.Set(sessionHeader, id)
	ok.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("right id status = %d, want 204", rec.Code)
	}
}

func TestSessionExpiryClosesWithoutKeepalive(t *testing.T) {
	a := newSessionAgent(t)
	s, held := a.acquireSession(SessionHolder{Kind: "agent", Name: "bot-a"}, 50*time.Millisecond)
	if held != nil || s == nil {
		t.Fatal("acquire failed")
	}
	time.Sleep(80 * time.Millisecond)
	if got := a.sessionState(); got != nil {
		t.Fatalf("expired session still live: %+v", got)
	}
	events := a.eventsSince(0, 10)
	found := false
	for _, ev := range events {
		if ev.Type == "session.expired" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session.expired event in %+v", events)
	}
}

func TestSessionEventsEndpoint(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	acquire(t, srv, "bot-a")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/session/events", nil)
	req.Header.Set("X-Dad-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Events []ActivityEvent `json:"events"`
		Latest uint64          `json:"latest_seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) == 0 || out.Events[0].Type != "session.acquired" {
		t.Fatalf("events = %+v", out.Events)
	}
	if out.Latest == 0 {
		t.Fatal("latest_seq is 0")
	}
}

func TestSessionAcquireValidation(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	code, _ := postJSON(t, srv.URL+"/v1/session/acquire", `{"holder":{"kind":"agent"}}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400", code)
	}
	code, _ = postJSON(t, srv.URL+"/v1/session/acquire", `{"holder":{"kind":"human","name":"x"}}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bad kind status = %d, want 400", code)
	}
}
