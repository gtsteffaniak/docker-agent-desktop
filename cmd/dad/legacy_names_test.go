package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The pre-rename names (COMPUTER_AGENT_TOKEN, X-Computer-Token, X-Dc-Session) stay accepted until
// existing clients move to the new ones; the new names win when both are present.
func TestTokenFromEnvLegacyFallback(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name       string
		vars       map[string]string
		want       string
		wantLegacy bool
	}{
		{"new only", map[string]string{"DAD_TOKEN": "n"}, "n", false},
		{"legacy only", map[string]string{"COMPUTER_AGENT_TOKEN": "o"}, "o", true},
		{"both prefers new", map[string]string{"DAD_TOKEN": "n", "COMPUTER_AGENT_TOKEN": "o"}, "n", false},
		{"neither", map[string]string{}, "", false},
	}
	for _, c := range cases {
		got, legacy := tokenFromEnv(env(c.vars))
		if got != c.want || legacy != c.wantLegacy {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, legacy, c.want, c.wantLegacy)
		}
	}
}

func TestTokenFromLegacyHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(legacyTokenHeader, "old")
	if got := tokenFrom(r); got != "old" {
		t.Fatalf("legacy header: got %q", got)
	}
	r.Header.Set(tokenHeader, "new")
	if got := tokenFrom(r); got != "new" {
		t.Fatalf("new header should win: got %q", got)
	}
}

func TestLegacySessionHeaderAccepted(t *testing.T) {
	a := newSessionAgent(t)
	srv := sessionServer(t, a)
	code, body := postJSON(t, srv.URL+"/v1/session/acquire", `{"holder":{"kind":"agent","name":"legacy"}}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("acquire = %d %s", code, body)
	}
	s := a.sessionState()
	for _, h := range []string{sessionHeader, legacySessionHeader} {
		r := httptest.NewRequest(http.MethodPost, "/v1/files/mkdir", nil)
		r.Header.Set(h, s.ID)
		if got, status, msg := a.sessionFromRequest(r); got == nil {
			t.Errorf("%s: rejected with %d %s", h, status, msg)
		}
	}
}
