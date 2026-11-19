# HTTP API

All routes are served by `dc-agent` on port 8080 inside the container. Publish that port to reach it from the host.

## Authentication

Send the token from `COMPUTER_AGENT_TOKEN` in the `X-Computer-Token` header. Browser sessions use the `dc_token` cookie set by the portal (see [portal.md](portal.md)). Requests with a missing or wrong token get `401 Unauthorized`. Comparison is constant-time.

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/healthz` | no | Liveness. Returns `200` when the agent is up. |
| GET | `/v1/health` | yes | Authenticated health check. |
| GET | `/v1/meta` | no | API version and build phase. |
| GET | `/api/display/screenshot?wait=1` | yes | PNG of the terminal display (the first GTK app). `wait=1` waits up to 15 s for the first frame. |
| POST | `/api/display/click` | yes | Body `{"x":80,"y":760}`. Left click in the terminal display. |
| POST | `/api/display/type` | yes | Body `{"text":"hello"}`. Types ASCII text into the terminal display. |
| GET | `/api/browser/frame` | yes | Latest Chromium screencast frame as JPEG. `?since=N` returns `204` when no newer frame exists. The `X-Frame-Seq` header carries the frame number. `503` if Chromium is not running. |
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

The `/api/` endpoints are the desktop's control surface. Their shape may change before a stable `/v1` API is published.

## Examples

```bash
TOKEN=dev-token
curl -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/display/screenshot?wait=1" -o desktop.png
curl -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/apps"   # registry with ports
curl -X POST -H "X-Computer-Token: $TOKEN" "http://localhost:8080/api/apps/start?id=terminal"
curl -X POST -H "X-Computer-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"url":"example.com"}' "http://localhost:8080/api/browser/navigate"
```
