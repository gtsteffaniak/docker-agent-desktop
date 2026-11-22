package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtsteffaniak/docker-agent-desktop/internal/broadway"
)

// newFullAgent mounts every /v1 route and /mcp on one mux, with a Broadway
// client that is never connected and no Chromium, so the error paths of every
// handler can be reached without real backends.
func newFullAgent(t *testing.T) (*Agent, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAD_BROWSER_START", "off")
	t.Setenv("DAD_APPS_DIRS", t.TempDir())
	log := slog.New(slog.DiscardHandler)
	a := &Agent{
		token:     testToken,
		log:       log,
		startedAt: time.Now(),
		broadway:  broadway.NewClient(broadway.Config{HostPort: "127.0.0.1:1", Width: 64, Height: 64}, log),
	}
	mux := http.NewServeMux()
	a.registerV1(mux, a.auth)
	a.registerMCP(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// mutatingRoutes is every session-gated /v1 route.
var mutatingRoutes = []struct{ method, path, body string }{
	{"POST", "/v1/shell/exec", `{"command":"true"}`},
	{"POST", "/v1/shell/jobs/job-x/kill", ``},
	{"POST", "/v1/files/write", `{"path":"a","content":"x"}`},
	{"POST", "/v1/files/upload", ``},
	{"POST", "/v1/files/mkdir", `{"path":"d"}`},
	{"POST", "/v1/files/move", `{"from":"a","to":"b"}`},
	{"DELETE", "/v1/files?path=a", ``},
	{"POST", "/v1/browser/start", `{}`},
	{"POST", "/v1/browser/stop", `{}`},
	{"POST", "/v1/browser/evaluate", `{"expression":"1"}`},
	{"POST", "/v1/browser/act", `{"kind":"click","x":1,"y":1}`},
	{"POST", "/v1/browser/navigate", `{"url":"http://example.com"}`},
	{"POST", "/v1/browser/back", ``},
	{"POST", "/v1/browser/forward", ``},
	{"POST", "/v1/browser/reload", ``},
	{"POST", "/v1/browser/input", `{"type":"key","key":"a"}`},
	{"POST", "/v1/browser/size", `{"w":800,"h":600}`},
	{"POST", "/v1/display/click", `{"x":1,"y":1}`},
	{"POST", "/v1/display/type", `{"text":"x"}`},
	{"POST", "/v1/display/key", `{"key":"Return"}`},
	{"POST", "/v1/display/scroll", `{"x":1,"y":1,"dy":1}`},
	{"POST", "/v1/display/drag", `{"x1":1,"y1":1,"x2":2,"y2":2}`},
	{"POST", "/v1/apps/start?id=terminal", ``},
	{"POST", "/v1/apps/stop?id=terminal", ``},
}

// readRoutes is every authenticated /v1 read.
var readRoutes = []string{
	"/v1/health", "/v1/session", "/v1/session/events", "/v1/shell/jobs", "/v1/shell/jobs/job-x",
	"/v1/files/list", "/v1/files/read?path=a", "/v1/browser/text", "/v1/browser/state",
	"/v1/display/screenshot", "/v1/display/meta", "/v1/apps",
}

func TestEveryRouteNeedsToken(t *testing.T) {
	_, srv := newFullAgent(t)
	bad := []map[string]string{
		nil,
		{"X-Dad-Token": ""},
		{"X-Dad-Token": "wrong"},
		{"X-Dad-Token": testToken[:len(testToken)-1]},
		{"X-Dad-Token": testToken + "x"},
		{"X-Dad-Token": strings.ToUpper(testToken)},
	}
	for _, h := range bad {
		for _, r := range mutatingRoutes {
			if code, _ := do(t, r.method, srv.URL+r.path, r.body, h); code != http.StatusUnauthorized {
				t.Errorf("%s %s with %v = %d, want 401", r.method, r.path, h, code)
			}
		}
		for _, p := range readRoutes {
			if code, _ := do(t, "GET", srv.URL+p, "", h); code != http.StatusUnauthorized {
				t.Errorf("GET %s with %v = %d, want 401", p, h, code)
			}
		}
		code, _ := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, h)
		if code != http.StatusUnauthorized {
			t.Errorf("POST /mcp with %v = %d, want 401", h, code)
		}
	}
}

func TestMutatingRoutesGateOnSession(t *testing.T) {
	a, srv := newFullAgent(t)
	tok := map[string]string{"X-Dad-Token": testToken}
	// No session at all.
	for _, r := range mutatingRoutes {
		code, body := do(t, r.method, srv.URL+r.path, r.body, tok)
		if code != http.StatusConflict || !strings.Contains(body, "no session held") {
			t.Errorf("%s %s without a session = %d %q, want 409 no session held", r.method, r.path, code, body)
		}
	}
	s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "holder-x"}, time.Minute)
	// Wrong or missing session id: 409 naming the holder.
	for _, sid := range []string{"", "ses_wrong"} {
		h := map[string]string{"X-Dad-Token": testToken, sessionHeader: sid}
		for _, r := range mutatingRoutes {
			code, body := do(t, r.method, srv.URL+r.path, r.body, h)
			if code != http.StatusConflict || !strings.Contains(body, "holder-x") || strings.Contains(body, s.ID) {
				t.Errorf("%s %s with session %q = %d %q, want 409 naming holder-x", r.method, r.path, sid, code, body)
			}
		}
	}
	// Paused: the holder's own id is refused too.
	if _, err := a.pauseSession(s.ID, true); err != nil {
		t.Fatal(err)
	}
	h := map[string]string{"X-Dad-Token": testToken, sessionHeader: s.ID}
	for _, r := range mutatingRoutes {
		code, body := do(t, r.method, srv.URL+r.path, r.body, h)
		if code != http.StatusConflict || !strings.Contains(body, "session paused") {
			t.Errorf("%s %s while paused = %d %q, want 409 session paused", r.method, r.path, code, body)
		}
	}
}

func TestHandlerErrorCodes(t *testing.T) {
	a, srv := newFullAgent(t)
	s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "h"}, time.Minute)
	tok := map[string]string{"X-Dad-Token": testToken}
	ses := map[string]string{"X-Dad-Token": testToken, sessionHeader: s.ID}
	if err := os.WriteFile(filepath.Join(a.homeDir(), "big.bin"), make([]byte, maxFileRead+1), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name               string
		method, path, body string
		hdr                map[string]string
		want               int
	}{
		{"acquire bad json", "POST", "/v1/session/acquire", `{`, tok, 400},
		{"acquire no name", "POST", "/v1/session/acquire", `{"holder":{"kind":"agent"}}`, tok, 400},
		{"acquire bad kind", "POST", "/v1/session/acquire", `{"holder":{"kind":"robot","name":"x"}}`, tok, 400},
		{"acquire held", "POST", "/v1/session/acquire", `{"holder":{"kind":"agent","name":"x"}}`, tok, 409},
		{"keepalive bad json", "POST", "/v1/session/keepalive", `nope`, tok, 400},
		{"keepalive other", "POST", "/v1/session/keepalive", `{"session_id":"ses_other"}`, tok, 409},
		{"release unknown", "POST", "/v1/session/release", `{"session_id":"ses_other"}`, tok, 404},
		{"pause other", "POST", "/v1/session/pause", `{"session_id":"ses_other"}`, tok, 409},
		{"exec neither", "POST", "/v1/shell/exec", `{}`, ses, 400},
		{"exec both", "POST", "/v1/shell/exec", `{"command":"x","argv":["x"]}`, ses, 400},
		{"exec cwd escape", "POST", "/v1/shell/exec", `{"command":"true","cwd":"/etc"}`, ses, 400},
		{"exec missing binary", "POST", "/v1/shell/exec", `{"argv":["/no/such/binary"]}`, ses, 400},
		{"exec bad json", "POST", "/v1/shell/exec", `[`, ses, 400},
		{"job unknown", "GET", "/v1/shell/jobs/job-nope", ``, tok, 404},
		{"kill unknown", "POST", "/v1/shell/jobs/job-nope/kill", ``, ses, 404},
		{"read missing", "GET", "/v1/files/read?path=nope.txt", ``, tok, 404},
		{"read escape", "GET", "/v1/files/read?path=../../etc/passwd", ``, tok, 400},
		{"read too big", "GET", "/v1/files/read?path=big.bin", ``, tok, 413},
		{"list escape", "GET", "/v1/files/list?path=/etc", ``, tok, 400},
		{"list missing", "GET", "/v1/files/list?path=nope", ``, tok, 404},
		{"write no path", "POST", "/v1/files/write", `{"content":"x"}`, ses, 400},
		{"write bad encoding", "POST", "/v1/files/write", `{"path":"a","content":"x","encoding":"rot13"}`, ses, 400},
		{"write bad base64", "POST", "/v1/files/write", `{"path":"a","content":"!!","encoding":"base64"}`, ses, 400},
		{"write escape", "POST", "/v1/files/write", `{"path":"/tmp/x","content":"x"}`, ses, 400},
		{"mkdir no path", "POST", "/v1/files/mkdir", `{}`, ses, 400},
		{"move missing fields", "POST", "/v1/files/move", `{"from":"a"}`, ses, 400},
		{"move missing source", "POST", "/v1/files/move", `{"from":"nope","to":"b"}`, ses, 404},
		{"delete no path", "DELETE", "/v1/files", ``, ses, 400},
		{"delete home", "DELETE", "/v1/files?path=.&recursive=1", ``, ses, 400},
		{"delete escape", "DELETE", "/v1/files?path=../x", ``, ses, 400},
		{"upload not multipart", "POST", "/v1/files/upload", `{}`, ses, 400},
		{"browser start disabled", "POST", "/v1/browser/start", `{}`, ses, 503},
		{"browser evaluate down", "POST", "/v1/browser/evaluate", `{"expression":"1"}`, ses, 503},
		{"browser text down", "GET", "/v1/browser/text", ``, tok, 503},
		{"display click bad button", "POST", "/v1/display/click", `{"x":1,"y":1,"button":9}`, ses, 400},
		{"display key unknown", "POST", "/v1/display/key", `{"key":"NoSuchKey"}`, ses, 400},
		{"display click disconnected", "POST", "/v1/display/click", `{"x":1,"y":1}`, ses, 503},
		{"display type disconnected", "POST", "/v1/display/type", `{"text":"x"}`, ses, 503},
		{"app start unknown", "POST", "/v1/apps/start?id=nope", ``, ses, 400},
		{"app stop unknown", "POST", "/v1/apps/stop?id=nope", ``, ses, 400},
	}
	for _, c := range cases {
		if code, body := do(t, c.method, srv.URL+c.path, c.body, c.hdr); code != c.want {
			t.Errorf("%s: %s %s = %d (%s), want %d", c.name, c.method, c.path, code, strings.TrimSpace(body), c.want)
		}
	}
}

// Concurrent health polls must not race on the degraded flag, and the
// transition into degraded is recorded exactly once.
func TestHealthTransitionRecordedOnce(t *testing.T) {
	a, _ := newFullAgent(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.checkHealth(t.Context())
		}()
	}
	wg.Wait()
	if got := countEvents(a, "health.degraded"); got != 1 {
		t.Fatalf("health.degraded events = %d, want 1", got)
	}
}

// A lazily started browser that is not running is "stopped", not a failure;
// only a supervised browser without CDP is down.
func TestHealthBrowserStoppedIsNotDegraded(t *testing.T) {
	a, _ := newFullAgent(t)
	t.Setenv("DAD_BROWSER_START", "lazy")
	h := a.sessionHealth(t.Context())
	checks := h.Checks
	if checks.Chromium != "stopped" {
		t.Fatalf("chromium = %v, want stopped", checks.Chromium)
	}
	a.browser = &browserRun{stop: func() {}}
	h = a.sessionHealth(t.Context())
	checks = h.Checks
	if checks.Chromium != "down" || h.Status != "degraded" {
		t.Fatalf("supervised without CDP: chromium = %v, status = %v; want down, degraded", checks.Chromium, h.Status)
	}
}
