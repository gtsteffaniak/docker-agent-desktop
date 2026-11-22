# Architecture

```
browser ──► dad :8080 ──► /desktop/      the web desktop (dock, frames, browser toolbar)
                 │
                 ├──► /apps/{id}/ ──► broadwayd :8081..8083 ──► GTK apps (terminal, files, editor)
                 │
                 └──► Chromium (CDP :9222, loopback) ──► screencast frames, navigation, input
                           ▲
       dad-playwright ──────┘  same browser over connectOverCDP: drive, record, trace, replay
```

Every window is drawn by the web desktop. A GTK app renders into its own `broadwayd` display, and the desktop shows that display in an iframe. The browser window shows Chromium's screencast on a canvas.

## Components

| Component | Role |
|-----------|------|
| `cmd/dad` | The only published process. Serves the desktop, proxies each app's Broadway display, and runs the HTTP API and the browser endpoints. |
| `cmd/dad/session.go` | The single live session lease: acquire/keepalive/release/pause/resume, the `X-Dad-Session` gate for mutating `/v1` calls, and the 5 s reaper (see [sessions.md](sessions.md)). |
| `cmd/dad/events.go` | The in-memory activity log (ring, cap 500) behind `GET /v1/session/events`. |
| `cmd/dad/health.go` | Real session health for `GET /v1/health` (Chromium, Broadway, disk, session). `/healthz` stays a bare 200 for container liveness. |
| `cmd/dad/shell.go` | Shell exec service: foreground/background jobs, process groups, capped output. |
| `cmd/dad/files.go` | File service jailed under the agent user's home. |
| `cmd/dad/browser_v1.go`, `display_v1.go`, `apps_v1.go` | The `/v1` browser, display and app surfaces. |
| `cmd/dad/mcp.go` | The MCP server at `POST /mcp`, wrapping the same service methods (see [mcp.md](mcp.md)). |
| `cmd/dad/desktop` | The web desktop: a window manager, the dock, and the browser window, in plain HTML, CSS and JavaScript. Embedded in the agent. |
| `cmd/dad/appproc.go` | Starts each app's `broadwayd` display and the app, on demand or at boot. |
| `cmd/dad-windows` | GTK module loaded into every app. Keeps printable keys out of menubar mnemonics, strips the app's decorations, and maximizes it. |
| `broadwayd` | Broadway display server, one per GTK app, on the port in its `apps.d` entry. |
| `internal/apps` | Parses the `apps.d` registry: id, name, icon, order, port and exec. |
| `internal/broadway` | Broadway protocol client. Used for the display screenshot and input endpoints. |
| `internal/cdp` | DevTools client for the Chromium screencast (started on the first viewer, stopped after the last), on-demand screenshots, navigation state, history and input. |
| `dad-playwright` (`docker/rootfs/usr/local/lib/dad-playwright`) | Node CLI on `playwright-core`. Attaches to the running Chromium over CDP to drive it, record actions and a trace into `~/recordings`, and replay recordings (see [playwright.md](playwright.md)). |
| `dad-apt` (`docker/rootfs/usr/local/bin/dad-apt`) | Rootless package tier: unpacks Debian packages under `/home/bot` and runs them under PRoot (see [image.md](image.md#installing-software-without-root-dad-apt)). |
| `docker/rootfs/usr/local/bin/entrypoint.sh` | Starts the first broadwayd (terminal's display) in the background, launches a D-Bus session, then execs `dad` (does not wait on broadwayd; dad reconnects). |
| `tini` | PID 1. Reaps children and forwards signals to the process group. |

## Notes

- Each GTK app has its own display: terminal on `:1` (port 8081), files on `:2` (8082) and the editor on `:3` (8083). The display number is the port minus 8080.
- The agent starts a display when its app first opens, or all of them at boot with `DAD_AUTOSTART_APPS=1`.
- Apps run with `GTK_CSD=0` and no decorations, so the desktop's frame is the only frame.
- The browser window's address bar follows the real URL from Chromium's navigation history.
