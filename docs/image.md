# Image

The image is built from `docker/Dockerfile`. It has four stages. Only the runtime stage ships:

| Stage | What it does | Shipped |
|-------|--------------|---------|
| `agent` | Builds `dc-agent` with Go. Module and build caches are BuildKit cache mounts. | the binary only |
| `module` | Builds the GTK keyboard module against the runtime's GTK. | the `.so` only |
| `rootfs` | Debian trixie upgraded to the current archive, with the desktop apps and Chromium. | its filesystem, flattened |
| `runtime` | `FROM scratch`: the `rootfs` filesystem as one layer, then the agent, module and config. | yes |

## Base image

`DEBIAN_IMAGE` at the top of the Dockerfile pins `debian:trixie-slim` by digest, so two builds of
the same commit start from the same base. The `rootfs` stage runs `apt-get upgrade` before it
installs packages, so security fixes published after the pinned base still land on every rebuild.

The official `trixie-slim` tag is rebuilt on Debian's schedule, not on every security update, so it
usually lags the archive (for example, the pinned base ships `libssl3t64` 3.5.7 while the archive
has 3.5.9). When apt replaces a base-layer file in a later layer, both copies are stored and dive
counts the old one as wasted. The runtime stage avoids that by starting `FROM scratch` and copying
the upgraded `rootfs` in as a single layer: every file is stored once, and dive reports 0 bytes
wasted. The cost is that the image no longer shares a base layer with other `debian:trixie-slim`
images on the same host or registry.

To move to a newer base:

```bash
make bump-base   # docker pull debian:trixie-slim, rewrite the ARG DEBIAN_IMAGE pin
make gate        # rebuild and run every check
```

Commit the Dockerfile change on its own so the base bump is easy to review and revert. A plain
rebuild without `bump-base` is still current: the upgrade step pulls the archive's latest packages.

## Build

```bash
DOCKER_BUILDKIT=1 docker build -f docker/Dockerfile -t docker-computer:dev .
```

Build arguments:

| Argument | Default | Purpose |
|----------|---------|---------|
| `WITH_SSH_CLIENT` | `true` | Install `openssh-client`, so the container can ssh out to other machines. `false` leaves it out. |
| `WITH_EMOJI_FONT` | `false` | Install `fonts-noto-color-emoji` (about 11 MB), so Chromium and the GTK apps draw colour emoji. Off by default: without it emoji render as empty boxes or monochrome fallbacks. `1` or `true` turns it on; any other value leaves it out. |

There is no build-time list of extra packages. Install software at runtime as root instead (below).

## Runtime user

The image runs as `bot` (uid 1000) by default. In that mode:

- Apps, Chromium and the agent run as `bot`.
- `apt-get` and writes to system paths fail. This is intended. Software is not installed by the unprivileged user.
- The home directory `/home/bot` is writable. Put user tools in `~/.local/bin` or in a volume.

## Installing software as root

Run the container as root to install packages with apt. The desktop still starts, because the entrypoint and agent work as root too.

```bash
# Start a root shell in a running container
docker exec -u root -it dc-dev bash
apt-get update && apt-get install -y --no-install-recommends jq

# Or run the container as root from the start
docker run --user root ... docker-computer:dev
```

Packages installed this way live in the container's writable layer. They are lost when the container is removed. To keep them, build a derived image:

```dockerfile
FROM docker-computer:dev
USER root
RUN apt-get update && apt-get install -y --no-install-recommends jq \
 && rm -rf /var/lib/apt/lists/*
USER 1000:1000
```

## SSH client

With `WITH_SSH_CLIENT=true` the image has `ssh`, `scp` and `ssh-keygen`. The image does not run an SSH server. Mount keys and known hosts into the home directory, with the permissions ssh requires:

```bash
docker run -v dc-ssh:/home/bot/.ssh ... docker-computer:dev
```

Put keys in the `dc-ssh` volume, owned by uid 1000 with mode 600 for private keys. Do not mount the host's `~/.ssh` read-write unless you accept that the container can read and change it.

## Size and efficiency

Measured on the runtime image:

- Size: about 950 MB (Chromium and its libraries, GTK 3, and the desktop apps make up most of it).
- Efficiency: about 99.6%, measured with dive.
- Removed on purpose: the Mesa GPU drivers, LLVM, z3, the Vulkan layers and the software Vulkan rasteriser. Headless Chromium runs with `--disable-gpu`. Also removed: man pages and documentation (copyright files stay).

## Gates

| Gate | Command | What it checks |
|------|---------|----------------|
| Dockerfile lint | `make hadolint` | Hadolint, with `.hadolint.yaml` |
| Efficiency | `make dive` | Dive with `.dive-ci` rules: wasted bytes, wasted percent and efficiency. Fails the build when a rule breaks. |

Dive's rules are in `.dive-ci`. They sit just above the current baseline, so a step that re-creates a large tree fails the gate. Recalibrate them after a deliberate change to the image contents.
