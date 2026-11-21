// MCP server: the Model Context Protocol surface for dc-agent, served as
// Streamable HTTP at POST /mcp behind the existing token auth. Every tool
// calls the same Agent service methods the /v1 HTTP handlers call — never an
// HTTP round-trip to self. Mutating tools enforce the session rule in-process
// (live session required, id must match, session must not be paused); reads
// never require a session.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/gtsteffaniak/docker-computer/internal/apps"
	"github.com/gtsteffaniak/docker-computer/internal/broadway"
	"github.com/gtsteffaniak/docker-computer/internal/cdp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerMCP mounts the MCP Streamable HTTP handler at POST /mcp, behind
// auth. GET /mcp (and any other method) is a 405: the mux only registers the
// POST pattern, and the SDK handler itself also rejects non-POST requests.
func (a *Agent) registerMCP(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	server := mcp.NewServer(&mcp.Implementation{Name: "docker-computer", Version: "1"}, nil)
	a.registerMCPTools(server)
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	mux.HandleFunc("POST /mcp", auth(handler.ServeHTTP))
}

// mcpSession enforces the session rule for mutating tools, with the same
// messages as the /v1 409s: no session held, held by someone else (naming the
// holder), or paused. It returns the live session and its holder's name.
func (a *Agent) mcpSession(id string) (*Session, error) {
	s := a.liveSession()
	if s == nil {
		return nil, errors.New("no session held")
	}
	if id == "" || id != s.ID {
		return nil, errors.New("session held by " + s.Holder.Name)
	}
	if s.Paused {
		return nil, errors.New("session paused")
	}
	return s, nil
}

// mcpScreencast returns the attached screencast, or a clean error when
// Chromium is not running (the 503 the /v1 handlers report, as a tool error).
func (a *Agent) mcpScreencast() (*cdp.Screencast, error) {
	a.touch()
	if sc := a.screencast(); sc != nil {
		return sc, nil
	}
	return nil, errors.New("chromium not running")
}

// mcpBroadway returns the Broadway client, or a clean error when there is no
// display client (nil only in tests — the real agent always has one).
func (a *Agent) mcpBroadway() (*broadway.Client, error) {
	if a.broadway == nil {
		return nil, errors.New("display unavailable")
	}
	return a.broadway, nil
}

// mcpApps lists the registry apps with their running state, exactly as
// GET /v1/apps reports them. Exec is deliberately not exposed.
func (a *Agent) mcpApps() []appV1 {
	list := a.loadApps()
	out := make([]appV1, 0, len(list))
	for _, app := range list {
		running := a.appRunning(app.ID)
		if app.Port == 0 {
			running = a.screencast() != nil
		}
		out = append(out, appV1{
			ID: app.ID, Name: app.Name, Icon: app.Icon,
			Order: app.Order, Port: app.Port, Running: running,
		})
	}
	return out
}

// registerMCPTools registers every MCP tool on server.
func (a *Agent) registerMCPTools(server *mcp.Server) {
	// Sessions.
	mcp.AddTool(server, &mcp.Tool{Name: "session_acquire", Description: "Acquire the single live session lease on the computer. Fails when another holder owns it."}, a.mcpSessionAcquire)
	mcp.AddTool(server, &mcp.Tool{Name: "session_keepalive", Description: "Extend a live session's lease by its TTL."}, a.mcpSessionKeepalive)
	mcp.AddTool(server, &mcp.Tool{Name: "session_release", Description: "Release a live session."}, a.mcpSessionRelease)
	mcp.AddTool(server, &mcp.Tool{Name: "session_pause", Description: "Pause a live session; mutating calls are refused while paused."}, a.mcpSessionPause)
	mcp.AddTool(server, &mcp.Tool{Name: "session_resume", Description: "Resume a paused live session."}, a.mcpSessionResume)
	mcp.AddTool(server, &mcp.Tool{Name: "session_get", Description: "Report the live session (or null) plus browser, viewer and app state."}, a.mcpSessionGet)
	mcp.AddTool(server, &mcp.Tool{Name: "session_events", Description: "Read the activity log: events with seq greater than since, up to limit."}, a.mcpSessionEvents)
	// Health.
	mcp.AddTool(server, &mcp.Tool{Name: "health_get", Description: "Real session health: agent, chromium, broadway, disk and session checks."}, a.mcpHealthGet)
	// Shell.
	mcp.AddTool(server, &mcp.Tool{Name: "shell_exec", Description: "Run a shell command (via /bin/sh -c) or an argv directly, foreground or background."}, a.mcpShellExec)
	mcp.AddTool(server, &mcp.Tool{Name: "shell_job_get", Description: "Get one shell job's current result by job id."}, a.mcpShellJobGet)
	mcp.AddTool(server, &mcp.Tool{Name: "shell_job_kill", Description: "SIGKILL a running shell job's whole process group."}, a.mcpShellJobKill)
	mcp.AddTool(server, &mcp.Tool{Name: "shell_jobs_list", Description: "List all tracked shell jobs, oldest first."}, a.mcpShellJobsList)
	// Files.
	mcp.AddTool(server, &mcp.Tool{Name: "files_list", Description: "List a directory under the home directory."}, a.mcpFilesList)
	mcp.AddTool(server, &mcp.Tool{Name: "files_read", Description: "Read a file (max 10 MiB). Text is returned as text; non-UTF-8 content is returned base64-encoded with a note."}, a.mcpFilesRead)
	mcp.AddTool(server, &mcp.Tool{Name: "files_write", Description: "Write a file, creating parent directories. Content is text by default, or base64 with encoding base64 (this is also how uploads are done over MCP)."}, a.mcpFilesWrite)
	mcp.AddTool(server, &mcp.Tool{Name: "files_delete", Description: "Delete a file, or a directory with recursive true. The home directory itself can never be deleted."}, a.mcpFilesDelete)
	mcp.AddTool(server, &mcp.Tool{Name: "files_mkdir", Description: "Create a directory, including parents."}, a.mcpFilesMkdir)
	mcp.AddTool(server, &mcp.Tool{Name: "files_move", Description: "Rename or move a file or directory within the home directory. The destination must not already exist."}, a.mcpFilesMove)
	// Browser.
	mcp.AddTool(server, &mcp.Tool{Name: "browser_start", Description: "Start Chromium and wait until its screencast is attached."}, a.mcpBrowserStart)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_stop", Description: "Stop Chromium."}, a.mcpBrowserStop)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_navigate", Description: "Navigate the browser to a URL (starts Chromium on first use)."}, a.mcpBrowserNavigate)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_evaluate", Description: "Run one JavaScript expression in the page and return its value as JSON."}, a.mcpBrowserEvaluate)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_act", Description: "One high-level browser action: click, type, select, scroll or key, by selector or coordinates."}, a.mcpBrowserAct)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_text", Description: "Return the page's visible text."}, a.mcpBrowserText)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_state", Description: "Return the page's URL, title and history state, and whether Chromium is running."}, a.mcpBrowserState)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_back", Description: "Go back in the browser history."}, a.mcpBrowserBack)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_forward", Description: "Go forward in the browser history."}, a.mcpBrowserForward)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_reload", Description: "Reload the current page."}, a.mcpBrowserReload)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_input", Description: "Forward one raw browser input event: pointer, key, scroll or paste."}, a.mcpBrowserInput)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_size", Description: "Set the browser viewport size and scale."}, a.mcpBrowserSize)
	mcp.AddTool(server, &mcp.Tool{Name: "browser_frame", Description: "Capture the current page as a PNG image."}, a.mcpBrowserFrame)
	// Display.
	mcp.AddTool(server, &mcp.Tool{Name: "display_screenshot", Description: "Capture the desktop as a PNG image."}, a.mcpDisplayScreenshot)
	mcp.AddTool(server, &mcp.Tool{Name: "display_click", Description: "Click at desktop coordinates. Button 1 left, 2 middle, 3 right."}, a.mcpDisplayClick)
	mcp.AddTool(server, &mcp.Tool{Name: "display_type", Description: "Type text into the focused window."}, a.mcpDisplayType)
	mcp.AddTool(server, &mcp.Tool{Name: "display_key", Description: "Press and release one key by name (Return, Tab, Escape, arrows, F1-F12, single characters)."}, a.mcpDisplayKey)
	mcp.AddTool(server, &mcp.Tool{Name: "display_scroll", Description: "Scroll vertically at desktop coordinates. dx is ignored: horizontal scrolling is not supported."}, a.mcpDisplayScroll)
	mcp.AddTool(server, &mcp.Tool{Name: "display_drag", Description: "Drag with the left button from (x1, y1) to (x2, y2)."}, a.mcpDisplayDrag)
	mcp.AddTool(server, &mcp.Tool{Name: "display_meta", Description: "Report desktop surface metadata: size, surfaces and connection state."}, a.mcpDisplayMeta)
	// Apps.
	mcp.AddTool(server, &mcp.Tool{Name: "apps_list", Description: "List the registry apps with whether each is running."}, a.mcpAppsList)
	mcp.AddTool(server, &mcp.Tool{Name: "apps_start", Description: "Start one registry app by id. The browser (port 0) has no process to start."}, a.mcpAppsStart)
	mcp.AddTool(server, &mcp.Tool{Name: "apps_stop", Description: "Stop one registry app by id. Stopping the browser (port 0) stops Chromium."}, a.mcpAppsStop)
}

// --- argument structs ---
// session_id is required on every mutating tool. Reads accept an optional
// session_id too, ignored, so a client can pass it uniformly to every tool.

type sessionAcquireIn struct {
	Kind  string `json:"kind" jsonschema:"agent or user"`
	Name  string `json:"name" jsonschema:"name of the session holder"`
	RunID string `json:"run_id,omitempty" jsonschema:"run id, for agent holders"`
	TTLMs int    `json:"ttl_ms,omitempty" jsonschema:"lease TTL in milliseconds, clamped to 10s..10min; default 90s"`
}

type sessionIDIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
}

type sessionEventsIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; ignored for reads"`
	Since     uint64 `json:"since,omitempty" jsonschema:"return only events with seq greater than this"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events to return, default 100"`
}

type emptyIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; ignored for reads"`
}

type shellExecIn struct {
	SessionID  string            `json:"session_id" jsonschema:"id of the live session"`
	Command    string            `json:"command,omitempty" jsonschema:"shell command, run via /bin/sh -c; exactly one of command or argv is required"`
	Argv       []string          `json:"argv,omitempty" jsonschema:"argv to run directly; exactly one of command or argv is required"`
	Cwd        string            `json:"cwd,omitempty" jsonschema:"working directory under home, default home"`
	TimeoutMs  int               `json:"timeout_ms,omitempty" jsonschema:"timeout in milliseconds, default 30000, clamped to 1000..300000"`
	Background bool              `json:"background,omitempty" jsonschema:"start and return immediately with status running"`
	Env        map[string]string `json:"env,omitempty" jsonschema:"extra environment variables; PATH and HOME cannot be overridden"`
}

type shellJobIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; required for shell_job_kill, ignored for shell_job_get"`
	JobID     string `json:"job_id" jsonschema:"job id"`
}

type filesListIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; ignored for reads"`
	Path      string `json:"path,omitempty" jsonschema:"directory under home, default home"`
}

type filesReadIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; ignored for reads"`
	Path      string `json:"path" jsonschema:"file under home"`
}

type filesWriteIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	Path      string `json:"path" jsonschema:"file under home"`
	Content   string `json:"content" jsonschema:"file content, text or base64 per encoding"`
	Encoding  string `json:"encoding,omitempty" jsonschema:"text (default) or base64"`
}

type filesDeleteIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	Path      string `json:"path" jsonschema:"file or directory under home"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"delete directories recursively"`
}

type filesPathIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	Path      string `json:"path" jsonschema:"path under home"`
}

type filesMoveIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	From      string `json:"from" jsonschema:"source path under home"`
	To        string `json:"to" jsonschema:"destination path under home; must not already exist"`
}

type browserNavigateIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	URL       string `json:"url" jsonschema:"address to load"`
}

type browserEvaluateIn struct {
	SessionID  string `json:"session_id" jsonschema:"id of the live session"`
	Expression string `json:"expression" jsonschema:"JavaScript expression to evaluate in the page"`
}

type browserActIn struct {
	SessionID string  `json:"session_id" jsonschema:"id of the live session"`
	Kind      string  `json:"kind" jsonschema:"click, type, select, scroll or key"`
	Selector  string  `json:"selector,omitempty" jsonschema:"CSS selector; click/type/select/scroll resolve it to the element centre"`
	Text      string  `json:"text,omitempty" jsonschema:"text to type, or value to select"`
	X         float64 `json:"x,omitempty" jsonschema:"x coordinate, for click/scroll without a selector"`
	Y         float64 `json:"y,omitempty" jsonschema:"y coordinate, for click/scroll without a selector"`
	DX        float64 `json:"dx,omitempty" jsonschema:"horizontal scroll delta"`
	DY        float64 `json:"dy,omitempty" jsonschema:"vertical scroll delta"`
	Button    int     `json:"button,omitempty" jsonschema:"mouse button: 1 left, 2 middle, 3 right; default 1"`
	Clicks    int     `json:"clicks,omitempty" jsonschema:"click count, default 1"`
	Key       string  `json:"key,omitempty" jsonschema:"key name, for kind key"`
}

type browserInputIn struct {
	SessionID string  `json:"session_id" jsonschema:"id of the live session"`
	Type      string  `json:"type" jsonschema:"pointer, key, scroll or paste"`
	Action    string  `json:"action,omitempty" jsonschema:"press, release or move for pointer; down or up for key; empty means press+release / down+up"`
	X         float64 `json:"x,omitempty" jsonschema:"x coordinate for pointer/scroll"`
	Y         float64 `json:"y,omitempty" jsonschema:"y coordinate for pointer/scroll"`
	Button    int     `json:"button,omitempty" jsonschema:"mouse button: 1 left, 2 middle, 3 right"`
	Clicks    int     `json:"clicks,omitempty" jsonschema:"click count for pointer"`
	Key       string  `json:"key,omitempty" jsonschema:"key name for key input"`
	Code      string  `json:"code,omitempty" jsonschema:"DOM code for key input"`
	Text      string  `json:"text,omitempty" jsonschema:"text for key input, or text to paste"`
	DX        float64 `json:"dx,omitempty" jsonschema:"horizontal scroll delta"`
	DY        float64 `json:"dy,omitempty" jsonschema:"vertical scroll delta"`
	Mods      int     `json:"mods,omitempty" jsonschema:"modifier bitmask"`
}

type browserSizeIn struct {
	SessionID string  `json:"session_id" jsonschema:"id of the live session"`
	W         int     `json:"w" jsonschema:"viewport width"`
	H         int     `json:"h" jsonschema:"viewport height"`
	Scale     float64 `json:"scale,omitempty" jsonschema:"viewport scale"`
}

type displayClickIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	X         int32  `json:"x" jsonschema:"x coordinate"`
	Y         int32  `json:"y" jsonschema:"y coordinate"`
	Button    uint32 `json:"button,omitempty" jsonschema:"mouse button: 1 left, 2 middle, 3 right; default 1"`
}

type displayTypeIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	Text      string `json:"text" jsonschema:"text to type"`
}

type displayKeyIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	Key       string `json:"key" jsonschema:"key name: Return, Tab, Escape, arrows, F1-F12, single characters"`
	Mods      uint32 `json:"mods,omitempty" jsonschema:"modifier mask: Shift 1, Control 4, Alt 8"`
}

type displayScrollIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	X         int32  `json:"x" jsonschema:"x coordinate"`
	Y         int32  `json:"y" jsonschema:"y coordinate"`
	DX        int    `json:"dx,omitempty" jsonschema:"horizontal delta, ignored"`
	DY        int    `json:"dy" jsonschema:"vertical scroll delta"`
}

type displayDragIn struct {
	SessionID string `json:"session_id" jsonschema:"id of the live session"`
	X1        int32  `json:"x1" jsonschema:"start x"`
	Y1        int32  `json:"y1" jsonschema:"start y"`
	X2        int32  `json:"x2" jsonschema:"end x"`
	Y2        int32  `json:"y2" jsonschema:"end y"`
}

type appIDIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"id of the live session; required for apps_start and apps_stop"`
	ID        string `json:"id" jsonschema:"app id from apps_list"`
}

// --- sessions ---

func (a *Agent) mcpSessionAcquire(ctx context.Context, _ *mcp.CallToolRequest, in sessionAcquireIn) (*mcp.CallToolResult, any, error) {
	if in.Name == "" {
		return nil, nil, errors.New("name is required")
	}
	if in.Kind != "agent" && in.Kind != "user" {
		return nil, nil, errors.New("kind must be agent or user")
	}
	s, held := a.acquireSession(SessionHolder{Kind: in.Kind, Name: in.Name, RunID: in.RunID}, requestedLeaseTTL(in.TTLMs))
	if held != nil {
		return nil, nil, errors.New("session held by " + held.Holder.Name)
	}
	return nil, s, nil
}

func (a *Agent) mcpSessionKeepalive(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.keepaliveSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	return nil, s, nil
}

func (a *Agent) mcpSessionRelease(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	if !a.releaseSession(in.SessionID) {
		return nil, nil, errors.New("no such session")
	}
	return nil, map[string]any{"released": true}, nil
}

func (a *Agent) mcpSessionPause(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.pauseSession(in.SessionID, true)
	if err != nil {
		return nil, nil, err
	}
	return nil, s, nil
}

func (a *Agent) mcpSessionResume(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.pauseSession(in.SessionID, false)
	if err != nil {
		return nil, nil, err
	}
	return nil, s, nil
}

func (a *Agent) mcpSessionGet(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	s := a.sessionState()
	sc := a.screencast()
	viewers := 0
	if sc != nil {
		viewers = sc.Viewers()
	}
	return nil, map[string]any{
		"session":         s,
		"browser_running": sc != nil,
		"viewers":         viewers,
		"apps_running":    a.runningAppIDs(),
		"uptime_s":        int64(time.Since(a.startedAt).Seconds()),
	}, nil
}

func (a *Agent) mcpSessionEvents(ctx context.Context, _ *mcp.CallToolRequest, in sessionEventsIn) (*mcp.CallToolResult, any, error) {
	events := a.eventsSince(in.Since, in.Limit)
	a.events.mu.Lock()
	latest := a.events.latest
	a.events.mu.Unlock()
	return nil, map[string]any{"events": events, "latest_seq": latest}, nil
}

// --- health ---

func (a *Agent) mcpHealthGet(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	return nil, a.checkHealth(ctx), nil
}

// --- shell ---

func (a *Agent) mcpShellExec(ctx context.Context, _ *mcp.CallToolRequest, in shellExecIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	req := ShellExecRequest{
		Command: in.Command, Argv: in.Argv, Cwd: in.Cwd,
		TimeoutMs: in.TimeoutMs, Background: in.Background, Env: in.Env,
	}
	res, err := a.execShell(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	detail := shellDisplay(req) + " (status=" + res.Status
	if res.ExitCode != nil {
		detail += fmt.Sprintf(", exit=%d", *res.ExitCode)
	}
	if req.Background {
		detail += ", job=" + res.JobID
	}
	detail += ")"
	a.recordEvent("shell.exec", s.Holder.Name, detail)
	return nil, res, nil
}

func (a *Agent) mcpShellJobGet(ctx context.Context, _ *mcp.CallToolRequest, in shellJobIn) (*mcp.CallToolResult, any, error) {
	res, ok := a.shellJob(in.JobID)
	if !ok {
		return nil, nil, errors.New("unknown job")
	}
	return nil, res, nil
}

func (a *Agent) mcpShellJobKill(ctx context.Context, _ *mcp.CallToolRequest, in shellJobIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if !a.killShellJob(in.JobID) {
		return nil, nil, errors.New("unknown job")
	}
	// Wait briefly for the waiter goroutine to finalize the result so the
	// response reflects the kill.
	shellJobs.mu.Lock()
	j, ok := shellJobs.jobs[in.JobID]
	shellJobs.mu.Unlock()
	if ok {
		select {
		case <-j.doneCh:
		case <-time.After(2 * time.Second):
		}
	}
	res, _ := a.shellJob(in.JobID)
	a.recordEvent("shell.exec", s.Holder.Name, "kill job="+in.JobID+" (status="+res.Status+")")
	return nil, res, nil
}

func (a *Agent) mcpShellJobsList(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	return nil, map[string]any{"jobs": a.listShellJobs()}, nil
}

// --- files ---

func (a *Agent) mcpFilesList(ctx context.Context, _ *mcp.CallToolRequest, in filesListIn) (*mcp.CallToolResult, any, error) {
	entries, err := a.listFiles(in.Path)
	if err != nil {
		return nil, nil, err
	}
	if entries == nil {
		entries = []FileEntry{}
	}
	return nil, map[string]any{"path": in.Path, "entries": entries}, nil
}

func (a *Agent) mcpFilesRead(ctx context.Context, _ *mcp.CallToolRequest, in filesReadIn) (*mcp.CallToolResult, any, error) {
	if in.Path == "" {
		return nil, nil, errors.New("path is required")
	}
	data, err := a.readFile(in.Path)
	if err != nil {
		return nil, nil, err
	}
	if utf8.Valid(data) {
		return nil, map[string]any{"path": in.Path, "content": string(data), "encoding": "text"}, nil
	}
	return nil, map[string]any{
		"path":     in.Path,
		"content":  base64.StdEncoding.EncodeToString(data),
		"encoding": "base64",
		"note":     "content is not valid UTF-8; returned base64-encoded",
	}, nil
}

func (a *Agent) mcpFilesWrite(ctx context.Context, _ *mcp.CallToolRequest, in filesWriteIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if in.Path == "" {
		return nil, nil, errors.New("path is required")
	}
	var data []byte
	switch in.Encoding {
	case "", "text":
		data = []byte(in.Content)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			return nil, nil, errors.New("invalid base64 content: " + err.Error())
		}
		data = b
	default:
		return nil, nil, errors.New("encoding must be text or base64")
	}
	if err := a.writeFile(in.Path, data); err != nil {
		return nil, nil, err
	}
	a.recordEvent("files.write", s.Holder.Name, in.Path)
	abs, _ := a.jailPath(in.Path)
	return nil, map[string]any{"path": a.relHome(abs), "size": len(data)}, nil
}

func (a *Agent) mcpFilesDelete(ctx context.Context, _ *mcp.CallToolRequest, in filesDeleteIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if in.Path == "" {
		return nil, nil, errors.New("path is required")
	}
	if err := a.deletePath(in.Path, in.Recursive); err != nil {
		return nil, nil, err
	}
	a.recordEvent("files.delete", s.Holder.Name, in.Path)
	return nil, map[string]any{"path": in.Path}, nil
}

func (a *Agent) mcpFilesMkdir(ctx context.Context, _ *mcp.CallToolRequest, in filesPathIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if in.Path == "" {
		return nil, nil, errors.New("path is required")
	}
	if err := a.mkdirPath(in.Path); err != nil {
		return nil, nil, err
	}
	a.recordEvent("files.mkdir", s.Holder.Name, in.Path)
	abs, _ := a.jailPath(in.Path)
	return nil, map[string]any{"path": a.relHome(abs)}, nil
}

func (a *Agent) mcpFilesMove(ctx context.Context, _ *mcp.CallToolRequest, in filesMoveIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if in.From == "" || in.To == "" {
		return nil, nil, errors.New("from and to are required")
	}
	if err := a.movePath(in.From, in.To); err != nil {
		return nil, nil, err
	}
	a.recordEvent("files.move", s.Holder.Name, in.From+" -> "+in.To)
	fromAbs, _ := a.jailPath(in.From)
	toAbs, _ := a.jailPath(in.To)
	return nil, map[string]any{"from": a.relHome(fromAbs), "to": a.relHome(toAbs)}, nil
}

// --- browser ---

func (a *Agent) mcpBrowserStart(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := a.ensureBrowser(ctx); err != nil {
		return nil, nil, err
	}
	a.recordEvent("browser.start", s.Holder.Name, "")
	return nil, map[string]any{"running": true}, nil
}

func (a *Agent) mcpBrowserStop(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	a.stopBrowser()
	a.recordEvent("browser.stop", s.Holder.Name, "")
	return nil, map[string]any{"running": false}, nil
}

func (a *Agent) mcpBrowserNavigate(ctx context.Context, _ *mcp.CallToolRequest, in browserNavigateIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	sc, err := a.ensureBrowser(ctx)
	if err != nil {
		return nil, nil, err
	}
	target, err := NormalizeURL(in.URL)
	if err != nil {
		return nil, nil, err
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	if err := sc.Navigate(c, target); err != nil {
		return nil, nil, err
	}
	a.recordEvent("browser.navigate", s.Holder.Name, target)
	return nil, map[string]any{"url": target}, nil
}

func (a *Agent) mcpBrowserEvaluate(ctx context.Context, _ *mcp.CallToolRequest, in browserEvaluateIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	if in.Expression == "" {
		return nil, nil, errors.New("expression is required")
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	raw, err := sc.Evaluate(c, in.Expression)
	if err != nil {
		return nil, nil, err
	}
	val, err := remoteValue(raw)
	if err != nil {
		return nil, nil, err
	}
	var value any
	if err := json.Unmarshal(val, &value); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"value": value}, nil
}

func (a *Agent) mcpBrowserAct(ctx context.Context, _ *mcp.CallToolRequest, in browserActIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	body := browserActRequest{
		Kind: in.Kind, Selector: in.Selector, Text: in.Text,
		X: in.X, Y: in.Y, DX: in.DX, DY: in.DY,
		Button: in.Button, Clicks: in.Clicks, Key: in.Key,
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	var actErr error
	switch body.Kind {
	case "click":
		actErr = browserActClick(c, sc, body)
	case "type":
		actErr = browserActType(c, sc, body)
	case "select":
		actErr = browserActSelect(c, sc, body)
	case "scroll":
		actErr = browserActScroll(c, sc, body)
	case "key":
		actErr = browserActKey(c, sc, body)
	default:
		return nil, nil, errors.New("unknown act kind " + strconv.Quote(body.Kind))
	}
	if actErr != nil {
		return nil, nil, actErr
	}
	a.recordEvent("browser.act", s.Holder.Name, actDetail(body))
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpBrowserText(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	text, err := sc.PageText(c)
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"text": text}, nil
}

func (a *Agent) mcpBrowserState(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	sc := a.screencast()
	if sc == nil {
		return nil, map[string]any{"running": false}, nil
	}
	st := sc.Nav()
	return nil, map[string]any{
		"running": true,
		"url":     st.URL, "title": st.Title,
		"can_back": st.CanGoBack, "can_forward": st.CanGoForward,
		"seq": st.Seq,
	}, nil
}

func (a *Agent) mcpBrowserNavAction(kind string) func(context.Context, *mcp.CallToolRequest, sessionIDIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
		if _, err := a.mcpSession(in.SessionID); err != nil {
			return nil, nil, err
		}
		sc, err := a.mcpScreencast()
		if err != nil {
			return nil, nil, err
		}
		c, cancel := context.WithTimeout(ctx, browserCallTimeout)
		defer cancel()
		var navErr error
		switch kind {
		case "back":
			navErr = sc.GoBack(c)
		case "forward":
			navErr = sc.GoForward(c)
		case "reload":
			navErr = sc.Reload(c)
		}
		if navErr != nil {
			return nil, nil, navErr
		}
		return nil, map[string]any{"ok": true}, nil
	}
}

func (a *Agent) mcpBrowserBack(ctx context.Context, req *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	return a.mcpBrowserNavAction("back")(ctx, req, in)
}

func (a *Agent) mcpBrowserForward(ctx context.Context, req *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	return a.mcpBrowserNavAction("forward")(ctx, req, in)
}

func (a *Agent) mcpBrowserReload(ctx context.Context, req *mcp.CallToolRequest, in sessionIDIn) (*mcp.CallToolResult, any, error) {
	return a.mcpBrowserNavAction("reload")(ctx, req, in)
}

func (a *Agent) mcpBrowserInput(ctx context.Context, _ *mcp.CallToolRequest, in browserInputIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	switch in.Type {
	case "pointer":
		action := in.Action
		if action == "" {
			// A bare pointer event is a click: press then release.
			press := cdp.MouseEvent{Action: "press", X: in.X, Y: in.Y, Button: in.Button, Buttons: mouseButtonsMask(in.Button), Clicks: in.Clicks, Mods: in.Mods}
			if in.Clicks == 0 {
				press.Clicks = 1
			}
			if err := sc.Mouse(c, press); err != nil {
				return nil, nil, err
			}
			press.Action = "release"
			press.Buttons = 0
			if err := sc.Mouse(c, press); err != nil {
				return nil, nil, err
			}
		} else {
			ev := cdp.MouseEvent{Action: action, X: in.X, Y: in.Y, Button: in.Button, Clicks: in.Clicks, Mods: in.Mods}
			if action == "press" {
				ev.Buttons = mouseButtonsMask(in.Button)
			}
			if err := sc.Mouse(c, ev); err != nil {
				return nil, nil, err
			}
		}
	case "key":
		if in.Key == "" {
			return nil, nil, errors.New("key is required for key input")
		}
		ev := cdp.KeyEvent{Key: in.Key, Code: in.Code, Text: in.Text, Mods: in.Mods}
		if ev.Code == "" {
			ev.Code = keyCodeFor(in.Key)
		}
		if len(in.Key) == 1 && ev.Text == "" {
			ev.Text = in.Key
		}
		if in.Action == "" || in.Action == "down" {
			ev.Action = "down"
			if err := sc.Key(c, ev); err != nil {
				return nil, nil, err
			}
		}
		if in.Action == "" || in.Action == "up" {
			ev.Action = "up"
			if err := sc.Key(c, ev); err != nil {
				return nil, nil, err
			}
		}
	case "scroll":
		if err := sc.Scroll(c, in.X, in.Y, in.DX, in.DY, in.Mods); err != nil {
			return nil, nil, err
		}
	case "paste":
		if err := sc.InsertText(c, in.Text); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, errors.New("unknown input type " + strconv.Quote(in.Type))
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpBrowserSize(ctx context.Context, _ *mcp.CallToolRequest, in browserSizeIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	if err := sc.SetViewport(c, in.W, in.H, in.Scale); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpBrowserFrame(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	sc, err := a.mcpScreencast()
	if err != nil {
		return nil, nil, err
	}
	c, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	img, err := sc.Capture(c, "png")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: img, MIMEType: "image/png"}}}, nil, nil
}

// --- display ---

func (a *Agent) mcpDisplayScreenshot(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	png, err := bw.ScreenshotPNG()
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil, nil
}

func (a *Agent) mcpDisplayClick(ctx context.Context, _ *mcp.CallToolRequest, in displayClickIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	button := in.Button
	if button == 0 {
		button = 1
	}
	if button < 1 || button > 3 {
		return nil, nil, errors.New("button must be 1, 2 or 3")
	}
	if err := bw.ClickButton(in.X, in.Y, button); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpDisplayType(ctx context.Context, _ *mcp.CallToolRequest, in displayTypeIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	if err := bw.TypeText(in.Text); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpDisplayKey(ctx context.Context, _ *mcp.CallToolRequest, in displayKeyIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	ks, ok := broadway.KeysymForName(in.Key)
	if !ok {
		return nil, nil, errors.New("unknown key " + strconv.Quote(in.Key))
	}
	if err := bw.Key(ks, in.Mods); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpDisplayScroll(ctx context.Context, _ *mcp.CallToolRequest, in displayScrollIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	if err := bw.ScrollAt(in.X, in.Y, in.DX, in.DY); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpDisplayDrag(ctx context.Context, _ *mcp.CallToolRequest, in displayDragIn) (*mcp.CallToolResult, any, error) {
	if _, err := a.mcpSession(in.SessionID); err != nil {
		return nil, nil, err
	}
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	if err := bw.Drag(in.X1, in.Y1, in.X2, in.Y2); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"ok": true}, nil
}

func (a *Agent) mcpDisplayMeta(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	bw, err := a.mcpBroadway()
	if err != nil {
		return nil, nil, err
	}
	return nil, bw.DebugMeta(), nil
}

// --- apps ---

func (a *Agent) mcpAppsList(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
	return nil, map[string]any{"apps": a.mcpApps()}, nil
}

func (a *Agent) mcpAppsStart(ctx context.Context, _ *mcp.CallToolRequest, in appIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	app, ok := apps.Find(a.loadApps(), in.ID)
	if !ok {
		return nil, nil, errors.New("unknown app")
	}
	if app.Port == 0 {
		return nil, map[string]any{"id": in.ID, "running": a.screencast() != nil}, nil
	}
	if err := a.startApp(app); err != nil {
		return nil, nil, err
	}
	a.recordEvent("apps.start", s.Holder.Name, in.ID)
	return nil, map[string]any{"id": in.ID, "running": true}, nil
}

func (a *Agent) mcpAppsStop(ctx context.Context, _ *mcp.CallToolRequest, in appIDIn) (*mcp.CallToolResult, any, error) {
	s, err := a.mcpSession(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	app, ok := apps.Find(a.loadApps(), in.ID)
	if !ok {
		return nil, nil, errors.New("unknown app")
	}
	if app.Port == 0 {
		a.stopBrowser()
	} else if err := a.stopApp(in.ID); err != nil {
		return nil, nil, err
	}
	a.recordEvent("apps.stop", s.Holder.Name, in.ID)
	return nil, map[string]any{"id": in.ID, "running": false}, nil
}
