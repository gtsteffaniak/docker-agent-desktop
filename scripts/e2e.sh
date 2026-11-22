#!/usr/bin/env bash
# End-to-end checks against the real image (real Chromium and broadwayd, no fakes): health, auth,
# the web desktop, the app registry, per-app Broadway pages, the browser window's screencast,
# navigation state and history, network exposure, and then the full /v1 API and MCP server
# (scripts/e2e_v1.py).
set -euo pipefail

IMAGE="${DAD_IMAGE:-docker-agent-desktop:dev}"
TOKEN="${DAD_TOKEN:-e2e-token}"
NAME="dad-e2e-$$"
DOCKER="${DOCKER:-docker}"
WORKDIR=$(mktemp -d)
HOMEVOL="dad-e2e-home-$$"
trap 'rm -rf "$WORKDIR"; $DOCKER rm -f "$NAME" "$NAME-tier" >/dev/null 2>&1 || true; $DOCKER volume rm -f "$HOMEVOL" >/dev/null 2>&1 || true' EXIT

fail() { echo "FAIL: $*" >&2; $DOCKER logs "$NAME" 2>&1 | tail -n 50 >&2 || true; exit 1; }

# Port 0 lets Docker pick a free host port on loopback only.
$DOCKER run -d --name "$NAME" -p 127.0.0.1::8080 \
  -e DAD_TOKEN="$TOKEN" \
  -e DAD_BROWSER_START=boot \
  -e DAD_BROWSER_HOME=about:blank \
  -e DESKTOP_WIDTH=1280 \
  -e DESKTOP_HEIGHT=800 \
  "$IMAGE" >/dev/null

hostport=$($DOCKER port "$NAME" 8080 | head -n 1 | awk -F: '{print $NF}')
[[ -n "$hostport" ]] || fail "could not determine published port"
BASE="http://127.0.0.1:${hostport}"
H=(-H "X-Dad-Token: $TOKEN")

echo "== wait healthz =="
healthy=0
for _ in $(seq 1 100); do
  if curl -sf "$BASE/healthz" >/dev/null; then healthy=1; break; fi
  sleep 0.1
done
[[ $healthy == 1 ]] || fail "healthz never answered"
echo OK

echo "== auth =="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/apps")
[[ "$code" == 401 ]] || fail "unauthenticated request returned $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Dad-Token: wrong" "$BASE/v1/apps")
[[ "$code" == 401 ]] || fail "wrong token returned $code, want 401"
# The pre-rename header was removed with the /api surface: it must not authenticate.
code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Computer-Token: $TOKEN" "$BASE/v1/apps")
[[ "$code" == 401 ]] || fail "legacy X-Computer-Token returned $code, want 401"
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

echo "== acquire a session (one lease for the whole run) =="
SID=$(curl -sf "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"holder":{"kind":"agent","name":"e2e"}}' "$BASE/v1/session/acquire" \
  | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[[ -n "$SID" ]] || fail "session acquire returned no id"
S=(-H "X-Dad-Session: $SID")
echo OK

echo "== app registry =="
curl -sf "${H[@]}" "$BASE/v1/apps" -o "$WORKDIR/apps.json" || fail "list apps"
python3 -I - "$WORKDIR/apps.json" <<'PY' || fail "registry should list terminal, editor, files and browser with ports"
import json, sys
apps = {a["id"]: a for a in json.load(open(sys.argv[1]))["apps"]}
assert apps["terminal"]["port"] == 8081 and apps["editor"]["port"] == 8083 and apps["files"]["port"] == 8082, apps
assert apps["browser"]["port"] == 0, apps["browser"]
print("apps", sorted(apps))
PY
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "${S[@]}" -X POST "$BASE/v1/apps/start?id=not-registered")
[[ "$code" == 400 ]] || fail "unregistered app returned $code, want 400"

echo "== start gtk apps and load each app page =="
for app in terminal editor files; do
  curl -sf "${H[@]}" "${S[@]}" -X POST "$BASE/v1/apps/start?id=${app}" >/dev/null || fail "start $app"
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
# Chromium boots in the background: the frame route is 503 until its screencast is attached.
ok=0
for _ in $(seq 1 120); do
  if curl -sf -m 30 "${H[@]}" "$BASE/v1/browser/frame" -o "$WORKDIR/browser.jpg"; then ok=1; break; fi
  sleep 0.25
done
[[ $ok == 1 ]] || fail "browser frame"
python3 -I - "$WORKDIR/browser.jpg" <<'PY' || fail "browser frame is not a JPEG"
import sys
d = open(sys.argv[1], 'rb').read()
assert d[:2] == b'\xff\xd8' and len(d) > 500, len(d)
print("jpeg_bytes", len(d))
PY
seq=$(curl -s -D - -o /dev/null "${H[@]}" "$BASE/v1/browser/frame?since=0" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-frame-seq"{print $2}')
[[ -n "$seq" && "$seq" -ge 1 ]] || fail "frame did not carry X-Frame-Seq"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/v1/browser/frame?since=$seq")
[[ "$code" == 204 ]] || fail "no newer frame should return 204, got $code"
echo OK

echo "== browser navigation and address state =="
# localhost resolves to the agent itself, so this needs no network access.
curl -sf "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' -d '{"url":"http://localhost:8080/healthz"}' \
  "$BASE/v1/browser/navigate" >/dev/null || fail "navigate"
url=""
for _ in $(seq 1 40); do
  url=$(curl -sf "${H[@]}" "$BASE/v1/browser/state" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["url"])')
  [[ "$url" == *"/healthz" ]] && break
  sleep 0.25
done
[[ "$url" == *"/healthz" ]] || fail "state url should follow the navigation, got '$url'"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "${S[@]}" -H 'Content-Type: application/json' -d '{"url":"javascript:alert(1)"}' "$BASE/v1/browser/navigate")
[[ "$code" == 400 ]] || fail "javascript: URL returned $code, want 400"
curl -sf "${H[@]}" "${S[@]}" -X POST "$BASE/v1/browser/back" >/dev/null || fail "back"
echo OK

echo "== v1 api: sessions, shell, files, health, apps, browser, display =="
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/v1/meta")
[[ "$code" == 200 ]] || fail "v1/meta returned $code, want 200"
curl -sf "${H[@]}" "$BASE/v1/meta" | grep -q '"api_version":"1"' || fail "v1/meta is not api_version 1"
# Mutating calls without a session are 409, and the removed legacy session
# header does not count as one.
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"command":"echo hi"}' "$BASE/v1/shell/exec")
[[ "$code" == 409 ]] || fail "shell exec without a session returned $code, want 409"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H "X-Dc-Session: $SID" -H 'Content-Type: application/json' \
  -d '{"command":"echo hi"}' "$BASE/v1/shell/exec")
[[ "$code" == 409 ]] || fail "shell exec with only the legacy session header returned $code, want 409"
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

echo "== browser stream gate, wait and takeover =="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/browser/stream")
[[ "$code" == 401 ]] || fail "stream without a token returned $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" "$BASE/v1/browser/stream?session=ses_wrong")
[[ "$code" == 409 ]] || fail "stream with an unknown session returned $code, want 409"
# Wait reports the free lease immediately; takeover needs explicit force.
curl -sf "${H[@]}" -H 'Content-Type: application/json' -d '{"timeout_ms":1000}' \
  "$BASE/v1/session/wait" | grep -q '"free":true' || fail "wait on a free lease did not report free"
HSID=$(curl -sf "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"holder":{"kind":"agent","name":"e2e-holder"}}' "$BASE/v1/session/acquire" \
  | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[[ -n "$HSID" ]] || fail "holder acquire returned no id"
curl -sf "${H[@]}" -H 'Content-Type: application/json' -d '{"timeout_ms":1000}' \
  "$BASE/v1/session/wait" | grep -q '"free":false' || fail "wait on a held lease did not report held"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"holder":{"kind":"agent","name":"e2e-taker"},"force":false}' "$BASE/v1/session/takeover")
[[ "$code" == 400 ]] || fail "takeover without force returned $code, want 400"
TSID=$(curl -sf "${H[@]}" -H 'Content-Type: application/json' \
  -d '{"holder":{"kind":"agent","name":"e2e-taker"},"force":true}' "$BASE/v1/session/takeover" \
  | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["session"]["id"])')
[[ -n "$TSID" ]] || fail "takeover returned no session id"
code=$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H "X-Dad-Session: $HSID" -H 'Content-Type: application/json' \
  -d '{"command":"echo hi"}' "$BASE/v1/shell/exec")
[[ "$code" == 409 ]] || fail "evicted holder could still mutate: $code"
curl -sf "${H[@]}" -H 'Content-Type: application/json' -d "{\"session_id\":\"$TSID\"}" \
  "$BASE/v1/session/release" >/dev/null || fail "takeover session release"
echo OK

echo "== gtk app and broadway display processes running =="
$DOCKER exec "$NAME" sh -c 'pgrep -x lxterminal >/dev/null && pgrep -x broadwayd >/dev/null' || fail "terminal or broadwayd not running"
echo OK

echo "== network exposure: only the agent port is reachable from other containers =="
# Broadway has no authentication, so every broadwayd must listen on loopback only. Probe the
# container's address from a second container on the same network.
ip=$($DOCKER inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$NAME")
[[ -n "$ip" ]] || fail "no container IP"
probe() { $DOCKER run --rm --entrypoint curl "$IMAGE" -s -o /dev/null -m 3 -w '%{http_code}' "http://$ip:$1/$2" || true; }
code=$(probe 8080 healthz)
[[ "$code" == 200 ]] || fail "agent port 8080 should be reachable from the network, got '$code'"
for port in 8081 8082 8083; do
  code=$(probe "$port" "")
  [[ "$code" == 000 ]] || fail "broadwayd port $port is reachable from the network (HTTP $code)"
done
echo OK

echo "== v1 api and MCP (real Chromium + broadwayd) =="
python3 -I "$(dirname "$0")/e2e_v1.py" "$BASE" "$TOKEN" "$NAME" "$DOCKER" || fail "v1/MCP e2e"

echo "== rootless package tier: dad-apt installs as uid 1000 into a home volume, and it persists =="
# tier_run: a fresh container (the real entrypoint and agent) with the home volume mounted, then a
# command as the default user. Each call removes the previous container, so the second call proves
# the install survived in the volume alone.
tier_run() {
  $DOCKER rm -f "$NAME-tier" >/dev/null 2>&1 || true
  $DOCKER run -d --name "$NAME-tier" -v "$HOMEVOL:/home/bot" \
    -e DAD_TOKEN="$TOKEN" -e DAD_BROWSER_START=off -e DAD_AUTOSTART_APPS=0 "$IMAGE" >/dev/null
  $DOCKER exec "$NAME-tier" bash -c "$1"
}
# shellcheck disable=SC2016 # expands inside the container
tier_run '
  set -eux
  [ "$(id -u)" = 1000 ]
  ! command -v jq && ! command -v figlet
  dad-apt install jq figlet
  [ "$(jq -cn "{tier:1}")" = "{\"tier\":1}" ]       # library (libjq/libonig) bound in
  figlet hi | grep -q "|_|"                            # data dir /usr/share/figlet + alternative
  dad-apt exec ls /usr/share/figlet | grep -q standard.flf
  [ ! -e /usr/bin/jq ] && [ ! -e /usr/share/figlet ]   # the image itself is untouched
' >"$WORKDIR/tier1.log" 2>&1 || { cat "$WORKDIR/tier1.log" >&2; fail "dad-apt install as uid 1000"; }
# shellcheck disable=SC2016 # expands inside the container
tier_run '
  set -eux
  dad-apt list | grep -q "^jq"
  [ "$(jq -cn "{persisted:true}")" = "{\"persisted\":true}" ]
  figlet ok | grep -q "|"
  dad-apt remove figlet
  ! command -v figlet
  jq --version
' >"$WORKDIR/tier2.log" 2>&1 || { cat "$WORKDIR/tier2.log" >&2; fail "dad-apt packages did not persist in the home volume"; }
$DOCKER rm -f "$NAME-tier" >/dev/null
echo OK

echo "E2E passed"
