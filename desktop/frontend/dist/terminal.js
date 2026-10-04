// Interactive terminals (xterm.js) on the server's existing SSH connection: a host shell, or a shell
// inside a container (docker exec). Every server tab has its own terminals; they keep running while hidden.
import { Terminal } from './vendor/xterm.mjs';
import { FitAddon } from './vendor/addon-fit.mjs';
import { SearchAddon } from './vendor/addon-search.mjs';
import { h, toast } from './util.js';
import { call, on, copyText } from './bridge.js';
import { cssVar } from './chart.js';

const host = document.getElementById('term-host');
const byServer = new Map(); // server id -> [term]
const byId = new Map(); // backend id -> term
let shown = null; // term currently visible
let fontSize = 13;
try { fontSize = Number(localStorage.getItem('termFontSize')) || 13; } catch {}

// ANSI colors readable on each theme's background.
const PALETTES = {
  dark: {
    black: '#1a1a19', red: '#e66767', green: '#4fc46a', yellow: '#e5b93b', blue: '#5b9cf0', magenta: '#d57ad5', cyan: '#3fbfbf', white: '#c3c2b7',
    brightBlack: '#6f6d68', brightRed: '#ff8a8a', brightGreen: '#7ee08f', brightYellow: '#f5d36b', brightBlue: '#86b6ef', brightMagenta: '#ec9cec', brightCyan: '#6fdcdc', brightWhite: '#ffffff',
  },
  light: {
    black: '#0b0b0b', red: '#b42d2d', green: '#1f7a1f', yellow: '#8a6100', blue: '#1c5cab', magenta: '#8f3f8f', cyan: '#0e7272', white: '#6f6d68',
    brightBlack: '#52514e', brightRed: '#d03b3b', brightGreen: '#2a9a2a', brightYellow: '#a87800', brightBlue: '#2a78d6', brightMagenta: '#a64ca6', brightCyan: '#138a8a', brightWhite: '#3a3a38',
  },
};

function isDark() {
  const t = document.documentElement.dataset.theme;
  return t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
}

function theme() {
  return {
    ...PALETTES[isDark() ? 'dark' : 'light'],
    background: cssVar('--page'),
    foreground: cssVar('--ink'),
    cursor: cssVar('--accent'),
    cursorAccent: cssVar('--page'),
    selectionBackground: isDark() ? 'rgba(134, 182, 239, 0.35)' : 'rgba(42, 120, 214, 0.25)',
  };
}

function applyTheme() {
  for (const list of byServer.values()) for (const t of list) t.xterm.options.theme = theme();
}
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', applyTheme);
new MutationObserver(applyTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

function decode(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

async function paste(t) {
  try {
    const text = await window.runtime.ClipboardGetText();
    if (text) t.xterm.paste(text);
  } catch (err) {
    toast(`Could not paste: ${err.message}`, true);
  }
}

async function copySelection(t) {
  const text = t.xterm.getSelection();
  if (!text) return false;
  await copyText(text);
  t.xterm.clearSelection();
  return true;
}

async function connect(t) {
  t.ended = false;
  t.finished = false;
  const { cols, rows } = t.xterm;
  try {
    if (t.task) {
      const r = await call('OpenTask', t.server, t.task.kind, t.task.arg || '', cols, rows);
      t.id = r.id;
    } else {
      t.id = await call('OpenTerminal', t.server, t.container, cols, rows);
    }
    byId.set(t.id, t);
  } catch (err) {
    t.ended = true;
    t.xterm.write(`\r\n\x1b[31m${err.message}\x1b[0m\r\n\x1b[2mPress Enter to try again.\x1b[0m\r\n`);
  }
  // The dock tab shows whether the session is connected.
  document.dispatchEvent(new CustomEvent('dock:changed', { detail: { key: t.server } }));
}

// Creates a terminal for a server. opts: a container name (shell inside it), or
// { task: { kind, arg, title, once } } to run an operation (compose, apt upgrade, journal…).
export function createTerminal(serverSnap, opts = '') {
  const { container = '', task = null } = typeof opts === 'string' ? { container: opts } : opts;
  const list = byServer.get(serverSnap.id) || [];
  // Created visible: xterm measures its character cells when it opens.
  hideAll();
  const el = h('div', { class: 'term-view' });
  host.append(el);
  const xterm = new Terminal({
    fontFamily: '"Cascadia Mono", Consolas, "DejaVu Sans Mono", monospace',
    fontSize,
    cursorBlink: true,
    scrollback: 5000,
    allowProposedApi: true,
    theme: theme(),
  });
  const fit = new FitAddon();
  const search = new SearchAddon();
  xterm.loadAddon(fit);
  xterm.loadAddon(search);
  xterm.open(el);
  const base = task ? task.title : container ? `exec ${container}` : serverSnap.name;
  const used = new Set(list.map((x) => x.title));
  let title = base;
  for (let k = 2; used.has(title); k++) title = `${base} #${k}`;
  const t = {
    id: null, server: serverSnap.id, container, task, ended: true, finished: false, el, xterm, fit, search,
    title,
    target: task ? task.title : container ? `container ${container}` : serverSnap.name,
  };
  shown = t;
  list.push(t);
  byServer.set(serverSnap.id, list);

  xterm.onData((data) => {
    if (t.ended) {
      // A one-off task (compose, upgrade) is not run again by accident: it has to be started again on purpose.
      if (t.task?.once && t.finished) return;
      if (data.includes('\r')) { xterm.reset(); connect(t); }
      return;
    }
    call('TermInput', t.id, data).catch(() => {});
  });
  let resizeTimer = 0;
  xterm.onResize(({ cols, rows }) => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => { if (t.id && !t.ended) call('TermResize', t.id, cols, rows).catch(() => {}); }, 80);
  });
  // Copy / paste like Windows Terminal; keep the app's own shortcuts out of the shell.
  xterm.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown') return true;
    const k = e.key.toLowerCase();
    if (e.ctrlKey && e.shiftKey && k === 'c') { copySelection(t); return false; }
    if (e.ctrlKey && e.shiftKey && k === 'v') { paste(t); e.preventDefault(); return false; }
    if (e.ctrlKey && e.shiftKey && k === 'f') { document.dispatchEvent(new CustomEvent('term:search')); return false; }
    if (e.ctrlKey && e.shiftKey && k === 'w') return false; // closes this terminal (handled by the dock)
    if (e.ctrlKey && !e.shiftKey && (k === '=' || k === '+')) { setFontSize(fontSize + 1); return false; }
    if (e.ctrlKey && !e.shiftKey && k === '-') { setFontSize(fontSize - 1); return false; }
    return true;
  });
  el.addEventListener('contextmenu', async (e) => {
    e.preventDefault();
    if (!(await copySelection(t))) paste(t);
  });
  new ResizeObserver(() => { if (!el.hidden) fitSafe(t); }).observe(el);
  return t;
}

function fitSafe(t) {
  try { t.fit.fit(); } catch {}
}

export async function startTerminal(t) {
  fitSafe(t);
  t.xterm.write(t.task
    ? `\x1b[2mRunning "${t.target}" over the existing SSH connection…\x1b[0m\r\n`
    : `\x1b[2mOpening a shell on ${t.target} over the existing SSH connection…\x1b[0m\r\n`);
  await connect(t);
  t.xterm.focus();
}

export function show(t) {
  for (const list of byServer.values()) for (const x of list) x.el.hidden = x !== t;
  shown = t;
  if (t) {
    requestAnimationFrame(() => {
      fitSafe(t);
      t.xterm.focus();
    });
  }
}

export function hideAll() {
  for (const list of byServer.values()) for (const x of list) x.el.hidden = true;
  shown = null;
}

export const terminalsOf = (serverId) => byServer.get(serverId) || [];

export function closeTerminal(t) {
  if (t.id) {
    call('CloseTerminal', t.id).catch(() => {});
    byId.delete(t.id);
  }
  t.xterm.dispose();
  t.el.remove();
  const list = terminalsOf(t.server).filter((x) => x !== t);
  if (list.length) byServer.set(t.server, list);
  else byServer.delete(t.server);
  if (shown === t) shown = null;
}

export function closeServerTerminals(serverId) {
  for (const t of [...terminalsOf(serverId)]) closeTerminal(t);
}

export function setFontSize(size) {
  fontSize = Math.max(9, Math.min(24, size));
  try { localStorage.setItem('termFontSize', String(fontSize)); } catch {}
  for (const list of byServer.values()) for (const t of list) t.xterm.options.fontSize = fontSize;
  if (shown) fitSafe(shown);
}
export const getFontSize = () => fontSize;
export const shownTerminal = () => shown;

export function initTerminals() {
  on('term.data', (msg) => byId.get(msg.id)?.xterm.write(decode(msg.data)));
  on('term.exit', (msg) => {
    const t = byId.get(msg.id);
    if (!t) return;
    byId.delete(msg.id);
    t.id = null;
    t.ended = true;
    if (t.task?.once) {
      t.finished = true;
      t.xterm.write('\r\n\x1b[2mFinished. Close this tab with ✕ or Ctrl+Shift+W.\x1b[0m\r\n');
    } else {
      t.xterm.write(`\r\n\x1b[2m[${msg.reason}] Press Enter to reconnect.\x1b[0m\r\n`);
    }
    document.dispatchEvent(new CustomEvent('dock:changed', { detail: { key: t.server } }));
    if (t.task) document.dispatchEvent(new CustomEvent('task:exit', { detail: { key: t.server, task: t.task, code: msg.code ?? 0 } }));
  });
}
