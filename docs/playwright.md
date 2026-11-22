# Playwright on the live browser

Playwright is part of the image. `dc-playwright` drives, records and replays the **same Chromium
the user sees** in the browser window: it attaches with `chromium.connectOverCDP` to the browser
that `dc-agent` runs (CDP on `127.0.0.1:9222`, inside the container only). It never launches a
second browser and never downloads Playwright's browsers. Disconnecting leaves Chromium, its pages,
cookies and history as they were.

What ships:

| Piece | Where | Notes |
|-------|-------|-------|
| Node.js 20 (Debian `nodejs`) | `/usr/bin/node` | No npm in the runtime image. |
| `playwright-core` (pinned in `docker/playwright/package-lock.json`) | `/usr/local/lib/dc-playwright/node_modules` | `NODE_PATH` points here, so `require('playwright-core')` works in any script. `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1` is set. |
| `dc-playwright` | `/usr/local/bin/dc-playwright` | The CLI below. |

## Commands

```text
dc-playwright pages                                 list pages; "live": true is the one on screen
dc-playwright record [--name N] [--duration S] [--no-trace]
dc-playwright stop [N]                              stop a recording (the active one by default)
dc-playwright replay DIR|FILE [--trace OUT.zip] [--timeout MS]
dc-playwright run FILE [--trace OUT.zip] [--timeout MS]   same as replay, for hand-written scripts
```

Every command prints one JSON object per line on stdout. Exit codes: 0 success, 1 failure (for
replay: the script threw, see `"error"`), 2 usage error, 3 the browser went away while recording.

The browser starts lazily. If it is not running, commands wait up to 15 s and then fail with a
hint; start it with `POST /v1/browser/start` (or by opening the browser window) first.

The live page is the one the browser window streams: `dc-agent` attaches its screencast to the
first page target in `/json/list`, and `dc-playwright` picks the Playwright page with that target id.

## Recording

`dc-playwright record` uses Playwright's own code generator (the engine behind `playwright
codegen`) in its programmatic mode. It watches real input in every page of the browser: what the
user does in the browser window, what an agent does through `/v1/browser/act`, `/v1/browser/input`
or the MCP tools, and what another Playwright script does. With tracing on (the default) it also
records a Playwright trace of the same session.

Output goes to `~/recordings/<name>/` (`DC_RECORDINGS` overrides the parent directory):

| File | Contents |
|------|----------|
| `script.js` | A replayable script. Rewritten after every action, so it is usable even if the recorder is killed. Starts with `page.goto(<url at start>)` unless that was `about:` or `chrome:`. |
| `actions.jsonl` | One JSON line per recorder event: the action (selector, text, aria snapshot) and its generated code. |
| `trace.zip` | Playwright trace: actions, DOM snapshots, screenshots and network. Open with `npx playwright show-trace trace.zip` or https://trace.playwright.dev. Written when the recording stops. |
| `recording.json` | Name, start/stop times, start URL, action count, status (`recording`, `complete`, `browser-gone`). |

Recordings contain everything typed, passwords included. The directory is created `0700` and the
files `0600`; treat `~/recordings` as sensitive.

Only one recording runs at a time. A recording ends on `dc-playwright stop`, SIGINT/SIGTERM, or
after `--duration` seconds. Killing it with SIGKILL (for example `POST /v1/shell/jobs/{id}/kill`)
keeps `script.js` and `actions.jsonl` but loses the trace, so prefer `dc-playwright stop`.

Example `script.js` (recorded from `about:blank`, so there is no `goto`):

```js
// Recorded by dc-playwright from the live browser.
// Name: demo  Started: 2026-11-22T01:27:00.838Z
// Replay on the live browser: dc-playwright replay /home/bot/recordings/demo
// The function gets the page the browser window shows, its context and the browser.
module.exports = async ({ page, context, browser }) => {
  await page.getByRole('textbox', { name: 'Search' }).fill('hello');
  await page.getByRole('textbox', { name: 'Search' }).press('Enter');
  await page.getByRole('button', { name: 'Go' }).click();
};
```

## Replaying and running scripts

`dc-playwright replay ~/recordings/demo` runs `script.js` against the live page, so the user
watches it happen. `--trace OUT.zip` records a trace of the replay. `--timeout` sets Playwright's
default action timeout (15000 ms). A script is any CommonJS or ES module whose default export is an
async function taking `{ page, context, browser }`.

## Through the API

From an agent, run the commands with the shell API (or the MCP shell tool):

```bash
# start recording in the background
curl -s -H "X-Computer-Token: $T" -H "X-Dc-Session: $S" -X POST $BASE/v1/shell/exec \
  -d '{"command":"dc-playwright record --name checkout","background":true,"timeout_ms":3600000}'
# ... user or agent works in the browser ...
curl -s -H "X-Computer-Token: $T" -H "X-Dc-Session: $S" -X POST $BASE/v1/shell/exec \
  -d '{"command":"dc-playwright stop checkout"}'
curl -s -H "X-Computer-Token: $T" -H "X-Dc-Session: $S" -X POST $BASE/v1/shell/exec \
  -d '{"command":"dc-playwright replay recordings/checkout"}'
```

Use `GET /v1/files/read?path=recordings/checkout/script.js` to fetch the script.

## Own scripts with the library

```js
const { chromium } = require('playwright-core');
const browser = await chromium.connectOverCDP(process.env.DC_CDP_URL); // http://127.0.0.1:9222
const page = browser.contexts()[0].pages()[0];
// ... drive the page ...
await browser.close(); // disconnects only; Chromium keeps running
```

Do not call `chromium.launch()` for this: it would start a separate browser the user cannot see.

## Limits

- Recording relies on `BrowserContext._startRecording`, which is not public Playwright API.
  `playwright-core` is pinned; the e2e suite (`make e2e`) records and replays a real flow, so an
  upgrade that breaks it fails the gate.
- Codegen records what it can express as Playwright calls (click, fill, press, select, check,
  navigation, popups). Scrolling and hovering are not recorded; the trace still shows them.
- Replays run in the user's live browser state (cookies, logged-in sessions). A recording made while
  logged in replays as that user.
