import { defineConfig } from '@playwright/test';

// The dad server runs on the runner (no Docker): Playwright's webServer starts
// `go run ./cmd/dad` from the repo root and waits for /healthz. The server needs
// a Chromium binary on PATH as `chromium` — the CI job symlinks Playwright's own
// Chromium there before the tests run (see .github/workflows/ci.yml). Locally,
// run `npm run install-browsers` once and symlink it the same way, or start a
// server yourself and set DAD_BASE_URL (reuseExistingServer is on outside CI).
const PORT = Number(process.env.DAD_PORT ?? 8080);
const TOKEN = process.env.DAD_TOKEN ?? 'e2e-playwright-token';
const HOME = process.env.DAD_TEST_HOME ?? process.env.HOME ?? '';

export const BASE_URL = process.env.DAD_BASE_URL ?? `http://127.0.0.1:${PORT}`;
export const DAD_TOKEN = TOKEN;

export default defineConfig({
  testDir: './specs',

  // One lease, one browser: parallel files would fight over the session.
  workers: 1,
  fullyParallel: false,

  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 90_000,
  expect: { timeout: 30_000 },
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
  },

  webServer: {
    // CI builds the binary first so Playwright does not re-download modules on every run.
    command: process.env.DAD_BIN ?? 'go run ./cmd/dad',
    cwd: '../..',
    url: `${BASE_URL}/healthz`,
    timeout: 120_000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      DAD_TOKEN: TOKEN,
      DAD_ADDR: `127.0.0.1:${PORT}`,
      DAD_BROWSER_START: 'boot',
      DAD_BROWSER_HOME: 'about:blank',
      // The files API jails under HOME: point the server at a scratch dir so
      // the suite never writes into the runner's real home.
      HOME,
    },
  },
});
