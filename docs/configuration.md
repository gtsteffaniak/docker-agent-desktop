# Configuration

Set these as environment variables on `docker run` or in `make dev`.

## Agent and desktop

| Variable | Default | Purpose |
|----------|---------|---------|
| `DAD_TOKEN` | none (required) | API and portal token. The container exits if it is unset. The deprecated name `COMPUTER_AGENT_TOKEN` is still read when `DAD_TOKEN` is unset (with a warning). |
| `DAD_ADDR` | `0.0.0.0:8080` | Address the agent listens on inside the container. |
| `DESKTOP_WIDTH` / `DESKTOP_HEIGHT` | `1280` / `800` | Size of the first GTK display (the terminal). |
| `BROADWAY_DISPLAY` | `:1` | Display name of the first broadwayd. |
| `BROADWAY_PORT` | `8081` | Port of the first broadwayd. Not published. Other apps use the port in their `apps.d` entry. |
| `DAD_BROWSER_START` | `lazy` | `lazy` starts Chromium the first time the browser window needs it. `boot` starts it with the agent. `off` disables the browser. |
| `DAD_BROWSER_IDLE_MINUTES` | `30` | Stop Chromium after this many minutes with no command or input from a client. The next navigation starts it again. `0` disables the idle stop. |
| `DAD_AUTOSTART_APPS` | `0` | `1` starts every GTK app at boot. Otherwise each app starts when its window opens. |
| `DAD_IMAGE` | `docker-agent-desktop:dev` | Image the e2e and budget scripts test. |
| `BUDGET_BOOT_SECONDS` | `3` | Ceiling for `MEASURED budget_boot_to_healthy_seconds` in `scripts/budgets.sh` (integer). Also logs `budget_boot_docker_run_seconds` and `budget_boot_healthz_wait_seconds`. |
| `DAD_DESKTOP_DIR` | unset | Serve the desktop from this directory instead of the embedded copy. For development. |

## Apps

| Variable | Default | Purpose |
|----------|---------|---------|
| `DAD_APPS_DIRS` | `/etc/dad/apps.d:~/.config/dad/apps.d` | Colon-separated app registry directories. Later ones override earlier ones by id. |

Each `apps.d` entry sets `id`, `name`, `exec`, `icon`, `order` and `port`. See [desktop.md](desktop.md).

## Logging

| Variable | Default | Purpose |
|----------|---------|---------|
| `DAD_LOG_BACKEND` | `gologger` | `gologger` uses [go-logger](https://github.com/gtsteffaniak/go-logger). `slog` uses the standard library handlers. |
| `DAD_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DAD_LOG_JSON` | `0` | `1` writes JSON lines. |
| `DAD_LOG_NO_COLOR` | `0` | `1` disables colour in text output. |

Request logs record method, path, status and duration. Headers, query strings and cookies are never logged, so tokens stay out of the log.

## Renamed from docker-computer

The project was `docker-computer` and its agent `dc-agent`. Every name moved to the `dad` /
`DAD_` prefix:

| Old | New |
|-----|-----|
| `github.com/gtsteffaniak/docker-computer` (Go module, repo) | `github.com/gtsteffaniak/docker-agent-desktop` |
| image `docker-computer`, `ghcr.io/gtsteffaniak/docker-computer` | `docker-agent-desktop`, `ghcr.io/gtsteffaniak/docker-agent-desktop` |
| `dc-agent`, `cmd/dc-agent` | `dad`, `cmd/dad` |
| `dc-playwright`, `dc-apt`, `dc-windows` (GTK module) | `dad-playwright`, `dad-apt`, `dad-windows` |
| `DC_*` environment variables (`DC_AGENT_ADDR` became `DAD_ADDR`) | `DAD_*` |
| `COMPUTER_AGENT_TOKEN` | `DAD_TOKEN` |
| `X-Computer-Token`, `X-Dc-Session` headers | `X-Dad-Token`, `X-Dad-Session` |
| `dc_token` portal cookie | `dad_token` |
| `/etc/dc/apps.d`, `~/.config/dc/apps.d`, `~/.dc/logs` | `/etc/dad/apps.d`, `~/.config/dad/apps.d`, `~/.dad/logs` |
| MCP server name `docker-computer` | `docker-agent-desktop` |

Deprecated aliases, accepted until existing clients switch: `COMPUTER_AGENT_TOKEN`,
`X-Computer-Token` and `X-Dc-Session`. The new name wins when both are set. No other old name is
accepted. Portal users sign in again once (the cookie name changed), and per-user app overrides
move from `~/.config/dc/apps.d` to `~/.config/dad/apps.d`.
