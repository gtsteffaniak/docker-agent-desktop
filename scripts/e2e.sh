#!/usr/bin/env bash
# End-to-end checks: health, auth, the web desktop, the app registry, per-app Broadway pages,
# and the browser window's screencast, navigation state and history.
set -euo pipefail

IMAGE="${DC_IMAGE:-docker-computer:dev}"
TOKEN="${COMPUTER_AGENT_TOKEN:-e2e-token}"
NAME="dc-e2e-$$"
DOCKER="${DOCKER:-docker}"
WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"; $DOCKER rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

fail() { echo "FAIL: $*" >&2; $DOCKER logs "$NAME" 2>&1 | tail -n 50 >&2 || true; exit 1; }

# Port 0 lets Docker pick a free host port on loopback only.
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e COMPUTER_AGENT_TOKEN="$TOKEN" \
  -e DC_BROWSER_START=boot \
  -e DESKTOP_WIDTH=1280 \
  -e DESKTOP_HEIGHT=800 \
  "$IMAGE" >/dev/null

hostport=$($DOCKER port "$NAME" 8080 | head -n 1 | awk -F: '{print $NF}')
[[ -n "$hostport" ]] || fail "could not determine published port"
BASE="http://127.0.0.1:${hostport}"
H=(-H "X-Computer-Token: $TOKEN")

echo "== wait healthz =="
healthy=0
for _ in $(seq 1 100); do
  if curl -sf "$BASE/healthz" >/dev/null; then healthy=1; break; fi
  sleep 0.1
done
[[ $healthy == 1 ]] || fail "healthz never answered"
echo OK

echo "== auth =="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/apps")
[[ "$code" == 401 ]] || fail "unauthenticated request returned $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Computer-Token: wrong" "$BASE/api/apps")
[[ "$code" == 401 ]] || fail "wrong token returned $code, want 401"
echo OK

echo "== desktop =="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/desktop/")
[[ "$code" == 401 ]] || fail "desktop without token returned $code, want 401"
curl -s -c "$WORKDIR/jar" -o /dev/null "$BASE/portal/?token=$TOKEN"
code=$(curl -s -b "$WORKDIR/jar" -o "$WORKDIR/desktop.html" -w '%{http_code}' "$BASE/desktop/")
[[ "$code" == 200 ]] || fail "desktop with cookie returned $code, want 200"
grep -q 'id="dock"' "$WORKDIR/desktop.html" || fail "desktop page has no dock"
for f in desktop.js desktop.css; do
  code=$(curl -s -b "$WORKDIR/jar" -o /dev/null -w '%{http_code}' "$BASE/desktop/$f")
  [[ "$code" == 200 ]] || fail "desktop asset $f returned $code"
done
echo OK

echo "== app registry =="
curl -sf "${H[@]}" "$BASE/api/apps" -o "$WORKDIR/apps.json" || fail "list apps"
python3 -I - "$WORKDIR/apps.json" <<'PY' || fail "registry should list terminal, editor, files and browser with ports"
import json, sys
apps = {a["id"]: a for a in json.load(open(sys.argv[1]))["apps"]}
assert apps["terminal"]["port"] == 8081 and apps["editor"]["port"] == 8083 and apps["files"]["port"] == 8082, apps
assert apps["browser"]["port"] == 0, apps["browser"]
print("apps", sorted(apps))
PY
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -X POST "$BASE/api/apps/start?id=not-registered")
[[ "$code" == 400 ]] || fail "unregistered app returned $code, want 400"

echo "== start gtk apps and load each app page =="
for app in terminal editor files; do
  curl -sf "${H[@]}" -X POST "$BASE/api/apps/start?id=${app}" >/dev/null || fail "start $app"
done
for app in terminal editor files; do
  ok=0
  for _ in $(seq 1 60); do
    body=$(curl -s -b "$WORKDIR/jar" "$BASE/apps/${app}/" || true)
    if grep -q 'keys={F11:1,F12:1}' <<<"$body"; then ok=1; break; fi
    sleep 0.5
  done
  [[ $ok == 1 ]] || fail "app $app page never came up with the keys script"
done
echo OK

echo "== cdp screencast =="
curl -sf -m 30 "${H[@]}" "$BASE/api/browser/frame" -o "$WORKDIR/browser.jpg" || fail "browser frame"
python3 -I - "$WORKDIR/browser.jpg" <<'PY' || fail "browser frame is not a JPEG"
import sys
d = open(sys.argv[1], 'rb').read()
assert d[:2] == b'\xff\xd8' and len(d) > 500, len(d)
print("jpeg_bytes", len(d))
PY
seq=$(curl -s -D - -o /dev/null "${H[@]}" "$BASE/api/browser/frame?since=0" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-frame-seq"{print $2}')
[[ -n "$seq" && "$seq" -ge 1 ]] || fail "frame did not carry X-Frame-Seq"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/api/browser/frame?since=$seq")
[[ "$code" == 204 ]] || fail "no newer frame should return 204, got $code"
echo OK

echo "== browser navigation and address state =="
# localhost resolves to the agent itself, so this needs no network access.
curl -sf "${H[@]}" -H 'Content-Type: application/json' -d '{"url":"http://localhost:8080/healthz"}' \
  "$BASE/api/browser/navigate" >/dev/null || fail "navigate"
url=""
for _ in $(seq 1 40); do
  url=$(curl -sf "${H[@]}" "$BASE/api/browser/state" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["url"])')
  [[ "$url" == *"/healthz" ]] && break
  sleep 0.25
done
[[ "$url" == *"/healthz" ]] || fail "state url should follow the navigation, got '$url'"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H 'Content-Type: application/json' -d '{"url":"javascript:alert(1)"}' "$BASE/api/browser/navigate")
[[ "$code" == 400 ]] || fail "javascript: URL returned $code, want 400"
curl -sf "${H[@]}" -X POST "$BASE/api/browser/back" >/dev/null || fail "back"
echo OK

echo "== v1 api: sessions, shell, files, health, apps, browser, display =="
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/v1/meta")
[[ "$code" == 200 ]] || fail "v1/meta returned $code, want 200"
curl -sf "${H[@]}" "$BASE/v1/meta" | grep -q '"api_version":"1"' || fail "v1/meta is not api_version 1"
# Mutating calls without a session are 409.
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"command":"echo hi"}' "$BASE/v1/shell/exec")
[[ "$code" == 409 ]] || fail "shell exec without a session returned $code, want 409"
SID=$(curl -sf "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"holder":{"kind":"agent","name":"e2e"}}' "$BASE/v1/session/acquire" \
  | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[[ -n "$SID" ]] || fail "session acquire returned no id"
S=(-H "X-Dc-Session: $SID")
curl -sf "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' -d '{"command":"echo e2e-ok"}' \
  "$BASE/v1/shell/exec" | grep -q 'e2e-ok' || fail "shell exec did not echo"
curl -sf "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' -d "{\"session_id\":\"$SID\"}" \
  "$BASE/v1/session/keepalive" >/dev/null || fail "keepalive"
curl -sf "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' \
  -d '{"path":"e2e.txt","content":"hello-v1","encoding":"text"}' "$BASE/v1/files/write" >/dev/null || fail "files write"
curl -sf "${H[@]}" "$BASE/v1/files/read?path=e2e.txt" | grep -q 'hello-v1' || fail "files read did not return the written content"
curl -sf "${H[@]}" "$BASE/v1/files/list?path=" | grep -q 'e2e.txt' || fail "files list missing e2e.txt"
curl -sf "${H[@]}" "${S[@]}" -X DELETE "$BASE/v1/files?path=e2e.txt" >/dev/null || fail "files delete"
curl -sf "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' -d '{"expression":"1+1"}' \
  "$BASE/v1/browser/evaluate" | grep -q '"value":2' || fail "browser evaluate did not return 2"
curl -sf "${H[@]}" "$BASE/v1/display/screenshot" -o "$WORKDIR/v1-display.png" || fail "v1 display screenshot"
python3 -I - "$WORKDIR/v1-display.png" <<'PY' || fail "v1 display screenshot is not a PNG"
import sys
d = open(sys.argv[1], 'rb').read()
assert d[:4] == b'\x89PNG' and len(d) > 100, len(d)
print("png_bytes", len(d))
PY
curl -sf "${H[@]}" "$BASE/v1/apps" | python3 -I -c 'import json,sys; a={x["id"]:x for x in json.load(sys.stdin)["apps"]}; assert a["terminal"]["running"], a' \
  || fail "v1/apps should report terminal running"
curl -sf "${H[@]}" "$BASE/v1/session/events" | grep -q 'shell.exec' || fail "session events missing shell.exec"
curl -sf "${H[@]}" "$BASE/v1/health" | python3 -I -c 'import json,sys; h=json.load(sys.stdin); assert h["status"]=="ok", h; assert h["checks"]["chromium"]=="ok", h' \
  || fail "v1/health is not ok"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/mcp")
[[ "$code" == 405 ]] || fail "GET /mcp returned $code, want 405"
curl -sf "${H[@]}" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}' \
  "$BASE/mcp" | grep -q '"result"' || fail "MCP initialize did not return a result"
curl -sf "${H[@]}" "${S[@]}" -X POST -H 'Content-Type: application/json' -d "{\"session_id\":\"$SID\"}" \
  "$BASE/v1/session/release" >/dev/null || fail "session release"
echo OK

echo "== gtk app and broadway display processes running =="
$DOCKER exec "$NAME" sh -c 'pgrep -x lxterminal >/dev/null && pgrep -x broadwayd >/dev/null' || fail "terminal or broadwayd not running"
echo OK

echo "E2E passed"
