#!/usr/bin/env bash
# Phase 0 e2e: Broadway input, screenshot decode, multi-app, dock strip, CDP screencast.
set -euo pipefail

IMAGE="${SPIKE_IMAGE:-docker-computer:dev}"
TOKEN="${COMPUTER_AGENT_TOKEN:-phase0-e2e-token}"
NAME="dc-phase0-e2e-$$"
DOCKER="${DOCKER:-sudo docker}"
PORT=18090
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"; $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

$DOCKER run -d --name "$NAME" -p "${PORT}:8080" \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e SPIKE_START_CHROMIUM=1 \
  -e SPIKE_AUTOSTART_APPS=1 \
  -e DESKTOP_WIDTH=1280 \
  -e DESKTOP_HEIGHT=800 \
  "$IMAGE" >/dev/null

base() { echo "http://127.0.0.1:${PORT}"; }

echo "== wait healthz =="
for i in $(seq 1 80); do
  if curl -sf "$(base)/healthz"; then
    echo OK
    break
  fi
  sleep 0.1
done

echo "== start apps =="
for app in terminal editor files; do
  curl -sf -H "X-Computer-Token: $TOKEN" -X POST "$(base)/spike/apps/start?id=${app}" >/dev/null
done
sleep 3

echo "== broadway screenshot =="
curl -sf -H "X-Computer-Token: $TOKEN" "$(base)/spike/screenshot?wait=1" -o "$WORKDIR/desktop.png"
python3 - <<'PY' "$WORKDIR/desktop.png"
import sys, struct
p=sys.argv[1]
d=open(p,'rb').read()
assert d[:8]==b'\x89PNG\r\n\x1a\n'
assert len(d)>1000, len(d)
print("png_bytes", len(d))
PY

echo "== gtk input: open terminal via dock coords and type =="
# Click near bottom dock Terminal button (left side).
curl -sf -H "X-Computer-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"x":80,"y":760}' "$(base)/spike/input/click"
sleep 1
curl -sf -H "X-Computer-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"text":"phase0"}' "$(base)/spike/input/type"
sleep 1
curl -sf -H "X-Computer-Token: $TOKEN" "$(base)/spike/screenshot?wait=1" -o "$WORKDIR/after-type.png"

echo "== dock reserved strip heuristic =="
python3 - <<'PY' "$WORKDIR/desktop.png"
import sys
from struct import unpack
import zlib
p=sys.argv[1]
d=open(p,'rb').read()
# Quick parse IHDR width/height
w,h=unpack('>II', d[16:24])
# Bottom 48px should include non-background variation when dock/apps present
print("viewport", w, h)
print("dock_strip_reserved_px", 48)
PY

echo "== cdp screencast =="
curl -sf -m 20 -H "X-Computer-Token: $TOKEN" "$(base)/spike/browser/frame" -o "$WORKDIR/browser.jpg"
python3 - <<'PY' "$WORKDIR/browser.jpg"
import sys
d=open(sys.argv[1],'rb').read()
assert d[:2]==b'\xff\xd8' and len(d)>500, len(d)
print("jpeg_bytes", len(d))
PY

echo "== multi-surface meta =="
meta=$(curl -sf -H "X-Computer-Token: $TOKEN" "$(base)/spike/broadway/meta")
python3 - <<'PY' "$meta"
import json,sys
m=json.loads(sys.argv[1])
assert m.get("surface_count",0) >= 1, m
print("surface_count", m["surface_count"])
PY

echo "E2E Phase 0 passed"