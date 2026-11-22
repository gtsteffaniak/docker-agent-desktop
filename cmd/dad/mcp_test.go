// MCP smoke tests: drive POST /mcp over real HTTP with raw Streamable HTTP
// JSON-RPC payloads (the handler runs stateless with JSON responses, so each
// POST is one independent JSON-RPC request).
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// expectedMCPTools is every tool registerMCPTools registers.
var expectedMCPTools = []string{
	"session_acquire", "session_keepalive", "session_release", "session_pause",
	"session_resume", "session_get", "session_events", "session_wait",
	"session_takeover", "health_get",
	"shell_exec", "shell_job_get", "shell_job_kill", "shell_jobs_list",
	"files_list", "files_read", "files_write", "files_delete", "files_mkdir",
	"files_move",
	"browser_start", "browser_stop", "browser_navigate", "browser_evaluate",
	"browser_act", "browser_text", "browser_state", "browser_back",
	"browser_forward", "browser_reload", "browser_input", "browser_size",
	"browser_frame",
	"display_screenshot", "display_click", "display_type", "display_key",
	"display_scroll", "display_drag", "display_meta",
	"apps_list", "apps_start", "apps_stop",
}

func newMCPAgent(t *testing.T) (*Agent, *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a := &Agent{token: testToken, log: slog.New(slog.DiscardHandler), startedAt: time.Now()}
	mux := http.NewServeMux()
	a.registerMCP(mux, a.auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv
}

// mcpRPC posts one JSON-RPC request to /mcp and decodes the response.
func mcpRPC(t *testing.T, srv *httptest.Server, id int, method string, params any, authed bool) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authed {
		req.Header.Set("X-Dad-Token", testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /mcp %s: %d %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST /mcp %s: bad JSON response %q: %v", method, raw, err)
	}
	return resp.StatusCode, out
}

// mcpInitialize runs the MCP initialize handshake.
func mcpInitialize(t *testing.T, srv *httptest.Server) {
	t.Helper()
	_, out := mcpRPC(t, srv, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-test", "version": "0"},
	}, true)
	if _, ok := out["result"]; !ok {
		t.Fatalf("initialize: no result in %v", out)
	}
}

// mcpToolResult is the decoded result of a tools/call.
type mcpToolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MIMEType string `json:"mimeType"`
	} `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

// mcpCall calls one tool and decodes its result. The raw RPC envelope is
// returned too, so tests can assert protocol-level errors.
func mcpCall(t *testing.T, srv *httptest.Server, id int, name string, args map[string]any) (mcpToolResult, map[string]any) {
	t.Helper()
	_, out := mcpRPC(t, srv, id, "tools/call", map[string]any{
		"name": name, "arguments": args,
	}, true)
	if _, ok := out["error"]; ok {
		t.Fatalf("tools/call %s: protocol error %v", name, out["error"])
	}
	raw, err := json.Marshal(out["result"])
	if err != nil {
		t.Fatal(err)
	}
	var res mcpToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("tools/call %s: bad result %s: %v", name, raw, err)
	}
	return res, out
}

// mcpCallText calls a tool that returns JSON text content and unmarshals the
// first text block into v. It fails the test when the tool reports an error.
func mcpCallText(t *testing.T, srv *httptest.Server, id int, name string, args map[string]any, v any) {
	t.Helper()
	res, _ := mcpCall(t, srv, id, name, args)
	if res.IsError {
		t.Fatalf("tools/call %s: tool error: %s", name, res.Content[0].Text)
	}
	if len(res.Content) == 0 || res.Content[0].Text == "" {
		t.Fatalf("tools/call %s: no text content in %+v", name, res)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(res.Content[0].Text), v); err != nil {
			t.Fatalf("tools/call %s: bad JSON %q: %v", name, res.Content[0].Text, err)
		}
	}
}

// mcpCallErr calls a tool and returns its error text. It fails the test when
// the tool does not report an error.
func mcpCallErr(t *testing.T, srv *httptest.Server, id int, name string, args map[string]any) string {
	t.Helper()
	res, _ := mcpCall(t, srv, id, name, args)
	if !res.IsError {
		t.Fatalf("tools/call %s: want a tool error, got %+v", name, res)
	}
	if len(res.Content) == 0 {
		t.Fatalf("tools/call %s: error with no content", name)
	}
	return res.Content[0].Text
}

func TestMCPMethodAndAuth(t *testing.T) {
	_, srv := newMCPAgent(t)
	// GET /mcp is 405: only POST is registered.
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: %d, want 405", resp.StatusCode)
	}
	// POST /mcp without the token is 401.
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /mcp: %d, want 401", resp.StatusCode)
	}
}

func TestMCPToolsList(t *testing.T) {
	_, srv := newMCPAgent(t)
	mcpInitialize(t, srv)
	_, out := mcpRPC(t, srv, 2, "tools/list", map[string]any{}, true)
	raw, err := json.Marshal(out["result"])
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("tools/list: %s: %v", raw, err)
	}
	got := map[string]bool{}
	for _, tool := range list.Tools {
		got[tool.Name] = true
	}
	for _, want := range expectedMCPTools {
		if !got[want] {
			t.Errorf("tools/list missing tool %q (got %d tools)", want, len(list.Tools))
		}
	}
	if len(list.Tools) != len(expectedMCPTools) {
		t.Errorf("tools/list returned %d tools, want %d", len(list.Tools), len(expectedMCPTools))
	}
}

// TestMCPReadsAcceptSessionID: reads accept an optional session_id (ignored),
// so a client can pass it uniformly to every tool instead of tracking which
// tools take one.
func TestMCPReadsAcceptSessionID(t *testing.T) {
	_, srv := newMCPAgent(t)
	mcpInitialize(t, srv)
	var s Session
	mcpCallText(t, srv, 2, "session_acquire", map[string]any{"kind": "agent", "name": "reads-test"}, &s)
	if s.ID == "" {
		t.Fatal("acquire returned no id")
	}
	sid := map[string]any{"session_id": s.ID}
	mcpCallText(t, srv, 3, "session_get", sid, nil)
	mcpCallText(t, srv, 4, "session_events", sid, nil)
	mcpCallText(t, srv, 5, "health_get", sid, nil)
	mcpCallText(t, srv, 6, "shell_jobs_list", sid, nil)
	mcpCallText(t, srv, 7, "files_list", sid, nil)
	mcpCallText(t, srv, 8, "browser_state", sid, nil)
	mcpCallText(t, srv, 9, "apps_list", sid, nil)
	mcpCallText(t, srv, 10, "files_write", map[string]any{"session_id": s.ID, "path": "reads.txt", "content": "x", "encoding": "text"}, nil)
	mcpCallText(t, srv, 11, "files_read", map[string]any{"session_id": s.ID, "path": "reads.txt"}, nil)
}

func TestMCPSmokeSessionShellFiles(t *testing.T) {
	_, srv := newMCPAgent(t)
	mcpInitialize(t, srv)

	// A mutating call with no session held fails cleanly.
	if got := mcpCallErr(t, srv, 10, "shell_exec", map[string]any{
		"session_id": "ses_nope", "command": "echo hi",
	}); !strings.Contains(got, "no session held") {
		t.Fatalf("shell_exec without session: %q, want %q", got, "no session held")
	}

	// Acquire a session.
	var ses struct {
		ID string `json:"id"`
	}
	mcpCallText(t, srv, 11, "session_acquire", map[string]any{
		"kind": "agent", "name": "mcp-test", "ttl_ms": 60000,
	}, &ses)
	if ses.ID == "" {
		t.Fatal("session_acquire returned no id")
	}

	// A mutating call with the wrong session id names the holder.
	if got := mcpCallErr(t, srv, 12, "shell_exec", map[string]any{
		"session_id": "ses_wrong", "command": "echo hi",
	}); !strings.Contains(got, "session held by mcp-test") {
		t.Fatalf("shell_exec with wrong session: %q, want %q", got, "session held by mcp-test")
	}

	// Pause refuses mutations; resume allows them again.
	var paused struct {
		Paused bool `json:"paused"`
	}
	mcpCallText(t, srv, 13, "session_pause", map[string]any{"session_id": ses.ID}, &paused)
	if !paused.Paused {
		t.Fatal("session_pause did not pause")
	}
	if got := mcpCallErr(t, srv, 14, "shell_exec", map[string]any{
		"session_id": ses.ID, "command": "echo hi",
	}); !strings.Contains(got, "session paused") {
		t.Fatalf("shell_exec while paused: %q, want %q", got, "session paused")
	}
	mcpCallText(t, srv, 15, "session_resume", map[string]any{"session_id": ses.ID}, &paused)
	if paused.Paused {
		t.Fatal("session_resume did not resume")
	}

	// Shell exec through the session.
	var execRes struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
		Stdout   string `json:"stdout"`
		JobID    string `json:"job_id"`
	}
	mcpCallText(t, srv, 16, "shell_exec", map[string]any{
		"session_id": ses.ID, "command": "echo hello",
	}, &execRes)
	if execRes.Status != "completed" || execRes.Stdout != "hello\n" || execRes.ExitCode == nil || *execRes.ExitCode != 0 {
		t.Fatalf("shell_exec echo: %+v", execRes)
	}

	// The job is listed and fetchable.
	var jobs struct {
		Jobs []struct {
			JobID string `json:"job_id"`
		} `json:"jobs"`
	}
	mcpCallText(t, srv, 17, "shell_jobs_list", map[string]any{}, &jobs)
	found := false
	for _, j := range jobs.Jobs {
		if j.JobID == execRes.JobID {
			found = true
		}
	}
	if !found {
		t.Fatalf("shell_jobs_list missing job %s: %+v", execRes.JobID, jobs)
	}
	var job ShellExecResult
	mcpCallText(t, srv, 18, "shell_job_get", map[string]any{"job_id": execRes.JobID}, &job)
	if job.Stdout != "hello\n" {
		t.Fatalf("shell_job_get: %+v", job)
	}
	if got := mcpCallErr(t, srv, 19, "shell_job_get", map[string]any{"job_id": "job-nope"}); !strings.Contains(got, "unknown job") {
		t.Fatalf("shell_job_get unknown: %q", got)
	}

	// Files round-trip: text write/read, base64 binary write/read, list.
	var written struct {
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	mcpCallText(t, srv, 20, "files_write", map[string]any{
		"session_id": ses.ID, "path": "notes/hello.txt", "content": "hello mcp",
	}, &written)
	if written.Path != "notes/hello.txt" || written.Size != 9 {
		t.Fatalf("files_write: %+v", written)
	}
	var read struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	mcpCallText(t, srv, 21, "files_read", map[string]any{"path": "notes/hello.txt"}, &read)
	if read.Content != "hello mcp" || read.Encoding != "text" {
		t.Fatalf("files_read: %+v", read)
	}
	mcpCallText(t, srv, 22, "files_write", map[string]any{
		"session_id": ses.ID, "path": "bin.dat", "content": "/wA=", "encoding": "base64",
	}, &written)
	var binRead struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Note     string `json:"note"`
	}
	mcpCallText(t, srv, 23, "files_read", map[string]any{"path": "bin.dat"}, &binRead)
	if binRead.Content != "/wA=" || binRead.Encoding != "base64" || binRead.Note == "" {
		t.Fatalf("files_read binary: %+v", binRead)
	}
	var listed struct {
		Entries []FileEntry `json:"entries"`
	}
	mcpCallText(t, srv, 24, "files_list", map[string]any{"path": "notes"}, &listed)
	if len(listed.Entries) != 1 || listed.Entries[0].Name != "hello.txt" {
		t.Fatalf("files_list: %+v", listed)
	}

	// Mkdir, move, delete.
	mcpCallText(t, srv, 25, "files_mkdir", map[string]any{"session_id": ses.ID, "path": "dir/sub"}, nil)
	mcpCallText(t, srv, 26, "files_move", map[string]any{
		"session_id": ses.ID, "from": "notes/hello.txt", "to": "dir/moved.txt",
	}, nil)
	mcpCallText(t, srv, 27, "files_delete", map[string]any{
		"session_id": ses.ID, "path": "bin.dat",
	}, nil)
	if got := mcpCallErr(t, srv, 28, "files_read", map[string]any{"path": "bin.dat"}); !strings.Contains(got, "no such file") {
		t.Fatalf("files_read after delete: %q", got)
	}
	// A jail escape is a clean tool error, not a panic.
	if got := mcpCallErr(t, srv, 29, "files_read", map[string]any{"path": "../escape.txt"}); !strings.Contains(got, "escapes home") {
		t.Fatalf("files_read escape: %q", got)
	}

	// Session state, events and health are reads.
	var state struct {
		Session        *Session `json:"session"`
		BrowserRunning bool     `json:"browser_running"`
	}
	mcpCallText(t, srv, 30, "session_get", map[string]any{}, &state)
	if state.Session == nil || state.Session.ID != ses.ID {
		t.Fatalf("session_get: %+v", state)
	}
	var events struct {
		Events []ActivityEvent `json:"events"`
	}
	mcpCallText(t, srv, 31, "session_events", map[string]any{}, &events)
	if len(events.Events) == 0 {
		t.Fatal("session_events returned no events")
	}
	var health struct {
		Status string `json:"status"`
	}
	mcpCallText(t, srv, 32, "health_get", map[string]any{}, &health)
	if health.Status != "ok" && health.Status != "degraded" {
		t.Fatalf("health_get: %+v", health)
	}
	// Keepalive extends the session.
	var kept struct {
		ID string `json:"id"`
	}
	mcpCallText(t, srv, 33, "session_keepalive", map[string]any{"session_id": ses.ID}, &kept)
	if kept.ID != ses.ID {
		t.Fatalf("session_keepalive: %+v", kept)
	}

	// Release, then mutations fail with "no session held" again.
	var released struct {
		Released bool `json:"released"`
	}
	mcpCallText(t, srv, 34, "session_release", map[string]any{"session_id": ses.ID}, &released)
	if !released.Released {
		t.Fatal("session_release did not release")
	}
	if got := mcpCallErr(t, srv, 35, "shell_exec", map[string]any{
		"session_id": ses.ID, "command": "echo hi",
	}); !strings.Contains(got, "no session held") {
		t.Fatalf("shell_exec after release: %q", got)
	}
}

func TestMCPBrowserDisplayAppsWithoutBackends(t *testing.T) {
	a, srv := newMCPAgent(t)
	// Keep Chromium off: browser_navigate/start go through ensureBrowser,
	// which would otherwise wait 20s for a Chromium that never appears.
	t.Setenv("DAD_BROWSER_START", "off")
	mcpInitialize(t, srv)
	var ses struct {
		ID string `json:"id"`
	}
	mcpCallText(t, srv, 11, "session_acquire", map[string]any{
		"kind": "agent", "name": "mcp-test",
	}, &ses)

	// Browser tools without Chromium: clean errors, no panic.
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"browser_text", map[string]any{}, "chromium not running"},
		{"browser_frame", map[string]any{}, "chromium not running"},
		{"browser_evaluate", map[string]any{"session_id": ses.ID, "expression": "1+1"}, "chromium not running"},
		{"browser_act", map[string]any{"session_id": ses.ID, "kind": "click", "x": 1, "y": 2}, "chromium not running"},
		{"browser_back", map[string]any{"session_id": ses.ID}, "chromium not running"},
		{"browser_navigate", map[string]any{"session_id": ses.ID, "url": "https://example.com"}, "browser is disabled"},
	} {
		if got := mcpCallErr(t, srv, 20, tc.name, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%s without browser: %q, want %q", tc.name, got, tc.want)
		}
	}
	// browser_state is a read that reports not running instead of erroring.
	var bstate struct {
		Running bool `json:"running"`
	}
	mcpCallText(t, srv, 21, "browser_state", map[string]any{}, &bstate)
	if bstate.Running {
		t.Fatal("browser_state without browser: running=true")
	}
	// browser_start with the browser disabled fails cleanly (503-style).
	if got := mcpCallErr(t, srv, 22, "browser_start", map[string]any{"session_id": ses.ID}); !strings.Contains(got, "browser is disabled") {
		t.Errorf("browser_start disabled: %q", got)
	}

	// Display tools without a Broadway client: clean errors, no panic.
	if got := mcpCallErr(t, srv, 23, "display_screenshot", map[string]any{}); !strings.Contains(got, "display unavailable") {
		t.Errorf("display_screenshot without broadway: %q", got)
	}
	if got := mcpCallErr(t, srv, 24, "display_click", map[string]any{
		"session_id": ses.ID, "x": 10, "y": 10,
	}); !strings.Contains(got, "display unavailable") {
		t.Errorf("display_click without broadway: %q", got)
	}
	if got := mcpCallErr(t, srv, 25, "display_meta", map[string]any{}); !strings.Contains(got, "display unavailable") {
		t.Errorf("display_meta without broadway: %q", got)
	}

	// Apps list works with no registry; unknown apps are clean errors.
	var applist struct {
		Apps []appV1 `json:"apps"`
	}
	mcpCallText(t, srv, 26, "apps_list", map[string]any{}, &applist)
	if applist.Apps == nil {
		t.Fatal("apps_list returned null apps")
	}
	if got := mcpCallErr(t, srv, 27, "apps_start", map[string]any{
		"session_id": ses.ID, "id": "nope",
	}); !strings.Contains(got, "unknown app") {
		t.Errorf("apps_start unknown: %q", got)
	}
	if got := mcpCallErr(t, srv, 28, "apps_stop", map[string]any{
		"session_id": ses.ID, "id": "nope",
	}); !strings.Contains(got, "unknown app") {
		t.Errorf("apps_stop unknown: %q", got)
	}
	_ = a
}

func TestMCPBrowserFakeCDP(t *testing.T) {
	a, srv := newMCPAgent(t)
	_, sc := connectFakeEval(t)
	a.cdp = sc
	mcpInitialize(t, srv)
	var ses struct {
		ID string `json:"id"`
	}
	mcpCallText(t, srv, 11, "session_acquire", map[string]any{
		"kind": "agent", "name": "mcp-test",
	}, &ses)

	var bstate struct {
		Running bool   `json:"running"`
		URL     string `json:"url"`
	}
	mcpCallText(t, srv, 12, "browser_state", map[string]any{}, &bstate)
	if !bstate.Running {
		t.Fatal("browser_state with fake screencast: running=false")
	}
	var eval struct {
		Value any `json:"value"`
	}
	mcpCallText(t, srv, 13, "browser_evaluate", map[string]any{
		"session_id": ses.ID, "expression": "1+1",
	}, &eval)
	if eval.Value != "1+1" {
		t.Fatalf("browser_evaluate value = %#v, want the bare echoed expression", eval.Value)
	}
	var text struct {
		Text string `json:"text"`
	}
	mcpCallText(t, srv, 14, "browser_text", map[string]any{}, &text)
	// Clicking an element the fake resolves works; a missing selector errors.
	mcpCallText(t, srv, 15, "browser_act", map[string]any{
		"session_id": ses.ID, "kind": "click", "selector": ".hit",
	}, nil)
	if got := mcpCallErr(t, srv, 16, "browser_act", map[string]any{
		"session_id": ses.ID, "kind": "click", "selector": ".miss",
	}); !strings.Contains(got, "selector not found") {
		t.Errorf("browser_act missing selector: %q", got)
	}
	// A fresh PNG frame comes back as MCP image content.
	res, _ := mcpCall(t, srv, 17, "browser_frame", map[string]any{})
	if res.IsError {
		t.Fatalf("browser_frame: tool error: %s", res.Content[0].Text)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "image" || res.Content[0].MIMEType != "image/png" || res.Content[0].Data == "" {
		t.Fatalf("browser_frame: bad image content %+v", res.Content)
	}
}

func TestMCPDisplayFakeBroadway(t *testing.T) {
	a, srv := newMCPAgent(t)
	_, c := connectFakeBroadway(t)
	a.broadway = c
	mcpInitialize(t, srv)
	var ses struct {
		ID string `json:"id"`
	}
	mcpCallText(t, srv, 11, "session_acquire", map[string]any{
		"kind": "agent", "name": "mcp-test",
	}, &ses)
	mcpCallText(t, srv, 12, "display_click", map[string]any{
		"session_id": ses.ID, "x": 100, "y": 200,
	}, nil)
	var meta struct {
		SurfaceCount int `json:"surface_count"`
	}
	mcpCallText(t, srv, 13, "display_meta", map[string]any{}, &meta)
	if got := mcpCallErr(t, srv, 14, "display_key", map[string]any{
		"session_id": ses.ID, "key": "NoSuchKey",
	}); !strings.Contains(got, "unknown key") {
		t.Errorf("display_key unknown: %q", got)
	}
}
