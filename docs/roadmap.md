# Roadmap

## Known gaps

- Closing a window does not quit its app. There is no quit action yet.
- The desktop has no clipboard sharing between apps, and no audio.
- The `/api/display/*` endpoints send ASCII text and left click to the terminal display only. Browser input (pointer, scroll, keys, paste) goes through `/api/browser/input`.
- Broadway command parsing for some opcodes (`r`, `R`, `p`, `g`, `u`, `k`) skips fields without checking their widths against the protocol. Verify against GTK's `broadway.js` before relying on them.
- The `/api/` endpoints are not yet a stable API. They will move to a stable `/v1` API with apps, files and recording.
- Chromium runs with `--no-sandbox` inside the container. Acceptable for a single-tenant container, but revisit before multi-tenant use.
- No resource limits are documented yet. Recommended run flags: `--memory`, `--pids-limit`, `--cap-drop=ALL`, `--read-only` with writable volumes for `/home/bot`.

## Planned

1. Stable `/v1` API for apps, files, input and recording.
2. Broader input: right click, drag, scroll and key combinations.
3. Resource limits and hardening guide.
4. Slim the image further once the Node toolchain is needed again.
