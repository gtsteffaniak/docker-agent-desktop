import type { APIRequestContext } from '@playwright/test';
import { BASE_URL, DAD_TOKEN } from '../playwright.config.js';

export interface ApiResult {
  status: number;
  headers: Record<string, string>;
  text: string;
  json: any;
}

/** One HTTP call to the dad server. Never throws on HTTP errors. */
export async function api(
  request: APIRequestContext,
  method: string,
  path: string,
  opts: { body?: unknown; sid?: string | null; token?: string | null; headers?: Record<string, string> } = {},
): Promise<ApiResult> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) };
  if (opts.token !== null) headers['X-Dad-Token'] = opts.token ?? DAD_TOKEN;
  if (opts.sid) headers['X-Dad-Session'] = opts.sid;
  const res = await request.fetch(`${BASE_URL}${path}`, { method, headers, data: opts.body });
  const text = await res.text();
  let json: any = null;
  try {
    json = JSON.parse(text);
  } catch {
    // Plain-text error bodies.
  }
  return { status: res.status(), headers: res.headers(), text, json };
}

/** Acquire a session lease; throws unless the server answers 201. */
export async function acquire(request: APIRequestContext, name: string, ttlMs?: number): Promise<any> {
  const body: Record<string, unknown> = { holder: { kind: 'agent', name, run_id: 'e2e-playwright' } };
  if (ttlMs) body.ttl_ms = ttlMs;
  const r = await api(request, 'POST', '/v1/session/acquire', { body });
  if (r.status !== 201) throw new Error(`acquire failed: ${r.status} ${r.text}`);
  return r.json;
}

/** Release a session lease; returns the raw result so callers can assert the status. */
export async function release(request: APIRequestContext, sid: string): Promise<ApiResult> {
  return api(request, 'POST', '/v1/session/release', { body: { session_id: sid } });
}

/** Release the live session via the agent token (any holder, including the desktop page). */
export async function releaseLiveSession(request: APIRequestContext): Promise<void> {
  const cur = await api(request, 'GET', '/v1/session');
  const id = cur.json?.session?.id as string | undefined;
  if (id) await release(request, id).catch(() => {});
}

/** Drop any live session, then poll until the lease is free. */
export async function ensureSessionFree(request: APIRequestContext, timeoutMs = 15_000): Promise<void> {
  await releaseLiveSession(request);
  await waitForSessionFree(request, timeoutMs);
}

/** Poll until no session is held (or throw). The desktop and API specs hand the lease over. */
export async function waitForSessionFree(request: APIRequestContext, timeoutMs = 10_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const cur = await api(request, 'GET', '/v1/session');
    if (cur.status === 200 && cur.json.session === null) return;
    if (Date.now() > deadline) throw new Error(`session still held after ${timeoutMs}ms: ${cur.text}`);
    await new Promise((r) => setTimeout(r, 250));
  }
}

/** One MCP call over Streamable HTTP. Returns the parsed JSON body (never SSE: the server answers JSON). */
export async function mcp(request: APIRequestContext, payload: unknown, token?: string): Promise<ApiResult> {
  const res = await request.fetch(`${BASE_URL}/mcp`, {
    method: 'POST',
    headers: {
      'X-Dad-Token': token ?? DAD_TOKEN,
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
    },
    data: payload,
  });
  const text = await res.text();
  let json: any = null;
  try {
    json = JSON.parse(text);
  } catch {
    // Non-JSON (e.g. an SSE stream) surfaces as a test failure with the raw body.
  }
  return { status: res.status(), headers: res.headers(), text, json };
}

/** tools/call; throws unless the server answers 200 with a result. */
export async function mcpCall(
  request: APIRequestContext,
  name: string,
  args: Record<string, unknown>,
  id = 1,
): Promise<any> {
  const r = await mcp(request, { jsonrpc: '2.0', id, method: 'tools/call', params: { name, arguments: args } });
  if (r.status !== 200 || !r.json?.result) throw new Error(`tools/call ${name}: ${r.status} ${r.text}`);
  return r.json.result;
}

/** tools/call that must succeed; returns the tool's JSON payload. */
export async function mcpJson(
  request: APIRequestContext,
  name: string,
  args: Record<string, unknown>,
  id = 1,
): Promise<any> {
  const res = await mcpCall(request, name, args, id);
  if (res.isError) throw new Error(`${name} failed: ${JSON.stringify(res)}`);
  if (res.structuredContent !== undefined) return res.structuredContent;
  return JSON.parse(res.content[0].text);
}
