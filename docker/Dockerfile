# syntax=docker/dockerfile:1.7
#
# docker-computer runtime image: a web desktop with GTK 3 apps, a headless browser and the agent.
#
# Stages (only the runtime stage is shipped):
#   agent    Go binary for dc-agent. Module and build caches are BuildKit cache mounts.
#   module   The GTK keyboard module (libdc-windows.so), built against the runtime's GTK.
#   runtime  Debian trixie with the apps, Chromium and the agent.
#
# Build:    DOCKER_BUILDKIT=1 docker build -f docker/Dockerfile -t docker-computer:dev .
# Options:  --build-arg WITH_SSH_CLIENT=false   leave out the OpenSSH client
#
# Runtime: the image runs as the unprivileged user "bot" (uid 1000) and apps are not installed
# there. To install software, run as root (docker run --user root, or docker exec -u root) and use
# apt-get. Build-time package lists are not supported.

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

FROM debian:trixie-slim AS module
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

FROM debian:trixie-slim AS runtime
ARG WITH_SSH_CLIENT=true
COPY docker/dpkg-excludes.cfg /etc/dpkg/dpkg.cfg.d/50-computer
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
 && echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache
# One layer. The GPU stack is excluded at unpack time by docker/dpkg-excludes.cfg, so it is never
# written. Headless Chromium runs with --disable-gpu, so none of it is loaded.
# hadolint ignore=SC2086
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux; \
    pkgs="ca-certificates curl tini dbus-x11 libgtk-3-bin hicolor-icon-theme librsvg2-common \
          lxterminal mousepad thunar chromium fonts-liberation fonts-noto-color-emoji"; \
    if [ "$WITH_SSH_CLIENT" = "true" ]; then pkgs="$pkgs openssh-client"; fi; \
    DEBIAN_FRONTEND=noninteractive apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $pkgs; \
    rm -rf /var/lib/apt/lists/*

RUN groupadd -g 1000 bot && useradd -m -u 1000 -g 1000 -s /bin/bash bot

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
ENV GTK_PATH=/usr/local/lib/gtk-3.0 \
    GTK_MODULES=dc-windows

EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=2s --start-period=8s --retries=6 \
  CMD ["curl", "-sf", "http://127.0.0.1:8080/healthz"]

# tini is PID 1: it reaps orphaned children and forwards signals to the whole process group (-g).
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/entrypoint.sh"]
