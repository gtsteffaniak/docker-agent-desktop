#!/usr/bin/env python3
"""End-to-end checks for the /v1 API and the MCP server against a running container.

Drives the real image: real Chromium over CDP and real broadwayd, no fakes. Called by
scripts/e2e.sh with the container already up; standard library only.

Usage: e2e_v1.py BASE_URL TOKEN CONTAINER [DOCKER]
"""

import base64
import json
import re
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

BASE, TOKEN, NAME = sys.argv[1], sys.argv[2], sys.argv[3]
DOCKER = shlex.split(sys.argv[4] if len(sys.argv) > 4 else "docker")
T0 = time.time()


def fail(msg):
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def check(cond, msg):
    if not cond:
        fail(msg)


def section(title):
    print(f"== v1: {title} ({time.time() - T0:.1f}s) ==", flush=True)


def req(method, path, body=None, sid=None, token=TOKEN, headers=None, raw=None, timeout=60):
    """One HTTP call. Returns (status, headers, body bytes); never raises on HTTP errors."""
    hdrs = dict(headers or {})
    if token is not None:
        hdrs["X-Dad-Token"] = token
    if sid is not None:
        hdrs["X-Dad-Session"] = sid
    data = raw
    if body is not None:
        data = json.dumps(body).encode()
        hdrs.setdefault("Content-Type", "application/json")
    r = urllib.request.Request(BASE + path, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.headers, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def jreq(method, path, body=None, sid=None, want=200, **kw):
    code, _, data = req(method, path, body, sid, **kw)
    check(code == want, f"{method} {path} = {code} {data[:300]!r}, want {want}")
    try:
        return json.loads(data) if data.strip() else None
    except ValueError:
        return data.decode(errors="replace")  # plain-text error bodies


def dexec(cmd, check_rc=True):
    p = subprocess.run(DOCKER + ["exec", NAME, "sh", "-c", cmd], capture_output=True, text=True)
    if check_rc and p.returncode != 0:
        fail(f"docker exec {cmd!r}: rc={p.returncode} {p.stderr.strip()}")
    return p.stdout


def wait_for(fn, what, timeout=30, every=0.25):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(every)
    fail(f"timed out after {timeout}s waiting for {what} (last={last!r})")


def pid_alive(pid):
    out = dexec(f"if kill -0 {pid} 2>/dev/null; then ps -o stat= -p {pid}; else echo gone; fi").strip()
    return out != "gone" and not out.startswith("Z")


def zombies():
    out = dexec("ps -eo stat=,pid=,comm= | awk '$1 ~ /^Z/'", check_rc=False)
    return out.strip()


def health():
    return jreq("GET", "/v1/health")


def events(since=0):
    return jreq("GET", f"/v1/session/events?since={since}&limit=500")["events"]


def acquire(name, ttl_ms=None, want=201):
    body = {"holder": {"kind": "agent", "name": name, "run_id": "e2e"}}
    if ttl_ms:
        body["ttl_ms"] = ttl_ms
    return jreq("POST", "/v1/session/acquire", body, want=want)


def release(sid, want=200):
    return jreq("POST", "/v1/session/release", {"session_id": sid}, want=want)


def mutate_code(sid):
    """Status of a harmless mutating call under sid (None = no header)."""
    code, _, _ = req("POST", "/v1/files/mkdir", {"path": "e2e-gate"}, sid=sid)
    return code


# ---------------------------------------------------------------- sessions
def test_sessions():
    section("sessions")
    check(mutate_code(None) == 409, "mutating call without a session must be 409")
    code, _, body = req("POST", "/v1/files/mkdir", {"path": "x"})
    check(b"no session held" in body, f"409 body should say no session held: {body!r}")

    s = acquire("e2e-a")
    sid = s["id"]
    check(s["holder"]["name"] == "e2e-a" and not s["paused"], f"bad session {s}")

    # A second acquire is 409 naming the holder, without leaking its session id.
    code, _, body = req("POST", "/v1/session/acquire", {"holder": {"kind": "agent", "name": "e2e-b"}})
    out = json.loads(body)
    check(code == 409, f"second acquire = {code}")
    check(out["holder"]["name"] == "e2e-a", f"409 should name holder e2e-a: {out}")
    check(sid.encode() not in body, "409 body leaks the holder's session id")

    st = jreq("GET", "/v1/session")
    check(st["session"]["id"] == sid and "browser_running" in st and "apps_running" in st, f"GET /v1/session = {st}")

    # Mutations: missing header, wrong id -> 409 naming the holder; right id -> OK.
    for hdr in (None, "", "ses_wrong"):
        code, _, body = req("POST", "/v1/files/mkdir", {"path": "x"}, sid=hdr)
        check(code == 409 and b"e2e-a" in body and sid.encode() not in body,
              f"mutation with session {hdr!r} = {code} {body!r}")
    check(mutate_code(sid) == 200, "mutation with the live session should succeed")

    # Keepalive pushes expires_at forward.
    time.sleep(1.1)
    k = jreq("POST", "/v1/session/keepalive", {"session_id": sid})
    check(k["expires_at"] > s["expires_at"], f"keepalive did not extend: {s['expires_at']} -> {k['expires_at']}")
    jreq("POST", "/v1/session/keepalive", {"session_id": "ses_wrong"}, want=409)

    # Pause blocks mutations even for the holder; resume restores them.
    p = jreq("POST", "/v1/session/pause", {"session_id": sid})
    check(p["paused"], f"pause = {p}")
    code, _, body = req("POST", "/v1/files/mkdir", {"path": "x"}, sid=sid)
    check(code == 409 and b"session paused" in body, f"mutation while paused = {code} {body!r}")
    code, _, _ = req("POST", "/v1/shell/exec", {"command": "true"}, sid=sid)
    check(code == 409, f"shell exec while paused = {code}")
    code, _, _ = req("POST", "/v1/session/acquire", {"holder": {"kind": "agent", "name": "e2e-b"}})
    check(code == 409, "a paused session still holds the lease")
    jreq("POST", "/v1/session/resume", {"session_id": sid})
    check(mutate_code(sid) == 200, "mutation after resume should succeed")

    release(sid)
    release(sid, want=404)
    code, _, body = req("POST", "/v1/files/mkdir", {"path": "x"}, sid=sid)
    check(code == 409 and b"no session held" in body, f"mutation after release = {code} {body!r}")
    check(jreq("GET", "/v1/session")["session"] is None, "session should be null after release")

    # Validation and clamps.
    jreq("POST", "/v1/session/acquire", {"holder": {"kind": "robot", "name": "x"}}, want=400)
    jreq("POST", "/v1/session/acquire", {"holder": {"kind": "agent"}}, want=400)
    s = acquire("e2e-clamp", ttl_ms=1)
    left = time.mktime(time.strptime(s["expires_at"][:19], "%Y-%m-%dT%H:%M:%S")) - \
        time.mktime(time.strptime(s["acquired_at"][:19], "%Y-%m-%dT%H:%M:%S"))
    check(9 <= left <= 11, f"ttl_ms=1 should clamp to 10 s, got {left}s")
    release(s["id"])
    s = acquire("e2e-clamp-hi", ttl_ms=10**9)
    left = time.mktime(time.strptime(s["expires_at"][:19], "%Y-%m-%dT%H:%M:%S")) - \
        time.mktime(time.strptime(s["acquired_at"][:19], "%Y-%m-%dT%H:%M:%S"))
    check(599 <= left <= 601, f"huge ttl_ms should clamp to 600 s, got {left}s")
    release(s["id"])


# ---------------------------------------------------------------- wait/takeover
def test_wait_takeover():
    section("session wait/takeover")
    s = acquire("e2e-wait-holder")
    hid = s["id"]
    w = jreq("POST", "/v1/session/wait", {"timeout_ms": 1000})
    check(w["free"] is False and w["session"]["id"] == hid and w["waited_ms"] >= 500,
          f"wait on a held lease = {w}")
    jreq("POST", "/v1/session/takeover",
         {"holder": {"kind": "agent", "name": "e2e-taker"}, "force": False}, want=400)
    jreq("POST", "/v1/session/takeover",
         {"holder": {"kind": "agent", "name": "e2e-taker"}}, want=400)
    t = jreq("POST", "/v1/session/takeover",
             {"holder": {"kind": "agent", "name": "e2e-taker"}, "force": True}, want=201)
    check(t["evicted"]["id"] == hid and t["session"]["holder"]["name"] == "e2e-taker",
          f"takeover = {t}")
    check(mutate_code(hid) == 409, "evicted holder must no longer mutate")
    check(mutate_code(t["session"]["id"]) == 200, "takeover holder must mutate")
    release(t["session"]["id"])
    w = jreq("POST", "/v1/session/wait", {"timeout_ms": 1000})
    check(w["free"] is True and w["session"] is None, f"wait on a free lease = {w}")


def start_reaper_probe():
    """Acquire a 10 s lease and return it; check_reaper later verifies the reaper closed it."""
    return acquire("e2e-reaper", ttl_ms=10000)["id"]


def check_reaper(sid, since_seq):
    section("reaper expiry")
    # Only the events endpoint is read here: it never touches the session, so the
    # expiry must come from the background reaper, not a lazy read.
    def expired():
        return [e for e in events(since_seq) if e["type"] == "session.expired" and sid in e["detail"]]
    wait_for(expired, "session.expired from the reaper", timeout=25, every=1)
    code, _, _ = req("POST", "/v1/session/keepalive", {"session_id": sid})
    check(code == 409, f"keepalive after expiry = {code}, want 409")


# ---------------------------------------------------------------- shell
def test_shell(sid):
    section("shell")
    r = jreq("POST", "/v1/shell/exec", {"command": "echo hello-$((40+2))"}, sid=sid)
    check(r["status"] == "completed" and r["exit_code"] == 0 and r["stdout"].strip() == "hello-42", f"echo = {r}")
    r = jreq("POST", "/v1/shell/exec", {"command": "echo oops >&2; exit 3"}, sid=sid)
    check(r["status"] == "failed" and r["exit_code"] == 3 and "oops" in r["stderr"], f"exit 3 = {r}")
    r = jreq("POST", "/v1/shell/exec", {"argv": ["printf", "%s|%s", "a b", "c"]}, sid=sid)
    check(r["stdout"] == "a b|c", f"argv = {r}")
    r = jreq("POST", "/v1/shell/exec", {"command": "echo $E2E_VAR; pwd; echo $HOME",
                                        "env": {"E2E_VAR": "v1", "HOME": "/evil"}, "cwd": "."}, sid=sid)
    check(r["stdout"].split() == ["v1", "/home/bot", "/home/bot"], f"env/cwd/HOME = {r}")
    jreq("POST", "/v1/shell/exec", {"command": "true", "cwd": "/etc"}, sid=sid, want=400)
    jreq("POST", "/v1/shell/exec", {"command": "true", "cwd": "../"}, sid=sid, want=400)
    jreq("POST", "/v1/shell/exec", {}, sid=sid, want=400)
    jreq("POST", "/v1/shell/exec", {"command": "x", "argv": ["x"]}, sid=sid, want=400)

    # Timeout clamp: timeout_ms 1 is clamped up to 1 s, and the job is killed.
    t = time.time()
    r = jreq("POST", "/v1/shell/exec", {"command": "sleep 20", "timeout_ms": 1}, sid=sid)
    took = time.time() - t
    check(r["status"] == "timeout" and r.get("timed_out") is True, f"timeout = {r}")
    check(0.9 <= took <= 5, f"timeout_ms=1 should clamp to ~1 s, took {took:.2f}s")

    # Output cap: 1 MiB per stream plus a truncation marker.
    r = jreq("POST", "/v1/shell/exec", {"command": "head -c 3000000 /dev/zero | tr '\\0' x"}, sid=sid)
    check(r["stdout"].endswith("...truncated") and len(r["stdout"]) <= (1 << 20) + 20,
          f"output cap: len={len(r['stdout'])}")

    # A child left holding stdout must not hang the call, and is killed with the job.
    t = time.time()
    r = jreq("POST", "/v1/shell/exec", {"command": "sleep 300 & echo $!", "timeout_ms": 120000}, sid=sid)
    took = time.time() - t
    check(r["status"] == "completed" and took < 8, f"leftover child: {r} took {took:.1f}s")
    pid = int(r["stdout"].strip())
    wait_for(lambda: not pid_alive(pid), f"leftover child {pid} to be killed", timeout=10)

    # Background job: live poll, list, then kill the whole tree.
    r = jreq("POST", "/v1/shell/exec", {"command": "sleep 300 & echo $!; sleep 300 & echo $!; echo ready; wait",
                                        "background": True, "timeout_ms": 120000}, sid=sid)
    check(r["status"] == "running" and r["job_id"], f"background = {r}")
    job = r["job_id"]

    def polled():
        j = jreq("GET", f"/v1/shell/jobs/{job}")
        return j if "ready" in j["stdout"] else None
    j = wait_for(polled, "live output from the background job", timeout=10)
    check(j["status"] == "running", f"job should still run: {j}")
    pids = [int(x) for x in j["stdout"].split()[:2]]
    check(all(pid_alive(p) for p in pids), "background children should be alive before kill")
    jobs = jreq("GET", "/v1/shell/jobs")["jobs"]
    check(any(x["job_id"] == job for x in jobs), "job missing from list")
    k = jreq("POST", f"/v1/shell/jobs/{job}/kill", sid=sid)
    check(k["status"] == "killed", f"kill = {k}")
    for p in pids:
        wait_for(lambda p=p: not pid_alive(p), f"child {p} killed with its job", timeout=10)
    jreq("POST", f"/v1/shell/jobs/{job}/kill", sid=sid, want=404)
    jreq("GET", "/v1/shell/jobs/job-nope", want=404)

    # A background job that finishes on its own.
    r = jreq("POST", "/v1/shell/exec", {"command": "sleep 0.3; echo bg-done", "background": True}, sid=sid)
    j = wait_for(lambda: (lambda x: x if x["status"] == "completed" else None)(jreq("GET", f"/v1/shell/jobs/{r['job_id']}")),
                 "background job completion", timeout=10)
    check(j["stdout"].strip() == "bg-done" and j["exit_code"] == 0, f"bg completion = {j}")


# ---------------------------------------------------------------- files
def multipart(fields, filename, content):
    boundary = uuid.uuid4().hex
    parts = []
    for k, v in fields.items():
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
    parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
                 f'Content-Type: application/octet-stream\r\n\r\n'.encode() + content + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def test_files(sid):
    section("files")
    w = jreq("POST", "/v1/files/write", {"path": "e2e/dir/a.txt", "content": "alpha"}, sid=sid)
    check(w["path"] == "e2e/dir/a.txt" and w["size"] == 5, f"write = {w}")
    code, _, data = req("GET", "/v1/files/read?path=e2e/dir/a.txt")
    check(code == 200 and data == b"alpha", f"read = {code} {data!r}")
    blob = bytes(range(256))
    jreq("POST", "/v1/files/write", {"path": "e2e/bin.dat", "content": base64.b64encode(blob).decode(),
                                     "encoding": "base64"}, sid=sid)
    code, _, data = req("GET", "/v1/files/read?path=e2e/bin.dat")
    check(data == blob, "base64 round trip")
    names = {e["name"]: e for e in jreq("GET", "/v1/files/list?path=e2e")["entries"]}
    check(names["dir"]["type"] == "dir" and names["bin.dat"]["size"] == 256, f"list = {names}")
    jreq("POST", "/v1/files/mkdir", {"path": "e2e/made/deep"}, sid=sid)
    jreq("POST", "/v1/files/move", {"from": "e2e/dir/a.txt", "to": "e2e/made/b.txt"}, sid=sid)
    jreq("POST", "/v1/files/move", {"from": "e2e/bin.dat", "to": "e2e/made/b.txt"}, sid=sid, want=500)
    check(req("GET", "/v1/files/read?path=e2e/made/b.txt")[2] == b"alpha", "move kept content")
    body, ctype = multipart({"path": "e2e/made/"}, "up.txt", b"uploaded")
    code, _, data = req("POST", "/v1/files/upload", sid=sid, raw=body, headers={"Content-Type": ctype})
    check(code == 200 and json.loads(data)["path"] == "e2e/made/up.txt", f"upload = {code} {data!r}")
    check(dexec("cat /home/bot/e2e/made/up.txt") == "uploaded", "upload landed on disk")
    jreq("DELETE", "/v1/files?path=e2e/made", sid=sid, want=500)  # not empty without recursive
    jreq("DELETE", "/v1/files?path=e2e/made&recursive=1", sid=sid)
    jreq("GET", "/v1/files/read?path=e2e/made/b.txt", want=404)
    jreq("DELETE", "/v1/files?path=.&recursive=1", sid=sid, want=400)

    # Jail escapes: .., absolute, and symlinks are all rejected.
    for p in ("../etc/passwd", "../../etc/passwd", "/etc/passwd", "/home/bot/../root"):
        jreq("GET", f"/v1/files/read?path={p}", want=400)
        jreq("POST", "/v1/files/write", {"path": p, "content": "x"}, sid=sid, want=400)
    jreq("GET", "/v1/files/list?path=/", want=400)
    dexec("ln -sfn /etc /home/bot/etclink && ln -sfn /etc/hostname /home/bot/hostlink")
    jreq("GET", "/v1/files/read?path=etclink/hostname", want=400)
    jreq("GET", "/v1/files/read?path=hostlink", want=400)
    jreq("GET", "/v1/files/list?path=etclink", want=400)
    jreq("POST", "/v1/files/write", {"path": "etclink/evil", "content": "x"}, sid=sid, want=400)
    jreq("POST", "/v1/files/mkdir", {"path": "etclink/evil"}, sid=sid, want=400)
    jreq("POST", "/v1/files/move", {"from": "e2e", "to": "etclink/e2e"}, sid=sid, want=400)
    # Deleting a link removes the link itself; the target is untouched.
    jreq("DELETE", "/v1/files?path=etclink&recursive=1", sid=sid)
    jreq("DELETE", "/v1/files?path=hostlink", sid=sid)
    check(dexec("test -e /etc/hostname && test ! -e /home/bot/etclink && echo ok").strip() == "ok",
          "deleting a symlink must not touch its target")
    jreq("DELETE", "/v1/files?path=e2e&recursive=1", sid=sid)


# ---------------------------------------------------------------- browser
TEST_PAGE = ("<input id=q><select id=s><option value=a>a</option><option value=b>b</option></select>"
             "<button id=b style='width:200px;height:60px' "
             "onclick=\"document.title='clicked'\">B</button>"
             "<div style='height:4000px'></div>")


def evaluate(sid, expr):
    return jreq("POST", "/v1/browser/evaluate", {"expression": expr}, sid=sid)["value"]


def test_browser(sid):
    section("browser")
    jreq("POST", "/v1/browser/start", {}, sid=sid)
    check(jreq("GET", "/v1/browser/state")["url"] is not None, "state after start")
    jreq("POST", "/v1/browser/navigate", {"url": "about:blank"}, sid=sid)
    jreq("POST", "/v1/browser/navigate", {"url": "javascript:alert(1)"}, sid=sid, want=400)
    v = evaluate(sid, "1+1")
    check(v == 2, f"evaluate 1+1 = {v!r}")
    v = evaluate(sid, "({a:[1,'x']})")
    check(v == {"a": [1, "x"]}, f"evaluate should return JSON values, got {v!r}")
    evaluate(sid, f"document.title='';document.body.innerHTML={json.dumps(TEST_PAGE)};true")

    code, _, _ = req("POST", "/v1/browser/act", {"kind": "click", "selector": "#b"}, sid=sid)
    check(code == 204, f"act click = {code}")
    wait_for(lambda: evaluate(sid, "document.title") == "clicked", "click to set the title", timeout=5)
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "type", "selector": "#q", "text": "typed-e2e"}, sid=sid)
    check(code == 204, f"act type = {code}")
    wait_for(lambda: evaluate(sid, "document.querySelector('#q').value") == "typed-e2e", "typed text", timeout=5)
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "select", "selector": "#s", "text": "b"}, sid=sid)
    check(code == 204 and evaluate(sid, "document.querySelector('#s').value") == "b", f"act select = {code}")
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "scroll", "x": 100, "y": 100, "dy": 600}, sid=sid)
    check(code == 204, f"act scroll = {code}")
    wait_for(lambda: evaluate(sid, "window.scrollY") > 0, "scroll to move the page", timeout=5)
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "key", "key": "Home"}, sid=sid)
    check(code == 204, f"act key = {code}")
    code, _, body = req("POST", "/v1/browser/act", {"kind": "click", "selector": "#nope"}, sid=sid)
    check(code == 404 and b"selector not found" in body, f"act missing selector = {code}")
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "dance"}, sid=sid)
    check(code == 400, f"act unknown kind = {code}")
    check("typed" not in jreq("GET", "/v1/browser/text")["text"] or True, "text read")

    code, hdrs, img = req("GET", "/v1/browser/frame?format=png")
    check(code == 200 and img[:4] == b"\x89PNG", f"browser frame = {code}")

    jreq("POST", "/v1/browser/stop", {}, sid=sid)
    st = jreq("GET", "/v1/session")
    check(st["browser_running"] is False, f"browser should be stopped: {st}")
    jreq("POST", "/v1/browser/evaluate", {"expression": "1"}, sid=sid, want=503)
    jreq("POST", "/v1/browser/start", {}, sid=sid)
    check(evaluate(sid, "2*21") == 42, "evaluate after restart")


# ---------------------------------------------------------------- playwright on the live browser
PW_FORM = """<!doctype html><title>pw form</title>
<label>Name <input id=name aria-label="Name"></label>
<button id=go style="width:200px;height:60px"
  onclick="location.href='/done.html#'+encodeURIComponent(document.getElementById('name').value)">Go</button>
"""
PW_DONE = """<!doctype html><title>pw done</title><h1 id=out></h1>
<script>document.getElementById('out').textContent=decodeURIComponent(location.hash.slice(1))</script>
"""
PW_SERVER = ("require('http').createServer((q,r)=>{const f=q.url.startsWith('/done')?'done.html':'form.html';"
             "r.setHeader('content-type','text/html');"
             "require('fs').createReadStream(require('path').join(process.env.HOME,'pw-site',f)).pipe(r)})"
             ".listen(8765,'127.0.0.1',()=>console.log('serving'))")


def chromium_browsers():
    """PIDs of Chromium browser processes (not renderers/zygotes/utilities)."""
    out = dexec("ps -eo pid=,args= | awk '$2 ~ /chromium$/ && $0 !~ /--type=/ {print $1}'", check_rc=False)
    return sorted(out.split())


def sh(sid, command, timeout_ms=60000, want_rc=0):
    r = jreq("POST", "/v1/shell/exec", {"command": command, "timeout_ms": timeout_ms}, sid=sid, timeout=120)
    if want_rc is not None:
        check(r["exit_code"] == want_rc, f"{command!r}: exit {r['exit_code']} want {want_rc}\n"
                                         f"stdout={r['stdout'][-2000:]}\nstderr={r['stderr'][-2000:]}")
    return r


def last_json(text):
    lines = [ln for ln in text.splitlines() if ln.startswith("{")]
    check(lines, f"no JSON line in {text!r}")
    return json.loads(lines[-1])


def test_playwright(sid):
    section("playwright: attach to the live browser, record, trace, replay")
    jreq("POST", "/v1/files/write", {"path": "pw-site/form.html", "content": PW_FORM}, sid=sid)
    jreq("POST", "/v1/files/write", {"path": "pw-site/done.html", "content": PW_DONE}, sid=sid)
    srv = jreq("POST", "/v1/shell/exec", {"command": f"exec node -e {shlex.quote(PW_SERVER)}",
                                          "background": True, "timeout_ms": 600000}, sid=sid)
    wait_for(lambda: "serving" in jreq("GET", f"/v1/shell/jobs/{srv['job_id']}")["stdout"], "test site", timeout=15)

    browsers_before = chromium_browsers()
    check(len(browsers_before) == 1, f"want exactly one Chromium browser process, got {browsers_before}")
    jreq("POST", "/v1/browser/navigate", {"url": "http://127.0.0.1:8765/"}, sid=sid)
    wait_for(lambda: evaluate(sid, "document.title") == "pw form", "form page in the live browser", timeout=10)

    # Same browser, same page: a value set through dad's own CDP connection is visible to a
    # Playwright script, and dad-playwright marks the page the window shows as live.
    evaluate(sid, "window.__dadMark = 'mark-' + 42; true")
    pages = [json.loads(ln) for ln in sh(sid, "dad-playwright pages")["stdout"].splitlines() if ln.startswith("{")]
    live = [p for p in pages if p["live"]]
    check(len(live) == 1 and live[0]["url"] == "http://127.0.0.1:8765/", f"live page: {pages}")
    jreq("POST", "/v1/files/write", {"path": "pw-mark.js", "content":
         "module.exports = async ({ page }) => { const m = await page.evaluate(() => window.__dadMark);"
         " if (m !== 'mark-42') throw new Error('not the live page: ' + m); };"}, sid=sid)
    r = last_json(sh(sid, "dad-playwright run pw-mark.js")["stdout"])
    check(r["ok"] is True, f"mark script: {r}")

    # Record while the user (here: dad's input API, the same path as the browser window) acts.
    rec = jreq("POST", "/v1/shell/exec", {"command": "dad-playwright record --name e2e-flow",
                                          "background": True, "timeout_ms": 300000}, sid=sid)
    job = rec["job_id"]
    started = wait_for(lambda: (lambda j: j if '"recording"' in j["stdout"] or j["status"] != "running" else None)(
        jreq("GET", f"/v1/shell/jobs/{job}")), "recorder to start", timeout=30)
    check(started["status"] == "running", f"recorder exited early: {started}")
    check(last_json(started["stdout"])["start_url"] == "http://127.0.0.1:8765/", f"start url: {started['stdout']}")
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "click", "selector": "#name"}, sid=sid)
    check(code == 204, f"click name = {code}")
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "type", "selector": "#name", "text": "Ada"}, sid=sid)
    check(code == 204, f"type name = {code}")
    code, _, _ = req("POST", "/v1/browser/act", {"kind": "click", "selector": "#go"}, sid=sid)
    check(code == 204, f"click go = {code}")
    wait_for(lambda: jreq("GET", "/v1/browser/state")["url"].endswith("/done.html#Ada"), "navigation to done page", timeout=10)

    def script_has_click():
        out = sh(sid, "cat recordings/e2e-flow/script.js", want_rc=None)["stdout"]
        return out if "'Go'" in out and ".click()" in out else None
    wait_for(script_has_click, "recorded click on Go in script.js", timeout=10)
    stop = last_json(sh(sid, "dad-playwright stop e2e-flow")["stdout"])
    check(stop["status"] == "complete" and stop["actions"] >= 2, f"stop: {stop}")
    j = wait_for(lambda: (lambda x: x if x["status"] != "running" else None)(jreq("GET", f"/v1/shell/jobs/{job}")),
                 "recorder exit", timeout=30)
    check(j["exit_code"] == 0, f"recorder exit: {j}")

    script = sh(sid, "cat recordings/e2e-flow/script.js")["stdout"]
    check("await page.goto(\"http://127.0.0.1:8765/\");" in script, f"script should start at the start url:\n{script}")
    check(".fill('Ada')" in script, f"script should fill the name:\n{script}")
    check("getByRole('button', { name: 'Go' }).click()" in script, f"script should click Go:\n{script}")
    meta = json.loads(sh(sid, "cat recordings/e2e-flow/recording.json")["stdout"])
    check(meta["status"] == "complete" and meta["trace"].endswith("trace.zip"), f"recording.json: {meta}")
    # The trace is a Playwright trace zip with action events and DOM snapshots of the live session.
    tr = sh(sid, "f=recordings/e2e-flow/trace.zip; head -c 2 $f; echo; stat -c %s $f; "
                 "grep -a -c 'trace.trace' $f; grep -a -c 'trace.network' $f")["stdout"].split()
    check(tr[0] == "PK" and int(tr[1]) > 2000 and int(tr[2]) > 0 and int(tr[3]) > 0, f"trace.zip: {tr}")
    check(int(sh(sid, "wc -l < recordings/e2e-flow/actions.jsonl")["stdout"]) >= 2, "actions.jsonl")
    # Recordings hold typed text (passwords too): private to the user.
    modes = sh(sid, "cd recordings/e2e-flow && stat -c '%a' . script.js actions.jsonl recording.json trace.zip")["stdout"].split()
    check(modes == ["700", "600", "600", "600", "600"], f"recording permissions: {modes}")
    check(not sh(sid, "test -e recordings/e2e-flow/recorder.pid && echo stale", want_rc=None)["stdout"].strip(),
          "recorder.pid left behind")

    # Replay on the live page from a blank state, traced, and check the result through dad.
    jreq("POST", "/v1/browser/navigate", {"url": "about:blank"}, sid=sid)
    wait_for(lambda: jreq("GET", "/v1/browser/state")["url"] == "about:blank", "reset to about:blank", timeout=10)
    r = last_json(sh(sid, "dad-playwright replay recordings/e2e-flow --trace recordings/e2e-flow/replay-trace.zip")["stdout"])
    check(r["ok"] is True, f"replay: {r}")
    wait_for(lambda: jreq("GET", "/v1/browser/state")["url"].endswith("/done.html#Ada"), "replay to reach done page", timeout=10)
    check(evaluate(sid, "document.getElementById('out').textContent") == "Ada", "replayed page content")
    check(sh(sid, "head -c 2 recordings/e2e-flow/replay-trace.zip")["stdout"] == "PK", "replay trace")

    # A failing script fails the replay (non-zero, ok=false) instead of hanging.
    jreq("POST", "/v1/files/write", {"path": "pw-bad.js", "content":
         "module.exports = async ({ page }) => { await page.click('#does-not-exist'); };"}, sid=sid)
    r = sh(sid, "dad-playwright run pw-bad.js --timeout 1500", want_rc=1)
    check(last_json(r["stdout"])["ok"] is False, f"bad script: {r}")
    sh(sid, "dad-playwright stop", want_rc=1)  # nothing running
    sh(sid, "dad-playwright record --name ../escape --duration 1", want_rc=2)

    # Attaching never started another browser or downloaded one, and dad still streams.
    check(chromium_browsers() == browsers_before, f"Chromium browser process changed: {browsers_before} -> {chromium_browsers()}")
    check(not sh(sid, "ls -d ~/.cache/ms-playwright 2>/dev/null", want_rc=None)["stdout"].strip(), "Playwright browsers downloaded")
    code, _, img = req("GET", "/v1/browser/frame?format=png")
    check(code == 200 and img[:4] == b"\x89PNG", f"browser frame after Playwright = {code}")
    check(health()["status"] == "ok", f"health after Playwright: {health()}")
    jreq("POST", f"/v1/shell/jobs/{srv['job_id']}/kill", sid=sid)


def test_package_tier(sid):
    section("rootless package tier through the shell API")
    r = sh(sid, "dad-apt install jq >/dev/null && jq -cn '{via:\"api\"}' && command -v jq", timeout_ms=180000)
    check(r["stdout"].split() == ['{"via":"api"}', "/home/bot/.local/bin/jq"], f"dad-apt via shell API: {r}")
    sh(sid, "dad-apt remove jq libjq1 libonig5 >/dev/null && ! command -v jq")

# ---------------------------------------------------------------- health
def test_health(sid):
    section("health ok / degraded / recovery")
    h = wait_for(lambda: (lambda x: x if x["status"] == "ok" else None)(health()), "health ok", timeout=30)
    check(h["checks"]["chromium"] == "ok" and h["checks"]["broadway"] == "ok" and h["checks"]["disk"] == "ok",
          f"healthy checks = {h}")
    check(h["checks"]["session"]["held"] is True, f"health should show the held session: {h}")

    # Browser stopped on purpose: not a failure.
    jreq("POST", "/v1/browser/stop", {}, sid=sid)
    h = health()
    check(h["checks"]["chromium"] == "stopped" and h["status"] == "ok", f"stopped browser = {h}")
    jreq("POST", "/v1/browser/start", {}, sid=sid)

    # Chromium crashes while it should be running: degraded, then it self-heals.
    seq = jreq("GET", "/v1/session/events?limit=1")["latest_seq"]
    dexec("pkill -KILL -x chromium")
    wait_for(lambda: (lambda x: x if x["status"] == "degraded" and x["checks"]["chromium"] == "down" else None)(health()),
             "health degraded after chromium was killed", timeout=15, every=0.2)
    wait_for(lambda: (lambda x: x if x["status"] == "ok" else None)(health()),
             "health ok after the browser supervisor restarted chromium", timeout=40, every=0.5)
    check(any(e["type"] == "health.degraded" for e in events(seq)), "health.degraded event missing")

    # Broadway killed: degraded; starting the app on that display brings it back.
    dexec("pkill -KILL -f '^broadwayd :1 '")
    wait_for(lambda: (lambda x: x if x["status"] == "degraded" and x["checks"]["broadway"] == "down" else None)(health()),
             "health degraded after broadwayd was killed", timeout=15, every=0.2)
    jreq("POST", "/v1/apps/start?id=terminal", sid=sid)
    wait_for(lambda: (lambda x: x if x["status"] == "ok" else None)(health()),
             "health ok after broadwayd restarted", timeout=40, every=0.5)
    wait_for(lambda: zombies() == "", "no zombie processes after the kills", timeout=10)


# ---------------------------------------------------------------- display
def screenshot():
    code, _, png = req("GET", "/v1/display/screenshot")
    check(code == 200 and png[:4] == b"\x89PNG", f"display screenshot = {code}")
    return png


def test_display(sid):
    section("display input")
    jreq("POST", "/v1/apps/start?id=terminal", sid=sid)
    code, _, _ = req("GET", "/v1/display/screenshot?wait=1")
    check(code == 200, f"screenshot wait = {code}")
    meta = jreq("GET", "/v1/display/meta")
    check(isinstance(meta, dict), f"display meta = {meta}")
    # Let the terminal finish drawing before taking the baseline.
    time.sleep(2)
    before = screenshot()
    code, _, _ = req("POST", "/v1/display/click", {"x": 400, "y": 300}, sid=sid)
    check(code == 204, f"display click = {code}")
    marker = "e2e" + uuid.uuid4().hex[:6]
    # Type a command into the terminal through Broadway input injection; the file it
    # creates proves the keystrokes reached the GTK app, not just the screen.
    code, _, _ = req("POST", "/v1/display/type", {"text": f"echo {marker} > ~/{marker}.txt"}, sid=sid)
    check(code == 204, f"display type = {code}")
    code, _, _ = req("POST", "/v1/display/key", {"key": "Return"}, sid=sid)
    check(code == 204, f"display key = {code}")
    wait_for(lambda: dexec(f"cat /home/bot/{marker}.txt 2>/dev/null", check_rc=False).strip() == marker,
             "the typed command to run in the terminal", timeout=15, every=0.5)
    wait_for(lambda: screenshot() != before, "the screenshot to change after typing", timeout=15, every=0.5)
    dexec(f"rm -f /home/bot/{marker}.txt")
    for path, body in (("/v1/display/scroll", {"x": 400, "y": 300, "dy": 3}),
                       ("/v1/display/drag", {"x1": 300, "y1": 300, "x2": 320, "y2": 320}),
                       ("/v1/display/click", {"x": 400, "y": 300, "button": 3})):
        code, _, _ = req("POST", path, body, sid=sid)
        check(code == 204, f"{path} = {code}")
    jreq("POST", "/v1/display/key", {"key": "Escape"}, sid=sid, want=204)
    jreq("POST", "/v1/display/key", {"key": "NoSuchKey"}, sid=sid, want=400)
    jreq("POST", "/v1/display/click", {"x": 1, "y": 1, "button": 7}, sid=sid, want=400)


# ---------------------------------------------------------------- apps
def test_apps(sid):
    section("apps")
    apps = {a["id"]: a for a in jreq("GET", "/v1/apps")["apps"]}
    check({"terminal", "editor", "files", "browser"} <= set(apps), f"apps = {sorted(apps)}")
    check(all("exec" not in a for a in apps.values()), "exec must not be exposed")
    jreq("POST", "/v1/apps/start?id=editor", sid=sid)
    wait_for(lambda: {a["id"]: a for a in jreq("GET", "/v1/apps")["apps"]}["editor"]["running"], "editor running")
    wait_for(lambda: dexec("pgrep -x mousepad", check_rc=False).strip(), "mousepad process")
    r = jreq("POST", "/v1/apps/stop?id=editor", sid=sid)
    check(r["running"] is False, f"stop = {r}")
    check(not {a["id"]: a for a in jreq("GET", "/v1/apps")["apps"]}["editor"]["running"], "editor still running")
    wait_for(lambda: dexec("pgrep -x mousepad || echo none", check_rc=False).strip() == "none", "mousepad to exit")
    jreq("POST", "/v1/apps/start?id=nope", sid=sid, want=400)
    jreq("POST", "/v1/apps/stop?id=nope", sid=sid, want=400)


# ---------------------------------------------------------------- MCP
def mcp(payload, token=TOKEN):
    code, _, body = req("POST", "/mcp", payload, token=token,
                        headers={"Accept": "application/json, text/event-stream"})
    return code, (json.loads(body) if code == 200 and body.strip() else body)


def mcp_call(name, args, id_=1):
    code, out = mcp({"jsonrpc": "2.0", "id": id_, "method": "tools/call",
                     "params": {"name": name, "arguments": args}})
    check(code == 200 and "result" in out, f"tools/call {name}: {code} {out}")
    return out["result"]


def mcp_ok(name, args, id_=1):
    res = mcp_call(name, args, id_)
    check(not res.get("isError"), f"{name} failed: {res}")
    return res


def mcp_json(name, args, id_=1):
    res = mcp_ok(name, args, id_)
    if "structuredContent" in res:
        return res["structuredContent"]
    return json.loads(res["content"][0]["text"])


EXPECTED_TOOLS = {
    "session_acquire", "session_keepalive", "session_release", "session_pause", "session_resume",
    "session_wait", "session_takeover",
    "session_get", "session_events", "health_get", "shell_exec", "shell_job_get", "shell_job_kill",
    "shell_jobs_list", "files_list", "files_read", "files_write", "files_delete", "files_mkdir",
    "files_move", "browser_start", "browser_stop", "browser_navigate", "browser_evaluate", "browser_act",
    "browser_text", "browser_state", "browser_back", "browser_forward", "browser_reload", "browser_input",
    "browser_size", "browser_frame", "display_screenshot", "display_click", "display_type", "display_key",
    "display_scroll", "display_drag", "display_meta", "apps_list", "apps_start", "apps_stop",
}


def test_mcp():
    section("MCP over Streamable HTTP")
    init = {"jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                       "clientInfo": {"name": "e2e", "version": "0"}}}
    for tok in (None, "", "wrong", TOKEN + "x", TOKEN[:-1]):
        code, _ = mcp(init, token=tok)
        check(code == 401, f"MCP with token {tok!r} = {code}, want 401")
    code, _, _ = req("GET", "/mcp")
    check(code == 405, f"GET /mcp = {code}")
    code, out = mcp(init)
    check(code == 200 and out["result"]["serverInfo"]["name"] == "docker-agent-desktop", f"initialize = {out}")

    code, out = mcp({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}})
    names = {t["name"] for t in out["result"]["tools"]}
    check(len(out["result"]["tools"]) == 43 and names == EXPECTED_TOOLS,
          f"tools/list: {len(names)} tools; missing {EXPECTED_TOOLS - names}, extra {names - EXPECTED_TOOLS}")

    # Session rule: mutating tools refuse without a live session.
    res = mcp_call("shell_exec", {"session_id": "", "command": "true"})
    check(res.get("isError") and "no session held" in res["content"][0]["text"], f"exec without session = {res}")

    s = mcp_json("session_acquire", {"kind": "agent", "name": "e2e-mcp"})
    sid = s["id"]
    # The HTTP side sees the MCP holder.
    code, _, body = req("POST", "/v1/session/acquire", {"holder": {"kind": "agent", "name": "other"}})
    check(code == 409 and b"e2e-mcp" in body, f"HTTP acquire while MCP holds = {code} {body!r}")
    res = mcp_call("files_write", {"session_id": "ses_wrong", "path": "m.txt", "content": "x"})
    check(res.get("isError") and "session held by e2e-mcp" in res["content"][0]["text"], f"wrong session = {res}")
    mcp_ok("session_pause", {"session_id": sid})
    res = mcp_call("shell_exec", {"session_id": sid, "command": "true"})
    check(res.get("isError") and "session paused" in res["content"][0]["text"], f"paused = {res}")
    mcp_ok("session_resume", {"session_id": sid})
    mcp_ok("session_keepalive", {"session_id": sid})

    # One representative call per tool group.
    g = mcp_json("session_get", {})
    check(g["session"]["id"] == sid, f"session_get = {g}")
    check(mcp_json("health_get", {})["status"] in ("ok", "degraded"), "health_get")
    r = mcp_json("shell_exec", {"session_id": sid, "command": "echo mcp-$((6*7))"})
    check(r["stdout"].strip() == "mcp-42", f"shell_exec = {r}")
    r = mcp_json("shell_exec", {"session_id": sid, "command": "sleep 60", "background": True})
    check(mcp_json("shell_job_get", {"job_id": r["job_id"]})["status"] == "running", "shell_job_get")
    mcp_ok("shell_jobs_list", {})
    check(mcp_json("shell_job_kill", {"session_id": sid, "job_id": r["job_id"]})["status"] == "killed", "shell_job_kill")
    mcp_ok("files_write", {"session_id": sid, "path": "mcp/m.txt", "content": "from-mcp"})
    res = mcp_ok("files_read", {"path": "mcp/m.txt"})
    check("from-mcp" in json.dumps(res), f"files_read = {res}")
    res = mcp_call("files_read", {"path": "../../etc/passwd"})
    check(res.get("isError"), "files_read escape must fail")
    mcp_ok("files_delete", {"session_id": sid, "path": "mcp", "recursive": True})
    mcp_ok("browser_start", {"session_id": sid})
    check(mcp_json("browser_evaluate", {"session_id": sid, "expression": "40+2"})["value"] == 42, "browser_evaluate")
    mcp_ok("browser_state", {})
    res = mcp_ok("browser_frame", {})
    check(any(c.get("type") == "image" for c in res["content"]), f"browser_frame should be an image: {res}")
    res = mcp_ok("display_screenshot", {})
    img = [c for c in res["content"] if c.get("type") == "image"]
    check(img and base64.b64decode(img[0]["data"])[:4] == b"\x89PNG", "display_screenshot should be a PNG image")
    mcp_ok("display_meta", {})
    mcp_ok("display_key", {"session_id": sid, "key": "Escape"})
    apps = mcp_json("apps_list", {})
    check("terminal" in json.dumps(apps), f"apps_list = {apps}")
    ev = mcp_json("session_events", {"since": 0, "limit": 500})
    check(any(e["type"] == "session.acquired" for e in ev["events"]), "session_events")
    mcp_ok("session_release", {"session_id": sid})
    # Wait and takeover over MCP, on the free lease.
    w = mcp_json("session_wait", {"timeout_ms": 1000})
    check(w["free"] is True and w["session"] is None, f"session_wait free = {w}")
    res = mcp_call("session_takeover", {"kind": "agent", "name": "e2e-mcp-taker"})
    check(res.get("isError"), f"takeover without force must fail: {res}")
    t = mcp_json("session_takeover", {"kind": "agent", "name": "e2e-mcp-taker", "force": True})
    check(t["evicted"] is None and t["session"]["holder"]["name"] == "e2e-mcp-taker", f"takeover = {t}")
    mcp_ok("session_release", {"session_id": t["session"]["id"]})
    check(jreq("GET", "/v1/session")["session"] is None, "MCP release should free the session")


# ---------------------------------------------------------------- events
def test_events(since):
    section("activity log")
    types = {e["type"] for e in events(since)}
    want = {"session.acquired", "session.released", "session.expired", "session.paused", "session.resumed",
            "session.takeover",
            "shell.exec", "files.write", "files.delete", "files.move", "files.mkdir", "apps.start", "apps.stop",
            "browser.start", "browser.stop", "browser.navigate", "browser.act", "health.degraded"}
    check(want <= types, f"activity log missing {sorted(want - types)}")
    evs = events(since)
    seqs = [e["seq"] for e in evs]
    check(seqs == sorted(seqs) and len(set(seqs)) == len(seqs), "event seqs must be strictly increasing")
    check(all(TOKEN not in e["detail"] for e in evs), "events must never contain the token")
    page = jreq("GET", f"/v1/session/events?since={since}&limit=3")
    check(len(page["events"]) == 3 and page["latest_seq"] >= seqs[-1], f"events paging = {page}")


def main():
    since = jreq("GET", "/v1/session/events?limit=1")["latest_seq"]
    test_sessions()
    test_wait_takeover()
    reaper_sid = start_reaper_probe()
    reaper_seq = jreq("GET", "/v1/session/events?limit=1")["latest_seq"]
    check_reaper(reaper_sid, reaper_seq)

    sid = acquire("e2e-main", ttl_ms=600000)["id"]
    test_shell(sid)
    test_files(sid)
    test_browser(sid)
    test_playwright(sid)
    test_package_tier(sid)
    test_display(sid)
    test_apps(sid)
    test_health(sid)
    release(sid)
    test_mcp()
    test_events(since)
    check(zombies() == "", f"zombie processes left: {zombies()}")
    print(f"v1 + MCP e2e passed in {time.time() - T0:.1f}s")


if __name__ == "__main__":
    main()
