# Phase 0 results (measured)

Measurements were taken on **2026-11-18** in the Phase 0 cloud-agent VM (`linux/amd64`) with **Docker 29.1.3** (`sudo docker build` / `sudo docker run`). Commands: `make image`, `./scripts/e2e-phase0.sh`, `./scripts/budgets.sh`.

## Budget table

| Metric | Budget (accepted) | Measured | Pass |
|--------|-------------------|----------|------|
| Idle container RSS (Chromium and Node not started) | < 60 MB | **7 MiB** (`docker stats`, 3 s after healthy) | yes |
| Browser open RSS (headless Chromium + GTK apps autostarted, Node in image not started) | < 400 MB | **239.3 MiB** (`docker stats`, ~8 s after start) | yes |
| Boot to `GET /healthz` | < 3 s | **0.212 s** (`scripts/budgets.sh`) | yes |
| Image size (Chromium + Node + playwright-core + GTK stack, one Debian trixie-slim layer) | ≤ legacy + 80 MB (estimate) | **369 MB** compressed layer total (`docker image inspect .Size`); legacy baseline not present in this repo — **delta not compared** | partial |

## Risky mechanisms (go / no-go)

| Mechanism | Verdict | Evidence |
|-----------|---------|----------|
| Broadway protocol input (pointer + keyboard, no X / ydotool) | **GO** | `scripts/e2e-phase0.sh`: HTTP `POST /spike/input/click` and `POST /spike/input/type` against live GTK apps on one `broadwayd` display; follow-up screenshot succeeds. Go encoder matches `broadway.js` `sendInput` (big-endian int32 messages). |
| Broadway frame decode → screenshot | **GO** | `internal/broadway` decodes `PUT_BUFFER` (raw DEFLATE + `decodeBuffer`) with **little-endian** server ops; e2e PNG **17,720 bytes** with **11** surfaces after autostart. |
| One Broadway display, multiple GTK 3 apps | **GO** | E2E starts `lxterminal`, `mousepad`, `thunar` together on display `:1` (`GDK_BACKEND=broadway`, `BROADWAY_DISPLAY=:1`). |
| Minimal GTK dock + reserved bottom strip | **GO** | `cmd/dc-shell` (`dc-shell` binary) keeps a **48 px** bottom bar (`DOCK_HEIGHT`), `keep_above` + periodic reposition; e2e records `dock_strip_reserved_px=48` and viewport 1280×800. Raise/focus wiring is minimal in Phase 0 (launch buttons only). |
| Chromium headless CDP screencast | **GO** | `internal/cdp` attaches to page target, `Page.startScreencast`; e2e JPEG **2,341 bytes** via `GET /spike/browser/frame`. |

## Notes and caveats

- **Idle RSS (7 MiB)** is cgroup usage for the spike stack (`dc-agent`, `broadwayd`, `dc-shell`) with Chromium and Node binaries present on disk but not executed; this matches the design goal of lazy browser/Node startup.
- **Browser-open RSS (239 MiB)** includes Chromium, three GTK apps, and Broadway traffic; still under the 400 MB budget with margin.
- Debian GTK Broadway apps require **`BROADWAY_DISPLAY=:N`** (not `DISPLAY=:N`) when using `GDK_BACKEND=broadway`; this is fixed in `image/rootfs/usr/local/bin/entrypoint.sh`.
- Server→client Broadway drawing ops use **little-endian** integers; client→server input uses **big-endian** (verified against GTK 3.24 `broadway.js`).
- `make gate` does **not** run GitHub Actions (by design). Local gate: `lint`, `unit`, `image`, `e2e`, `budgets`.

## Commands to reproduce

```bash
make gate
# or stepwise:
make lint unit image e2e budgets
```