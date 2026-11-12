// Interactive terminals (xterm.js) on the server's existing SSH connection: a host shell, or a shell
// inside a container (docker exec). Every server tab has its own terminals; they keep running while hidden.
import { Terminal } from './vendor/xterm.mjs';
import { FitAddon } from './vendor/addon-fit.mjs';
import { SearchAddon } from './vendor/addon-search.mjs';
import { h, toast, IS_MAC, kbd } from './util.js';
import { call, on, copyText } from './bridge.js';

const host = document.getElementById('term-host');
const byServer = new Map(); // server id -> [term]
const byId = new Map(); // backend id -> term
let shown = null; // term currently visible

const IS_LINUX = !IS_MAC && /Linux|X11/.test(navigator.platform || navigator.userAgent);
const IS_WIN = !IS_MAC && !IS_LINUX;

// Looks and keys of the terminal people already use on their system, so their habits carry over:
// Windows Terminal (Cascadia Mono 12 pt, Campbell colours, bar cursor), Terminal.app (SF Mono 11 pt, block cursor),
// GNOME Terminal (the system monospace font 11 pt, GNOME colours). Sizes are CSS px: 12 pt on Windows is 16 px,
// on a Mac a point is a CSS pixel.
const PROFILE = IS_MAC ? {
  fontFamily: 'ui-monospace, "SF Mono", Menlo, Monaco, monospace',
  fontSize: 11,
  cursorStyle: 'block',
  cursorBlink: false,
  fontWeightBold: 'bold',
  drawBoldTextInBrightColors: true,
  wordSeparator: ' ()[]{}\',"`',
  theme: {
    background: '#1e1e1e', foreground: '#f2f2f2', cursor: '#929292', cursorAccent: '#1e1e1e',
    selectionBackground: 'rgba(120, 150, 200, 0.45)',
    black: '#000000', red: '#c23621', green: '#25bc24', yellow: '#adad27', blue: '#492ee1', magenta: '#d338d3', cyan: '#33bbc8', white: '#cbcccd',
    brightBlack: '#818383', brightRed: '#fc391f', brightGreen: '#31e722', brightYellow: '#eaec23', brightBlue: '#5833ff', brightMagenta: '#f935f8', brightCyan: '#14f0f0', brightWhite: '#e9ebeb',
  },
} : IS_LINUX ? {
  // "monospace" is the system fixed-width font that GNOME Terminal uses by default.
  fontFamily: 'monospace',
  fontSize: 15,
  cursorStyle: 'block',
  cursorBlink: true,
  fontWeightBold: 'bold',
  drawBoldTextInBrightColors: false,
  wordSeparator: ' ()[]{}\',"`<>|;!$*^',
  theme: {
    background: '#1e1e1e', foreground: '#d0cfcc', cursor: '#d0cfcc', cursorAccent: '#1e1e1e',
    selectionBackground: 'rgba(53, 132, 228, 0.5)',
    black: '#171421', red: '#c01c28', green: '#26a269', yellow: '#a2734c', blue: '#12488b', magenta: '#a347ba', cyan: '#2aa1b3', white: '#d0cfcc',
    brightBlack: '#5e5c64', brightRed: '#f66151', brightGreen: '#33da7a', brightYellow: '#e9ad0c', brightBlue: '#2a7bde', brightMagenta: '#c061cb', brightCyan: '#33c7de', brightWhite: '#ffffff',
  },
} : {
  fontFamily: '"Cascadia Mono", Consolas, "Courier New", monospace',
  fontSize: 16,
  cursorStyle: 'bar',
  cursorBlink: true,
  // Windows Terminal shows bold (intense) text in the bright colour, not in a bold font.
  fontWeightBold: 'normal',
  drawBoldTextInBrightColors: true,
  // Windows Terminal's word delimiters: a double-click selects one part of a path or an address.
  wordSeparator: ' /\\()"\'-.,:;<>~!@#$%^&*|+=[]{}?│',
  theme: {
    background: '#0c0c0c', foreground: '#cccccc', cursor: '#ffffff', cursorAccent: '#0c0c0c',
    selectionBackground: 'rgba(255, 255, 255, 0.3)',
    black: '#0c0c0c', red: '#c50f1f', green: '#13a10e', yellow: '#c19c00', blue: '#0037da', magenta: '#881798', cyan: '#3a96dd', white: '#cccccc',
    brightBlack: '#767676', brightRed: '#e74856', brightGreen: '#16c60c', brightYellow: '#f9f1a5', brightBlue: '#3b78ff', brightMagenta: '#b4009e', brightCyan: '#61d6d6', brightWhite: '#f2f2f2',
  },
};

const FONT_MIN = 8;
const FONT_MAX = 36;
// A new key: sizes saved before the terminal matched the system one were relative to the old 13 px default.
const FONT_KEY = 'terminalFontSize';
let fontSize = PROFILE.fontSize;
try { fontSize = Number(localStorage.getItem(FONT_KEY)) || PROFILE.fontSize; } catch {}
host.style.background = PROFILE.theme.background;

// Links: Ctrl+click opens them (⌘-click on a Mac), like Windows Terminal, Terminal.app and GNOME Terminal.
const isLinkClick = (e) => (IS_MAC ? e.metaKey : e.ctrlKey);
function openLink(e, uri) {
  if (!isLinkClick(e)) return;
  if (!/^https?:\/\//i.test(uri)) return;
  window.runtime.BrowserOpenURL(uri);
}
const URL_RE = /\bhttps?:\/\/[^\s"'<>`│]+/g;

// Finds the URLs on one buffer row. Cells and string offsets differ with wide characters, so map each one.
function provideLinks(xterm, el, y, done) {
  const line = xterm.buffer.active.getLine(y - 1);
  if (!line) return done(undefined);
  let text = '';
  const col = []; // string offset -> cell (0-based)
  for (let x = 0; x < xterm.cols; x++) {
    const cell = line.getCell(x);
    if (!cell || cell.getWidth() === 0) continue;
    const ch = cell.getChars() || ' ';
    for (let i = 0; i < ch.length; i++) col.push(x);
    text += ch;
  }
  const links = [];
  for (const m of text.matchAll(URL_RE)) {
    let url = m[0].replace(/[.,;:!?'"]+$/, '');
    // A closing bracket belongs to the URL only when it also opens one: (see https://x/y).
    while (/[)\]}]$/.test(url) && count(url, url.at(-1)) > count(url, { ')': '(', ']': '[', '}': '{' }[url.at(-1)])) url = url.slice(0, -1);
    const start = col[m.index];
    const end = col[m.index + url.length - 1];
    links.push({
      text: url,
      range: { start: { x: start + 1, y }, end: { x: end + 1, y } },
      activate: (e, uri) => openLink(e, uri),
      hover: () => { el.title = `${url}\n${kbd('Ctrl+click')} to open`; },
      leave: () => { el.title = ''; },
    });
  }
  done(links.length ? links : undefined);
}
const count = (s, c) => s.split(c).length - 1;

function decode(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

async function paste(t) {
  try {
    let text = await window.runtime.ClipboardGetText();
    // Like Windows Terminal: a single pasted line loses its trailing newline, so it is not run straight away.
    if (IS_WIN && !/[\r\n]/.test(text.trimEnd())) text = text.trimEnd();
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
    fontFamily: PROFILE.fontFamily,
    fontSize,
    fontWeight: 'normal',
    fontWeightBold: PROFILE.fontWeightBold,
    drawBoldTextInBrightColors: PROFILE.drawBoldTextInBrightColors,
    cursorStyle: PROFILE.cursorStyle,
    cursorBlink: PROFILE.cursorBlink,
    wordSeparator: PROFILE.wordSeparator,
    scrollback: 10000,
    allowProposedApi: true,
    theme: PROFILE.theme,
    // Hyperlinks a program prints with OSC 8 (ls --hyperlink, systemctl…).
    linkHandler: { activate: (e, uri) => openLink(e, uri), allowNonHttpProtocols: false },
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
  // The shortcuts of the system's own terminal; the app's own shortcuts stay out of the shell.
  xterm.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown') return true;
    const act = (IS_MAC ? macKey : pcKey)(t, e);
    if (act === undefined) return true; // to the shell
    if (act) {
      e.preventDefault();
      act();
    }
    return false;
  });
  el.addEventListener('contextmenu', async (e) => {
    e.preventDefault();
    if (!(await copySelection(t))) paste(t);
  });
  // Ctrl+mouse wheel changes the text size, like Windows Terminal and GNOME Terminal.
  el.addEventListener('wheel', (e) => {
    if (IS_MAC || !e.ctrlKey || !e.deltaY) return;
    e.preventDefault();
    e.stopPropagation();
    setFontSize(fontSize + (e.deltaY < 0 ? 1 : -1));
  }, { capture: true, passive: false });
  xterm.registerLinkProvider({ provideLinks: (y, done) => provideLinks(xterm, el, y, done) });
  new ResizeObserver(() => { if (!el.hidden) fitSafe(t); }).observe(el);
  return t;
}

function fitSafe(t) {
  try { t.fit.fit(); } catch {}
}

const emit = (name, detail = {}) => document.dispatchEvent(new CustomEvent(name, { detail }));
const zoom = (step) => () => setFontSize(step ? fontSize + step : PROFILE.fontSize);

// Keys of Windows Terminal (Windows) and GNOME Terminal (Linux). Returns a function to run instead of
// sending the key, null to drop the key (another part of the app handles it), undefined to send it to the shell.
function pcKey(t, e) {
  const { xterm } = t;
  const k = e.key.toLowerCase();
  if (e.altKey || e.metaKey) return undefined;
  // Ctrl+Insert / Shift+Insert: the classic copy and paste keys.
  if (k === 'insert' && e.ctrlKey && !e.shiftKey) return xterm.hasSelection() ? () => copySelection(t) : undefined;
  if (k === 'insert' && e.shiftKey && !e.ctrlKey) return () => paste(t);
  if (!e.ctrlKey) return undefined;
  if (e.shiftKey) {
    switch (k) {
      case 'c': return () => copySelection(t);
      case 'v': return () => paste(t);
      case 'f': return () => emit('term:search');
      case 'a': return () => xterm.selectAll();
      case 't': return () => emit('term:new', { server: t.server });
      case 'd': return () => emit('term:new', { server: t.server, container: t.container }); // duplicate tab
      case 'w': return null; // closes this terminal (dock.js)
      case 'tab': return () => emit('term:cycle', { dir: -1 });
      case 'arrowup': return () => xterm.scrollLines(-1);
      case 'arrowdown': return () => xterm.scrollLines(1);
      case 'pageup': return () => xterm.scrollPages(-1);
      case 'pagedown': return () => xterm.scrollPages(1);
      case 'home': return () => xterm.scrollToTop();
      case 'end': return () => xterm.scrollToBottom();
      case '+': return zoom(1);
      default: return undefined;
    }
  }
  switch (k) {
    case 'tab': return () => emit('term:cycle', { dir: 1 });
    case '=': case '+': return zoom(1);
    case '-': return zoom(-1);
    case '0': return zoom(0);
  }
  if (IS_WIN) {
    // Windows Terminal: Ctrl+C copies when text is selected and interrupts otherwise; Ctrl+V pastes.
    if (k === 'c' && xterm.hasSelection()) return () => copySelection(t);
    if (k === 'v') return () => paste(t);
  }
  if (IS_LINUX) {
    // GNOME Terminal switches tabs with Ctrl+Page Up / Page Down.
    if (k === 'pageup') return () => emit('term:cycle', { dir: -1 });
    if (k === 'pagedown') return () => emit('term:cycle', { dir: 1 });
  }
  return undefined;
}

// Keys of Terminal.app. Ctrl keys (Ctrl+C, Ctrl+W…) go to the shell. ⌘C / ⌘V go through the Edit menu to xterm's
// own copy and paste handling; ⌘W, ⌘1..9 to the app (dock.js, tabs.js). No ⌘ key ever reaches the shell.
function macKey(t, e) {
  const { xterm } = t;
  const k = e.key.toLowerCase();
  // Page Up / Page Down scroll back through the output; in full-screen programs (less, vim) they go to the program.
  if (!e.metaKey && !e.ctrlKey && !e.altKey && !e.shiftKey && xterm.buffer.active.type === 'normal') {
    if (k === 'pageup') return () => xterm.scrollPages(-1);
    if (k === 'pagedown') return () => xterm.scrollPages(1);
  }
  if (e.ctrlKey && !e.metaKey && !e.altKey && k === 'tab') return () => emit('term:cycle', { dir: e.shiftKey ? -1 : 1 });
  if (!e.metaKey) return undefined;
  if (e.ctrlKey || e.altKey) return null;
  if (e.shiftKey && e.code === 'BracketLeft') return () => emit('term:cycle', { dir: -1 });
  if (e.shiftKey && e.code === 'BracketRight') return () => emit('term:cycle', { dir: 1 });
  switch (k) {
    case 'a': return () => xterm.selectAll();
    case 'f': return () => emit('term:search');
    case 'k': return () => xterm.clear();
    case 't': return () => emit('term:new', { server: t.server });
    case '=': case '+': return zoom(1);
    case '-': return zoom(-1);
    case '0': return zoom(0);
    case 'home': return () => xterm.scrollToTop();
    case 'end': return () => xterm.scrollToBottom();
    default: return null;
  }
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
  fontSize = Math.max(FONT_MIN, Math.min(FONT_MAX, size));
  try {
    if (fontSize === PROFILE.fontSize) localStorage.removeItem(FONT_KEY);
    else localStorage.setItem(FONT_KEY, String(fontSize));
  } catch {}
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
      t.xterm.write(`\r\n\x1b[2mFinished. Close this tab with ✕ or ${kbd('Ctrl+Shift+W')}.\x1b[0m\r\n`);
    } else {
      t.xterm.write(`\r\n\x1b[2m[${msg.reason}] Press Enter to reconnect.\x1b[0m\r\n`);
    }
    document.dispatchEvent(new CustomEvent('dock:changed', { detail: { key: t.server } }));
    if (t.task) document.dispatchEvent(new CustomEvent('task:exit', { detail: { key: t.server, task: t.task, code: msg.code ?? 0 } }));
  });
}
