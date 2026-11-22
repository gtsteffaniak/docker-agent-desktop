// Web desktop: a dock, window frames, and a window manager. Ported from quantum-bots'
// computer-desktop.tsx and computer-window-manager.ts, without React or a build step.
//
// Windows are plain rectangles. GTK apps are iframes onto their own Broadway display, and the
// browser is a canvas that shows Chromium's CDP screencast. The desktop draws every frame, so
// GTK apps run with client-side decorations off and fill their window's content area.

const MARGIN = 6;      // gap between windows, the screen edge and the dock
const DOCK_BOTTOM = 8; // gap between the dock and the bottom of the screen
const SNAP_EDGE = 12;  // how close to an edge a released window snaps or maximizes
const MIN_W = 280;
const MIN_H = 160;
const LAYOUT_KEY = "dad:computer-layout";
const FALLBACK_APPS = [
  { id: "browser", name: "Browser", icon: "chromium", order: 5, port: 0 },
  { id: "terminal", name: "Terminal", icon: "lxterminal", order: 10, port: 8081 },
  { id: "files", name: "Files", icon: "org.xfce.thunar", order: 30, port: 8082 },
  { id: "editor", name: "Text Editor", icon: "org.xfce.mousepad", order: 20, port: 8083 },
];

const screenEl = document.getElementById("screen");
const dockEl = document.getElementById("dock");

let apps = [];
let layout = {};            // id -> { x, y, w, h, z, minimized, maximized, restore }
const windows = new Map();  // id -> section element
const browserRuns = new Map(); // id -> { alive: bool }

// ---- Session ----------------------------------------------------------------------------------
//
// The /v1 mutating routes (app start, browser control over the socket) are session-gated: only
// the session holder may act. The desktop holds one "user" session for its mutations, acquired
// lazily on first use, kept alive every 30 s, and released on unload. If an agent holds the
// session, acquisition returns 409 and the desktop keeps working read-only (frames and state
// still flow) until the agent releases it — mutations are refused with the holder named,
// instead of silently failing as they did before. The acquire is de-duplicated so a browser
// window mounting while an app window opens does not race itself into a 409.

let desktopSession = null;       // our session id, or null when we hold nothing
let desktopHolder = null;        // who holds the session when it is not ours
let sessionPromise = null;       // in-flight acquire, so concurrent callers share one
let sessionTimer = 0;            // keepalive interval

async function acquireDesktopSession() {
  if (desktopSession) return desktopSession;
  if (!sessionPromise) {
    sessionPromise = (async () => {
      try {
        const res = await fetch("/v1/session/acquire", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ holder: { kind: "user", name: "desktop" }, ttl_ms: 300000 }),
        });
        if (res.ok) {
          const body = await res.json();
          desktopSession = body.id;
          desktopHolder = null;
          if (!sessionTimer) sessionTimer = setInterval(keepaliveDesktopSession, 30000);
          return desktopSession;
        }
        if (res.status === 409) {
          const body = await res.json().catch(() => null);
          desktopHolder = body && body.holder ? body.holder : null;
        }
      } catch {
        // Offline or the agent is down: retry on the next mutation.
      }
      return null;
    })().finally(() => { sessionPromise = null; });
  }
  return sessionPromise;
}

async function keepaliveDesktopSession() {
  if (!desktopSession) return;
  try {
    const res = await fetch("/v1/session/keepalive", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session_id: desktopSession }),
    });
    if (!res.ok) desktopSession = null; // expired or taken over; re-acquire on next use
  } catch {
    // Keep the id; the next keepalive or acquire retries.
  }
}

function releaseDesktopSession() {
  if (sessionTimer) { clearInterval(sessionTimer); sessionTimer = 0; }
  if (!desktopSession) return;
  const id = desktopSession;
  desktopSession = null;
  fetch("/v1/session/release", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ session_id: id }),
    keepalive: true,
  }).catch(() => {});
}

window.addEventListener("pagehide", releaseDesktopSession);

// ---- Icons ------------------------------------------------------------------------------------

const ICONS = {
  browser: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.6 3 2.6 15 0 18M12 3c-2.6 3-2.6 15 0 18"/>',
  terminal: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9l3 3-3 3M13 15h4"/>',
  files: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
  editor: '<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M14 6l4 4"/>',
  default: '<rect x="3" y="4" width="18" height="16" rx="2"/>',
};

function iconFor(id) {
  return `<svg viewBox="0 0 24 24" aria-hidden="true">${ICONS[id] || ICONS.default}</svg>`;
}

// ---- Layout model ------------------------------------------------------------------------------

function desk() {
  return { w: screenEl.clientWidth, h: screenEl.clientHeight };
}

function maxZ() {
  return Math.max(0, ...Object.values(layout).map((w) => w.z));
}

function clamp(v, lo, hi) {
  return Math.max(lo, Math.min(v, hi));
}

// Keeps a window fully on screen. A window larger than the screen is pinned to the top-left.
function clampRect(r, d) {
  const w = clamp(r.w, MIN_W, Math.max(MIN_W, d.w));
  const h = clamp(r.h, MIN_H, Math.max(MIN_H, d.h));
  const x = clamp(r.x, 0, Math.max(0, d.w - w));
  const y = clamp(r.y, 0, Math.max(0, d.h - h));
  return { ...r, x, y, w, h };
}

// Snaps to the left or right half when the window is released near a side edge.
function snapRect(r, d) {
  const half = Math.floor((d.w - MARGIN * 3) / 2);
  const height = d.h - dockReserve();
  if (r.x <= SNAP_EDGE) return { ...r, x: MARGIN, y: MARGIN, w: half, h: height, maximized: false };
  if (r.x + r.w >= d.w - SNAP_EDGE) return { ...r, x: MARGIN * 2 + half, y: MARGIN, w: half, h: height, maximized: false };
  return r;
}

// Vertical space the dock needs at the bottom of the screen, plus the gap kept above it. The dock
// sits DOCK_BOTTOM from the bottom edge, so a maximized window stops MARGIN above the dock's top edge.
function dockReserve() {
  return (dockEl.offsetHeight || 66) + DOCK_BOTTOM + MARGIN * 2;
}

// The area above the dock that a maximized window fills.
function maximizedRect(d) {
  return {
    x: MARGIN,
    y: MARGIN,
    w: Math.max(MIN_W, d.w - MARGIN * 2),
    h: Math.max(MIN_H, d.h - dockReserve()),
  };
}

function defaultRect(id, index) {
  if (id === "browser") return { x: 32, y: 28, w: 920, h: 580 };
  const cascade = (index % 5) * 24;
  return { x: 48 + cascade, y: 48 + cascade, w: 720, h: 480 };
}

function loadLayout() {
  try {
    const raw = localStorage.getItem(LAYOUT_KEY);
    const parsed = raw ? JSON.parse(raw) : {};
    const out = {};
    for (const [id, v] of Object.entries(parsed || {})) {
      if (!v || typeof v !== "object") continue;
      const nums = [v.x, v.y, v.w, v.h, v.z].map(Number);
      if (!nums.every(Number.isFinite)) continue;
      out[id] = {
        x: nums[0], y: nums[1], w: nums[2], h: nums[3], z: nums[4],
        minimized: !!v.minimized, maximized: !!v.maximized, restore: v.restore || null,
      };
    }
    return out;
  } catch {
    return {};
  }
}

function saveLayout() {
  try {
    localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout));
  } catch {
    // Storage can be full or disabled. The desktop still works without it.
  }
}

function bringToFront(id) {
  const w = layout[id];
  if (!w || w.minimized) return;
  const top = maxZ();
  if (w.z !== top) {
    layout[id] = { ...w, z: top + 1 };
    renderWindow(id);
    saveLayout();
  }
}

function focusedId() {
  const visible = Object.entries(layout).filter(([, w]) => !w.minimized);
  if (visible.length === 0) return null;
  return visible.reduce((a, b) => (b[1].z >= a[1].z ? b : a))[0];
}

// ---- Window actions ----------------------------------------------------------------------------

function openWindow(id) {
  if (!layout[id]) {
    const index = Object.keys(layout).length;
    const r = defaultRect(id, index);
    layout[id] = { ...r, z: maxZ() + 1, minimized: false, maximized: false, restore: null };
  } else {
    layout[id] = { ...layout[id], minimized: false, z: maxZ() + 1 };
  }
  ensureWindow(id);
  renderWindow(id);
  saveLayout();
  renderDock();
}

// Dock click: open, restore, focus, or minimize when already focused (the quantum-bots rule).
function dockActivate(id) {
  const target = layout[id];
  if (!target) return openWindow(id);
  if (target.minimized) return openWindow(id);
  if (focusedId() === id) return minimizeWindow(id);
  bringToFront(id);
}

function minimizeWindow(id) {
  if (!layout[id]) return;
  layout[id] = { ...layout[id], minimized: true };
  renderWindow(id);
  saveLayout();
  renderDock();
}

function closeWindow(id) {
  const el = windows.get(id);
  if (el) el.remove();
  windows.delete(id);
  const run = browserRuns.get(id);
  if (run) {
    run.alive = false;
    if (run.stop) run.stop();
  }
  browserRuns.delete(id);
  delete layout[id];
  saveLayout();
  renderDock();
}

function toggleMaximize(id) {
  const w = layout[id];
  if (!w) return;
  if (w.maximized) {
    const back = w.restore || defaultRect(id, 0);
    layout[id] = { ...w, ...back, maximized: false, restore: null };
  } else {
    const d = desk();
    const restore = { x: w.x, y: w.y, w: w.w, h: w.h };
    layout[id] = {
      ...w,
      restore,
      maximized: true,
      x: MARGIN,
      y: MARGIN,
      w: Math.max(MIN_W, d.w - MARGIN * 2),
      h: Math.max(MIN_H, d.h - dockReserve()),
    };
  }
  layout[id] = clampRect(layout[id], desk());
  renderWindow(id);
  saveLayout();
}

// ---- Rendering ---------------------------------------------------------------------------------

function appById(id) {
  return apps.find((a) => a.id === id) || { id, name: id, port: 0 };
}

function ensureWindow(id) {
  if (windows.has(id)) return windows.get(id);
  const app = appById(id);
  const el = document.createElement("section");
  el.className = "window";
  el.dataset.window = id;
  el.dataset.testid = `computer-window-${id}`;

  const header = document.createElement("header");
  header.className = "titlebar";
  header.dataset.testid = `computer-window-title-${id}`;
  header.innerHTML = `
    <span class="lights">
      <button class="light close" type="button" aria-label="Close" data-testid="computer-window-close-${id}"></button>
      <button class="light min" type="button" aria-label="Minimize" data-testid="computer-window-min-${id}"></button>
      <button class="light max" type="button" aria-label="Maximize" data-testid="computer-window-max-${id}"></button>
    </span>
    <span class="title"></span>
    <span class="title-spacer" aria-hidden="true"></span>`;
  el.appendChild(header);

  const content = document.createElement("div");
  content.className = "content";
  el.appendChild(content);

  if (id === "browser") {
    mountBrowser(content, el);
  } else {
    const frame = document.createElement("iframe");
    frame.title = app.name;
    frame.allow = "clipboard-read; clipboard-write";
    content.appendChild(frame);
    // Start the app's display first, so the frame does not load before the app is listening.
    // Starting a running app does nothing on the agent, so reopening a window is cheap.
    // App start is session-gated: the desktop acquires its user session first. If an agent
    // holds the session the start is refused (409) and the frame still loads — reads are
    // open — but the app may not be running behind it; reopening the window retries.
    acquireDesktopSession().then((sid) => {
      const headers = sid ? { "X-Dad-Session": sid } : {};
      fetch(`/v1/apps/start?id=${encodeURIComponent(app.id)}`, { method: "POST", headers, cache: "no-store" })
        .catch(() => {})
        .finally(() => { frame.src = `/apps/${encodeURIComponent(app.id)}/`; });
    });
  }

  for (const edge of ["n", "s", "e", "w", "ne", "nw", "se", "sw"]) {
    const handle = document.createElement("div");
    handle.className = `edge ${edge}`;
    handle.dataset.edge = edge;
    el.appendChild(handle);
  }

  header.querySelector(".close").addEventListener("click", (e) => { e.stopPropagation(); closeWindow(id); });
  header.querySelector(".min").addEventListener("click", (e) => { e.stopPropagation(); minimizeWindow(id); });
  header.querySelector(".max").addEventListener("click", (e) => { e.stopPropagation(); toggleMaximize(id); });
  header.addEventListener("pointerdown", (e) => {
    if (e.target.closest("button")) return;
    beginMove(id, e);
  });
  header.addEventListener("dblclick", (e) => {
    if (!e.target.closest("button")) toggleMaximize(id);
  });
  el.addEventListener("pointerdown", () => bringToFront(id), true);
  for (const handle of el.querySelectorAll(".edge")) {
    handle.addEventListener("pointerdown", (e) => beginResize(id, handle.dataset.edge, e));
  }

  screenEl.appendChild(el);
  windows.set(id, el);
  el.querySelector(".title").textContent = app.name;
  return el;
}

function renderWindow(id) {
  const el = windows.get(id);
  const w = layout[id];
  if (!el || !w) return;
  el.hidden = !!w.minimized;
  el.style.left = `${w.x}px`;
  el.style.top = `${w.y}px`;
  el.style.width = `${w.w}px`;
  el.style.height = `${w.h}px`;
  el.style.zIndex = String(w.z);
  el.classList.toggle("active", focusedId() === id);
  for (const handle of el.querySelectorAll(".edge")) handle.hidden = !!w.maximized;
}

function renderAll() {
  for (const id of Object.keys(layout)) {
    ensureWindow(id);
    renderWindow(id);
  }
  for (const id of [...windows.keys()]) {
    if (!layout[id]) closeWindow(id);
  }
  renderDock();
}

function renderDock() {
  dockEl.innerHTML = "";
  for (const app of apps) {
    const w = layout[app.id];
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "dock-item" + (w && w.minimized ? " minimized" : "");
    btn.dataset.testid = `computer-dock-${app.id}`;
    btn.setAttribute("aria-label", app.name);
    btn.title = app.name;
    btn.innerHTML = iconFor(app.id);
    btn.addEventListener("click", () => dockActivate(app.id));
    if (w) {
      const dot = document.createElement("span");
      dot.className = "dock-dot";
      dot.dataset.testid = `computer-dock-dot-${app.id}`;
      btn.appendChild(dot);
    }
    dockEl.appendChild(btn);
  }
}

// ---- Pointer interaction -----------------------------------------------------------------------

// Drags a window by its titlebar. Pointer capture keeps events on the titlebar even when the
// pointer crosses an iframe.
function beginMove(id, e) {
  if (e.button !== 0) return;
  e.preventDefault();
  let r = layout[id];
  if (!r) return;
  if (r.maximized) {
    const back = r.restore || defaultRect(id, 0);
    const grab = (e.clientX - r.x) / Math.max(1, r.w);
    r = { ...r, ...back, maximized: false, restore: null };
    r.x = Math.round(e.clientX - grab * r.w);
    r.y = Math.round(e.clientY - 18);
    layout[id] = r;
  }
  const el = windows.get(id);
  const start = { x: e.clientX, y: e.clientY, ox: r.x, oy: r.y };
  let current = { ...r };
  bringToFront(id);
  const header = e.currentTarget;
  header.setPointerCapture(e.pointerId);

  const move = (ev) => {
    current = clampRect({ ...r, x: start.ox + ev.clientX - start.x, y: start.oy + ev.clientY - start.y }, desk());
    el.style.left = `${current.x}px`;
    el.style.top = `${current.y}px`;
  };
  const up = () => {
    header.removeEventListener("pointermove", move);
    header.removeEventListener("pointerup", up);
    header.removeEventListener("pointercancel", up);
    const d = desk();
    const dropped = { ...layout[id], x: current.x, y: current.y };
    const sideways = dropped.x <= SNAP_EDGE || dropped.x + dropped.w >= d.w - SNAP_EDGE;
    if (!sideways && dropped.y <= SNAP_EDGE) {
      // Released at the top edge: maximize, and keep the pre-drag size for restoring.
      layout[id] = { ...dropped, ...maximizedRect(d), maximized: true, restore: { x: r.x, y: r.y, w: r.w, h: r.h } };
    } else {
      layout[id] = clampRect(snapRect(dropped, d), d);
    }
    renderWindow(id);
    saveLayout();
  };
  header.addEventListener("pointermove", move);
  header.addEventListener("pointerup", up);
  header.addEventListener("pointercancel", up);
}

function resizeRect(start, edge, dx, dy, d) {
  let { x, y, w, h } = start;
  if (edge.includes("e")) w = Math.max(MIN_W, w + dx);
  if (edge.includes("w")) {
    const nw = Math.max(MIN_W, w - dx);
    x = x + (w - nw);
    w = nw;
  }
  if (edge.includes("s")) h = Math.max(MIN_H, h + dy);
  if (edge.includes("n")) {
    const nh = Math.max(MIN_H, h - dy);
    y = y + (h - nh);
    h = nh;
  }
  return clampRect({ ...start, x, y, w, h, maximized: false }, d);
}

function beginResize(id, edge, e) {
  if (e.button !== 0) return;
  e.preventDefault();
  e.stopPropagation();
  const r = layout[id];
  if (!r || r.maximized) return;
  const el = windows.get(id);
  const handle = e.currentTarget;
  handle.setPointerCapture(e.pointerId);
  bringToFront(id);
  const start = { x: e.clientX, y: e.clientY };
  let current = { ...r };
  const move = (ev) => {
    current = resizeRect(r, edge, ev.clientX - start.x, ev.clientY - start.y, desk());
    el.style.left = `${current.x}px`;
    el.style.top = `${current.y}px`;
    el.style.width = `${current.w}px`;
    el.style.height = `${current.h}px`;
  };
  const up = () => {
    handle.removeEventListener("pointermove", move);
    handle.removeEventListener("pointerup", up);
    handle.removeEventListener("pointercancel", up);
    layout[id] = { ...layout[id], ...current, maximized: false };
    renderWindow(id);
    saveLayout();
  };
  handle.addEventListener("pointermove", move);
  handle.addEventListener("pointerup", up);
  handle.addEventListener("pointercancel", up);
}

// ---- Browser window ----------------------------------------------------------------------------
//
// Chromium runs headless on the agent. The agent streams frames (/v1/browser/stream), reports
// the real URL and history (/v1/browser/state), and takes input over the same socket. The
// address bar follows the real URL unless the user is typing in it.

function mountBrowser(content, windowEl) {
  content.innerHTML = `
    <div class="browser-bar">
      <button type="button" class="back" aria-label="Back" title="Back">←</button>
      <button type="button" class="forward" aria-label="Forward" title="Forward">→</button>
      <button type="button" class="reload" aria-label="Reload" title="Reload">⟳</button>
      <input class="address" type="text" spellcheck="false" autocomplete="off"
             placeholder="Search or enter address" aria-label="Address">
      <button type="button" class="go" aria-label="Go" title="Go"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h13M13 6l6 6-6 6"/></svg></button>
    </div>
    <div class="page" tabindex="0" data-testid="computer-browser-page">
      <canvas aria-label="Browser"></canvas>
      <div class="page-status">Connecting to browser…</div>
      <div class="start-page" data-testid="computer-browser-start">
        <div class="start-inner">
          <h1>Browser</h1>
          <p class="start-note">Pages open here. Chromium starts the first time you go somewhere.</p>
          <form class="start-search">
            <input type="text" placeholder="Search or enter address" aria-label="Search or enter address" autocomplete="off">
          </form>
          <h2>Getting started</h2>
          <ul class="start-links">
            <li><button type="button" data-url="https://example.com">Example Domain</button><span>A simple test page</span></li>
            <li><button type="button" data-url="https://en.wikipedia.org">Wikipedia</button><span>The free encyclopedia</span></li>
            <li><button type="button" data-url="https://developer.mozilla.org">MDN Web Docs</button><span>Reference for the web platform</span></li>
            <li><button type="button" data-url="https://news.ycombinator.com">Hacker News</button><span>Technology news and discussion</span></li>
          </ul>
        </div>
      </div>
    </div>`;

  const id = "browser";
  const run = { alive: true };
  browserRuns.set(id, run);
  const back = content.querySelector(".back");
  const forward = content.querySelector(".forward");
  const address = content.querySelector(".address");
  const go = content.querySelector(".go");
  const page = content.querySelector(".page");
  const canvas = content.querySelector("canvas");
  const status = content.querySelector(".page-status");
  const start = content.querySelector(".start-page");
  const startNote = content.querySelector(".start-note");
  const ctx = canvas.getContext("2d");

  let dirty = false;        // the user is editing the address bar
  let pendingUntil = 0;     // after Enter, keep the typed text until this time
  let pendingFrom = "";     // the URL when Enter was pressed
  let lastUrl = "";
  let running = false;      // Chromium is up (from the agent's state messages)
  let live = false;         // a frame has arrived recently
  let lastBitmap = null;
  let sock = null;
  let nextId = 1;
  const waiting = new Map(); // command id -> resolve, for commands that get an ack

  const setLive = (v) => {
    if (live === v) return;
    live = v;
    status.hidden = v;
  };

  // Draws the last frame stretched to the canvas. The canvas always matches the window box, so
  // during a resize the picture stretches to fit instead of showing a mismatched size.
  const paint = () => {
    if (!lastBitmap) return;
    const dpr = window.devicePixelRatio || 1;
    const cw = Math.max(1, Math.round(page.clientWidth * dpr));
    const ch = Math.max(1, Math.round(page.clientHeight * dpr));
    if (canvas.width !== cw || canvas.height !== ch) {
      canvas.width = cw;
      canvas.height = ch;
    }
    ctx.drawImage(lastBitmap, 0, 0, canvas.width, canvas.height);
  };

  // -- The socket: frames and state come down, commands and input go up --

  const sendRaw = (msg) => {
    if (sock && sock.readyState === WebSocket.OPEN) {
      sock.send(JSON.stringify(msg));
      return true;
    }
    return false;
  };

  // A command that gets an ack. Resolves with {ok, error}, and never rejects.
  const command = (msg) => new Promise((resolve) => {
    const cid = nextId++;
    waiting.set(cid, resolve);
    if (!sendRaw({ ...msg, id: cid })) {
      waiting.delete(cid);
      resolve({ ok: false, error: "Not connected to the desktop" });
    }
  });

  const applyState = (st) => {
    const wasRunning = running;
    running = !!st.running;
    if (!running) {
      // Chromium has not started. The start page stays in front, and nothing is streamed.
      start.hidden = false;
      back.disabled = true;
      forward.disabled = true;
      windowEl.querySelector(".title").textContent = "Browser";
      if (!dirty) address.value = "";
      lastUrl = "";
      setLive(false);
      return;
    }
    start.hidden = true;
    if (!wasRunning) sendSize(); // Chromium just started: give it the window's size
    back.disabled = !st.can_back;
    forward.disabled = !st.can_forward;
    windowEl.querySelector(".title").textContent = st.title || st.url || "Browser";
    const pending = Date.now() < pendingUntil && st.url === pendingFrom;
    if (!pending) pendingUntil = 0;
    if (!dirty && !pending && st.url !== address.value) address.value = st.url;
    lastUrl = st.url;
  };

  const connect = async () => {
    if (!run.alive) return;
    // A WebSocket cannot carry the X-Dad-Session header, so the session travels as
    // ?session=<id> at connect time. Without a session the socket still streams frames
    // and state, but every control message is refused until a session is bound.
    const sid = await acquireDesktopSession();
    if (!run.alive) return;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const query = sid ? `?session=${encodeURIComponent(sid)}` : "";
    const s = new WebSocket(`${proto}//${location.host}/v1/browser/stream${query}`);
    s.binaryType = "blob";
    s.onmessage = async (ev) => {
      if (typeof ev.data !== "string") {
        try {
          const bmp = await createImageBitmap(ev.data);
          if (lastBitmap) lastBitmap.close();
          lastBitmap = bmp;
          paint();
          setLive(true);
        } catch {
          // A bad frame is skipped; the next one replaces it.
        }
        return;
      }
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (msg.type === "state") {
        applyState(msg);
      } else if (msg.type === "ack" && waiting.has(msg.id)) {
        const done = waiting.get(msg.id);
        waiting.delete(msg.id);
        done(msg);
      }
    };
    s.onclose = () => {
      if (sock === s) sock = null;
      for (const done of waiting.values()) done({ ok: false, error: "Disconnected" });
      waiting.clear();
      setLive(false);
      if (run.alive) setTimeout(connect, 1000);
    };
    sock = s;
  };

  // -- Size: the viewport follows the window, throttled while it changes --
  let sizeTimer = 0;
  let sizeAt = 0;
  const sendSize = () => {
    if (!running) return;
    sizeAt = Date.now();
    const w = Math.round(page.clientWidth);
    const h = Math.round(page.clientHeight);
    if (w > 0 && h > 0) sendRaw({ type: "size", w, h, scale: window.devicePixelRatio || 1 });
  };
  const resizeObs = new ResizeObserver(() => {
    paint(); // stretch the last picture now, so the frame never lags the window
    const wait = Math.max(0, 120 - (Date.now() - sizeAt));
    clearTimeout(sizeTimer);
    sizeTimer = setTimeout(sendSize, wait);
  });
  resizeObs.observe(page);
  run.stop = () => {
    resizeObs.disconnect();
    clearTimeout(sizeTimer);
    if (sock) sock.close();
    if (lastBitmap) lastBitmap.close();
  };

  // -- Actions --

  const navigate = async (text) => {
    const value = text.trim();
    if (!value) return;
    dirty = false;
    pendingFrom = lastUrl;
    pendingUntil = Date.now() + 4000;
    const res = await command({ type: "navigate", url: value });
    if (!res.ok) {
      pendingUntil = 0;
      startNote.textContent = res.error || "Could not open that address";
      address.title = res.error || "";
    }
    page.focus();
  };

  // The start page's links and search box use the same navigation as the address bar.
  start.querySelectorAll("[data-url]").forEach((b) => {
    b.addEventListener("click", () => {
      startNote.textContent = "Starting browser…";
      navigate(b.dataset.url);
    });
  });
  start.querySelector(".start-search").addEventListener("submit", (e) => {
    e.preventDefault();
    const input = start.querySelector(".start-search input");
    startNote.textContent = "Starting browser…";
    navigate(input.value);
  });

  back.addEventListener("click", () => command({ type: "back" }));
  forward.addEventListener("click", () => command({ type: "forward" }));
  content.querySelector(".reload").addEventListener("click", () => command({ type: "reload" }));
  go.addEventListener("click", () => navigate(address.value));
  address.addEventListener("keydown", (e) => {
    e.stopPropagation();
    if (e.key === "Enter") navigate(address.value);
    if (e.key === "Escape") {
      dirty = false;
      address.value = lastUrl;
      address.blur();
    }
  });
  address.addEventListener("input", () => { dirty = true; });
  address.addEventListener("blur", () => { dirty = false; });
  address.addEventListener("focus", () => address.select());
  address.addEventListener("pointerdown", (e) => e.stopPropagation());

  // -- Input: sent in order over the socket. Pointer moves are sent once per frame. --

  const pagePoint = (e) => {
    const r = canvas.getBoundingClientRect();
    return {
      x: Math.round(clamp(e.clientX - r.left, 0, r.width)),
      y: Math.round(clamp(e.clientY - r.top, 0, r.height)),
    };
  };
  const mods = (e) => (e.altKey ? 1 : 0) | (e.ctrlKey ? 2 : 0) | (e.metaKey ? 4 : 0) | (e.shiftKey ? 8 : 0);
  // The CDP button numbers: left 1, middle 2, right 3.
  const buttonCode = (b) => (b === 0 ? 1 : b === 1 ? 2 : b === 2 ? 3 : 0);

  const sendPointer = (action, e, button = 0) => {
    const p = pagePoint(e);
    sendRaw({ type: "pointer", action, x: p.x, y: p.y, button, buttons: e.buttons, clicks: e.detail || 1, mods: mods(e) });
  };

  let moveFrame = 0;
  let lastMove = null;
  page.addEventListener("mousedown", (e) => {
    page.focus();
    sendPointer("press", e, buttonCode(e.button));
  });
  page.addEventListener("mouseup", (e) => sendPointer("release", e, buttonCode(e.button)));
  page.addEventListener("mousemove", (e) => {
    lastMove = e;
    if (moveFrame) return;
    moveFrame = requestAnimationFrame(() => {
      moveFrame = 0;
      if (lastMove) sendPointer("move", lastMove);
    });
  });
  page.addEventListener("wheel", (e) => {
    e.preventDefault();
    const p = pagePoint(e);
    sendRaw({ type: "scroll", x: p.x, y: p.y, dx: Math.round(e.deltaX), dy: Math.round(e.deltaY), mods: mods(e) });
  }, { passive: false });
  page.addEventListener("contextmenu", (e) => e.preventDefault());

  // Browser-only shortcuts stay with the desktop, as the quantum-bots keys script does for apps.
  const keepsBrowserKey = (e) =>
    e.key === "F11" || e.key === "F12" ||
    (e.ctrlKey && e.shiftKey && /^[ICJ]$/i.test(e.key));

  page.addEventListener("keydown", (e) => {
    if (keepsBrowserKey(e)) return;
    e.preventDefault();
    if ((e.ctrlKey || e.metaKey) && (e.key === "v" || e.key === "V")) {
      if (navigator.clipboard && navigator.clipboard.readText) {
        navigator.clipboard.readText().then((text) => {
          if (text) sendRaw({ type: "paste", text });
        }).catch(() => {});
      }
      return;
    }
    const text = e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey ? e.key : "";
    sendRaw({ type: "key", action: "down", key: e.key, code: e.code, text, mods: mods(e) });
  });
  page.addEventListener("keyup", (e) => {
    if (keepsBrowserKey(e)) return;
    e.preventDefault();
    sendRaw({ type: "key", action: "up", key: e.key, code: e.code, text: "", mods: mods(e) });
  });

  connect();
}

// ---- Startup -----------------------------------------------------------------------------------

async function loadApps() {
  try {
    const res = await fetch("/v1/apps", { cache: "no-store" });
    if (res.ok) {
      const body = await res.json();
      if (Array.isArray(body.apps) && body.apps.length) return body.apps;
    }
  } catch {
    // Fall through to the built-in list.
  }
  return FALLBACK_APPS;
}

function reflowAll() {
  for (const id of Object.keys(layout)) {
    if (layout[id].maximized) {
      toggleMaximize(id);
      toggleMaximize(id);
    } else {
      layout[id] = clampRect(layout[id], desk());
      renderWindow(id);
    }
  }
  saveLayout();
}

async function start() {
  apps = (await loadApps()).slice().sort((a, b) => (a.order ?? 100) - (b.order ?? 100));
  const known = new Set(apps.map((a) => a.id));
  layout = loadLayout();
  for (const id of Object.keys(layout)) {
    if (!known.has(id)) delete layout[id];
  }
  // First boot: only the browser window, as in quantum-bots.
  if (Object.keys(layout).length === 0 && known.has("browser")) {
    layout.browser = { ...defaultRect("browser", 0), z: 1, minimized: false, maximized: false, restore: null };
  }
  renderAll();
  window.addEventListener("resize", reflowAll);
}

start();
