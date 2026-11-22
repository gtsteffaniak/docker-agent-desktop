# syntax=docker/dockerfile:1.7
#
# docker-computer runtime image: a web desktop with GTK 3 apps, a headless browser and the agent.
#
# Stages (only the runtime stage is shipped):
#   agent    Go binary for dc-agent. Module and build caches are BuildKit cache mounts.
#   module   The GTK keyboard module (libdc-windows.so), built against the runtime's GTK.
#   rootfs   Debian trixie with the apps and Chromium, upgraded to the current archive.
#   runtime  FROM scratch: the rootfs copied in as one layer, then the agent and config.
#
# Build:    DOCKER_BUILDKIT=1 docker build -f docker/Dockerfile -t docker-computer:dev .
# Options:  --build-arg WITH_SSH_CLIENT=false   leave out the OpenSSH client
#
# Base image: DEBIAN_IMAGE is pinned by digest so builds are reproducible. `make bump-base`
# re-resolves debian:trixie-slim and rewrites the pin (see docs/image.md). The rootfs stage runs
# apt-get upgrade, so security fixes newer than the pinned base still land on every rebuild.
#
# Runtime: the image runs as the unprivileged user "bot" (uid 1000) and apps are not installed
# there. To install software, run as root (docker run --user root, or docker exec -u root) and use
# apt-get. Build-time package lists are not supported.

ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

FROM golang:1.27-bookworm AS agent
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY internal ./internal
COPY cmd/dc-agent ./cmd/dc-agent
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/dc-agent ./cmd/dc-agent

FROM ${DEBIAN_IMAGE} AS module
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
 && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    DEBIAN_FRONTEND=noninteractive apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    gcc libc6-dev libgtk-3-dev pkg-config
COPY cmd/dc-windows/module.c /src/module.c
# hadolint ignore=SC2046
RUN cc -O2 -Wall -Wno-deprecated-declarations -shared -fPIC -o /src/libdc-windows.so \
    /src/module.c $(pkg-config --cflags --libs gtk+-3.0)

FROM ${DEBIAN_IMAGE} AS rootfs
ARG WITH_SSH_CLIENT=true
COPY docker/dpkg-excludes.cfg /etc/dpkg/dpkg.cfg.d/50-computer
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
 && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
# The GPU stack is excluded at unpack time by docker/dpkg-excludes.cfg, so it is never written.
# Headless Chromium runs with --disable-gpu, so none of it is loaded. apt-get upgrade brings base
# packages (libssl3t64, zlib1g, ...) up to the archive; the runtime stage below flattens the result,
# so the copies replaced here are not kept in a lower layer.
# hadolint ignore=SC2086,DL3005
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux; \
    pkgs="ca-certificates curl tini dbus-x11 libgtk-3-bin hicolor-icon-theme librsvg2-common \
          lxterminal mousepad thunar chromium fonts-liberation fonts-noto-color-emoji"; \
    if [ "$WITH_SSH_CLIENT" = "true" ]; then pkgs="$pkgs openssh-client"; fi; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get upgrade -y --no-install-recommends; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $pkgs; \
    rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log /var/cache/debconf/*-old; \
    groupadd -g 1000 bot; \
    useradd -m -u 1000 -g 1000 -s /bin/bash bot

# The shipped image starts from scratch so the base layer and the apt layer are one layer: files the
# upgrade replaced exist once, not twice (dive no longer reports the base's libssl/libcrypto as
# wasted). The agent, module and config are later, small layers so code changes do not re-copy
# the rootfs.
FROM scratch AS runtime
COPY --from=rootfs / /

COPY --from=agent /out/dc-agent /usr/local/bin/dc-agent
COPY --from=module /src/libdc-windows.so /usr/local/lib/gtk-3.0/3.0.0/modules/libdc-windows.so
COPY --chmod=0755 docker/rootfs/usr/local/bin/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY docker/rootfs/etc/dc/apps.d/ /etc/dc/apps.d/

LABEL org.opencontainers.image.title="docker-computer" \
      org.opencontainers.image.description="Web desktop with GTK 3 apps, a headless browser and an agent API" \
      org.opencontainers.image.licenses="MIT" \
      io.docker-computer.api-version="1"

USER 1000:1000
WORKDIR /home/bot
# No default token: the container refuses to start without COMPUTER_AGENT_TOKEN.
# FROM scratch carries no ENV from the base, so PATH is set here.
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GTK_PATH=/usr/local/lib/gtk-3.0 \
    GTK_MODULES=dc-windows

EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=2s --start-period=8s --retries=6 \
  CMD ["curl", "-sf", "http://127.0.0.1:8080/healthz"]

# tini is PID 1: it reaps orphaned children and forwards signals to the whole process group (-g).
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/entrypoint.sh"]
