#!/bin/bash
set -euo pipefail

export HOME=/home/bot
export USER=bot
export GDK_BACKEND=broadway
export BROADWAY_DISPLAY="${BROADWAY_DISPLAY:-:1}"
unset DISPLAY
export DESKTOP_WIDTH="${DESKTOP_WIDTH:-1280}"
export DESKTOP_HEIGHT="${DESKTOP_HEIGHT:-800}"

mkdir -p "${HOME}/.cache" "${HOME}/.config" "${HOME}/.local/share" "${HOME}/recordings" "${HOME}/.dc"

broadwayd "${BROADWAY_DISPLAY}" -p "${BROADWAY_PORT:-8081}" &
BROADWAY_PID=$!

for i in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:${BROADWAY_PORT:-8081}/" >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done

/usr/local/bin/dc-shell &
SHELL_PID=$!

if [[ "${SPIKE_AUTOSTART_APPS:-0}" == "1" ]]; then
  lxterminal &
  mousepad &
  thunar &
fi

exec /usr/local/bin/dc-agent