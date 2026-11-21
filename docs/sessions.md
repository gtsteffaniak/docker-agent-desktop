# Sessions

At most one live session may drive the computer at a time. This is the contract the agent side (today: quantum-bots; tomorrow: any MCP client) implements against. The session is a lease on the whole computer, not on one app: while an agent holds it, the user can still watch — and take over from — the desktop, but no second agent can drive it.

## Acquiring

```
POST /v1/session/acquire
{"holder": {"kind": "agent", "name": "qb-run-42", "run_id": "run-42"}, "ttl_ms": 90000}
```

- `holder.kind` is `agent` or `user`; `holder.name` is required and is what a waiting caller sees. Agents should put a run id in both `name` (or `run_id`) so a human can tell who is driving.
- `ttl_ms` is optional. The lease TTL comes from `DC_SESSION_LEASE_MS` (default 90000 ms), clamped to 10 s..10 min; a per-acquire `ttl_ms` is clamped the same way.
- Success is `201` with the session: `{"id": "ses_…", "holder": {...}, "acquired_at": "…", "expires_at": "…", "paused": false}`.
- When a session is already held, acquire returns `409` with the holder, so the caller knows who to wait for:

```json
{"error": "session held by qb-run-42", "holder": {"kind": "agent", "name": "qb-run-42"}, "session_id": "ses_…", "expires_at": "…"}
```

There is no queue and no force-take on this side. A caller that needs the computer waits and retries, or the user takes over from the desktop (see below).

## Keepalive

```
POST /v1/session/keepalive
{"session_id": "ses_…"}
```

Extends the lease by its TTL and returns the updated session. The holder is expected to keepalive well inside the TTL — quantum-bots does it every 15 s against a 90 s lease, so several missed check-ins are tolerated before the session is treated as abandoned. A keepalive for an unknown, expired, or someone else's session is `409`. A session that is not kept alive expires on its own: a reaper closes stale sessions every 5 s, and every session read expires one lazily, so a dead agent never holds the computer forever. Expiry is recorded as a `session.expired` activity event.

## Acting

Mutating `/v1` endpoints require the `X-Dc-Session` header with the live session's id. Reads never do. A call with no session, a wrong session, or a paused session gets `409` with the same holder body as acquire. The MCP tools enforce the identical rule in-process via their `session_id` argument (see [mcp.md](mcp.md)).

## Pause and resume

```
POST /v1/session/pause   {"session_id": "ses_…"}
POST /v1/session/resume  {"session_id": "ses_…"}
```

A paused session keeps the lease — nobody else can acquire — but mutating calls with it get `409`. This is the user-takeover hook: the desktop (or the agent's owner) pauses the agent's session, drives the computer directly, then resumes it. The agent sees the `409 "session paused"` and waits instead of failing its run.

## Release

```
POST /v1/session/release
{"session_id": "ses_…"}
```

Ends the session. Unknown ids are `404`. Agents must release when their run ends; if they do not, the lease expires on its own. Closing a session does not stop the browser or any app — use `/v1/browser/stop` and `/v1/apps/stop` for that when the run is done.

## Inspecting

```
GET /v1/session
```

Returns the live session (or `null`) plus what a waiting caller needs to decide whether to wait: whether the browser is running, how many viewers are watching the screencast, which apps are running, and the agent's uptime.

## Activity log

```
GET /v1/session/events?since=N&limit=100
```

An in-memory ring (cap 500, oldest drops first) of what happened on the computer: `session.acquired/released/expired/paused/resumed`, `shell.exec`, `files.write/delete/move/mkdir`, `apps.start/stop`, `browser.start/stop/navigate/act`, `health.degraded`. Each event has a monotonic `seq`, a timestamp, a `type`, an `actor` (the session holder's name, or `user`/`system`), and a human-readable `detail` — never secrets. Poll with `since` = the last `latest_seq` seen. The log is in-memory by design: dc-agent is stateless across restarts, and the agent side persists what it needs. Display input is deliberately not logged — it is far too noisy.

## Environment

| Variable | Default | Meaning |
|----------|---------|---------|
| `DC_SESSION_LEASE_MS` | `90000` | Lease TTL in milliseconds, clamped to 10000..600000. |

## What is NOT here

- `/healthz` is container liveness only (bare `200`). It is not session health and is intentionally untouched. Real session health is `GET /v1/health` (see [api.md](api.md)).
- There is no server-side queue, no priority, and no force-take. If the quantum-bots side wants a wait-your-turn UX, it builds the waiting there, on top of the `409` + holder body and `GET /v1/session`.
