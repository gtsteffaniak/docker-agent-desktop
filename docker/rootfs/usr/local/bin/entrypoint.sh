#!/bin/bash
set -euo pipefail

# DAD_TOKEN is required.
if [[ -z "${DAD_TOKEN:-}" ]]; then
  echo "DAD_TOKEN must be set" >&2
  exit 1
fi

export HOME=/home/bot
export USER=bot
export GDK_BACKEND=broadway
# The web desktop draws every window frame, so GTK apps run without client-side decorations.
export GTK_CSD=0
export BROADWAY_DISPLAY="${BROADWAY_DISPLAY:-:1}"
export BROADWAY_PORT="${BROADWAY_PORT:-8081}"
unset DISPLAY
export DESKTOP_WIDTH="${DESKTOP_WIDTH:-1280}"
export DESKTOP_HEIGHT="${DESKTOP_HEIGHT:-800}"
export NO_AT_BRIDGE=1

mkdir -p "${HOME}/.cache" "${HOME}/.config/dad/apps.d" "${HOME}/.local/share" "${HOME}/recordings" "${HOME}/.dad" "${HOME}/.dad/logs"

# Loopback only: Broadway has no authentication; dad proxies it behind the token check.
# The subshell makes broadwayd an orphan of tini (PID 1) instead of a child of this shell, which
# becomes dad on exec below. dad never waits on a child it did not start, so a
# broadwayd that exits under it would stay a zombie; tini reaps it instead.
# Do not block boot on broadwayd answering: dad's Broadway client reconnects with backoff.
( broadwayd "${BROADWAY_DISPLAY}" -a 127.0.0.1 -p "${BROADWAY_PORT}" & )

# One session bus for every app. GTK apps (thunar, mousepad) need it for settings, and without
# it they print "Cannot autolaunch D-Bus without X11 $DISPLAY" on every start.
# Start broadwayd first so dbus-launch can overlap with it warming up.
if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
  eval "$(dbus-launch --sh-syntax)"
  export DBUS_SESSION_BUS_ADDRESS
fi

# The web desktop (served by dad) draws every window, so the dock and browser window are not
# GTK processes. dad starts each GTK app's broadwayd display and app when its window opens,
# or all of them at boot with DAD_AUTOSTART_APPS=1. The first display above (:1, port 8081)
# is the terminal's, so the agent reuses it.
exec /usr/local/bin/dad
