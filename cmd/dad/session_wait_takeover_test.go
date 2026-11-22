package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionWaitFreeImmediately(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)

	code, body := postJSON(t, srv.URL+"/v1/session/wait", `{"timeout_ms":1000}`, nil)
	if code != 200 {
		t.Fatalf("wait status = %d, body %s", code, body)
	}
	var out sessionWaitResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Free || out.Session != nil {
		t.Fatalf("wait = %+v, want free with no session", out)
	}
}

func TestSessionWaitHeldThenFree(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	id := acquire(t, srv, "bot-a")

	// Held: the wait times out and reports the holder.
	code, body := postJSON(t, srv.URL+"/v1/session/wait", `{"timeout_ms":1000}`, nil)
	if code != 200 {
		t.Fatalf("wait status = %d, body %s", code, body)
	}
	var out sessionWaitResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Free || out.Session == nil || out.Session.Holder.Name != "bot-a" {
		t.Fatalf("wait while held = %+v, want free=false held by bot-a", out)
	}
	if out.WaitedMs < 900 {
		t.Fatalf("waited_ms = %d, want the full ~1000ms timeout", out.WaitedMs)
	}

	// Released in the background: the wait returns early with free=true.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		postJSON(t, srv.URL+"/v1/session/release", `{"session_id":"`+id+`"}`, nil)
	}()
	code, body = postJSON(t, srv.URL+"/v1/session/wait", `{"timeout_ms":10000}`, nil)
	if code != 200 {
		t.Fatalf("wait status = %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Free || out.Session != nil {
		t.Fatalf("wait after release = %+v, want free with no session", out)
	}
	if out.WaitedMs > 5000 {
		t.Fatalf("waited_ms = %d, want an early return, not the full timeout", out.WaitedMs)
	}
	<-done
}

func TestSessionTakeover(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)

	// No force flag: refused.
	code, body := postJSON(t, srv.URL+"/v1/session/takeover",
		`{"holder":{"kind":"agent","name":"bot-b"},"force":false}`, nil)
	if code != 400 || !strings.Contains(body, "force=true") {
		t.Fatalf("takeover without force: status = %d, body %s", code, body)
	}

	// Nothing held: takeover acts like an acquire, evicted is null.
	code, body = postJSON(t, srv.URL+"/v1/session/takeover",
		`{"holder":{"kind":"agent","name":"bot-b"},"force":true}`, nil)
	if code != 201 {
		t.Fatalf("takeover status = %d, body %s", code, body)
	}
	var out sessionTakeoverResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Session == nil || out.Session.Holder.Name != "bot-b" || out.Evicted != nil {
		t.Fatalf("takeover with nothing held = %+v, want bot-b session, evicted null", out)
	}

	// Held by bot-b: takeover by bot-c evicts bot-b and names it.
	code, body = postJSON(t, srv.URL+"/v1/session/takeover",
		`{"holder":{"kind":"agent","name":"bot-c"},"force":true}`, nil)
	if code != 201 {
		t.Fatalf("takeover status = %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Session == nil || out.Session.Holder.Name != "bot-c" {
		t.Fatalf("takeover session = %+v, want bot-c", out.Session)
	}
	if out.Evicted == nil || out.Evicted.Holder.Name != "bot-b" {
		t.Fatalf("evicted = %+v, want the bot-b session", out.Evicted)
	}

	// The eviction is in the activity log.
	events := a.eventsSince(0, 100)
	found := false
	for _, ev := range events {
		if ev.Type == "session.takeover" && strings.Contains(ev.Detail, "bot-b") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session.takeover event naming bot-b in %v", events)
	}

	// The old session id no longer works.
	code, _ = postJSON(t, srv.URL+"/v1/session/keepalive", `{"session_id":"`+out.Evicted.ID+`"}`, nil)
	if code != 409 {
		t.Fatalf("keepalive with evicted id: status = %d, want 409", code)
	}
}

func TestMCPSessionWaitTakeover(t *testing.T) {
	a, srv := newMCPAgent(t)
	mcpInitialize(t, srv)

	// Wait with nothing held: free immediately.
	var waitOut sessionWaitResponse
	mcpCallText(t, srv, 1, "session_wait", map[string]any{"timeout_ms": 1000}, &waitOut)
	if !waitOut.Free || waitOut.Session != nil {
		t.Fatalf("session_wait with nothing held: %+v", waitOut)
	}

	// Takeover without force: refused.
	errMsg := mcpCallErr(t, srv, 2, "session_takeover", map[string]any{
		"kind": "agent", "name": "mcp-b", "force": false,
	})
	if !strings.Contains(errMsg, "force=true") {
		t.Fatalf("session_takeover without force: %q, want force=true", errMsg)
	}

	// Acquire as mcp-a, then takeover as mcp-b: evicted names mcp-a.
	s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "mcp-a"}, time.Minute)
	_ = s
	var takeOut sessionTakeoverResponse
	mcpCallText(t, srv, 3, "session_takeover", map[string]any{
		"kind": "agent", "name": "mcp-b", "force": true,
	}, &takeOut)
	if takeOut.Session == nil || takeOut.Session.Holder.Name != "mcp-b" {
		t.Fatalf("takeover session = %+v, want mcp-b", takeOut.Session)
	}
	if takeOut.Evicted == nil || takeOut.Evicted.Holder.Name != "mcp-a" {
		t.Fatalf("takeover evicted = %+v, want holder mcp-a", takeOut.Evicted)
	}
}
