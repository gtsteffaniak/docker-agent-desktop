# docker-computer

A containerized desktop environment designed for agentic workflows and bots. Native GTK apps with a recordable browser. A small Go agent (`dc-agent`) exposes an HTTP API for apps, input, screenshots and browser frames. Viewable from a lightweight browser.

[Documentation](docs/) · [API](docs/api.md) · [Configuration](docs/configuration.md) · [Desktop](docs/desktop.md) · [Image](docs/image.md) · [Portal](docs/portal.md) · [Development](docs/development.md)

## Features

| Area | What you get |
|------|--------------|
| Desktop | A web desktop with a floating dock, rounded window frames and a gradient wallpaper. GTK 3 apps and the browser run inside it |
| Customization | Apps from `apps.d` files (`port` for GTK apps) and the desktop CSS |
| Browser | Headless Chromium, with frames streamed over CDP |
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
docker run --rm -p 8080:8080 -e COMPUTER_AGENT_TOKEN=change-me docker-computer:dev
```

The container refuses to start without `COMPUTER_AGENT_TOKEN`.

## Documentation

| Page | Covers |
|------|--------|
| [API](docs/api.md) | Endpoints, authentication, examples |
| [Configuration](docs/configuration.md) | Environment variables, logging |
| [Desktop](docs/desktop.md) | Windows, dock, browser window, keyboard |
| [Portal](docs/portal.md) | Browser access, cookies, limits |
| [Development](docs/development.md) | Make targets, tests, CI, cross-arch builds |
| [Architecture](docs/architecture.md) | Components and data flow |
| [Roadmap](docs/roadmap.md) | Planned work and known gaps |

## License

MIT. See [LICENSE](LICENSE).
