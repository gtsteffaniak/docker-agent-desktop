'use strict';
// dad-playwright: drive and record the container's live Chromium with Playwright.
//
// Every command attaches with chromium.connectOverCDP to the Chromium that dad runs and
// streams to the browser window (CDP on 127.0.0.1:9222). It never launches a browser and never
// downloads one: the page you drive or record is the page the user sees. Disconnecting leaves the
// browser, its pages and its state alone.
//
// Commands:
//   dad-playwright pages                      list pages; the one the browser window shows is marked
//   dad-playwright record [--name N] [--duration S] [--no-trace]
//                                            record actions in the live browser until stopped
//   dad-playwright stop [N]                   stop a running recording (the active one by default)
//   dad-playwright replay DIR|FILE [--trace OUT.zip] [--timeout MS]
//                                            run a recorded (or hand-written) script on the live page
//   dad-playwright run FILE [...]             alias of replay, for hand-written scripts
//
// Scripts export one async function. It gets the live page, its context and the browser:
//   module.exports = async ({ page, context, browser }) => { await page.click('text=Go'); };
//
// Environment: DAD_CDP_URL (default http://127.0.0.1:9222), DAD_RECORDINGS (default ~/recordings).

const fs = require('fs');
const os = require('os');
const path = require('path');
const { pathToFileURL } = require('url');
const { chromium } = require('playwright-core');

const CDP_URL = process.env.DAD_CDP_URL || 'http://127.0.0.1:9222';
const RECORDINGS = process.env.DAD_RECORDINGS || path.join(os.homedir(), 'recordings');
const ACTIVE = path.join(RECORDINGS, '.active');
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

class UsageError extends Error {}

function parseArgs(argv, spec) {
  const out = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) { out._.push(a); continue; }
    const key = a.slice(2);
    if (!(key in spec)) throw new UsageError(`unknown option ${a}`);
    if (spec[key] === 'bool') { out[key] = true; continue; }
    if (i + 1 >= argv.length) throw new UsageError(`${a} needs a value`);
    out[key] = argv[++i];
  }
  return out;
}

function log(obj) { process.stdout.write(JSON.stringify(obj) + '\n'); }

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

// Recordings hold whatever was typed, passwords included, so they are private to the user:
// directories 0700, files 0600.
function writeAtomic(file, data) {
  const tmp = `${file}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, data, { mode: 0o600 });
  fs.renameSync(tmp, file);
}

function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === 'EPERM'; }
}

// liveTargetId returns the id of the page target the browser window shows. dad attaches its
// screencast to the first page target in /json/list, so that is the one the user sees.
async function liveTargetId() {
  const res = await fetch(`${CDP_URL}/json/list`);
  if (!res.ok) throw new Error(`GET ${CDP_URL}/json/list: HTTP ${res.status}`);
  const targets = await res.json();
  const page = targets.find((t) => t.type === 'page' && t.webSocketDebuggerUrl);
  return page ? page.id : null;
}

async function targetIdOf(context, page) {
  const s = await context.newCDPSession(page);
  try {
    const { targetInfo } = await s.send('Target.getTargetInfo');
    return targetInfo.targetId;
  } finally {
    await s.detach().catch(() => {});
  }
}

// connect attaches to the running Chromium and returns the browser, its default context and the
// page the user sees. It waits up to waitMs for the browser to come up (it starts lazily).
async function connect(waitMs = 15000) {
  const deadline = Date.now() + waitMs;
  let browser;
  for (;;) {
    try {
      browser = await chromium.connectOverCDP(CDP_URL, { timeout: 5000 });
      break;
    } catch (e) {
      if (Date.now() > deadline) {
        throw new Error(`no browser on ${CDP_URL}: start it first (POST /v1/browser/start, or open ` +
          `the browser window). ${e.message.split('\n')[0]}`);
      }
      await sleep(250);
    }
  }
  const context = browser.contexts()[0];
  if (!context) throw new Error('the browser has no default context');
  const liveId = await liveTargetId();
  let page = null;
  for (const p of context.pages()) {
    if (liveId && (await targetIdOf(context, p)) === liveId) { page = p; break; }
  }
  if (!page) page = context.pages()[0] || null;
  if (!page) throw new Error('the browser has no open page');
  return { browser, context, page };
}

// disconnect drops the CDP connection. For a connectOverCDP browser, close() only disconnects:
// Chromium, the default context and its pages keep running.
async function disconnect(browser) {
  await browser.close().catch(() => {});
}

async function cmdPages() {
  const { browser, context, page } = await connect();
  try {
    for (const p of context.pages()) {
      log({ live: p === page, target_id: await targetIdOf(context, p), url: p.url(), title: await p.title() });
    }
  } finally {
    await disconnect(browser);
  }
}

function scriptSource(meta, codes) {
  const header = [
    '// Recorded by dad-playwright from the live browser.',
    `// Name: ${meta.name}  Started: ${meta.started}`,
    `// Replay on the live browser: dad-playwright replay ${meta.dir}`,
    '// The function gets the page the browser window shows, its context and the browser.',
    'module.exports = async ({ page, context, browser }) => {',
  ];
  const body = [];
  if (meta.start_url && !/^(about:|chrome:|devtools:)/.test(meta.start_url)) {
    body.push(`  await page.goto(${JSON.stringify(meta.start_url)});`);
  }
  for (const c of codes) body.push(c.replace(/\s+$/, ''));
  return header.concat(body, ['};', '']).join('\n');
}

function readActive() {
  try {
    const a = JSON.parse(fs.readFileSync(ACTIVE, 'utf8'));
    if (a && a.pid && pidAlive(a.pid)) return a;
  } catch (_) { /* none */ }
  return null;
}

async function cmdRecord(argv) {
  const opt = parseArgs(argv, { name: 'str', duration: 'str', 'no-trace': 'bool' });
  if (opt._.length) throw new UsageError(`unexpected argument ${opt._[0]}`);
  const stamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..*/, '').replace('T', '-');
  const name = opt.name || `rec-${stamp}`;
  if (!NAME_RE.test(name)) throw new UsageError(`bad recording name ${JSON.stringify(name)}: use letters, digits, . _ -`);
  const duration = opt.duration === undefined ? 0 : Number(opt.duration);
  if (!Number.isFinite(duration) || duration < 0) throw new UsageError('--duration must be a number of seconds');

  // Catch stop signals from the start, so a stop that races startup still ends cleanly.
  let stopRequested = null;
  const stopWaiters = [];
  const requestStop = (why) => { if (!stopRequested) { stopRequested = why; stopWaiters.forEach((f) => f(why)); } };
  process.once('SIGTERM', () => requestStop('stopped'));
  process.once('SIGINT', () => requestStop('stopped'));

  fs.mkdirSync(RECORDINGS, { recursive: true });
  const active = readActive();
  if (active) throw new Error(`recording ${active.name} is already running (pid ${active.pid}); stop it first`);
  const dir = path.join(RECORDINGS, name);
  if (fs.existsSync(dir)) throw new Error(`${dir} already exists; pick another --name`);

  const { browser, context, page } = await connect();
  fs.mkdirSync(dir, { mode: 0o700 });
  const meta = {
    name, dir, pid: process.pid, started: new Date().toISOString(), stopped: null,
    start_url: page.url(), actions: 0, trace: null, script: path.join(dir, 'script.js'), status: 'recording',
  };
  const codes = [];
  const actionsFile = path.join(dir, 'actions.jsonl');
  const flush = () => {
    writeAtomic(meta.script, scriptSource(meta, codes));
    writeAtomic(path.join(dir, 'recording.json'), JSON.stringify(meta, null, 2) + '\n');
  };
  const note = (event, data, code) => {
    fs.appendFileSync(actionsFile, JSON.stringify({ t: new Date().toISOString(), event, action: data, code }) + '\n', { mode: 0o600 });
  };
  flush();
  writeAtomic(path.join(dir, 'recorder.pid'), `${process.pid}\n`);
  writeAtomic(ACTIVE, JSON.stringify({ name, pid: process.pid, dir }) + '\n');

  const tracing = !opt['no-trace'];
  if (tracing) await context.tracing.start({ name, title: name, screenshots: true, snapshots: true });
  // Playwright's own codegen, in API mode: it watches real input in every page of the context
  // (what the user and dad do in the browser window) and emits the code for each action.
  // _startRecording is not public API; playwright-core is pinned in docker/playwright.
  await context._startRecording({ language: 'javascript' }, {
    actionAdded: (_p, data, code) => { codes.push(code); meta.actions = codes.length; note('added', data, code); flush(); },
    actionUpdated: (_p, data, code) => { codes[codes.length - 1] = code; note('updated', data, code); flush(); },
    signalAdded: (_p, data, code) => {
      if (code && codes.length) codes[codes.length - 1] = code;
      note('signal', data, code);
      flush();
    },
  });
  log({ recording: name, dir, pid: process.pid, start_url: meta.start_url, trace: tracing });

  const reason = await new Promise((resolve) => {
    if (stopRequested) { resolve(stopRequested); return; }
    stopWaiters.push(resolve);
    browser.once('disconnected', () => resolve('browser-gone'));
    if (duration > 0) setTimeout(() => resolve('duration'), duration * 1000);
  });

  let code = 0;
  if (reason !== 'browser-gone') {
    await context._stopRecording().catch(() => {});
    if (tracing) {
      const out = path.join(dir, 'trace.zip');
      await context.tracing.stop({ path: out });
      fs.chmodSync(out, 0o600);
      meta.trace = out;
    }
    meta.status = 'complete';
  } else {
    meta.status = 'browser-gone';
    code = 3;
  }
  meta.stopped = new Date().toISOString();
  flush();
  fs.rmSync(path.join(dir, 'recorder.pid'), { force: true });
  const cur = readActive();
  if (!cur || cur.pid === process.pid) fs.rmSync(ACTIVE, { force: true });
  if (reason !== 'browser-gone') await disconnect(browser);
  log({ stopped: name, reason, status: meta.status, actions: meta.actions, script: meta.script, trace: meta.trace });
  return code;
}

async function cmdStop(argv) {
  const opt = parseArgs(argv, {});
  let name = opt._[0];
  let pid;
  if (!name) {
    const a = readActive();
    if (!a) throw new Error('no recording is running');
    name = a.name; pid = a.pid;
  } else {
    if (!NAME_RE.test(name)) throw new UsageError(`bad recording name ${JSON.stringify(name)}`);
    try { pid = Number(fs.readFileSync(path.join(RECORDINGS, name, 'recorder.pid'), 'utf8').trim()); } catch (_) {
      throw new Error(`recording ${name} is not running`);
    }
  }
  if (!pid || !pidAlive(pid)) throw new Error(`recording ${name} is not running`);
  process.kill(pid, 'SIGTERM');
  const deadline = Date.now() + 30000;
  while (pidAlive(pid)) {
    if (Date.now() > deadline) throw new Error(`recorder pid ${pid} did not exit within 30s`);
    await sleep(100);
  }
  const meta = JSON.parse(fs.readFileSync(path.join(RECORDINGS, name, 'recording.json'), 'utf8'));
  log({ stopped: name, status: meta.status, actions: meta.actions, script: meta.script, trace: meta.trace });
  return meta.status === 'complete' ? 0 : 1;
}

async function cmdReplay(argv) {
  const opt = parseArgs(argv, { trace: 'str', timeout: 'str' });
  if (opt._.length !== 1) throw new UsageError('replay takes one recording directory or script file');
  let file = path.resolve(opt._[0]);
  if (fs.existsSync(file) && fs.statSync(file).isDirectory()) file = path.join(file, 'script.js');
  if (!fs.existsSync(file)) throw new Error(`${file} does not exist`);
  const timeout = opt.timeout === undefined ? 15000 : Number(opt.timeout);
  if (!Number.isFinite(timeout) || timeout <= 0) throw new UsageError('--timeout must be milliseconds');

  const mod = await import(pathToFileURL(file).href);
  const fn = typeof mod.default === 'function' ? mod.default : mod.default && mod.default.default;
  if (typeof fn !== 'function') throw new Error(`${file} must export an async function ({ page, context, browser })`);

  const { browser, context, page } = await connect();
  const started = Date.now();
  let traced = false;
  try {
    context.setDefaultTimeout(timeout);
    if (opt.trace) { await context.tracing.start({ screenshots: true, snapshots: true }); traced = true; }
    await fn({ page, context, browser });
    await page.waitForLoadState('load').catch(() => {});
    if (traced) {
      await context.tracing.stop({ path: path.resolve(opt.trace) });
      fs.chmodSync(path.resolve(opt.trace), 0o600);
      traced = false;
    }
    log({ replayed: file, ok: true, url: page.url(), duration_ms: Date.now() - started, trace: opt.trace ? path.resolve(opt.trace) : null });
    return 0;
  } catch (e) {
    if (traced) await context.tracing.stop({ path: path.resolve(opt.trace) }).catch(() => {});
    log({ replayed: file, ok: false, error: String(e && e.message || e).split('\n')[0], duration_ms: Date.now() - started });
    return 1;
  } finally {
    await disconnect(browser);
  }
}

const USAGE = `usage: dad-playwright <command>
  pages                                   list pages in the live browser (live = the one on screen)
  record [--name N] [--duration S] [--no-trace]
                                          record the live browser into ${RECORDINGS}/N
  stop [N]                                stop a running recording
  replay DIR|FILE [--trace OUT] [--timeout MS]
                                          run a recorded script on the live page
  run FILE [--trace OUT] [--timeout MS]   same as replay, for hand-written scripts`;

async function main(argv) {
  const [cmd, ...rest] = argv;
  switch (cmd) {
    case 'pages': await cmdPages(); return 0;
    case 'record': return cmdRecord(rest);
    case 'stop': return cmdStop(rest);
    case 'replay': case 'run': return cmdReplay(rest);
    case undefined: case '-h': case '--help': case 'help':
      process.stdout.write(USAGE + '\n'); return cmd ? 0 : 2;
    default: throw new UsageError(`unknown command ${cmd}`);
  }
}

main(process.argv.slice(2)).then((code) => process.exit(code || 0), (e) => {
  process.stderr.write(`dad-playwright: ${e.message}\n`);
  if (e instanceof UsageError) process.stderr.write(USAGE + '\n');
  process.exit(e instanceof UsageError ? 2 : 1);
});
