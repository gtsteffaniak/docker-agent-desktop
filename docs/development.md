# Development

## Make targets

Run `make help` for the full list.

| Target | Does |
|--------|------|
| `make dev` | Build the image, run it on `127.0.0.1:8080`, and print the portal URL |
| `make dev-stop` / `make dev-logs` | Stop the dev container, or follow its logs |
| `make unit` | `go test -race ./...` |
| `make lint` | `gofmt`, `go vet`, `shellcheck` and `node --check` when installed (a finding fails the target), and hadolint |
| `make hadolint` | Lint `docker/Dockerfile` |
| `make build` | Build `docker-agent-desktop:dev` |
| `make e2e` | Run `scripts/e2e.sh` against the image |
| `make playwright` | Run the Playwright e2e suite in `tests/e2e` (server on the host, no docker; needs `chromium` on PATH) |
| `make budgets` | Measure image size (bytes, MB, MiB), memory in MiB (idle, browser open, browser while recording) and boot time |
| `make bump-base` | Re-pin `DEBIAN_IMAGE` to the current `debian:trixie-slim` digest |
| `make dive` | Image efficiency gate, with the rules in `.dive-ci` |
| `make gate` | All of the above except `make playwright` (the Playwright suite is a separate CI job) |

Override defaults on the command line, for example `make dev DEV_PORT=9090 DOCKER="docker"`.

## Building the image

See [image.md](image.md) for the stages, build arguments, the runtime user, root installs, the SSH client and the gates.

- The Go toolchain is Go 1.27, set in `go.mod` and the `golang:1.27-bookworm` build stage.
- Builds use BuildKit cache mounts for Go modules and apt packages, so a rebuild after a source change only repeats the steps that changed.

## Tests

- `internal/broadway`: decoder bounds and malformed input, inflate size cap, surface size limits.
- `internal/logging`: slog and go-logger backends, level filtering, group flattening, and `With` attributes in both output modes.
- `internal/apps`: registry parsing, directory overrides, invalid entries.
- `internal/background`: gradient parsing and rendering, rejection of unsafe input.
- `internal/broadway`: also covers click encoding, surface hit-testing, configure confirmation, and decoding after a resize.
- `cmd/dad`: desktop routes and assets, the token cookie exchange, per-app proxying with the keys script, the app registry, browser URL normalization and history state.
- `internal/cdp`: navigation history, main-frame filtering, key and mouse mapping.
- `scripts/e2e.sh`: runs the real container. It covers health, auth, the desktop page with its cookie, the app registry and ports, each GTK app page, CDP frames, browser navigation and the address state, the URL guard, network exposure, the `/v1` API and MCP (`scripts/e2e_v1.py`), Playwright attached to the live browser (record, trace, replay), and the `dad-apt` tier persisting across containers on a home volume.
- `tests/e2e`: Playwright suite against the server running on the host (no docker). It covers the `/v1` API end to end (auth, session lease, keepalive, pause/resume, wait/takeover, shell, files, browser frames), the MCP server (initialize, tools/list, a session and a shell call over MCP), the desktop page (portal login, dock, browser window, live frames, address-bar navigation), and the browser stream socket (unknown session refused, live session streams frames). GTK apps have no backend outside the container, so the desktop spec asserts windows, not app content. Run it with `make playwright`; the server needs a `chromium` binary on PATH — symlink Playwright's own Chromium there after `npm run install-browsers`.

## CI

`.github/workflows/ci.yml` runs on every push to `main` and on pull requests. The jobs, all required to pass:

| Job | Does |
|-----|------|
| `format` | `gofmt -l` must be empty |
| `vet` | `go vet ./...`, and `go.mod`/`go.sum` must be tidy |
| `lint` | golangci-lint v2.15.0 (see `.golangci.yml`: staticcheck, ineffassign, misspell) |
| `test` | `go test -race` with coverage, on Go 1.27 and `stable` |
| `build` | `go build ./...` cross-compiled for linux/amd64 and linux/arm64 |
| `swagger` | `make swagger` must produce no diff in `cmd/dad/docs/` |
| `scripts-lint` | shellcheck over `scripts/*.sh` and the rootfs wrappers, `node --check` on `dad-playwright.js` |
| `workflows-lint` | actionlint over the workflow files |
| `image` | hadolint, docker build, `scripts/e2e.sh`, `scripts/budgets.sh`, dive gate (runs after `test`) |
| `e2e-playwright` | Playwright suite in `tests/e2e` against the server on the runner, no docker (runs after `test` and `build`) |

## Dependencies

- `github.com/coder/websocket`: WebSocket client and server.
- `github.com/gtsteffaniak/go-logger`: default logging backend, wrapped as a `slog.Handler` in `internal/logging`.
