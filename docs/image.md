# Image

The image is built from `docker/Dockerfile`. It has six stages. Only the runtime stage ships:

| Stage | What it does | Shipped |
|-------|--------------|---------|
| `agent` | Builds `dad` with Go. Module and build caches are BuildKit cache mounts. | the binary only |
| `module` | Builds the GTK keyboard module against the runtime's GTK. | the `.so` only |
| `playwright` | `npm ci` of `playwright-core` from `docker/playwright/package-lock.json`, with browser downloads skipped. | `node_modules` only (npm is not shipped) |
| `proot` | Builds PRoot v5.5.0 from a pinned upstream commit (sha256-checked). Debian's proot 5.1.0 lacks `statx`, so `stat()` of bound paths fails. | the binary only |
| `rootfs` | Debian trixie upgraded to the current archive, with the desktop apps, Chromium and Node.js. | its filesystem, flattened |
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
DOCKER_BUILDKIT=1 docker build -f docker/Dockerfile -t docker-agent-desktop:dev .
```

Build arguments:

| Argument | Default | Purpose |
|----------|---------|---------|
| `WITH_SSH_CLIENT` | `true` | Install `openssh-client`, so the container can ssh out to other machines. `false` leaves it out. |
| `WITH_EMOJI_FONT` | `false` | Install `fonts-noto-color-emoji` (about 11 MB), so Chromium and the GTK apps draw colour emoji. Off by default: without it emoji render as empty boxes or monochrome fallbacks. `1` or `true` turns it on; any other value leaves it out. |

There is no build-time list of extra packages. Install software at runtime instead: without root
with `dad-apt` (below), or as root.

## Runtime user

The image runs as `bot` (uid 1000) by default. In that mode:

- Apps, Chromium and the agent run as `bot`.
- `apt-get install` and writes to system paths fail. Use `dad-apt` for user-level packages.
- The home directory `/home/bot` is writable. `~/.local/bin` is first on `PATH`.

## Installing software without root (dad-apt)

`dad-apt` installs Debian packages as `bot`, under `/home/bot`, so they persist with a home volume
(`-v dad-home:/home/bot`) across container restarts and image upgrades.

```bash
dad-apt install jq figlet      # resolves against the image, downloads only what is missing
jq --version                  # wrappers in ~/.local/bin run each installed command
dad-apt list                   # package and version
dad-apt exec ls /usr/share/figlet   # any command, with installed files at their real paths
dad-apt shell                  # bash with the installed files visible
dad-apt remove figlet
```

How it works:

1. `apt-get` runs as `bot` in download-only mode, with its package lists and archive cache under
   `~/.local/share/dad-apt`. It resolves against the image's dpkg database, so dependencies the
   image already has are not downloaded.
2. Each `.deb` is unpacked with `dpkg-deb` into `~/.local/share/dad-apt/root` (docs and man pages are
   skipped). `update-alternatives --install` links from the package's postinst (for example
   `/usr/bin/figlet`) are recreated in the tier.
3. Commands run under [PRoot](https://proot-me.github.io/), which binds each installed file or new
   directory over its real path (`/usr/bin/jq`, `/usr/lib/x86_64-linux-gnu/libjq.so.1`,
   `/usr/share/figlet`) for that process only. Programs find libraries and data where Debian put
   them; the image's files are never changed; everything else is the real system.

Limits:

- Maintainer scripts are not run. Packages that need them (services, system users, setuid helpers,
  triggers, debconf questions) do not fully work.
- No real root: no setuid, no binding privileged ports, no writes to system paths.
- PRoot uses ptrace (with seccomp acceleration). Startup adds a few milliseconds per command;
  syscall-heavy work is slower than native.
- Removing a package does not check whether another tier package depends on it.
- The tier needs network access to `deb.debian.org` (or the sources in `/etc/apt/sources.list.d`).

## Installing software as root

Run the container as root to install packages with apt. The desktop still starts, because the entrypoint and agent work as root too.

```bash
# Start a root shell in a running container
docker exec -u root -it dad-dev bash
apt-get update && apt-get install -y --no-install-recommends jq

# Or run the container as root from the start
docker run --user root ... docker-agent-desktop:dev
```

Packages installed this way live in the container's writable layer. They are lost when the container is removed. To keep them, build a derived image:

```dockerfile
FROM docker-agent-desktop:dev
USER root
RUN apt-get update && apt-get install -y --no-install-recommends jq \
 && rm -rf /var/lib/apt/lists/*
USER 1000:1000
```

## SSH client

With `WITH_SSH_CLIENT=true` the image has `ssh`, `scp` and `ssh-keygen`. The image does not run an SSH server. Mount keys and known hosts into the home directory, with the permissions ssh requires:

```bash
docker run -v dad-ssh:/home/bot/.ssh ... docker-agent-desktop:dev
```

Put keys in the `dad-ssh` volume, owned by uid 1000 with mode 600 for private keys. Do not mount the host's `~/.ssh` read-write unless you accept that the container can read and change it.

## Size and efficiency

Measured on the default build (`make budgets`, `make dive`). Sizes are bytes as Docker reports them
(`docker image inspect .Size`, uncompressed on overlay2); MB = 10^6 bytes. "gzip" is the sum of each
layer tar from `docker save` compressed with gzip -6, an approximation of the registry download.

| | Uncompressed | gzip | dive |
|---|---|---|---|
| Default build | 712.8 MB (679 MiB) | 278.9 MB | 0 B wasted, 100% efficiency |
| `WITH_EMOJI_FONT=1` | 724.1 MB | 289.3 MB | 0 B wasted, 100% efficiency |

What is in it: Chromium and its libraries, GTK 3 and the desktop apps make up most of the image.
Node.js and `playwright-core` add about 79 MB (Debian's `nodejs` 20 with `libnode`, 64 MiB, and
`playwright-core`, 14 MiB). PRoot adds 0.3 MB.

Removed on purpose: the Mesa GPU drivers, LLVM, z3, the Vulkan layers and the software Vulkan
rasteriser (headless Chromium runs with `--disable-gpu`); man pages and documentation (copyright
files stay); the colour emoji font unless `WITH_EMOJI_FONT` is set; npm (build stage only); and
Playwright's own browsers (never downloaded; `dad-playwright` uses the image's Chromium).

Memory (container cgroup usage, MiB): idle about 6 MiB; Chromium plus the GTK apps about 260 MiB
(budget 400); while `dad-playwright record` runs, about 80 MiB more (the Node process), released
when the recording stops. Boot to healthy: about 0.25 s.

## Gates

| Gate | Command | What it checks |
|------|---------|----------------|
| Dockerfile lint | `make hadolint` | Hadolint, with `.hadolint.yaml` |
| Efficiency | `make dive` | Dive with `.dive-ci` rules: wasted bytes, wasted percent and efficiency. Fails the build when a rule breaks. |

Dive's rules are in `.dive-ci`. They sit just above the current baseline, so a step that re-creates a large tree fails the gate. Recalibrate them after a deliberate change to the image contents.
