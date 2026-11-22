# docker-agent-desktop

A containerized desktop environment designed for agentic workflows. Native GTK apps and a Chromium browser that agents drive, record and replay with Playwright. A small Go agent, `dad` (short for docker-agent-desktop), exposes an HTTP API for apps, input, screenshots and browser frames. Viewable from a lightweight browser.

[Documentation](docs/) · [API](docs/api.md) · [Configuration](docs/configuration.md) · [Desktop](docs/desktop.md) · [Image](docs/image.md) · [Playwright](docs/playwright.md) · [Portal](docs/portal.md) · [Sessions](docs/sessions.md) · [MCP](docs/mcp.md) · [Development](docs/development.md)

## Features

| Area | What you get |
|------|--------------|
| Desktop | A web desktop with a floating dock, rounded window frames and a gradient wallpaper. GTK 3 apps and the browser run inside it |
| Customization | Apps from `apps.d` files (`port` for GTK apps) and the desktop CSS |
| Browser | Headless Chromium, with frames streamed over CDP |
| Playwright | `dad-playwright` attaches to that same Chromium over CDP to drive it, record actions and a trace into `~/recordings`, and replay recordings |
| Input | Pointer clicks and text typed through the API |
| Portal | The desktop in any browser at `/desktop/` |
| Agent | Versioned HTTP API, token auth, request and lifecycle logs |
| Image | Debian trixie-slim, runs as uid 1000, tini as PID 1 |

## Quick start

Requires Docker and Make.

```bash
make dev
```

This builds the image, runs it on `127.0.0.1:8080`, and prints a portal link:

```
Desktop: http://localhost:8080/desktop/?token=dev-token
```

Press Ctrl-C to stop. Run `make help` for every target.

For a production-style run, set your own token and publish the port as you need:

```bash
docker run --rm -p 8080:8080 -e DAD_TOKEN=change-me docker-agent-desktop:dev
```

The container refuses to start without `DAD_TOKEN`.

## Documentation

| Page | Covers |
|------|--------|
| [API](docs/api.md) | Endpoints, authentication, examples |
| [Configuration](docs/configuration.md) | Environment variables, logging |
| [Desktop](docs/desktop.md) | Windows, dock, browser window, keyboard |
| [Playwright](docs/playwright.md) | Driving, recording and replaying the live browser |
| [Image](docs/image.md) | Stages, build arguments, base image, installing software |
| [Portal](docs/portal.md) | Browser access, cookies, limits |
| [Sessions](docs/sessions.md) | The single-session lease, wait/takeover, desktop session |
| [MCP](docs/mcp.md) | MCP tools, session rule, frame/stream notes |
| [Development](docs/development.md) | Make targets, tests, CI, cross-arch builds |
| [Architecture](docs/architecture.md) | Components and data flow |
| [Roadmap](docs/roadmap.md) | Planned work and known gaps |

## License

MIT. See [LICENSE](LICENSE).
