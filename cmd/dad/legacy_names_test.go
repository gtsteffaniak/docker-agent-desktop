package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The pre-rename names (COMPUTER_AGENT_TOKEN, X-Computer-Token, X-Dc-Session)
// were removed outright in the initial release: there are no deprecated
// aliases, and the old names must not authenticate anything. This file stays
// in place as the removal regression test — the snapshot push cannot carry
// file deletions, so deleting it is a follow-up for the merger, not this PR.

func TestTokenFromEnvIgnoresLegacy(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := tokenFromEnv(env(map[string]string{"COMPUTER_AGENT_TOKEN": "o"})); got != "" {
		t.Errorf("legacy env var: got %q, want empty", got)
	}
	if got := tokenFromEnv(env(map[string]string{"DAD_TOKEN": "n", "COMPUTER_AGENT_TOKEN": "o"})); got != "n" {
		t.Errorf("new env var: got %q, want %q", got, "n")
	}
}

func TestTokenFromIgnoresLegacyHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Computer-Token", "old")
	if got := tokenFrom(r); got != "" {
		t.Fatalf("legacy header: got %q, want empty", got)
	}
	r.Header.Set(tokenHeader, "new")
	if got := tokenFrom(r); got != "new" {
		t.Fatalf("new header: got %q, want %q", got, "new")
	}
}

func TestLegacySessionHeaderIgnored(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	sid := acquire(t, srv, "holder")
	// The old header name must not authorize a mutating call: only
	// X-Dad-Session is read.
	r := httptest.NewRequest(http.MethodPost, "/v1/files/mkdir", nil)
	r.Header.Set("X-Dc-Session", sid)
	if _, code, _ := a.sessionFromRequest(r); code == 0 {
		t.Fatal("legacy session header was accepted as a session")
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/files/mkdir", nil)
	r2.Header.Set("X-Dad-Session", sid)
	if _, code, _ := a.sessionFromRequest(r2); code != 0 {
		t.Fatalf("new session header rejected: code %d", code)
	}
}
