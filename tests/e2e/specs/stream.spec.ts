import { test, expect } from '@playwright/test';
import { BASE_URL, DAD_TOKEN } from '../playwright.config.js';
import { acquire, ensureSessionFree, release } from './helpers.js';

// Runs after desktop.spec.ts (alphabetical order, workers: 1). The desktop
// page is used only as a same-origin page to open raw WebSockets from.
test.describe.serial('browser stream', () => {
  let sid: string | null = null;

  test.beforeAll(async ({ request }) => {
    await ensureSessionFree(request, 30_000);
    sid = (await acquire(request, 'e2e-playwright-stream')).id;
  });

  test.afterAll(async ({ request }) => {
    if (sid) {
      await release(request, sid).catch(() => {});
      sid = null;
    }
  });

  test('unknown session never opens the stream', async ({ page }) => {
    await page.goto(`${BASE_URL}/portal/?token=${DAD_TOKEN}`);
    const opened = await page.evaluate(
      () =>
        new Promise<boolean>((resolve) => {
          const ws = new WebSocket(`ws://${location.host}/v1/browser/stream?session=ses_wrong`);
          ws.onopen = () => {
            ws.close();
            resolve(true);
          };
          ws.onerror = () => resolve(false);
          ws.onclose = () => resolve(false);
          setTimeout(() => resolve(false), 5000);
        }),
    );
    expect(opened).toBe(false);
  });

  test('live session streams frames', async ({ page }) => {
    await page.goto(`${BASE_URL}/portal/?token=${DAD_TOKEN}`);
    const gotFrame = await page.evaluate(
      (session: string | null) =>
        new Promise<boolean>((resolve) => {
          const ws = new WebSocket(
            `ws://${location.host}/v1/browser/stream?session=${encodeURIComponent(session ?? '')}`,
          );
          ws.binaryType = 'arraybuffer';
          const timer = setTimeout(() => {
            ws.close();
            resolve(false);
          }, 15_000);
          ws.onmessage = (ev) => {
            if (typeof ev.data !== 'string') {
              clearTimeout(timer);
              ws.close();
              resolve(true);
            }
          };
          ws.onerror = () => {
            clearTimeout(timer);
            resolve(false);
          };
        }),
      sid,
    );
    expect(gotFrame).toBe(true);
  });
});
