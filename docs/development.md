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
| `make build` | Build `docker-computer:dev` |
| `make e2e` | Run `scripts/e2e.sh` against the image |
| `make budgets` | Measure image size (bytes, MB, MiB), memory in MiB (idle, browser open, browser while recording) and boot time |
| `make bump-base` | Re-pin `DEBIAN_IMAGE` to the current `debian:trixie-slim` digest |
| `make dive` | Image efficiency gate, with the rules in `.dive-ci` |
| `make gate` | All of the above |

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
- `cmd/dc-agent`: desktop routes and assets, the token cookie exchange, per-app proxying with the keys script, the app registry, browser URL normalization and history state.
- `internal/cdp`: navigation history, main-frame filtering, key and mouse mapping.
- `scripts/e2e.sh`: runs the real container. It covers health, auth, the desktop page with its cookie, the app registry and ports, each GTK app page, CDP frames, browser navigation and the address state, the URL guard, network exposure, the `/v1` API and MCP (`scripts/e2e_v1.py`), Playwright attached to the live browser (record, trace, replay), and the `dc-apt` tier persisting across containers on a home volume.

## CI

`.github/workflows/ci.yml` runs `gofmt`, `go vet`, unit tests, hadolint, a container build, the e2e script and the dive efficiency gate on every push to `main` and on pull requests.

## Dependencies

- `github.com/coder/websocket`: WebSocket client and server.
- `github.com/gtsteffaniak/go-logger`: default logging backend, wrapped as a `slog.Handler` in `internal/logging`.
