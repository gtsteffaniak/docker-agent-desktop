# syntax=docker/dockerfile:1

FROM golang:1.22-bookworm AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dc-agent ./cmd/dc-agent
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dc-broadway-spike ./cmd/dc-broadway-spike

FROM debian:trixie-slim AS dockbuild
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    gcc libc6-dev libgtk-3-dev pkg-config ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY cmd/dc-shell/main.c /src/main.c
RUN mkdir -p /out && cc -O2 -o /out/dc-shell /src/main.c $(pkg-config --cflags --libs gtk+-3.0)

FROM debian:trixie-slim AS nodepw
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    curl ca-certificates \
 && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && npm install -g playwright-core@1.49.1 \
 && rm -rf /var/lib/apt/lists/*

FROM debian:trixie-slim
LABEL io.docker-computer.api-version=0
LABEL io.docker-computer.phase=0
LABEL org.opencontainers.image.licenses=MIT

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    libgtk-3-bin \
    lxterminal mousepad thunar \
    chromium \
     curl ca-certificates \
    fontconfig fonts-liberation fonts-noto-color-emoji \
    proot \
    libnss3 libx11-6 libglib2.0-0 libgtk-3-0 libcairo2 \
 && rm -rf /var/lib/apt/lists/*

COPY --from=nodepw /usr/bin/node /usr/bin/node
COPY --from=nodepw /usr/lib/node_modules/playwright-core /usr/lib/node_modules/playwright-core
RUN ln -sf /usr/lib/node_modules/playwright-core/cli.js /usr/local/bin/playwright-core

COPY --from=gobuild /out/dc-agent /out/dc-broadway-spike /usr/local/bin/
COPY --from=dockbuild /out/dc-shell /usr/local/bin/dc-shell
COPY image/rootfs/ /

RUN groupadd -g 1000 bot && useradd -m -u 1000 -g 1000 -s /bin/bash bot \
 && chown -R bot:bot /home/bot \
 && chmod +x /usr/local/bin/entrypoint.sh

USER bot
WORKDIR /home/bot
ENV HOME=/home/bot
ENV COMPUTER_AGENT_TOKEN=replace-at-run
ENV BROADWAY_PORT=8081
ENV BROADWAY_DISPLAY=:1
ENV DESKTOP_WIDTH=1280
ENV DESKTOP_HEIGHT=800
ENV GDK_BACKEND=broadway

EXPOSE 8080 8081
HEALTHCHECK --interval=5s --timeout=2s --start-period=8s --retries=6 \
  CMD curl -sf http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]