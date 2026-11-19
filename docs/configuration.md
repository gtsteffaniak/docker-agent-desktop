# Configuration

Set these as environment variables on `docker run` or in `make dev`.

## Agent and desktop

| Variable | Default | Purpose |
|----------|---------|---------|
| `COMPUTER_AGENT_TOKEN` | none (required) | API and portal token. The container exits if it is unset. |
| `DC_AGENT_ADDR` | `0.0.0.0:8080` | Address the agent listens on inside the container. |
| `DESKTOP_WIDTH` / `DESKTOP_HEIGHT` | `1280` / `800` | Size of the first GTK display (the terminal). |
| `BROADWAY_DISPLAY` | `:1` | Display name of the first broadwayd. |
| `BROADWAY_PORT` | `8081` | Port of the first broadwayd. Not published. Other apps use the port in their `apps.d` entry. |
| `DC_BROWSER_START` | `lazy` | `lazy` starts Chromium the first time the browser window needs it. `boot` starts it with the agent. `off` disables the browser. |
| `DC_BROWSER_IDLE_MINUTES` | `30` | Stop Chromium after this many minutes with no command or input from a client. The next navigation starts it again. `0` disables the idle stop. |
| `DC_AUTOSTART_APPS` | `0` | `1` starts every GTK app at boot. Otherwise each app starts when its window opens. |
| `DC_IMAGE` | `docker-computer:dev` | Image the e2e and budget scripts test. |
| `DC_DESKTOP_DIR` | unset | Serve the desktop from this directory instead of the embedded copy. For development. |

## Apps

| Variable | Default | Purpose |
|----------|---------|---------|
| `DC_APPS_DIRS` | `/etc/dc/apps.d:~/.config/dc/apps.d` | Colon-separated app registry directories. Later ones override earlier ones by id. |

Each `apps.d` entry sets `id`, `name`, `exec`, `icon`, `order` and `port`. See [desktop.md](desktop.md).

## Logging

| Variable | Default | Purpose |
|----------|---------|---------|
| `DC_LOG_BACKEND` | `gologger` | `gologger` uses [go-logger](https://github.com/gtsteffaniak/go-logger). `slog` uses the standard library handlers. |
| `DC_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DC_LOG_JSON` | `0` | `1` writes JSON lines. |
| `DC_LOG_NO_COLOR` | `0` | `1` disables colour in text output. |

Request logs record method, path, status and duration. Headers, query strings and cookies are never logged, so tokens stay out of the log.
