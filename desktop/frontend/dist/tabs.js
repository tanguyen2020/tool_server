// Browser-like tabs: a fixed Overview tab plus one tab per opened server.
// Each tab keeps its own view (sub-tab, filters, scroll, log panel). Hidden tabs don't render;
// they remember which servers changed and catch up when shown.
import { h } from './util.js';
import { openMenu } from './context-menu.js';

const MAX_SERVER_TABS = 10;

export const hashFor = (key) => (key === 'overview' ? '#/' : `#/server/${key}`);

export function createTabManager({ strip, host, create, describe, onActivate, onClose, confirmClose = async () => true, busy = () => false }) {
  const tabs = new Map(); // key -> { key, root, view, used, pending: Set, events, scrollY }
  const order = ['overview'];
  let active = null;
  tabs.set('overview', { key: 'overview', root: null, view: null, used: 0, pending: new Set(), events: false, scrollY: 0 });

  function mount(t) {
    if (t.view) return;
    t.root = h('div', { class: 'tab-root' });
    host.append(t.root);
    t.view = create(t.key, t.root);
  }

  function add(key) {
    if (tabs.has(key)) return;
    tabs.set(key, { key, root: null, view: null, used: Date.now(), pending: new Set(), events: false, scrollY: 0 });
    order.push(key);
    // Too many tabs: close the least recently used one (never the active tab).
    const servers = order.filter((k) => k !== 'overview');
    if (servers.length > MAX_SERVER_TABS) {
      const candidates = servers.filter((k) => k !== active && k !== key).sort((a, b) => tabs.get(a).used - tabs.get(b).used);
      const lru = candidates.find((k) => !busy(k)) || candidates[0];
      if (lru) close(lru);
    }
  }

  // Opens a tab in the background (no switch).
  function open(key) {
    add(key);
    renderStrip();
  }

  function activate(key) {
    add(key);
    const prev = tabs.get(active);
    if (prev && prev.key !== key) {
      prev.scrollY = window.scrollY;
      prev.view?.hide?.();
    }
    active = key;
    const t = tabs.get(key);
    t.used = Date.now();
    mount(t);
    for (const x of tabs.values()) if (x.root) x.root.hidden = x !== t;
    flush(t);
    t.view.show?.();
    onActivate(key);
    renderStrip();
    requestAnimationFrame(() => window.scrollTo(0, t.scrollY));
  }

  // Apply what arrived while the tab was hidden.
  function flush(t) {
    if (!t?.view) return;
    for (const id of t.pending) t.view.update(id);
    t.pending.clear();
    if (t.events) {
      t.events = false;
      t.view.onEvents?.(t.key);
    }
  }

  function close(key) {
    if (key === 'overview' || !tabs.has(key)) return;
    const t = tabs.get(key);
    t.view?.destroy?.();
    t.root?.remove();
    tabs.delete(key);
    order.splice(order.indexOf(key), 1);
    onClose(key);
    if (active === key) {
      const next = [...tabs.values()].sort((a, b) => b.used - a.used)[0]?.key || 'overview';
      location.hash = hashFor(next);
    }
    renderStrip();
  }

  // Closing from the UI (button, middle click, menu, Ctrl+W) may ask for confirmation first.
  async function requestClose(key) {
    if (key === 'overview' || !tabs.has(key)) return;
    if (await confirmClose(key)) close(key);
  }

  const isVisible = (t) => t.key === active && !document.hidden;

  // A server's snapshot changed: the overview and that server's tab are affected.
  function notify(serverId) {
    for (const t of tabs.values()) {
      if (!t.view || (t.key !== 'overview' && t.key !== serverId)) continue;
      if (isVisible(t)) t.view.update(serverId);
      else t.pending.add(serverId);
    }
    renderStrip();
  }

  function notifyEvents(serverId) {
    const t = tabs.get(serverId);
    if (!t?.view) return;
    if (isVisible(t)) t.view.onEvents?.(serverId);
    else t.events = true;
  }

  // The server list changed: rebuild the overview, refresh server tabs, drop tabs of removed servers.
  function serversChanged(exists) {
    for (const key of [...order]) if (key !== 'overview' && !exists(key)) close(key);
    const ov = tabs.get('overview');
    if (ov.view) {
      ov.view.destroy?.();
      ov.root.replaceChildren();
      ov.view = create('overview', ov.root);
    }
    for (const t of tabs.values()) if (t.key !== 'overview' && t.view) notify(t.key);
    renderStrip();
  }

  function renderStrip() {
    strip.replaceChildren(...order.map((key, i) => {
      const d = describe(key);
      const selected = key === active;
      const btn = h('div', {
        class: `app-tab${selected ? ' active' : ''}${key === 'overview' ? ' home' : ''}`,
        role: 'tab',
        tabindex: selected ? '0' : '-1',
        'aria-selected': String(selected),
        title: `${d.tooltip || d.title}${i < 9 ? ` · Ctrl+${i + 1}` : ''}`,
        onclick: (e) => { if (!e.target.closest('.app-tab-close')) location.hash = hashFor(key); },
        onauxclick: (e) => { if (e.button === 1) { e.preventDefault(); requestClose(key); } },
        onmousedown: (e) => { if (e.button === 1) e.preventDefault(); },
        oncontextmenu: (e) => openMenu(e, btn, [
          ...(key === 'overview' ? [] : [{ label: 'Close tab', action: () => requestClose(key) }]),
          { label: 'Close other tabs', action: async () => { for (const k of order.filter((x) => x !== key)) await requestClose(k); } },
          { label: 'Close all server tabs', action: async () => { for (const k of order.filter((x) => x !== 'overview')) await requestClose(k); } },
        ]),
      },
      d.dot ? h('i', { class: `tab-dot ${d.dot}` }) : null,
      h('span', { class: 'app-tab-title' }, d.title),
      d.badge ? h('span', { class: 'tab-badge', title: d.badgeTitle }, d.badge) : null,
      key === 'overview' ? null : h('button', {
        type: 'button', class: 'app-tab-close', 'aria-label': `Close ${d.title}`, title: 'Close (Ctrl+W)',
        onclick: () => requestClose(key),
      }, '✕'));
      return btn;
    }));
  }

  // Ctrl+Tab / Ctrl+Shift+Tab, Ctrl+W, Ctrl+1..9
  document.addEventListener('keydown', (e) => {
    if (!e.ctrlKey || e.altKey || document.querySelector('dialog[open]')) return;
    if (e.target.closest?.('.xterm')) return; // Ctrl+W, Ctrl+Tab… belong to the shell inside a terminal
    if (e.key === 'Tab') {
      e.preventDefault();
      const i = order.indexOf(active);
      location.hash = hashFor(order[(i + (e.shiftKey ? order.length - 1 : 1)) % order.length]);
    } else if (e.key.toLowerCase() === 'w' && !e.shiftKey && active !== 'overview') {
      e.preventDefault();
      requestClose(active);
    } else if (/^[1-9]$/.test(e.key) && order[Number(e.key) - 1]) {
      e.preventDefault();
      location.hash = hashFor(order[Number(e.key) - 1]);
    }
  });

  return {
    open, activate, close, notify, notifyEvents, serversChanged, renderStrip,
    flushActive: () => flush(tabs.get(active)),
    get active() { return active; },
  };
}
