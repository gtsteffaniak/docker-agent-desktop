#!/usr/bin/env bash
# Measures image size, container memory (idle and browser-open), and boot-to-healthy time.
set -euo pipefail

IMAGE="${DC_IMAGE:-docker-computer:dev}"
TOKEN="${COMPUTER_AGENT_TOKEN:-budget-token}"
NAME="dc-budget-$$"
DOCKER="${DOCKER:-docker}"

idle_budget_mb=60
browser_budget_mb=400
boot_budget_s=3

cleanup() { $DOCKER rm -f "$NAME" "${NAME}-browser" >/dev/null 2>&1 || true; }
trap cleanup EXIT

now_ms() { python3 -I -c 'import time; print(int(time.time() * 1000))'; }

# wait_healthy NAME: prints the published host port once /healthz answers. Returns 1 on timeout.
wait_healthy() {
  local port
  port=$($DOCKER port "$1" 8080 | head -n 1 | awk -F: '{print $NF}')
  for _ in $(seq 1 100); do
    if curl -sf "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
      echo "$port"
      return 0
    fi
    sleep 0.05
  done
  return 1
}

# mem_mib NAME: current memory usage in MiB, from docker stats.
mem_mib() {
  $DOCKER stats "$1" --no-stream --format '{{.MemUsage}}' | awk -F/ '{print $1}' | python3 -I -c '
import re, sys
s = sys.stdin.read().strip()
m = re.match(r"([\d.]+)\s*([KMG]i?B|B)", s)
if not m:
    sys.exit(1)
n, unit = float(m.group(1)), m.group(2)
mult = {"B": 1/1048576, "KiB": 1/1024, "MiB": 1, "GiB": 1024, "KB": 1/1024, "MB": 1, "GB": 1024}[unit]
print(int(n * mult))
'
}

image_bytes=$($DOCKER image inspect "$IMAGE" --format='{{.Size}}')
image_mb=$((image_bytes / 1024 / 1024))
echo "MEASURED image_size_mb=$image_mb"

# Boot-to-healthy, idle (no browser, no apps).
start=$(now_ms)
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e DC_BROWSER_START=off \
  -e DC_AUTOSTART_APPS=0 \
  "$IMAGE" >/dev/null
port=$(wait_healthy "$NAME") || { echo "FAIL container never became healthy"; exit 1; }
boot_s=$(python3 -I -c "print(f'{($(now_ms) - $start) / 1000:.3f}')")
echo "MEASURED boot_to_healthy_s=$boot_s"

sleep 3
idle_mb=$(mem_mib "$NAME")
echo "MEASURED idle_rss_mb=$idle_mb"

# Browser open: Chromium plus the GTK apps, with a frame received.
$DOCKER rm -f "$NAME" >/dev/null
NAME="${NAME}-browser"
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e DC_BROWSER_START=boot \
  -e DC_AUTOSTART_APPS=1 \
  "$IMAGE" >/dev/null
port=$(wait_healthy "$NAME") || { echo "FAIL container never became healthy"; exit 1; }
for _ in $(seq 1 100); do
  if curl -sf -m 5 -H "X-Computer-Token: $TOKEN" "http://127.0.0.1:${port}/api/browser/frame" >/dev/null 2>&1; then
    break
  fi
  sleep 0.15
done
sleep 2
browser_mb=$(mem_mib "$NAME")
echo "MEASURED browser_open_rss_mb=$browser_mb"

fail=0
if (( idle_mb > idle_budget_mb )); then
  echo "FAIL idle ${idle_mb}MB > ${idle_budget_mb}MB"; fail=1
fi
if (( browser_mb > browser_budget_mb )); then
  echo "FAIL browser ${browser_mb}MB > ${browser_budget_mb}MB"; fail=1
fi
if python3 -I -c "import sys; sys.exit(0 if float('$boot_s') <= $boot_budget_s else 1)"; then :; else
  echo "FAIL boot ${boot_s}s > ${boot_budget_s}s"; fail=1
fi
exit $fail
