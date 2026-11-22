import { test, expect } from '@playwright/test';
import { BASE_URL, DAD_TOKEN } from '../playwright.config.js';
import { api, ensureSessionFree, waitForSessionFree } from './helpers.js';

// Runs after api.spec.ts (alphabetical order, workers: 1) and hands the lease
// back when its pages close, so stream.spec.ts can take it next. GTK apps have
// no backend on the runner (no broadwayd), so this spec asserts the desktop's
// own behaviour — dock, windows, browser canvas, address bar — not app content.
test.describe.serial('desktop', () => {
  test.beforeAll(async ({ request }) => {
    await waitForSessionFree(request, 15_000);
  });

  test.afterAll(async ({ request }) => {
    // pagehide release is flaky in headless CI; clear the desktop lease explicitly.
    await ensureSessionFree(request, 30_000);
  });

  test('portal login, dock, browser window and live frames', async ({ page, request }) => {
    await page.goto(`${BASE_URL}/portal/?token=${DAD_TOKEN}`);
    await expect(page).toHaveURL(/\/desktop\/$/);

    const dock = page.getByTestId('computer-dock');
    await expect(dock).toBeVisible();
    // No apps.d on the runner: the desktop falls back to its built-in list.
    for (const id of ['browser', 'terminal', 'files', 'editor']) {
      await expect(page.getByTestId(`computer-dock-${id}`)).toBeVisible();
    }

    // First boot opens the browser window on its own.
    await expect(page.getByTestId('computer-window-browser')).toBeVisible();
    await expect(page.getByTestId('computer-browser-page')).toBeVisible();
    // A frame has arrived over /v1/browser/stream: the "Connecting…" status hides.
    await expect(page.locator('.page-status')).toBeHidden({ timeout: 30_000 });

    // The address bar follows the real URL once Chromium is up.
    await expect
      .poll(
        async () => {
          const st = await api(request, 'GET', '/v1/browser/state');
          return st.json?.url;
        },
        { timeout: 30_000 },
      )
      .toBe('about:blank');
    await expect(page.locator('.address')).toHaveValue('about:blank');
  });

  test('address bar navigates, dock opens and closes windows', async ({ page, request }) => {
    await page.goto(`${BASE_URL}/portal/?token=${DAD_TOKEN}`);
    await expect(page.getByTestId('computer-window-browser')).toBeVisible();

    // The previous test's page released the desktop session on close; wait for
    // this page's desktop session before navigating, or the socket refuses
    // control messages and the address bar shows an error instead.
    await expect
      .poll(
        async () => {
          const st = await api(request, 'GET', '/v1/session');
          return st.json?.session?.holder?.name;
        },
        { timeout: 15_000 },
      )
      .toBe('desktop');

    const address = page.locator('.address');
    await address.click();
    await address.fill('about:blank');
    await address.press('Enter');
    await expect
      .poll(
        async () => {
          const st = await api(request, 'GET', '/v1/browser/state');
          return st.json?.url;
        },
        { timeout: 30_000 },
      )
      .toBe('about:blank');

    // Terminal opens a window. Its app backend is Docker-only, so the iframe
    // behind it stays empty here — the window itself is the desktop's job.
    await page.getByTestId('computer-dock-terminal').click();
    await expect(page.getByTestId('computer-window-terminal')).toBeVisible();

    // Close the browser window; the dock dot goes away with it.
    await page.getByTestId('computer-window-close-browser').click();
    await expect(page.getByTestId('computer-window-browser')).toBeHidden();
    await expect(page.getByTestId('computer-dock-dot-browser')).toHaveCount(0);
  });
});
