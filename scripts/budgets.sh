#!/usr/bin/env bash
# Measures image size, container RSS (idle and browser-open), and boot-to-healthy time.
set -euo pipefail

IMAGE="${SPIKE_IMAGE:-docker-computer:dev}"
TOKEN="${COMPUTER_AGENT_TOKEN:-phase0-budget-token}"
NAME="dc-phase0-budget-$$"
DOCKER="${DOCKER:-sudo docker}"

cleanup() { $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

idle_budget_mb=60
browser_budget_mb=400
boot_budget_s=3
image_delta_budget_mb=80

legacy_mb="${LEGACY_IMAGE_MB:-0}"
image_bytes=$($DOCKER image inspect "$IMAGE" --format='{{.Size}}')
image_mb=$((image_bytes / 1024 / 1024))

echo "MEASURED image_size_mb=$image_mb"

start_ms=$(date +%s%3N)
$DOCKER run -d --name "$NAME" \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e SPIKE_START_CHROMIUM=0 \
  -e SPIKE_AUTOSTART_APPS=0 \
  "$IMAGE" >/dev/null

for i in $(seq 1 60); do
  if curl -sf "http://127.0.0.1:8080/healthz" 2>/dev/null; then
    break
  fi
  cid=$($DOCKER port "$NAME" 8080 2>/dev/null | head -1 || true)
  if [[ -n "${cid:-}" ]]; then
    hostport="${cid##*:}"
    if curl -sf "http://127.0.0.1:${hostport}/healthz"; then
      export HEALTH_PORT="$hostport"
      break
    fi
  fi
  sleep 0.1
done

# Publish port explicitly if default curl failed (container health via docker inspect)
if [[ -z "${HEALTH_PORT:-}" ]]; then
  $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true
  start_ms=$(date +%s%3N)
  $DOCKER run -d --name "$NAME" -p 18080:8080 \
    -e COMPUTER_AGENT_TOKEN="$TOKEN" \
    -e SPIKE_START_CHROMIUM=0 \
    -e SPIKE_AUTOSTART_APPS=0 \
    "$IMAGE" >/dev/null
  for i in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:18080/healthz"; then
      HEALTH_PORT=18080
      break
    fi
    sleep 0.1
  done
fi

end_ms=$(date +%s%3N)
boot_ms=$((end_ms - start_ms))
boot_s=$(awk "BEGIN {printf \"%.3f\", $boot_ms/1000}")
echo "MEASURED boot_to_healthy_s=$boot_s"

mem_usage_to_kb() {
  local raw="$1"
  local num unit
  num=$(echo "$raw" | sed 's/[^0-9.].*$//')
  unit=$(echo "$raw" | sed 's/^[0-9.]* *//')
  case "$unit" in
    GiB) awk "BEGIN {printf \"%d\", $num * 1048576}" ;;
    MiB) awk "BEGIN {printf \"%d\", $num * 1024}" ;;
    KiB) awk "BEGIN {printf \"%d\", $num}" ;;
    B) awk "BEGIN {printf \"%d\", $num / 1024}" ;;
    *) echo 0 ;;
  esac
}
idle_raw=$($DOCKER stats "$NAME" --no-stream --format '{{.MemUsage}}' | awk -F/ '{print $1}' | xargs)
idle_rss_kb=$(mem_usage_to_kb "$idle_raw")
idle_mb=$((idle_rss_kb / 1024))
echo "MEASURED idle_rss_mb=$idle_mb"

$DOCKER rm -f "$NAME" >/dev/null
NAME="${NAME}-browser"
start_ms=$(date +%s%3N)
$DOCKER run -d --name "$NAME" -p 18081:8080 \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e SPIKE_START_CHROMIUM=1 \
  -e SPIKE_AUTOSTART_APPS=1 \
  "$IMAGE" >/dev/null
for i in $(seq 1 80); do
  if curl -sf -H "X-Computer-Token: $TOKEN" "http://127.0.0.1:18081/spike/browser/frame" >/dev/null 2>&1; then
    break
  fi
  sleep 0.15
done
sleep 1
browser_raw=$($DOCKER stats "$NAME" --no-stream --format '{{.MemUsage}}' | awk -F/ '{print $1}' | xargs)
browser_rss_kb=$(mem_usage_to_kb "$browser_raw")
browser_mb=$((browser_rss_kb / 1024))
echo "MEASURED browser_open_rss_mb=$browser_mb"

fail=0
if [[ "$idle_mb" -gt 0 ]]; then
  awk -v v="$idle_mb" -v b="$idle_budget_mb" 'BEGIN{ if (v>b) exit 1 }' || { echo "FAIL idle ${idle_mb}MB > ${idle_budget_mb}MB"; fail=1; }
else
  echo "WARN could not parse idle RSS"
  fail=1
fi
if [[ "$browser_mb" -gt 0 ]]; then
  awk -v v="$browser_mb" -v b="$browser_budget_mb" 'BEGIN{ if (v>b) exit 1 }' || { echo "FAIL browser ${browser_mb}MB > ${browser_budget_mb}MB"; fail=1; }
else
  echo "WARN could not parse browser RSS"
  fail=1
fi
awk -v v="$boot_s" -v b="$boot_budget_s" 'BEGIN{ if (v>b) exit 1 }' || { echo "FAIL boot ${boot_s}s > ${boot_budget_s}s"; fail=1; }
if [[ "$legacy_mb" -gt 0 ]]; then
  delta=$((image_mb - legacy_mb))
  echo "MEASURED image_delta_mb=$delta"
  if [[ "$delta" -gt "$image_delta_budget_mb" ]]; then
    echo "FAIL image delta ${delta}MB > ${image_delta_budget_mb}MB"
    fail=1
  fi
else
  echo "MEASURED image_delta_mb=skipped (no LEGACY_IMAGE_MB)"
fi

exit $fail