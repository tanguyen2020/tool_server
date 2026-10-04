// The bottom panel of a server tab, like VS Code's panel: one Logs session plus any number of terminals,
// each in its own dock tab. Sessions keep running when the panel is collapsed or another server tab is shown.
import { h } from './util.js';
import { openMenu } from './context-menu.js';
import { setLogsTab, closeTabLogs, hasLogs, logsTitle, showLogsPane, hideLogsPane } from './logs.js';
import {
  createTerminal, startTerminal, terminalsOf, show as showTerminal, hideAll as hideTerminals,
  closeTerminal, closeServerTerminals, setFontSize, getFontSize, shownTerminal,
} from './terminal.js';

const $ = (id) => document.getElementById(id);
const panel = $('logs');
const tabsEl = $('dock-tabs');
const logsPane = $('logs-pane');
const termPane = $('term-pane');
const termTools = $('term-tools');
const searchBar = $('term-search');
const searchInput = $('term-search-input');

let server = null; // server id of the active app tab (null on the Overview)
const activePane = new Map(); // server id -> 'logs' | terminal
let snapshotOf = () => null;

function items(key) {
  const list = [];
  if (hasLogs(key)) list.push({ kind: 'logs', title: `Logs · ${logsTitle(key)}` });
  for (const t of terminalsOf(key)) list.push({ kind: 'term', term: t, title: t.title });
  return list;
}

const isActive = (item, act) => (item.kind === 'logs' ? act === 'logs' : act === item.term);

function render() {
  const list = server ? items(server) : [];
  panel.hidden = !list.length;
  if (!list.length) {
    tabsEl.replaceChildren();
    hideLogsPane();
    hideTerminals();
    return;
  }
  let act = activePane.get(server);
  if (!list.some((i) => isActive(i, act))) {
    act = list[0].kind === 'logs' ? 'logs' : list[0].term;
    activePane.set(server, act);
  }
  tabsEl.replaceChildren(...list.map((i) => {
    const on = isActive(i, act);
    return h('div', {
      class: `dock-tab${on ? ' active' : ''}${i.term?.ended ? ' ended' : ''}`,
      role: 'tab',
      'aria-selected': String(on),
      title: i.term?.finished ? `${i.title} (finished)` : i.term?.ended ? `${i.title} (disconnected — press Enter in it to reconnect)` : i.title,
      onclick: (e) => { if (!e.target.closest('.dock-tab-close')) select(i.kind === 'logs' ? 'logs' : i.term); },
      onauxclick: (e) => { if (e.button === 1) closeItem(i); },
      oncontextmenu: (e) => openMenu(e, e.currentTarget, [
        { label: 'Close', action: () => closeItem(i) },
        { label: 'Close other tabs', action: () => items(server).filter((x) => x !== i && !(x.kind === 'logs' && i.kind === 'logs') && x.term !== i.term).forEach(closeItem) },
        { label: 'Close all terminals', action: () => terminalsOf(server).forEach((term) => closeItem({ kind: 'term', term })) },
      ]),
    },
    h('span', { class: 'dock-tab-icon', 'aria-hidden': 'true' }, i.kind === 'logs' ? '≡' : i.term?.task ? '⚙' : '>_'),
    h('span', { class: 'dock-tab-title' }, i.title),
    h('button', { type: 'button', class: 'dock-tab-close', 'aria-label': `Close ${i.title}`, onclick: () => closeItem(i) }, '✕'));
  }));
  const logs = act === 'logs';
  logsPane.hidden = !logs;
  termPane.hidden = logs;
  termTools.hidden = logs;
  if (logs) {
    hideTerminals();
    showLogsPane();
  } else {
    hideLogsPane();
    showTerminal(act);
  }
}

function select(pane) {
  activePane.set(server, pane);
  expand();
  render();
}

function closeItem(item) {
  if (item.kind === 'logs') closeTabLogs(server); // fires dock:changed
  else {
    closeTerminal(item.term);
    render();
  }
}

function expand() {
  panel.classList.remove('collapsed');
  $('dock-collapse').textContent = '▾';
  $('dock-collapse').title = 'Collapse (sessions keep running)';
}

export const dockTerminalCount = (key) => terminalsOf(key).length;

// Closes only the selected dock tab (a terminal or the logs), never the whole server tab.
function closeActiveItem() {
  if (!server || panel.hidden) return false;
  const act = activePane.get(server);
  const item = items(server).find((i) => isActive(i, act));
  if (!item) return false;
  closeItem(item);
  return true;
}

// The active app tab changed.
export function setDockServer(key) {
  server = key === 'overview' ? null : key;
  setLogsTab(key);
  render();
}

// A server tab was closed: end its log stream and terminals.
export function closeDockServer(key) {
  closeTabLogs(key);
  closeServerTerminals(key);
  activePane.delete(key);
  if (server === key) render();
}

// Runs an operation in a new terminal tab of the current server: { kind, arg, title, once }.
export function openTaskFor(serverId, task) {
  return openTerminalFor(serverId, { task });
}

// Types text into the terminal shown in the panel (a snippet). Returns false when no terminal is shown.
export function pasteToTerminal(text) {
  const t = shownTerminal();
  if (!t || t.ended || termPane.hidden || panel.hidden) return false;
  t.xterm.paste(text);
  t.xterm.focus();
  return true;
}

// Opens a shell on the server (container empty) or inside a container, in the current server tab.
export function openTerminalFor(serverId, container = '') {
  const snap = snapshotOf(serverId);
  if (!snap || serverId !== server) return;
  // The terminal must be laid out (visible) before xterm measures its cells.
  panel.hidden = false;
  expand();
  logsPane.hidden = true;
  termPane.hidden = false;
  termTools.hidden = false;
  hideLogsPane();
  const t = createTerminal(snap, container);
  activePane.set(serverId, t);
  render();
  startTerminal(t);
}

// ---------------------------------------------------------------- size, search, font

function loadHeight() {
  try { return Number(localStorage.getItem('logsHeight')) || 0; } catch { return 0; }
}
function setHeight(px) {
  const hgt = Math.max(220, Math.min(px, innerHeight - 100));
  panel.style.height = `${hgt}px`;
  try { localStorage.setItem('logsHeight', String(Math.round(hgt))); } catch {}
}

function openSearch() {
  if (termPane.hidden) return;
  searchBar.hidden = false;
  searchInput.focus();
  searchInput.select();
}
function closeSearch() {
  searchBar.hidden = true;
  shownTerminal()?.search.clearDecorations();
  shownTerminal()?.xterm.focus();
}
const SEARCH_OPTS = {
  caseSensitive: false,
  decorations: { matchBackground: '#fab21955', activeMatchBackground: '#fab219', matchOverviewRuler: '#fab219', activeMatchColorOverviewRuler: '#d03b3b' },
};
function find(backward = false) {
  const t = shownTerminal();
  const q = searchInput.value;
  if (!t || !q) return;
  if (backward) t.search.findPrevious(q, SEARCH_OPTS);
  else t.search.findNext(q, SEARCH_OPTS);
}

export function initDock({ snapshot }) {
  snapshotOf = snapshot;
  document.addEventListener('dock:show', (e) => {
    if (e.detail.key !== server) return;
    activePane.set(server, e.detail.pane);
    expand();
    render();
  });
  document.addEventListener('dock:changed', (e) => { if (e.detail.key === server) render(); });
  document.addEventListener('term:search', openSearch);
  // Capture phase: runs before the app's tab shortcuts and before xterm sees the key.
  window.addEventListener('keydown', (e) => {
    if (!e.ctrlKey || e.altKey || e.key.toLowerCase() !== 'w' || panel.hidden) return;
    if (e.target.closest?.('dialog')) return;
    const inDock = panel.contains(e.target);
    const inTerminal = !!e.target.closest?.('.xterm');
    // Ctrl+Shift+W anywhere, or Ctrl+W in the panel outside a shell (where Ctrl+W deletes a word).
    if (e.shiftKey || (inDock && !inTerminal)) {
      if (closeActiveItem()) {
        e.preventDefault();
        e.stopPropagation();
      }
    }
  }, true);

  $('dock-new-term').addEventListener('click', () => server && openTerminalFor(server));
  if (loadHeight()) panel.style.height = `${loadHeight()}px`;
  const handle = $('logs-resize');
  handle.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    expand();
    const move = (ev) => setHeight(innerHeight - ev.clientY);
    const up = () => { handle.removeEventListener('pointermove', move); handle.removeEventListener('pointerup', up); };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
  });
  $('logs-max').addEventListener('click', () => {
    expand();
    const max = panel.classList.toggle('max');
    $('logs-max').textContent = max ? '⤡' : '⤢';
    $('logs-max').title = max ? 'Restore' : 'Maximize';
  });
  $('dock-collapse').addEventListener('click', () => {
    const collapsed = panel.classList.toggle('collapsed');
    $('dock-collapse').textContent = collapsed ? '▴' : '▾';
    $('dock-collapse').title = collapsed ? 'Expand' : 'Collapse (sessions keep running)';
    if (!collapsed) render();
  });
  $('term-font-up').addEventListener('click', () => setFontSize(getFontSize() + 1));
  $('term-font-down').addEventListener('click', () => setFontSize(getFontSize() - 1));
  $('term-search-btn').addEventListener('click', openSearch);
  $('term-search-next').addEventListener('click', () => find(false));
  $('term-search-prev').addEventListener('click', () => find(true));
  $('term-search-close').addEventListener('click', closeSearch);
  searchInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); find(e.shiftKey); }
    if (e.key === 'Escape') { e.preventDefault(); closeSearch(); }
  });
  searchInput.addEventListener('input', () => find(false));
}
