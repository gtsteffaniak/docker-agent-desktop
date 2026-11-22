#!/usr/bin/env bash
# Measures image size, container memory (idle, browser-open, and browser-open while dad-playwright
# records), and boot-to-healthy time.
#
# Units: image size is reported in bytes, MB (10^6) and MiB (2^20). Memory is the container's cgroup
# usage from docker stats, in MiB; the memory budgets are MiB.
set -euo pipefail

IMAGE="${DAD_IMAGE:-docker-agent-desktop:dev}"
TOKEN="${DAD_TOKEN:-budget-token}"
NAME="dad-budget-$$"
DOCKER="${DOCKER:-docker}"

idle_budget_mib=60
browser_budget_mib=400
boot_budget_s=3

# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap
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

# docker image inspect .Size is the uncompressed size of all layers on this daemon (overlay2).
image_bytes=$($DOCKER image inspect "$IMAGE" --format='{{.Size}}')
echo "MEASURED image_size_bytes=$image_bytes image_size_MB=$((image_bytes / 1000000)) image_size_MiB=$((image_bytes / 1048576)) (uncompressed)"

# Boot-to-healthy, idle (no browser, no apps).
start=$(now_ms)
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e DAD_TOKEN="$TOKEN" \
  -e DAD_BROWSER_START=off \
  -e DAD_AUTOSTART_APPS=0 \
  "$IMAGE" >/dev/null
port=$(wait_healthy "$NAME") || { echo "FAIL container never became healthy"; exit 1; }
boot_s=$(python3 -I -c "print(f'{($(now_ms) - $start) / 1000:.3f}')")
echo "MEASURED boot_to_healthy_s=$boot_s"

sleep 3
idle_mib=$(mem_mib "$NAME")
echo "MEASURED idle_mem_MiB=$idle_mib"

# Browser open: Chromium plus the GTK apps, with a frame received.
$DOCKER rm -f "$NAME" >/dev/null
NAME="${NAME}-browser"
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e DAD_TOKEN="$TOKEN" \
  -e DAD_BROWSER_START=boot \
  -e DAD_AUTOSTART_APPS=1 \
  "$IMAGE" >/dev/null
port=$(wait_healthy "$NAME") || { echo "FAIL container never became healthy"; exit 1; }
for _ in $(seq 1 100); do
  if curl -sf -m 5 -H "X-Dad-Token: $TOKEN" "http://127.0.0.1:${port}/v1/browser/frame" >/dev/null 2>&1; then
    break
  fi
  sleep 0.15
done
sleep 2
browser_mib=$(mem_mib "$NAME")
echo "MEASURED browser_open_mem_MiB=$browser_mib"

# Same container while dad-playwright records the live browser (Node + playwright-core attached over
# CDP, tracing on). Informational: recording is on demand, so it has no budget of its own.
$DOCKER exec -d "$NAME" dad-playwright record --name budget
for _ in $(seq 1 100); do
  $DOCKER exec "$NAME" test -e /home/bot/recordings/budget/recorder.pid && break
  sleep 0.1
done
sleep 2
recording_mib=$(mem_mib "$NAME")
echo "MEASURED browser_recording_mem_MiB=$recording_mib"
$DOCKER exec "$NAME" dad-playwright stop budget >/dev/null || { echo "FAIL dad-playwright stop"; exit 1; }

fail=0
if (( idle_mib > idle_budget_mib )); then
  echo "FAIL idle ${idle_mib}MiB > ${idle_budget_mib}MiB"; fail=1
fi
if (( browser_mib > browser_budget_mib )); then
  echo "FAIL browser ${browser_mib}MiB > ${browser_budget_mib}MiB"; fail=1
fi
if python3 -I -c "import sys; sys.exit(0 if float('$boot_s') <= $boot_budget_s else 1)"; then :; else
  echo "FAIL boot ${boot_s}s > ${boot_budget_s}s"; fail=1
fi
exit $fail
