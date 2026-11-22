import { test, expect } from '@playwright/test';
import { BASE_URL, DAD_TOKEN } from '../playwright.config.js';
import { acquire, release, waitForSessionFree, mcp, mcpJson, api } from './helpers.js';

// The desktop spec runs after this file (alphabetical order, workers: 1) and
// needs the lease free, so every test releases what it acquires and the
// afterAll hook leaves no session behind.
test.describe.serial('v1 api', () => {
  let sid: string | null = null;

  test.afterAll(async ({ request }) => {
    if (sid) {
      await release(request, sid).catch(() => {});
      sid = null;
    }
    await waitForSessionFree(request, 10_000);
  });

  test('healthz, meta and auth', async ({ request }) => {
    const healthz = await request.get(`${BASE_URL}/healthz`);
    expect(healthz.status()).toBe(200);

    const meta = await api(request, 'GET', '/v1/meta');
    expect(meta.status).toBe(200);
    expect(meta.json.api_version).toBe('1');

    expect((await api(request, 'GET', '/v1/apps', { token: null })).status).toBe(401);
    expect((await api(request, 'GET', '/v1/apps', { token: 'wrong' })).status).toBe(401);
    // The pre-rename header was removed with the /api surface: it must not authenticate.
    expect(
      (await api(request, 'GET', '/v1/apps', { token: null, headers: { 'X-Computer-Token': DAD_TOKEN } })).status,
    ).toBe(401);
  });

  test('session lease: acquire, 409, keepalive, pause/resume, release', async ({ request }) => {
    await waitForSessionFree(request, 10_000);
    expect(
      (await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/gate' } })).status,
    ).toBe(409);

    const s = await acquire(request, 'e2e-playwright');
    sid = s.id;
    expect(s.holder.name).toBe('e2e-playwright');
    expect(s.paused).toBe(false);

    // A second acquire is 409 naming the holder, without leaking its session id.
    const second = await api(request, 'POST', '/v1/session/acquire', {
      body: { holder: { kind: 'agent', name: 'e2e-other' } },
    });
    expect(second.status).toBe(409);
    expect(second.json.holder.name).toBe('e2e-playwright');
    expect(JSON.stringify(second.json)).not.toContain(sid);

    // Mutations: missing / wrong session -> 409 naming the holder; live session -> 200.
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/x' } })).status).toBe(409);
    expect(
      (await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/x' }, sid: 'ses_wrong' })).status,
    ).toBe(409);
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/x' }, sid })).status).toBe(200);

    // Keepalive pushes expires_at forward.
    await new Promise((r) => setTimeout(r, 1100));
    const k = await api(request, 'POST', '/v1/session/keepalive', { body: { session_id: sid } });
    expect(k.status).toBe(200);
    expect(Date.parse(k.json.expires_at)).toBeGreaterThan(Date.parse(s.expires_at));

    // Pause blocks mutations even for the holder; resume restores them.
    const p = await api(request, 'POST', '/v1/session/pause', { body: { session_id: sid } });
    expect(p.json.paused).toBe(true);
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/y' }, sid })).status).toBe(409);
    await api(request, 'POST', '/v1/session/resume', { body: { session_id: sid } });
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/y' }, sid })).status).toBe(200);

    expect((await release(request, s.id)).status).toBe(200);
    sid = null;
    expect((await release(request, s.id)).status).toBe(404);
    const cur = await api(request, 'GET', '/v1/session');
    expect(cur.json.session).toBeNull();
  });

  test('shell exec and files round-trip', async ({ request }) => {
    const s = await acquire(request, 'e2e-playwright-shell');
    sid = s.id;

    const exec = await api(request, 'POST', '/v1/shell/exec', { body: { command: 'echo hello-$((40+2))' }, sid });
    expect(exec.status).toBe(200);
    expect(exec.json.status).toBe('completed');
    expect(exec.json.stdout.trim()).toBe('hello-42');

    const w = await api(request, 'POST', '/v1/files/write', {
      body: { path: 'pw-e2e/hello.txt', content: 'hello-v1' },
      sid,
    });
    expect(w.status).toBe(200);
    const read = await api(request, 'GET', '/v1/files/read?path=pw-e2e/hello.txt');
    expect(read.status).toBe(200);
    expect(read.text).toBe('hello-v1');
    const list = await api(request, 'GET', '/v1/files/list?path=pw-e2e');
    expect(list.json.entries.map((e: any) => e.name)).toContain('hello.txt');

    expect((await api(request, 'DELETE', '/v1/files?path=pw-e2e&recursive=1', { sid })).status).toBe(200);

    await release(request, s.id);
    sid = null;
  });

  test('browser: navigate, evaluate, frame', async ({ request }) => {
    const s = await acquire(request, 'e2e-playwright-browser');
    sid = s.id;

    // Chromium boots with the server (DAD_BROWSER_START=boot); wait for it.
    await expect
      .poll(
        async () => {
          const h = await api(request, 'GET', '/v1/health');
          return h.json?.checks?.chromium;
        },
        { timeout: 30_000 },
      )
      .toBe('ok');

    const nav = await api(request, 'POST', '/v1/browser/navigate', { body: { url: 'about:blank' }, sid });
    expect(nav.status).toBe(200);
    const ev = await api(request, 'POST', '/v1/browser/evaluate', { body: { expression: '1+1' }, sid });
    expect(ev.json.value).toBe(2);
    const state = await api(request, 'GET', '/v1/browser/state');
    expect(state.json.url).toBe('about:blank');

    const frame = await request.fetch(`${BASE_URL}/v1/browser/frame?format=png`, {
      headers: { 'X-Dad-Token': DAD_TOKEN },
    });
    expect(frame.status()).toBe(200);
    const buf = await frame.body();
    expect(buf[0]).toBe(0x89);
    expect(buf[1]).toBe(0x50); // PNG magic

    await release(request, s.id);
    sid = null;
  });

  test('session wait and takeover', async ({ request }) => {
    const free = await api(request, 'POST', '/v1/session/wait', { body: { timeout_ms: 1000 } });
    expect(free.json.free).toBe(true);

    const h = await acquire(request, 'e2e-playwright-holder');
    const held = await api(request, 'POST', '/v1/session/wait', { body: { timeout_ms: 1000 } });
    expect(held.json.free).toBe(false);
    expect(held.json.session.id).toBe(h.id);

    expect(
      (
        await api(request, 'POST', '/v1/session/takeover', {
          body: { holder: { kind: 'agent', name: 'e2e-taker' }, force: false },
        })
      ).status,
    ).toBe(400);

    const t = await api(request, 'POST', '/v1/session/takeover', {
      body: { holder: { kind: 'agent', name: 'e2e-taker' }, force: true },
    });
    expect(t.status).toBe(201);
    expect(t.json.evicted.id).toBe(h.id);
    expect(t.json.session.holder.name).toBe('e2e-taker');
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/z' }, sid: h.id })).status).toBe(
      409,
    );
    sid = t.json.session.id;
    expect((await api(request, 'POST', '/v1/files/mkdir', { body: { path: 'pw-e2e/z' }, sid })).status).toBe(200);

    await release(request, t.json.session.id);
    sid = null;
    await waitForSessionFree(request, 10_000);
  });

  test('MCP: initialize, tools/list, session + shell over MCP', async ({ request }) => {
    const init = await mcp(request, {
      jsonrpc: '2.0',
      id: 1,
      method: 'initialize',
      params: {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'e2e-playwright', version: '0' },
      },
    });
    expect(init.status).toBe(200);
    expect(init.json.result.serverInfo.name).toBe('docker-agent-desktop');

    const unauth = await mcp(
      request,
      {
        jsonrpc: '2.0',
        id: 1,
        method: 'initialize',
        params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'x', version: '0' } },
      },
      'wrong',
    );
    expect(unauth.status).toBe(401);

    const list = await mcp(request, { jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} });
    const names = list.json.result.tools.map((t: any) => t.name);
    expect(names).toHaveLength(43);
    expect(names).toContain('session_wait');
    expect(names).toContain('session_takeover');
    expect(names).toContain('browser_frame');

    const s = await mcpJson(request, 'session_acquire', { kind: 'agent', name: 'e2e-playwright-mcp' });
    sid = s.id;
    const shell = await mcpJson(request, 'shell_exec', { session_id: sid, command: 'echo mcp-$((6*7))' });
    expect(shell.stdout.trim()).toBe('mcp-42');
    const health = await mcpJson(request, 'health_get', {});
    expect(['ok', 'degraded']).toContain(health.status);

    await mcpJson(request, 'session_release', { session_id: sid });
    sid = null;
    await waitForSessionFree(request, 10_000);
  });

  test('activity log records the session flow', async ({ request }) => {
    const ev = await api(request, 'GET', '/v1/session/events?since=0&limit=500');
    const types = new Set(ev.json.events.map((e: any) => e.type));
    expect(types.has('session.acquired')).toBe(true);
    expect(types.has('session.released')).toBe(true);
  });
});
