package main

import (
	"strings"
	"testing"
	"time"
)

// mcpMutatingTools is every MCP tool that must enforce the session rule, with
// otherwise-valid arguments so only the session check can refuse the call.
var mcpMutatingTools = map[string]map[string]any{
	"shell_exec":       {"command": "true"},
	"shell_job_kill":   {"job_id": "job-x"},
	"files_write":      {"path": "a.txt", "content": "x"},
	"files_delete":     {"path": "a.txt"},
	"files_mkdir":      {"path": "d"},
	"files_move":       {"from": "a", "to": "b"},
	"browser_start":    {},
	"browser_stop":     {},
	"browser_navigate": {"url": "http://example.com"},
	"browser_evaluate": {"expression": "1"},
	"browser_act":      {"kind": "click", "x": 1, "y": 1},
	"browser_back":     {},
	"browser_forward":  {},
	"browser_reload":   {},
	"browser_input":    {"type": "key", "key": "a"},
	"browser_size":     {"w": 800, "h": 600},
	"display_click":    {"x": 1, "y": 1},
	"display_type":     {"text": "x"},
	"display_key":      {"key": "Return"},
	"display_scroll":   {"x": 1, "y": 1, "dy": 1},
	"display_drag":     {"x1": 1, "y1": 1, "x2": 2, "y2": 2},
	"apps_start":       {"id": "terminal"},
	"apps_stop":        {"id": "terminal"},
}

func withSession(args map[string]any, sid string) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	if sid != "" {
		out["session_id"] = sid
	}
	return out
}

// Every mutating MCP tool refuses without a session, with someone else's
// session, and with a paused session, exactly like the /v1 409s.
func TestMCPSessionRuleEveryMutatingTool(t *testing.T) {
	a, srv := newMCPAgent(t)
	mcpInitialize(t, srv)
	id := 100
	// A missing session_id is refused either by the tool's input schema
	// (required property) or, where the schema makes it optional, by the
	// session check itself.
	check := func(sid, want string) {
		t.Helper()
		for name, args := range mcpMutatingTools {
			id++
			msg := mcpCallErr(t, srv, id, name, withSession(args, sid))
			schemaRefused := sid == "" && strings.Contains(msg, `missing properties: ["session_id"]`)
			if !strings.Contains(msg, want) && !schemaRefused {
				t.Errorf("%s with session %q: error %q, want it to contain %q", name, sid, msg, want)
			}
		}
	}
	check("", "no session held")
	s, _ := a.acquireSession(SessionHolder{Kind: "agent", Name: "mcp-holder"}, time.Minute)
	check("", "session held by mcp-holder")
	check("ses_wrong", "session held by mcp-holder")
	if _, err := a.pauseSession(s.ID, true); err != nil {
		t.Fatal(err)
	}
	check(s.ID, "session paused")
	if len(mcpMutatingTools)+len(mcpReadTools)+len(mcpSessionTools) != len(expectedMCPTools) {
		t.Fatalf("tool groups cover %d tools, want %d", len(mcpMutatingTools)+len(mcpReadTools)+len(mcpSessionTools), len(expectedMCPTools))
	}
}

// mcpReadTools never need a session.
var mcpReadTools = []string{
	"health_get", "shell_job_get", "shell_jobs_list", "files_list", "files_read",
	"browser_text", "browser_state", "browser_frame", "display_screenshot", "display_meta", "apps_list",
}

// mcpSessionTools manage the lease itself.
var mcpSessionTools = []string{
	"session_acquire", "session_keepalive", "session_release", "session_pause",
	"session_resume", "session_get", "session_events",
}

// Read tools work for anyone while another holder has the session.
func TestMCPReadToolsNeedNoSession(t *testing.T) {
	a, srv := newMCPAgent(t)
	mcpInitialize(t, srv)
	a.acquireSession(SessionHolder{Kind: "agent", Name: "someone"}, time.Minute)
	for i, name := range []string{"health_get", "shell_jobs_list", "files_list", "browser_state", "apps_list", "session_get", "session_events"} {
		res, _ := mcpCall(t, srv, 200+i, name, map[string]any{})
		if res.IsError {
			t.Errorf("%s while another holder has the session: %+v", name, res)
		}
	}
}
