# MCP server

`dad` exposes the same computer surface as an MCP server at `POST /mcp`, so an agent host (any MCP client) can drive the computer without speaking raw HTTP. The MCP tools call the same service methods the `/v1` HTTP handlers call — never HTTP to self — so the two surfaces cannot drift apart.

## Transport

- **Endpoint:** `POST /mcp`, behind the same token auth as the rest of the API (`X-Dad-Token` header). `GET /mcp` returns `405`.
- **Protocol:** Streamable HTTP, stateless mode, plain JSON-RPC responses (no SSE, no `Mcp-Session-Id` dance). Each POST is an independent request: `initialize`, then `tools/list` / `tools/call` as usual.
- **Server:** name `docker-agent-desktop`, version `1` (matching `/v1/meta`'s `api_version`).
- **Library:** the official Go SDK, `github.com/modelcontextprotocol/go-sdk v1.8.0`.
- **Errors:** tool failures are `isError` tool results (not protocol errors), with the same message text as the HTTP errors (`no session held`, `session held by <name>`, `session paused`, `selector not found`, …).

## Sessions

The MCP tools enforce the identical session rule as the HTTP API (see [sessions.md](sessions.md)), in-process: every mutating tool takes a `session_id` argument, which must be a live, unpaused session. Reads never require one, but they accept an optional `session_id` (ignored), so a client can pass it uniformly to every tool.

## Tools (43)

| Tool | Session | Description |
|------|---------|-------------|
| `session_acquire` | no | Acquire the lease. Args: `kind` (`agent`/`user`), `name`, `run_id`, `ttl_ms` (clamped 10 s..10 min, default 90 s). |
| `session_keepalive` | no | Extend the lease by its TTL. |
| `session_release` | no | End the session. |
| `session_pause` | no | Pause: keeps the lease, blocks mutating calls. |
| `session_resume` | no | Resume a paused session. |
| `session_wait` | no | Wait until the lease is free or the timeout elapses. Never acquires. Args: `timeout_ms` (optional, default 30 s, clamp 1 s..5 min). |
| `session_takeover` | no | Force-acquire the lease, evicting whoever holds it. Args: `kind`, `name`, `run_id`, `ttl_ms`, `force` (required true). The evicted holder is recorded in the activity log. |
| `session_get` | no | Live session (or null) + browser/viewers/apps/uptime. |
| `session_events` | no | Activity log. Args: `since`, `limit`. |
| `health_get` | no | Real session health (Chromium, Broadway, disk, session). |
| `shell_exec` | yes | Run a command (`command` via `/bin/sh -c`, or `argv` direct) or a background job. Args: `cwd`, `timeout_ms`, `env`, `background`. |
| `shell_job_get` | no | One job by id. |
| `shell_job_kill` | yes | SIGKILL a job's process group. |
| `shell_jobs_list` | no | All tracked jobs. |
| `files_list` | no | Directory listing under home. |
| `files_read` | no | File content as text; non-UTF-8 as base64 with a note. 10 MiB cap. |
| `files_write` | yes | Write text or base64 content. Also the upload path: pass base64. |
| `files_delete` | yes | Delete a file or (recursive) directory. |
| `files_mkdir` | yes | Create a directory. |
| `files_move` | yes | Rename/move. Never overwrites. |
| `browser_start` | yes | Start Chromium. |
| `browser_stop` | yes | Stop Chromium. |
| `browser_navigate` | yes | Navigate to a URL. |
| `browser_evaluate` | yes | Run one JS expression, get its value as JSON. |
| `browser_act` | yes | One action: `click`/`type`/`select`/`scroll`/`key`, by selector or coordinates. |
| `browser_text` | no | The page's visible text. |
| `browser_state` | no | URL, title, back/forward, change counter. `running:false` when Chromium is down. |
| `browser_back` | yes | History back. |
| `browser_forward` | yes | History forward. |
| `browser_reload` | yes | Reload the page. |
| `browser_input` | yes | Raw CDP input: pointer/key/scroll/paste. |
| `browser_size` | yes | Resize the viewport. |
| `browser_frame` | no | Fresh screenshot of the page as an MCP image (PNG). |
| `display_screenshot` | no | PNG of the terminal display as an MCP image. |
| `display_click` | yes | Click at coordinates (button 1/2/3). |
| `display_type` | yes | Type text into the focused window. |
| `display_key` | yes | Press a key by name, with optional GDK modifier mask. |
| `display_scroll` | yes | Vertical scroll at coordinates. |
| `display_drag` | yes | Left-button drag. |
| `display_meta` | no | Display surface metadata. |
| `apps_list` | no | Registry apps with running state (`exec` never exposed). |
| `apps_start` | yes | Start an app by id (browser included). |
| `apps_stop` | yes | Stop an app by id; stopping the browser stops Chromium. |

There is no `files_upload` tool: uploads are `files_write` with base64 content. There is no streaming tool: `browser_frame` is the MCP way to see the screen (poll it for fresh frames; the HTTP `GET /v1/browser/frame?since=N` equivalent is a polling client). The live socket is an HTTP/WebSocket surface (`GET /v1/browser/stream`), not an MCP one, by design. Display input records no activity-log events (too noisy); everything else records the same events as its HTTP counterpart.

## Example: driving it from Python

```python
import json, urllib.request

BASE = "http://localhost:8080"
TOKEN = "dev-token"

def rpc(method, params=None, id=1):
    body = json.dumps({"jsonrpc": "2.0", "id": id, "method": method, "params": params or {}}).encode()
    req = urllib.request.Request(BASE + "/mcp", data=body, headers={
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "X-Dad-Token": TOKEN,
    })
    return json.load(urllib.request.urlopen(req))

rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                   "clientInfo": {"name": "example", "version": "0"}})
rpc("notifications/initialized", id=None)  # notification: no id, no response body to parse
acq = rpc("tools/call", {"name": "session_acquire",
    "arguments": {"kind": "agent", "name": "example-run", "ttl_ms": 90000}}, id=2)
sid = json.loads(acq["result"]["content"][0]["text"])["id"]
out = rpc("tools/call", {"name": "shell_exec",
    "arguments": {"session_id": sid, "command": "echo hello from mcp"}}, id=3)
print(out["result"]["content"][0]["text"])
rpc("tools/call", {"name": "session_release", "arguments": {"session_id": sid}}, id=4)
```

(Notifications such as `notifications/initialized` carry no `id` and get no response — send them fire-and-forget, or skip them; the stateless handler does not require them.)

## What an MCP client needs

- Point its MCP client at `POST /mcp` with the agent token, acquire one session per run, keepalive every ~15 s, and release on run end.
- The computer is single-session: a second run gets `session held by <name>` and must wait (`session_wait`, or poll `session_get`) or take the lease over explicitly (`session_takeover` with `force: true`, which evicts the holder and is recorded in the activity log).
- Everything the agent does is visible on the screencast (`browser_frame` / `display_screenshot`, or the desktop's live stream), so the user can watch and pause the session from the desktop when they want to take over.
