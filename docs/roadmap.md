# Roadmap

## Known gaps

- The desktop has no clipboard sharing between apps, and no audio.
- Broadway command parsing for some opcodes (`r`, `R`, `p`, `g`, `u`, `k`) skips fields without checking their widths against the protocol. Verify against GTK's `broadway.js` before relying on them.
- Chromium runs with `--no-sandbox` inside the container. Acceptable for a single-tenant container, but revisit before multi-tenant use.
- No resource limits are documented yet. Recommended run flags: `--memory`, `--pids-limit`, `--cap-drop=ALL`, `--read-only` with writable volumes for `/home/bot`.

## Done

- Stable `/v1` API for apps, files, shell, browser and display input, with a session lease, real session health, and an activity log (see [api.md](api.md) and [sessions.md](sessions.md)).
- App quit: `POST /v1/apps/stop` (SIGTERM, 5 s grace, SIGKILL) and `POST /v1/browser/stop`.
- Broader input: right/middle click, drag, scroll and key combinations on the display; selector-based click/type/select/scroll/key in the browser.
- MCP server wrapping the same surface at `POST /mcp` (see [mcp.md](mcp.md)).
- Playwright on the live browser: `dad-playwright` attaches over CDP to drive, record (codegen script + trace) and replay (see [playwright.md](playwright.md)).

## Planned

1. Resource limits and hardening guide.
2. First-class recording endpoints (`/v1/browser/recording/*` and MCP tools) on top of `dad-playwright`, so agents do not need the shell API to record.
