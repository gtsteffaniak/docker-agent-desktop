# HTTP API

All routes are served by `dc-agent` on port 8080 inside the container. Publish that port to reach it from the host.

## Authentication

Send the token from `COMPUTER_AGENT_TOKEN` in the `X-Computer-Token` header. Browser sessions use the `dc_token` cookie set by the portal (see [portal.md](portal.md)). Requests with a missing or wrong token get `401 Unauthorized`. Comparison is constant-time.

## The stable `/v1` API

`/v1` is the surface agents drive. The old `/api/*` routes stay exactly as they are (the web desktop uses them); `/v1` mirrors them where an endpoint already exists and adds the new ones. `GET /v1/meta` reports `{"api_version":"1"}`.

### Sessions

At most one live session may drive the computer at a time. A caller acquires a session, keeps it alive while working, and releases it when done. Mutating `/v1` endpoints require the `X-Dc-Session` header of a live, unpaused session; reads never do. The full contract — lease, keepalive, pause, takeover/wait, events — is in [sessions.md](sessions.md).

A mutating call without a valid session gets `409 Conflict` with a JSON body naming the current holder when there is one:

```json
{"error": "session held by agent-run-7", "holder": {"kind": "agent", "name": "agent-run-7"}, "expires_at": "…", "paused": false}
```

### Health

- `GET /healthz` (no auth) is container liveness only: a bare `200` when the process is up. It says nothing about whether the session is usable, and it is intentionally not changed.
- `GET /v1/health` (auth) is the real session health check: it probes Chromium (via a live CDP call; `disabled` when `DC_BROWSER_START=off`, and `stopped` — not a failure — when the browser simply is not running because it has not been needed yet, was stopped, or was shut down idle; only a browser that should be running but has no working CDP connection is `down`), the Broadway display, free disk on the home volume, and the live session. Overall `status` is `ok` or `degraded`, with a per-check breakdown in `checks`.

### Endpoints

| Method | Path | Session | Description |
|--------|------|---------|-------------|
| GET | `/healthz` | — | Liveness. Bare `200` when the agent is up. |
| GET | `/v1/health` | no | Real session health (Chromium, Broadway, disk, session). |
| GET | `/v1/meta` | no | `{"api_version":"1"}`. |
| POST | `/v1/session/acquire` | no | Body `{"holder":{"kind":"agent","name":"…","run_id":"…"},"ttl_ms":90000}`. `201` with the session, or `409` naming the holder. |
| POST | `/v1/session/keepalive` | no | Body `{"session_id":"…"}`. Extends the lease by its TTL. |
| POST | `/v1/session/release` | no | Body `{"session_id":"…"}`. Ends the session. |
| POST | `/v1/session/pause` | no | Body `{"session_id":"…"}`. Paused sessions keep the lease but mutating calls with them get `409`. |
| POST | `/v1/session/resume` | no | Body `{"session_id":"…"}`. |
| GET | `/v1/session` | no | The live session (or `null`), browser/viewer/app state, uptime. |
| GET | `/v1/session/events?since=N&limit=100` | no | Activity log: sessions, shell/files/apps/browser actions, health transitions. `latest_seq` for polling. |
| POST | `/v1/shell/exec` | yes | Body `{"command":"…"} ` or `{"argv":["…"]}`, optional `cwd` (jailed under home), `timeout_ms` (default 30000, clamp 1000–300000), `env`, `background`. Foreground returns the finished result (`status`, `exit_code`, `stdout`, `stderr`, capped at 1 MiB each); `background:true` returns `{job_id, status:"running"}`. Non-zero exit is `status:"failed"`, not an HTTP error. A job runs in its own process group, and the group dies with its main process: anything it left running (for example `cmd &`) is killed when the main process exits, and a child holding stdout open cannot delay the result by more than 2 s. Daemons that must outlive the call leave the group themselves (`setsid`). At most 32 jobs run at once (`429` beyond that); completed jobs keep at most 16 MiB of output in total, oldest dropped first. |
| GET | `/v1/shell/jobs` | no | All tracked jobs, oldest first. |
| GET | `/v1/shell/jobs/{id}` | no | One job. While it runs, `stdout`/`stderr` hold the output so far. `404` when unknown. |
| POST | `/v1/shell/jobs/{id}/kill` | yes | SIGKILLs the job's whole process group. |
| GET | `/v1/files/list?path=` | no | Directory listing. Paths are home-relative; entries carry `name`, `path`, `type`, `size`, `mod_time`. |
| GET | `/v1/files/read?path=` | no | Raw file bytes (10 MiB cap, `413` over). `?download=1` sets an attachment disposition. |
| POST | `/v1/files/write` | yes | Body `{"path":"…","content":"…","encoding":"text"\|"base64"}`. Creates parent dirs. Content over 10 MiB is `413`. |
| POST | `/v1/files/upload` | yes | Multipart `file` + `path`. A trailing slash or existing directory means "into this directory". 10 MiB cap. |
| POST | `/v1/files/mkdir` | yes | Body `{"path":"…"}`. |
| POST | `/v1/files/move` | yes | Body `{"from":"…","to":"…"}`. Never overwrites an existing destination. A symlink is moved as the link, not its target. |
| DELETE | `/v1/files?path=&recursive=1` | yes | Deletes a file, or a directory with `recursive=1`. Home itself can never be deleted (`400`). A symlink is deleted as the link; its target is never touched. |
| POST | `/v1/browser/start` | yes | Starts Chromium on first use. `503` when `DC_BROWSER_START=off`. |
| POST | `/v1/browser/stop` | yes | Stops Chromium. The next navigation starts it again. |
| POST | `/v1/browser/evaluate` | yes | Body `{"expression":"…"}` (64 KiB cap). Runs one JS expression (promises are awaited) and returns its own JSON value: `{"value": 2}` for `1+1`; `undefined` and values with no JSON form are `null`. A thrown exception is `502` with its message. |
| POST | `/v1/browser/act` | yes | One high-level action: `{"kind":"click"\|"type"\|"select"\|"scroll"\|"key", "selector":"…", …}`. Click/type/scroll take a `selector` (centre of the first match) or `x`/`y` coordinates. Unknown selector → `404 {"error":"selector not found"}`. Success → `204`. |
| GET | `/v1/browser/text` | no | The page's visible text. |
| GET | `/v1/browser/state` | no | The page's real `url`, `title`, `can_back`, `can_forward`, `seq`. |
| POST | `/v1/browser/navigate` | yes | Body `{"url":"…"}`. Same rules as the `/api` version. |
| POST | `/v1/browser/back`, `/forward`, `/reload` | yes | Moves through real navigation history. |
| POST | `/v1/browser/input` | yes | Body `{"type":"pointer"\|"key"\|"scroll"\|"paste", …}`. Raw CDP input, as `/api`. |
| POST | `/v1/browser/size` | yes | Body `{"w":918,"h":498}`. Resizes the viewport. |
| GET | `/v1/display/screenshot` | no | PNG of the terminal display (the first GTK app). Same as `/api`. |
| GET | `/v1/display/meta` | no | Surface metadata for debugging. |
| POST | `/v1/display/click` | yes | Body `{"x":80,"y":760,"button":1}`. Button 1 left, 2 middle, 3 right. |
| POST | `/v1/display/type` | yes | Body `{"text":"hello"}`. Types into the focused window. |
| POST | `/v1/display/key` | yes | Body `{"key":"Return","mods":0}`. Key names: Return/Enter, Tab, Escape, BackSpace, Delete, arrows, Home/End/PageUp/PageDown, Space, F1–F12, single characters. Mods are GDK masks (Shift 1, Control 4, Alt 8). |
| POST | `/v1/display/scroll` | yes | Body `{"x":…,"y":…,"dy":…}`. Vertical only; `dx` is ignored. |
| POST | `/v1/display/drag` | yes | Body `{"x1":…,"y1":…,"x2":…,"y2":…}`. Left-button drag. |
| GET | `/v1/apps` | no | The registry with each app's `running` state. `exec` is deliberately not exposed. |
| POST | `/v1/apps/start?id=` | yes | Starts a registered app (browser included, port 0). Unknown id → `400`. |
| POST | `/v1/apps/stop?id=` | yes | SIGTERM, 5 s grace, then SIGKILL. The app's broadwayd display stays running. Stopping the browser (port 0) stops Chromium. |

Files are jailed under the agent user's home (`$HOME`, default `/home/bot`): `..` escapes, absolute paths outside home, and symlinked escapes are all rejected with `400`. Reads are capped at 10 MiB (`413` over). Display input is deliberately not recorded in the activity log — it is far too noisy; shell, file, app and browser actions are.

Everything the agent does is visible on the screencast: browser actions drive the real Chromium window, display input drives the real GTK apps, and shell/file work happens in the same home directory the desktop shows.

## The `/api/*` endpoints (unchanged)

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/api/display/screenshot?wait=1` | yes | PNG of the terminal display (the first GTK app). `wait=1` waits up to 15 s for the first frame. |
| POST | `/api/display/click` | yes | Body `{"x":80,"y":760}`. Left click in the terminal display. |
| POST | `/api/display/type` | yes | Body `{"text":"hello"}`. Types ASCII text into the terminal display. |
| GET | `/api/browser/frame` | yes | A fresh screenshot of the page, taken on demand. JPEG by default; `?format=png` returns a lossless PNG, which is easier to read text from. With `?since=N` it serves the screencast for polling clients instead: each poll keeps the stream running for 5 s, the first poll after the stream was idle waits up to 3 s for a fresh frame, and `204` means no frame newer than `N` exists (JPEG only). The `X-Frame-Seq` header carries the latest screencast frame number. `503` if Chromium is not running. |
| GET | `/api/browser/state` | yes | The page's real `url`, `title`, `can_back`, `can_forward` and a change counter `seq`. `?fmt=lines` returns plain text. |
| POST | `/api/browser/navigate` | yes | Body `{"url":"example.com"}`. Adds `https://` (or `http://` for localhost and IP addresses). Only http, https and about:blank are allowed. |
| POST | `/api/browser/back`, `/forward`, `/reload` | yes | Moves through real navigation history. |
| POST | `/api/browser/input` | yes | Body `{"type":"pointer"|"key"|"scroll"|"paste", ...}`. Forwards input to the page through CDP. |
| POST | `/api/browser/size` | yes | Body `{"w":918,"h":498}`. Resizes the page viewport to the browser window. |
| GET | `/api/apps` | yes | The app registry, in dock order, with each GTK app's Broadway `port` (0 for the browser). |
| POST | `/api/apps/start?id=` | yes | Starts a registered GTK app and its Broadway display. A running app is left alone. Unregistered IDs return `400`. |
| GET | `/api/display/meta` | yes | Surface metadata for debugging. |
| GET | `/desktop/` | cookie or header | The web desktop (see [portal.md](portal.md)). |
| GET | `/apps/{id}/` | cookie or header | One GTK app's Broadway HTML5 client. |
| GET | `/apps/{id}/socket` | cookie or header | WebSocket relay to that app's broadwayd. |

The `/api/` endpoints are the desktop's control surface. Agents should use `/v1` (or [MCP](mcp.md)) instead.

## Examples

```bash
TOKEN=dev-token
curl -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/display/screenshot?wait=1" -o desktop.png
curl -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/apps"   # registry with ports
curl -X POST -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/apps/start?id=terminal"

# Agent flow: acquire a session, then act with it.
SID=$(curl -s -X POST -H "X-Computer-Token: $TOKEN" -d '{"holder":{"kind":"agent","name":"my-run"}}' \
  http://localhost:8080/v1/session/acquire | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
curl -H "X-Computer-Token: $TOKEN" -H "X-Dc-Session: $SID" \
  -d '{"command":"ls ~"}' http://localhost:8080/v1/shell/exec
curl -X POST -H "X-Computer-Token: $TOKEN" -d "{\"session_id\":\"$SID\"}" \
  http://localhost:8080/v1/session/keepalive
curl -X POST -H "X-Computer-Token: $TOKEN" -d "{\"session_id\":\"$SID\"}" \
  http://localhost:8080/v1/session/release
```
