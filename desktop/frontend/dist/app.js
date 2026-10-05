import { h, toast, fmtBytes, fmtRate, fmtPct, fmtUptime, fmtTime, meter, setMeter, statusChip, level } from './util.js';
import { TimeChart } from './chart.js';
import { initLogs, openLogs, onLogsSnapshot } from './logs.js';
import { initTerminals } from './terminal.js';
import { initDock, setDockServer, closeDockServer, openTerminalFor, openTaskFor, pasteToTerminal, dockTerminalCount } from './dock.js';
import { servicesTab } from './services.js';
import { filesTab } from './files.js';
import { networkTab } from './network.js';
import { openRunMany } from './runmany.js';
import { openSnippets, snippetMenu } from './snippets.js';
import { openActivity } from './activity.js';
import { initUpdates } from './updates.js';
import { createTabManager, hashFor } from './tabs.js';
import { openServerForm } from './server-form.js';
import { openInspect } from './inspect.js';
import { openMenu } from './context-menu.js';
import { containersPanel, hasIssue } from './containers-table.js';
import { containerMetricsTab } from './container-metrics.js';
import { openAlertSettings, setAlertSettingsHandler } from './alert-settings.js';
import { topProcessesPanel, maintenancePanel, dockerDiskPanel, diskExplorerPanel } from './server-extras.js';
import { call, on, confirmDialog } from './bridge.js';
import { icon, setLabel } from './icons.js';
import { initPrefs, getPref, setPref, getJSONPref, setJSONPref } from './prefs.js';

const state = {
  servers: new Map(), // config (no secrets)
  snaps: new Map(), // latest metrics
  history: new Map(), // [{t, cpu, mem, rx, tx}]
  settings: null, // { notifications, alerts: { cpu, memory, disk, iowait, sustainMinutes } }
  containerHistory: new Map(), // serverId -> { t: [], cpu|mem|netRx|netTx|blkRead|blkWrite: Map(name -> values) }
};
const HISTORY_MAX = 360;
const view = document.getElementById('view');
const notifyToggle = document.getElementById('notify-toggle');
let tabs = null; // tab manager, created once the first data has arrived

// ---------------------------------------------------------------- Events from the Go backend

// Same shape as the Go monitor.Point history entries.
function pointOf(snap) {
  const h = snap.host;
  const c = h.cpuSplit;
  return {
    t: snap.updatedAt, cpu: h.cpu, mem: h.mem.pct, rx: h.net.rx, tx: h.net.tx,
    user: c?.user ?? null, system: c?.system ?? null, iowait: c?.iowait ?? null, steal: c?.steal ?? null,
    load1: h.load[0], load5: h.load[1], load15: h.load[2],
    memUsed: h.mem.used, memCache: h.mem.cache ?? 0, ioRead: h.ioRead, ioWrite: h.ioWrite,
  };
}

function onSnapshot(snap) {
  state.snaps.set(snap.id, snap);
  if (snap.status === 'online' && snap.host?.cpu != null) {
    const hist = state.history.get(snap.id) || [];
    if (!hist.length || hist.at(-1).t < snap.updatedAt) {
      hist.push(pointOf(snap));
      if (hist.length > HISTORY_MAX) hist.splice(0, hist.length - HISTORY_MAX);
    }
    state.history.set(snap.id, hist);
  }
  pushContainerHistory(snap);
  onLogsSnapshot(snap);
  // Only the visible tab renders; hidden tabs catch up when shown.
  tabs?.notify(snap.id);
}

// Per-container CPU/RAM over time; every series array stays aligned with `t` (null = no sample).
// Container metric -> snapshot field. Every series array stays aligned with `t` (null = no sample).
const CONTAINER_METRICS = [['cpu', 'cpu'], ['mem', 'memUsed'], ['netRx', 'netRx'], ['netTx', 'netTx'], ['blkRead', 'blkRead'], ['blkWrite', 'blkWrite']];
const emptyContainerHistory = () => ({ t: [], ...Object.fromEntries(CONTAINER_METRICS.map(([k]) => [k, new Map()])) });

// Docker reports container CPU in % of ONE core (a busy container on 16 cores reads up to 1600%).
// The app shows it as % of the whole server, on the same 0–100 scale as the server CPU.
const cpuOfServer = (pctOfCore, cores) => (pctOfCore == null || !cores ? pctOfCore : pctOfCore / cores);

function pushContainerHistory(snap) {
  if (snap.status !== 'online' || !snap.docker?.containers) return;
  let ch = state.containerHistory.get(snap.id);
  if (!ch) {
    ch = emptyContainerHistory();
    state.containerHistory.set(snap.id, ch);
  }
  if (ch.t.length && ch.t.at(-1) >= snap.updatedAt) return;
  const n = ch.t.length;
  ch.t.push(snap.updatedAt);
  for (const [k] of CONTAINER_METRICS) for (const arr of ch[k].values()) arr.push(null);
  for (const c of snap.docker.containers) {
    for (const [k, field] of CONTAINER_METRICS) {
      const v = k === 'cpu' ? cpuOfServer(c[field], snap.host?.cores) : c[field];
      if (v == null) continue;
      let arr = ch[k].get(c.name);
      if (!arr) {
        arr = new Array(n + 1).fill(null);
        ch[k].set(c.name, arr);
      }
      arr[n] = v;
    }
  }
  if (ch.t.length > HISTORY_MAX) {
    const drop = ch.t.length - HISTORY_MAX;
    ch.t.splice(0, drop);
    for (const [k] of CONTAINER_METRICS) {
      for (const [name, arr] of ch[k]) {
        arr.splice(0, drop);
        if (arr.every((v) => v == null)) ch[k].delete(name);
      }
    }
  }
}

const fmtMem = (v) => fmtBytes(v).replace('.0 ', ' ');

async function boot() {
  const data = await call('Init');
  state.servers = new Map(data.servers.map((s) => [s.id, s]));
  state.snaps = new Map(data.snapshots.map((s) => [s.id, s]));
  state.history = new Map(Object.entries(data.history || {}));
  state.settings = data.settings;
  initPrefs(data.settings.ui);
  // The saved setting (settings.json) is the source of truth; the page's local copy only avoids a flash.
  const savedTheme = data.settings.theme || 'system';
  if (savedTheme !== currentTheme()) applyTheme(savedTheme, false);
  notifyToggle.checked = data.settings.notifications;
  initUpdates(state.settings, document.getElementById('version-btn'));

  tabs = createTabManager({
    strip: document.getElementById('tabs'),
    host: view,
    create: (key, root) => (key === 'overview' ? overviewView(root) : detailView(key, root)),
    describe: describeTab,
    onActivate: (key) => setDockServer(key),
    onClose: (key) => closeDockServer(key),
    busy: (key) => dockTerminalCount(key) > 0,
    confirmClose: async (key) => {
      const n = dockTerminalCount(key);
      if (!n) return true;
      return confirmDialog(`Close the tab of ${state.servers.get(key)?.name || 'this server'}?\n\n${n} open terminal${n > 1 ? 's' : ''} will be closed.`, 'Close tab');
    },
  });
  // Every start begins with the Overview only; server tabs from the last session are not restored.
  try { localStorage.removeItem('openTabs'); } catch {}
  if (location.hash !== '#/') history.replaceState(null, '', '#/');
  tabs.activate('overview');

  on('snapshot', onSnapshot);
  on('servers', (list) => {
    state.servers = new Map(list.map((s) => [s.id, s]));
    tabs.serversChanged((id) => state.servers.has(id));
  });
  on('removed', (id) => {
    state.snaps.delete(id);
    state.history.delete(id);
    state.containerHistory.delete(id);
    tabs.close(id);
  });
  on('container-events', (sid) => tabs.notifyEvents(sid));
  on('alert', (a) => toast(`${a.title}: ${a.body}`, a.level === 'critical'));
}

// ---------------------------------------------------------------- Routing: the hash selects the tab
function keyFromHash() {
  const m = /^#\/server\/([\w-]+)/.exec(location.hash);
  return m && state.servers.has(m[1]) ? m[1] : 'overview';
}
window.addEventListener('hashchange', () => tabs?.activate(keyFromHash()));

// What the tab strip shows for a tab: status dot, name and a count of problems.
function describeTab(key) {
  if (key === 'overview') {
    const down = [...state.servers.keys()].filter((id) => state.snaps.get(id)?.status === 'offline').length;
    return { title: '⌂ Overview', badge: down ? String(down) : '', badgeTitle: `${down} server${down > 1 ? 's' : ''} unreachable` };
  }
  const cfg = state.servers.get(key);
  const snap = state.snaps.get(key) || {};
  const issues = (snap.status === 'online' ? snap.alerts?.length || 0 : 0) + (snap.docker?.containers || []).filter(hasIssue).length;
  return {
    title: cfg?.name || key,
    dot: snap.status === 'online' ? 'good' : snap.status === 'offline' ? 'critical' : snap.status === 'rebooting' ? 'warning' : '',
    badge: issues ? String(issues) : '',
    badgeTitle: `${issues} alert${issues > 1 ? 's' : ''} or container issue${issues > 1 ? 's' : ''}`,
    tooltip: [cfg?.name, snap.host?.hostname !== cfg?.name && snap.host?.hostname, `${cfg?.username}@${cfg?.host}`, snap.status || 'connecting'].filter(Boolean).join(' · '),
  };
}

const groupsList = () => [...new Set([...state.servers.values()].map((s) => s.group).filter(Boolean))].sort();
const diskRoot = (host) => host?.disks?.find((d) => d.mount === '/') || host?.disks?.[0];

async function removeServer(id) {
  const s = state.servers.get(id);
  if (!s || !(await confirmDialog(`Remove "${s.name}" from the dashboard?\n\nOnly the entry in this app is removed: the server itself, its containers and data are not touched.`, 'Remove server'))) return false;
  try {
    await call('DeleteServer', id);
    toast(`Removed ${s.name}`);
    return true;
  } catch (e) {
    toast(e.message, true);
    return false;
  }
}

// Reboots a server after confirmation. The monitor shows it as "Rebooting…" and notifies when it is back.
async function rebootServer(id) {
  const s = state.servers.get(id);
  const snap = state.snaps.get(id);
  if (!s) return;
  if (snap?.status === 'rebooting') {
    toast(`${s.name} is already rebooting`);
    return;
  }
  const running = (snap?.docker?.containers || []).filter((c) => c.state === 'running').length;
  const lines = [`Reboot "${s.name}" (${s.username}@${s.host})?`, 'Terminals and log streams on this server will disconnect.'];
  if (running) lines.push(`${running} running container${running > 1 ? 's' : ''} will stop; only those with a restart policy start again on their own.`);
  if (!(await confirmDialog(lines.join('\n\n'), 'Reboot server'))) return;
  try {
    await call('RebootServer', id);
    toast(`Rebooting ${s.name}… you will be notified when it is back`);
  } catch (e) {
    toast(`Could not reboot ${s.name}: ${e.message}`, true);
  }
}

// ---------------------------------------------------------------- Overview
// Two views of the same servers: compact cards, or a dense sortable list. Both can be grouped, sorted
// (problems first by default) and filtered with the stat tiles. The choices are remembered.
const OV_KEY = 'overviewPrefs';
function loadOvPrefs() {
  const def = { view: 'cards', sort: 'issues', group: true, filter: 'all' };
  return { ...def, ...getJSONPref(OV_KEY) };
}

// Everything that needs attention on a server, most serious first: { level, text, title }.
function serverIssues(snap) {
  const out = [];
  if (!snap) return out;
  if (snap.status === 'offline') out.push({ level: 'critical', text: 'Unreachable', title: snap.error || '', weight: 1000 }); // always first
  if (snap.status === 'rebooting') out.push({ level: 'warning', text: 'Rebooting…', title: '' });
  if (snap.status === 'online') {
    for (const a of snap.alerts || []) out.push({ level: 'critical', text: a.title, title: `${a.title} · ${a.detail}` });
  }
  const bad = (snap.docker?.containers || []).filter(hasIssue);
  if (bad.length) out.push({ level: 'critical', text: `${bad.length} container issue${bad.length > 1 ? 's' : ''}`, title: bad.map((c) => c.name).join('\n') });
  const m = snap.maintenance;
  if (m) {
    const daysOf = (c) => Math.floor((c.notAfter - Date.now()) / 86400e3);
    const expired = (m.certs || []).filter((c) => daysOf(c) < 0);
    const expiring = (m.certs || []).filter((c) => daysOf(c) >= 0 && daysOf(c) <= 14);
    const names = (list) => list.map((c) => `${c.subject || c.path} (${daysOf(c) < 0 ? 'expired' : `${daysOf(c)} d left`})`).join('\n');
    if (expired.length) out.push({ level: 'critical', text: expired.length > 1 ? `${expired.length} certificates expired` : 'Certificate expired', title: names(expired) });
    if (expiring.length) {
      const soonest = Math.min(...expiring.map(daysOf));
      out.push({ level: 'critical', text: expiring.length > 1 ? `${expiring.length} certificates expire soon` : `Certificate expires in ${soonest} d`, title: names(expiring) });
    }
    if (m.rebootRequired) out.push({ level: 'warning', text: 'Reboot required', title: m.rebootReason || '' });
    if (m.securityUpdates) out.push({ level: 'warning', text: `${m.securityUpdates} security update${m.securityUpdates > 1 ? 's' : ''}`, title: '' });
    else if (m.updates) out.push({ level: 'neutral', text: `${m.updates} update${m.updates > 1 ? 's' : ''}`, title: '' });
  }
  return out;
}
const ISSUE_WEIGHT = { critical: 100, warning: 10, neutral: 1 };
const issueScore = (snap) => serverIssues(snap).reduce((a, i) => a + (i.weight || ISSUE_WEIGHT[i.level]), 0);
const hasProblem = (snap) => serverIssues(snap).some((i) => i.level !== 'neutral');

// Up to `max` chips, then "+N" with the rest in its tooltip.
function issueChips(issues, max) {
  const shown = issues.slice(0, max).map((i) => h('span', { class: `chip ${i.level === 'neutral' ? '' : i.level}`, title: i.title }, i.text));
  if (issues.length > max) {
    const rest = issues.slice(max);
    shown.push(h('span', { class: 'chip more', title: rest.map((i) => i.text).join('\n') }, `+${rest.length}`));
  }
  return shown;
}

// The machine's own hostname, shown next to the name given in the app (unless they are the same).
function hostLabel(cfg, host) {
  const hn = host?.hostname;
  return hn && hn !== cfg.name && hn !== cfg.host ? h('span', { class: 'host-name' }, hn) : '';
}

// "deploy@10.0.0.5 · Ubuntu 22.04" — without repeating the address when the name already is the address.
function serverSub(cfg, host) {
  const who = cfg.name === cfg.host ? cfg.username : `${cfg.username}@${cfg.host}`;
  return [who, host?.os].filter(Boolean).join(' · ');
}

// Open / open in a new tab / context menu, shared by cards and list rows.
function wireServerOpen(el, id) {
  el.addEventListener('click', (e) => {
    if (e.target.closest('.no-open')) return;
    if (e.ctrlKey || e.metaKey || e.shiftKey) {
      e.preventDefault();
      tabs.open(id);
    } else if (el.tagName !== 'A') {
      location.hash = hashFor(id);
    }
  });
  el.addEventListener('keydown', (e) => { if (e.key === 'Enter' && el.tagName !== 'A') location.hash = hashFor(id); });
  el.addEventListener('mousedown', (e) => { if (e.button === 1) e.preventDefault(); });
  el.addEventListener('auxclick', (e) => {
    if (e.button === 1) {
      e.preventDefault();
      tabs.open(id);
    }
  });
  el.addEventListener('contextmenu', (e) => openMenu(e, el, serverMenu(id)));
}
const serverMenu = (id) => [
  { label: 'Open', action: () => { location.hash = hashFor(id); } },
  { label: 'Open in new tab', action: () => tabs.open(id) },
  { label: 'Edit', action: () => openServerForm(state.servers.get(id), groupsList(), [...state.servers.values()]) },
  { label: 'Reboot server…', danger: true, action: () => rebootServer(id) },
  { label: 'Remove', danger: true, action: () => removeServer(id) },
];

function serverCard(id) {
  const name = h('div', { class: 'card-title' });
  const sub = h('div', { class: 'card-sub' });
  const chipSlot = h('span', { class: 'chips' });
  const issuesRow = h('div', { class: 'card-chips one-line' });
  const cpuVal = h('span', { class: 'num' });
  const cpuLbl = h('span');
  const memVal = h('span', { class: 'num' });
  const diskVal = h('span', { class: 'num' });
  const diskLbl = h('span');
  const cpuM = meter(0);
  const memM = meter(0);
  const diskM = meter(0);
  const foot = h('div', { class: 'card-foot' });
  const el = h('a', { class: 'card compact', href: `#/server/${id}` },
    h('div', { class: 'card-head' }, h('div', { style: 'min-width:0' }, name, sub), chipSlot),
    issuesRow,
    h('div', {}, h('div', { class: 'meter-row' }, cpuLbl, cpuVal), cpuM),
    h('div', {}, h('div', { class: 'meter-row' }, h('span', {}, 'RAM'), memVal), memM),
    h('div', {}, h('div', { class: 'meter-row' }, diskLbl, diskVal), diskM),
    foot,
  );
  const update = () => {
    const cfg = state.servers.get(id);
    const snap = state.snaps.get(id) || { status: 'connecting' };
    const host = snap.host;
    el.classList.toggle('offline', snap.status === 'offline' || snap.status === 'rebooting');
    name.replaceChildren(cfg.name, hostLabel(cfg, host));
    name.title = host?.hostname && host.hostname !== cfg.name ? `${cfg.name} · ${host.hostname}` : cfg.name;
    sub.textContent = serverSub(cfg, host);
    sub.title = `${cfg.username}@${cfg.host}:${cfg.port}${host?.os ? ` · ${host.os}` : ''}`;
    chipSlot.replaceChildren(statusChip(snap.status));
    const issues = serverIssues(snap);
    issuesRow.replaceChildren(...(issues.length ? issueChips(issues, 2) : [h('span', { class: 'muted ok-line' }, '✓ No issues')]));
    cpuLbl.textContent = host ? `CPU · ${host.cores} cores` : 'CPU';
    cpuVal.textContent = fmtPct(host?.cpu);
    setMeter(cpuM, host?.cpu);
    memVal.textContent = host ? `${fmtBytes(host.mem.used)} / ${fmtBytes(host.mem.total)}` : '–';
    setMeter(memM, host?.mem.pct);
    const disk = fullestDisk(host);
    diskLbl.textContent = disk ? `Disk ${disk.mount}` : 'Disk';
    diskVal.textContent = disk ? `${fmtBytes(disk.used)} / ${fmtBytes(disk.size)}` : '–';
    setMeter(diskM, disk?.pct);
    const d = snap.docker;
    foot.replaceChildren(
      h('span', {}, 'Load ', h('b', { class: `num ${host && host.load[0] > host.cores ? 'warn-text' : ''}` }, host ? host.load.map((v) => v.toFixed(2)).join(' ') : '–')),
      h('span', {}, 'Up ', h('b', {}, fmtUptime(host?.uptime))),
      h('span', {}, 'Containers ', h('b', { class: 'num' }, d ? (d.available ? `${d.running}/${d.total}` : 'error') : '–')),
    );
  };
  wireServerOpen(el, id);
  return { el, update };
}

// The fullest disk is the one that matters (falls back to /).
const fullestDisk = (host) => host?.disks?.reduce((a, b) => (b.pct > a.pct ? b : a), host.disks[0]) || diskRoot(host);

function miniMeter(pct) {
  const m = meter(pct);
  m.classList.add('cell-meter');
  return m;
}

function serverRow(id) {
  const tr = h('tr', { class: 'srv-row', tabindex: '0' });
  const more = h('button', { type: 'button', class: 'btn sm act act-inspect no-open', title: 'Actions', 'aria-label': 'Server actions' }, icon('more'));
  more.addEventListener('click', (e) => { e.preventDefault(); openMenu(e, more, serverMenu(id)); });
  const update = () => {
    const cfg = state.servers.get(id);
    const snap = state.snaps.get(id) || { status: 'connecting' };
    const host = snap.host;
    const d = snap.docker;
    const disk = fullestDisk(host);
    const issues = serverIssues(snap);
    tr.classList.toggle('offline', snap.status === 'offline' || snap.status === 'rebooting');
    tr.classList.toggle('row-issue', issues.some((i) => i.level === 'critical'));
    const pctCell = (pct, label, title) => h('td', { class: 'num', title }, h('div', { class: 'cell-pct' }, h('span', {}, fmtPct(pct, 0)), miniMeter(pct)), label ? h('div', { class: 'muted small' }, label) : '');
    const badContainers = (d?.containers || []).filter(hasIssue).length;
    tr.replaceChildren(
      h('td', {}, statusChip(snap.status)),
      h('td', { class: 'srv-name' }, h('div', { class: 'srv-title', title: host?.hostname || '' }, cfg.name, hostLabel(cfg, host)), h('div', { class: 'muted small srv-sub', title: `${cfg.username}@${cfg.host}:${cfg.port}` }, serverSub(cfg, host))),
      pctCell(host?.cpu, host ? `${host.cores} cores` : '', 'CPU'),
      pctCell(host?.mem.pct, host ? `${fmtBytes(host.mem.used, 0)} / ${fmtBytes(host.mem.total, 0)}` : '', 'RAM'),
      pctCell(disk?.pct, disk ? `${disk.mount} · ${fmtBytes(disk.size, 0)}` : '', 'Fullest disk'),
      h('td', { class: `num ${host && host.load[0] > host.cores ? 'warn-text' : ''}`, title: host ? `Load 1 / 5 / 15 min · ${host.cores} cores` : '' }, host ? host.load[0].toFixed(2) : '–'),
      h('td', { class: 'num' }, fmtUptime(host?.uptime)),
      h('td', { class: 'num' }, d ? (d.available ? `${d.running}/${d.total}` : h('span', { class: 'chip critical' }, 'error')) : '–',
        badContainers ? h('div', { class: 'small crit-text' }, `${badContainers} with issues`) : ''),
      h('td', { class: 'srv-issues' }, h('div', { class: 'card-chips one-line' }, ...(issues.length ? issueChips(issues, 3) : [h('span', { class: 'muted' }, '–')]))),
      h('td', {}, more),
    );
  };
  wireServerOpen(tr, id);
  return { el: tr, update };
}

const OV_SORTS = {
  issues: (a, b) => issueScore(state.snaps.get(b.id)) - issueScore(state.snaps.get(a.id)),
  name: () => 0,
  cpu: (a, b) => (state.snaps.get(b.id)?.host?.cpu ?? -1) - (state.snaps.get(a.id)?.host?.cpu ?? -1),
  mem: (a, b) => (state.snaps.get(b.id)?.host?.mem.pct ?? -1) - (state.snaps.get(a.id)?.host?.mem.pct ?? -1),
  disk: (a, b) => (fullestDisk(state.snaps.get(b.id)?.host)?.pct ?? -1) - (fullestDisk(state.snaps.get(a.id)?.host)?.pct ?? -1),
  load: (a, b) => {
    const r = (id) => { const h2 = state.snaps.get(id)?.host; return h2 ? h2.load[0] / h2.cores : -1; };
    return r(b.id) - r(a.id);
  },
};
const OV_FILTERS = {
  all: () => true,
  online: (s) => s?.status === 'online',
  offline: (s) => s?.status === 'offline',
  issues: (s) => hasProblem(s),
  reboot: (s) => !!s?.maintenance?.rebootRequired,
};

function overviewView(root) {
  const prefs = loadOvPrefs();
  const save = () => setJSONPref(OV_KEY, prefs);
  const search = h('input', { type: 'search', placeholder: 'Search server, IP, group…', 'aria-label': 'Search servers' });
  const statsEl = h('div', { class: 'stats ov-stats' });
  const list = h('div');
  const cards = new Map();
  const rows = new Map();

  const viewBtns = [['cards', 'Cards'], ['list', 'List']].map(([k, label]) => h('button', {
    type: 'button', class: 'tab', 'data-view': k, onclick: () => { prefs.view = k; save(); build(); },
  }, icon(k === 'cards' ? 'more' : 'logs'), label));
  const sortSel = h('select', { 'aria-label': 'Sort servers' },
    ...[['issues', 'Problems first'], ['name', 'Name'], ['cpu', 'CPU'], ['mem', 'RAM'], ['disk', 'Disk'], ['load', 'Load']].map(([v, l]) => h('option', { value: v }, `Sort: ${l}`)));
  sortSel.value = prefs.sort;
  sortSel.addEventListener('change', () => { prefs.sort = sortSel.value; save(); build(); });
  const groupBox = h('input', { type: 'checkbox', checked: prefs.group });
  groupBox.addEventListener('change', () => { prefs.group = groupBox.checked; save(); build(); });
  const addBtn = h('button', { class: 'btn primary', onclick: () => openServerForm(null, groupsList(), [...state.servers.values()]) }, icon('plus'), 'Add server');
  const exportBtn = h('button', { class: 'btn act act-logs', title: 'Save the server list and snippets to a file (no passwords or keys)', onclick: exportServers }, icon('exportIcon'), 'Export');
  const importBtn = h('button', { class: 'btn act act-logs', title: 'Add servers from an export file', onclick: importServers }, icon('importIcon'), 'Import');
  root.replaceChildren(
    statsEl,
    h('div', { class: 'toolbar' }, search,
      h('div', { class: 'tabs', role: 'group', 'aria-label': 'View' }, viewBtns),
      sortSel,
      h('label', { class: 'check' }, groupBox, 'Group by group'),
      h('span', { class: 'spacer' }), exportBtn, importBtn, addBtn),
    list,
  );

  // Stat tiles double as filters (click again to show all).
  function updateStats() {
    const snaps = [...state.servers.keys()].map((id) => state.snaps.get(id));
    const count = (k) => snaps.filter(OV_FILTERS[k]).length;
    const tile = (key, label, value, tone) => h('button', {
      type: 'button', class: `stat stat-filter ${tone && value ? `tone-${tone}` : ''} ${prefs.filter === key ? 'active' : ''}`,
      title: prefs.filter === key ? 'Show all servers' : `Show only: ${label.toLowerCase()}`,
      onclick: () => { prefs.filter = prefs.filter === key ? 'all' : key; save(); updateStats(); build(); },
    }, h('div', { class: 'stat-label' }, label), h('div', { class: 'stat-value' }, String(value)));
    statsEl.replaceChildren(
      tile('all', 'All servers', state.servers.size),
      tile('online', 'Online', count('online'), 'good'),
      tile('offline', 'Unreachable', count('offline'), 'critical'),
      tile('issues', 'Need attention', count('issues'), 'warning'),
      tile('reboot', 'Reboot required', count('reboot'), 'warning'),
    );
  }

  let lastOrder = '';
  function build() {
    viewBtns.forEach((b) => b.classList.toggle('active', b.dataset.view === prefs.view));
    if (!state.servers.size) {
      list.replaceChildren(h('div', { class: 'panel empty' },
        h('p', {}, 'No servers yet.'),
        h('button', { class: 'btn primary', onclick: () => openServerForm(null, [], []) }, '+ Add your first server')));
      lastOrder = '';
      return;
    }
    const q = search.value.trim().toLowerCase();
    const keep = OV_FILTERS[prefs.filter] || OV_FILTERS.all;
    const cmp = OV_SORTS[prefs.sort] || OV_SORTS.name;
    const servers = [...state.servers.values()]
      .filter((s) => keep(state.snaps.get(s.id)))
      .filter((s) => !q || [s.name, s.host, s.group, state.snaps.get(s.id)?.host?.hostname].some((v) => v?.toLowerCase().includes(q)))
      .sort((a, b) => cmp(a, b) || a.name.localeCompare(b.name, undefined, { numeric: true }));
    const groups = new Map();
    for (const s of servers) {
      const g = prefs.group ? s.group || 'Ungrouped' : '';
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(s);
    }
    const ordered = [...groups.entries()].sort(([a], [b]) => (a === 'Ungrouped') - (b === 'Ungrouped') || a.localeCompare(b));
    const order = `${prefs.view}|${ordered.map(([g, items]) => `${g}:${items.map((s) => s.id).join(',')}`).join(';')}`;
    if (order !== lastOrder) {
      lastOrder = order;
      const showTitles = prefs.group && (ordered.length > 1 || ordered[0]?.[0] !== 'Ungrouped');
      if (prefs.view === 'list') {
        const tbody = h('tbody');
        for (const [g, items] of ordered) {
          if (showTitles) tbody.append(h('tr', { class: 'project-row' }, h('td', { colspan: 10 }, `${g} (${items.length})`)));
          for (const s of items) {
            if (!rows.has(s.id)) rows.set(s.id, serverRow(s.id));
            tbody.append(rows.get(s.id).el);
          }
        }
        list.replaceChildren(h('div', { class: 'panel servers-panel' }, h('div', { class: 'table-wrap' }, h('table', { class: 'servers-table' },
          h('thead', {}, h('tr', {}, ['Status', 'Server', 'CPU', 'RAM', 'Disk', 'Load', 'Uptime', 'Containers', 'Issues', ''].map((x, i) => h('th', { class: i >= 2 && i <= 7 ? 'num' : '' }, x)))),
          tbody))));
      } else {
        list.replaceChildren(...ordered.flatMap(([g, items]) => [
          showTitles ? h('h2', { class: 'group-title' }, `${g} (${items.length})`) : null,
          h('div', { class: 'cards' }, items.map((s) => {
            if (!cards.has(s.id)) cards.set(s.id, serverCard(s.id));
            return cards.get(s.id).el;
          })),
        ]).filter(Boolean));
      }
      if (!servers.length) list.append(h('div', { class: 'empty' }, prefs.filter !== 'all' ? 'No server in this state.' : 'No servers match the search.'));
    }
    const views = prefs.view === 'list' ? rows : cards;
    for (const s of servers) views.get(s.id)?.update();
  }

  // Re-sorting on every snapshot would make rows jump: values update live, the order at most every 5 s.
  let reorderTimer = null;
  const scheduleBuild = () => {
    if (reorderTimer) return;
    reorderTimer = setTimeout(() => { reorderTimer = null; build(); }, prefs.sort === 'name' ? 0 : 5000);
  };

  search.addEventListener('input', build);
  updateStats();
  build();
  return {
    update(id) {
      (prefs.view === 'list' ? rows : cards).get(id)?.update();
      updateStats();
      scheduleBuild();
    },
    destroy() { clearTimeout(reorderTimer); },
  };
}

// ---------------------------------------------------------------- Detail
function statPanel(cls, title) {
  const body = h('div');
  const right = h('span', { class: 'right' });
  const el = h('section', { class: `panel ${cls}` }, h('h3', {}, title, right), body);
  return { el, body, right };
}

const COMPOSE_LABEL = { update: 'pull & recreate', up: 'up', restart: 'restart', stop: 'stop', start: 'start', down: 'down' };
const COMPOSE_ASK = {
  update: 'Pull newer images and recreate the containers that changed?\n\nRecreated containers restart (a short interruption).',
  restart: 'Restart every container of the project?',
  stop: 'Stop every container of the project?',
  down: 'Stop and REMOVE the containers and networks of the project?\n\nVolumes are kept. Bring it back with "Up".',
};

// Compose project actions run in a terminal tab so the pull progress and errors are visible.
async function composeAction(serverId, project, action) {
  const server = state.servers.get(serverId)?.name || '';
  if (COMPOSE_ASK[action] && !(await confirmDialog(`${COMPOSE_ASK[action]}\n\nProject ${project} on ${server}.`, `Compose: ${COMPOSE_LABEL[action]}`))) return;
  openTaskFor(serverId, { kind: `compose:${action}`, arg: project, title: `${project}: ${COMPOSE_LABEL[action]}`, once: true });
}

async function upgradePackages(serverId) {
  const s = state.servers.get(serverId);
  const m = state.snaps.get(serverId)?.maintenance;
  const msg = `Upgrade the packages of ${s?.name}?\n\n${m?.updates ? `${m.updates} package${m.updates > 1 ? 's' : ''} to upgrade${m.securityUpdates ? ` (${m.securityUpdates} security)` : ''}. ` : ''}apt-get update and upgrade run in a terminal tab: read what apt plans and answer its questions there.`;
  if (!(await confirmDialog(msg, 'Upgrade packages'))) return;
  openTaskFor(serverId, { kind: 'upgrade', title: `apt upgrade`, once: true });
}

async function exportServers() {
  try {
    const file = await call('ExportServers');
    if (file) toast(`Exported to ${file} (no passwords or keys)`);
  } catch (err) {
    toast(err.message, true);
  }
}

async function importServers() {
  try {
    const r = await call('ImportServers');
    if (!r || (!r.added.length && !r.skipped.length && !r.snippets)) return;
    const parts = [`${r.added.length} server${r.added.length === 1 ? '' : 's'} added`];
    if (r.skipped.length) parts.push(`${r.skipped.length} already in the list`);
    if (r.snippets) parts.push(`${r.snippets} snippet${r.snippets > 1 ? 's' : ''}`);
    toast(parts.join(' · '));
    if (r.needsSecrets.length) toast(`Enter the password or key again (Edit) for: ${r.needsSecrets.join(', ')}`, true);
  } catch (err) {
    toast(err.message, true);
  }
}

async function removeContainer(serverId, c) {
  const running = c.state === 'running';
  const question = running
    ? `Container "${c.name}" is running. Stop it and remove it?\n\nIts image and volumes are kept.`
    : `Remove container "${c.name}"?\n\nIts image and volumes are kept.`;
  if (!(await confirmDialog(question, 'Remove container'))) return;
  try {
    await call('RemoveContainer', serverId, c.name, running);
    toast(`Removed ${c.name}`);
  } catch (err) {
    toast(`Could not remove ${c.name}: ${err.message}`, true);
  }
}

async function doAction(serverId, c, action, buttons) {
  const done = { start: 'Started', stop: 'Stopped', restart: 'Restarted' };
  if (action !== 'start' && !(await confirmDialog(`${action[0].toUpperCase()}${action.slice(1)} container "${c.name}"?${action === 'stop' ? '\n\nIt stays stopped until you start it again (unless its restart policy starts it).' : '\n\nIt is stopped and started again: a short interruption.'}`, `${action[0].toUpperCase()}${action.slice(1)} container`))) return;
  buttons.forEach((b) => { b.disabled = true; });
  try {
    await call('ContainerAction', serverId, c.name, action);
    toast(`${done[action]} ${c.name}`);
  } catch (err) {
    toast(`Could not ${action} ${c.name}: ${err.message}`, true);
  } finally {
    buttons.forEach((b) => { b.disabled = false; });
  }
}

const inlineKV = (pairs) => pairs.map(([k, v]) => h('span', {}, `${k} `, h('b', { class: 'num' }, v)));

// Remember the last tab across servers and restarts (per-viewer convenience only).
const TAB_KEY = 'detailTab';
const TABS = ['server', 'containers', 'metrics', 'services', 'files', 'network'];
const RANGE_KEY = 'detailRange';
// 'live' = the in-memory 5-second data of this session; others come from the on-disk history.
const RANGES = [['live', 'Live 30m', 0], ['1h', '1h', 60], ['6h', '6h', 360], ['24h', '24h', 1440], ['7d', '7d', 10080]];
function loadRange() {
  try {
    const r = getPref(RANGE_KEY);
    return RANGES.some(([k]) => k === r) ? r : 'live';
  } catch {
    return 'live';
  }
}
function loadTab() {
  try {
    const t = getPref(TAB_KEY);
    if (t === 'docker') return 'containers'; // tab renamed
    return TABS.includes(t) ? t : 'server';
  } catch {
    return 'server';
  }
}

// Always-visible summary tile; clicking it opens the matching tab.
function summaryTile(label, onclick) {
  const lbl = h('div', { class: 'stat-label' }, label);
  const value = h('div', { class: 'stat-value num' }, '–');
  const bar = meter(0);
  const sub = h('div', { class: 'stat-sub' });
  const el = h('button', { type: 'button', class: 'stat stat-btn', onclick }, lbl, value, bar, sub);
  return { el, lbl, value, bar, sub };
}

function detailView(id, root) {
  const cfg = () => state.servers.get(id);

  const title = h('h1');
  const chipSlot = h('span');
  const meta = h('div', { class: 'meta' });
  const errBox = h('div', { class: 'panel card-error', hidden: true });
  const alertBox = h('div', { class: 'alert-box', role: 'status', hidden: true });
  const resetBtn = h('button', { class: 'btn ghost', hidden: true, onclick: async () => {
    if (!(await confirmDialog('Trust the new host key of this server?\n\n⚠ Only do this if you know the server was reinstalled or its SSH key was changed. Otherwise someone may be intercepting the connection.', 'Reset host key'))) return;
    await call('ResetHostKey', id).catch((e) => toast(e.message, true));
  } }, 'Reset host key');
  // Refresh: spins until a new snapshot of this server arrives, then confirms briefly.
  let refreshing = null; // { since, timer }
  const refreshBtn = h('button', { class: 'btn act act-restart refresh-btn', title: 'Collect fresh metrics now' }, icon('refresh'), 'Refresh');
  function finishRefresh(ok, message) {
    if (!refreshing) return;
    // A nearby server answers in a few ms: keep "Refreshing…" up long enough to be seen.
    const wait = refreshing.since + 600 - Date.now();
    if (wait > 0) {
      clearTimeout(refreshing.timer);
      refreshing.timer = setTimeout(() => finishRefresh(ok, message), wait);
      return;
    }
    clearTimeout(refreshing.timer);
    refreshing = null;
    refreshBtn.disabled = false;
    refreshBtn.classList.remove('busy');
    if (ok) setLabel(refreshBtn, 'check', 'Updated');
    else setLabel(refreshBtn, 'refresh', 'Refresh');
    if (ok) {
      meta.classList.remove('flash');
      void meta.offsetWidth; // restart the highlight animation
      meta.classList.add('flash');
      setTimeout(() => { if (!refreshing) setLabel(refreshBtn, 'refresh', 'Refresh'); }, 1500);
    } else {
      toast(message, true);
    }
  }
  refreshBtn.addEventListener('click', () => {
    if (refreshing) return;
    refreshing = { since: Date.now(), timer: setTimeout(() => finishRefresh(false, 'Refresh timed out — the server did not answer within 30 s'), 30000) };
    refreshBtn.disabled = true;
    refreshBtn.classList.add('busy');
    setLabel(refreshBtn, 'refresh', 'Refreshing…');
    call('Refresh', id);
    if (range !== 'live') loadStored();
    loadEvents();
  });
  const rebootBtn = h('button', { class: 'btn act act-remove', title: 'Restart the server (asks first)', onclick: () => rebootServer(id) }, icon('power'), 'Reboot');
  const head = h('div', { class: 'detail-head' },
    title, chipSlot,
    h('div', { class: 'actions' },
      h('button', { class: 'btn primary', title: 'Open a shell on this server', onclick: () => openTerminalFor(id) }, icon('terminal'), 'Terminal'),
      refreshBtn,
      resetBtn,
      rebootBtn,
      h('button', { class: 'btn act act-exec', title: 'Edit the connection settings', onclick: () => openServerForm(cfg(), groupsList(), [...state.servers.values()]) }, icon('edit'), 'Edit'),
      h('button', { class: 'btn act act-remove', title: 'Remove this server from the dashboard (the server itself is not affected)', onclick: async () => {
        if (await removeServer(id)) location.hash = '#/';
      } }, icon('trash'), 'Remove')),
    meta,
  );

  // ---- Summary strip (visible on both tabs)
  const tiles = {
    cpu: summaryTile('CPU', () => setTab('server')),
    mem: summaryTile('RAM', () => setTab('server')),
    disk: summaryTile('Disk', () => setTab('server')),
    docker: summaryTile('Containers', () => setTab('containers')),
  };
  const summary = h('div', { class: 'stats summary' }, Object.values(tiles).map((t) => t.el));

  // ---- Tabs
  const dockerBadge = h('span');
  const tabBtns = {
    server: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-server', 'aria-controls': 'pane-server' }, 'Server'),
    containers: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-containers', 'aria-controls': 'pane-containers' }, 'Containers', dockerBadge),
    metrics: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-metrics', 'aria-controls': 'pane-metrics' }, 'Container metrics'),
    services: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-services', 'aria-controls': 'pane-services' }, 'Services'),
    files: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-files', 'aria-controls': 'pane-files' }, 'Files'),
    network: h('button', { type: 'button', class: 'page-tab', role: 'tab', id: 'tab-network', 'aria-controls': 'pane-network' }, 'Network'),
  };
  const tabList = h('div', { class: 'page-tabs', role: 'tablist', 'aria-label': 'Server sections' }, TABS.map((k) => tabBtns[k]));

  // ---- Time range (applies to every chart on both tabs)
  let range = loadRange();
  let stored = null; // history.Result for the selected range
  let storedTimer = null;
  const rangeBtns = RANGES.map(([key, label]) => h('button', { type: 'button', class: 'tab', 'data-range': key, onclick: () => setRange(key) }, label));
  const rangePicker = h('div', { class: 'tabs range-picker', role: 'group', 'aria-label': 'Time range' }, rangeBtns);
  const rangeNote = h('span', { class: 'range-note muted' });
  async function loadStored() {
    const minutes = RANGES.find(([k]) => k === range)[2];
    if (!minutes) return;
    const want = range;
    try {
      const res = await call('History', id, minutes);
      if (want !== range) return;
      stored = res;
      rangeNote.textContent = `${Math.round(res.step / 60000)}-min averages · kept 7 days`;
    } catch (e) {
      rangeNote.textContent = e.message;
    }
    update();
  }
  let eventsTimer = null;
  let eventsSeq = 0;
  async function loadEvents() {
    const seq = ++eventsSeq;
    const minutes = RANGES.find(([k]) => k === range)[2] || 30;
    try {
      const events = await call('Events', id, minutes);
      if (seq !== eventsSeq) return;
      for (const chart of [cpuChart, loadChart, memChart, ioChart, netChart]) chart.setEvents(events);
      metrics.setEvents(events);
    } catch (e) {
      toast(`Could not load container events: ${e.message}`, true);
    }
  }
  function setRange(next) {
    range = next;
    setPref(RANGE_KEY, next);
    rangeBtns.forEach((b) => {
      b.classList.toggle('active', b.dataset.range === next);
      b.setAttribute('aria-pressed', String(b.dataset.range === next));
    });
    clearInterval(storedTimer);
    stored = null;
    rangeNote.textContent = next === 'live' ? '5-second samples since the app started' : 'Loading…';
    if (next !== 'live') {
      loadStored();
      storedTimer = setInterval(loadStored, 60000);
    }
    clearInterval(eventsTimer);
    loadEvents();
    eventsTimer = setInterval(loadEvents, 60000);
    update();
  }

  // ---- Server tab
  const cpuP = statPanel('span-6', 'CPU usage');
  const loadP = statPanel('span-6', 'Load average');
  const memP = statPanel('span-6', 'Memory');
  const ioP = statPanel('span-6', 'Disk I/O');
  const netP = statPanel('span-6', 'Network (excluding docker/veth)');
  const diskP = statPanel('span-6', 'Disks');
  const coresP = statPanel('span-6', 'CPU per core');
  const maintP = maintenancePanel(id, { onReboot: () => rebootServer(id), onUpgrade: () => upgradePackages(id) });
  const duP = diskExplorerPanel(id, { mounts: () => state.snaps.get(id)?.host?.disks });
  const topP = topProcessesPanel(id, (cid) => state.snaps.get(id)?.docker?.containers?.find((c) => c.id === cid)?.name);
  const dfP = dockerDiskPanel(id);
  let lastHost = null; // read by the dynamic axis max / reference line below
  // CPU axis: Auto fits the data (0–5% on a quiet server, up to 0–100%); 0–100% shows the share of the whole CPU.
  let cpuFull = getPref('cpuScale') === 'full';
  const scaleBtns = [['auto', 'Auto'], ['full', '0–100%']].map(([k, label]) => h('button', {
    type: 'button', class: 'tab', 'data-scale': k, title: k === 'auto' ? 'Fit the axis to the data' : 'Always show the whole 0–100% range',
    onclick: () => { cpuFull = k === 'full'; setPref('cpuScale', k); syncScale(); cpuChart.draw(); },
  }, label));
  const syncScale = () => scaleBtns.forEach((b) => b.classList.toggle('active', (b.dataset.scale === 'full') === cpuFull));
  cpuP.right.append(h('div', { class: 'tabs scale-tabs', role: 'group', 'aria-label': 'CPU axis' }, scaleBtns));
  syncScale();
  const cpuChart = new TimeChart(cpuP.body, {
    series: [
      { key: 'user', label: 'User', color: '--cat-1' },
      { key: 'system', label: 'System', color: '--cat-2' },
      { key: 'iowait', label: 'I/O wait', color: '--cat-3' },
      { key: 'steal', label: 'Steal', color: '--cat-4' },
    ],
    yMax: () => (cpuFull ? 100 : null),
    minMax: 5,
    maxCap: 100,
    format: (v) => (v < 10 && v % 1 ? `${v.toFixed(v * 10 % 1 ? 2 : 1)}%` : `${Math.round(v)}%`),
  });
  const loadChart = new TimeChart(loadP.body, {
    series: [
      { key: 'load1', label: '1 min', color: '--cat-1' },
      { key: 'load5', label: '5 min', color: '--cat-2' },
      { key: 'load15', label: '15 min', color: '--cat-3' },
    ],
    format: (v) => v.toFixed(2),
    // Load above the core count means processes are queueing for CPU.
    ref: { value: () => lastHost?.cores ?? null, label: () => `${lastHost?.cores} cores` },
  });
  const memChart = new TimeChart(memP.body, {
    series: [
      { key: 'memUsed', label: 'Used', color: '--cat-1' },
      { key: 'memCache', label: 'Cache', color: '--cat-3' },
    ],
    yMax: () => lastHost?.mem.total || null,
    format: fmtMem,
    bytes: true,
  });
  const memInfo = h('div', { class: 'panel-foot' });
  memP.body.append(memInfo);
  const ioChart = new TimeChart(ioP.body, {
    series: [{ key: 'ioRead', label: 'Read', color: '--series-1' }, { key: 'ioWrite', label: 'Write', color: '--series-3' }],
    format: (v) => fmtRate(v),
    bytes: true,
  });
  const ioDevices = h('div', { class: 'panel-foot' });
  ioP.body.append(ioDevices);
  const netChart = new TimeChart(netP.body, {
    series: [{ key: 'rx', label: 'Received (RX)', color: '--series-1' }, { key: 'tx', label: 'Sent (TX)', color: '--series-3' }],
    format: (v) => fmtRate(v),
    bytes: true,
  });

  // ---- Containers tab
  const containers = containersPanel({
    onLogs: (c) => openLogs(state.snaps.get(id), c),
    onInspect: (c) => openInspect(state.snaps.get(id), c),
    onExec: (c) => openTerminalFor(id, c.name),
    onRemove: (c) => removeContainer(id, c),
    onAction: (c, action, buttons) => doAction(id, c, action, buttons),
    onCompose: (project, action) => composeAction(id, project, action),
    onComposeOpen: async (project, mode) => {
      try {
        const info = await call('ComposeInfo', id, project);
        setTab('files');
        await files.openPath(mode === 'file' && info.files[0] ? info.files[0] : info.dir, mode === 'file' && !!info.files[0]);
      } catch (err) {
        toast(err.message, true);
      }
    },
    history: () => state.containerHistory.get(id),
    hostMemTotal: () => state.snaps.get(id)?.host?.mem.total,
    hostCores: () => state.snaps.get(id)?.host?.cores,
  });

  // ---- Container metrics tab
  const metrics = containerMetricsTab({
    projectOf: (name) => state.snaps.get(id)?.docker?.containers?.find((c) => c.name === name)?.project || '',
  });

  // ---- Services, Files, Network tabs (load when first shown)
  const serverName = () => cfg()?.name || 'the server';
  const services = servicesTab(id, { serverName, openJournal: (name) => openTaskFor(id, { kind: 'journal', arg: name, title: `journal ${name.replace(/\.service$/, '')}` }) });
  const files = filesTab(id, { serverName, openShellAt: (path) => openTaskFor(id, { kind: 'shell', arg: path, title: `${serverName()}:${path}` }) });
  const network = networkTab(id, { serverName, containers: () => state.snaps.get(id)?.docker?.containers || [] });

  const panes = {
    server: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-server', 'aria-labelledby': 'tab-server' },
      cpuP.el, loadP.el, memP.el, ioP.el, netP.el, diskP.el, maintP.el, coresP.el, topP.el, duP.el),
    services: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-services', 'aria-labelledby': 'tab-services' }, services.el),
    files: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-files', 'aria-labelledby': 'tab-files' }, files.el),
    network: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-network', 'aria-labelledby': 'tab-network' }, ...network.panels),
    containers: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-containers', 'aria-labelledby': 'tab-containers' }, containers.el),
    metrics: h('div', { class: 'panels', role: 'tabpanel', id: 'pane-metrics', 'aria-labelledby': 'tab-metrics' }, ...metrics.panels, dfP.el),
  };

  let tab = loadTab();
  function setTab(next) {
    tab = next;
    setPref(TAB_KEY, next);
    for (const key of TABS) {
      const active = key === next;
      tabBtns[key].classList.toggle('active', active);
      tabBtns[key].setAttribute('aria-selected', String(active));
      tabBtns[key].tabIndex = active ? 0 : -1;
      panes[key].hidden = !active;
    }
    topP.setActive(next === 'server');
    services.setActive(next === 'services');
    files.setActive(next === 'files');
    network.setActive(next === 'network');
    // The time range only applies to the charts (Server and Container metrics tabs).
    rangePicker.hidden = rangeNote.hidden = next !== 'server' && next !== 'metrics';
  }
  for (const key of TABS) tabBtns[key].addEventListener('click', () => setTab(key));
  tabList.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    const next = TABS[(TABS.indexOf(tab) + (e.key === 'ArrowRight' ? 1 : TABS.length - 1)) % TABS.length];
    setTab(next);
    tabBtns[next].focus();
  });
  setTab(tab);

  root.replaceChildren(head, errBox, alertBox, summary, h('div', { class: 'tabs-row' }, tabList, rangeNote, rangePicker), ...TABS.map((k) => panes[k]));

  const updateSummary = (snap) => {
    const host = snap.host;
    const { cpu, mem, disk, docker } = tiles;
    cpu.value.textContent = fmtPct(host?.cpu);
    setMeter(cpu.bar, host?.cpu);
    const split = host?.cpuSplit;
    const notable = split ? [split.iowait >= 5 && `iowait ${Math.round(split.iowait)}%`, split.steal >= 5 && `steal ${Math.round(split.steal)}%`] : [];
    cpu.sub.textContent = host ? [`${host.cores} cores`, `load ${host.load.map((v) => v.toFixed(2)).join(' ')}`, ...notable].filter(Boolean).join(' · ') : '';
    mem.value.textContent = fmtPct(host?.mem.pct);
    setMeter(mem.bar, host?.mem.pct);
    mem.sub.textContent = host
      ? `${fmtBytes(host.mem.used)} / ${fmtBytes(host.mem.total)} · swap ${host.swap.total ? fmtBytes(host.swap.used) : 'none'}`
      : '';
    // The fullest disk is the one that matters.
    const worst = host?.disks?.reduce((a, b) => (b.pct > a.pct ? b : a), host.disks[0]);
    disk.lbl.textContent = worst ? `Disk ${worst.mount}` : 'Disk';
    disk.value.textContent = fmtPct(worst?.pct);
    setMeter(disk.bar, worst?.pct);
    disk.sub.textContent = worst
      ? `${fmtBytes(worst.used)} / ${fmtBytes(worst.size)}${host.disks.length > 1 ? ` · ${host.disks.length - 1} more` : ''}`
      : '';

    const d = snap.docker;
    const stopped = d ? d.total - d.running : 0;
    const issues = (d?.containers || []).filter(hasIssue).length;
    docker.value.textContent = d?.available ? `${d.running} / ${d.total}` : d ? 'error' : '–';
    docker.bar.hidden = true;
    docker.sub.replaceChildren(...(
      !d ? [] : !d.available ? [statusChip('offline')] : [
        issues ? h('span', { class: 'chip critical' }, `${issues} with issues`) : null,
        stopped ? h('span', { class: 'chip warning' }, `${stopped} stopped`) : null,
        !issues && !stopped ? 'all running' : null,
      ].filter(Boolean)));
    dockerBadge.replaceChildren(...(
      !d ? [] : !d.available ? [h('span', { class: 'chip critical' }, 'error')] : [
        h('span', { class: `chip ${stopped ? 'warning' : ''}` }, `${d.running}/${d.total}`),
        issues ? h('span', { class: 'chip critical' }, `${issues} issue${issues > 1 ? 's' : ''}`) : null,
      ].filter(Boolean)));
  };

  const update = () => {
    const c = cfg();
    if (!c) return;
    const snap = state.snaps.get(id) || { status: 'connecting' };
    const host = snap.host;
    title.replaceChildren(c.name, hostLabel(c, host));
    chipSlot.replaceChildren(statusChip(snap.status));
    const rebooting = snap.status === 'rebooting';
    rebootBtn.disabled = rebooting || snap.status !== 'online';
    setLabel(rebootBtn, 'power', rebooting ? 'Rebooting…' : 'Reboot');
    if (rebooting) chipSlot.firstChild.title = `Reboot requested at ${fmtTime(snap.rebootingSince)} — waiting for the server to come back`;
    meta.textContent = [`${c.username}@${c.host}:${c.port}`, host?.hostname, host?.os, host && `kernel ${host.kernel}`, host?.cpuModel,
      host && `up ${fmtUptime(host.uptime)}`, snap.updatedAt && `updated ${fmtTime(snap.updatedAt)}`].filter(Boolean).join(' · ');
    if (refreshing && snap.updatedAt > refreshing.since) {
      if (snap.status === 'online' || snap.status === 'rebooting') finishRefresh(true);
      else finishRefresh(false, `Refresh failed: ${snap.error || 'server unreachable'}`);
    }
    errBox.hidden = !snap.error || rebooting; // expected while it restarts
    errBox.textContent = snap.error ? `Connection error: ${snap.error}` : '';
    resetBtn.hidden = !/host key/i.test(snap.error || '');
    updateSummary(snap);
    maintP.update(snap.maintenance);
    duP.update();

    const alerts = snap.status === 'online' ? snap.alerts || [] : [];
    alertBox.hidden = !alerts.length;
    alertBox.replaceChildren(
      ...alerts.map((a) => h('span', { class: 'chip critical' }, `${a.title} · ${a.detail} · since ${fmtTime(a.since)}`)),
      h('button', { type: 'button', class: 'btn sm ghost', onclick: openAlertSettings }, 'Thresholds…'),
    );

    if (host) {
      lastHost = host;
      diskP.right.textContent = `${host.disks.length} mounted`;
      diskP.body.replaceChildren(...host.disks.map((d) => h('div', { class: 'disk-row' },
        h('div', { class: 'meter-row' }, h('span', {}, `${d.mount} `, h('span', { class: 'muted' }, d.fs)),
          h('span', { class: 'num' }, `${fmtBytes(d.used)} / ${fmtBytes(d.size)} · ${fmtPct(d.pct, 0)}`)),
        meter(d.pct),
        h('div', { class: `disk-inodes ${level(d.inodePct)}` }, d.inodePct == null ? 'inodes: n/a (no fixed inode table)' : `inodes ${fmtPct(d.inodePct, 0)} used`))));
      memP.right.textContent = `${fmtBytes(host.mem.total)} total`;
      memInfo.replaceChildren(...inlineKV([
        ['Available', fmtBytes(host.mem.available)],
        ['Swap', host.swap.total ? `${fmtBytes(host.swap.used)} / ${fmtBytes(host.swap.total)}` : 'none'],
        ['Swap in / out', `${fmtRate(host.swapIn)} / ${fmtRate(host.swapOut)}`],
        ['OOM kills since boot', String(host.oomKills ?? 0)],
      ]));
      ioP.right.textContent = `${host.diskIO?.length || 0} disks`;
      ioDevices.replaceChildren(host.diskIO?.length
        ? h('table', { class: 'mini-table' },
          h('thead', {}, h('tr', {}, ['Device', 'Read', 'Write', 'IOPS', 'Utilization'].map((x, i) => h('th', { class: i ? 'num' : '' }, x)))),
          h('tbody', {}, host.diskIO.map((d) => {
            const util = meter(d.util);
            util.classList.add('mini-meter');
            return h('tr', {}, h('td', {}, d.name), h('td', { class: 'num' }, fmtRate(d.readBps)), h('td', { class: 'num' }, fmtRate(d.writeBps)),
              h('td', { class: 'num' }, d.iops.toFixed(0)), h('td', { class: 'num' }, fmtPct(d.util, 0), util));
          })))
        : h('div', { class: 'muted' }, 'Collecting…'));
      coresP.right.textContent = `${host.cores} cores`;
      coresP.body.replaceChildren(h('div', { class: 'cores' }, host.perCore.map((p, i) => {
        const fill = h('span', { class: 'fill' });
        fill.style.width = `${p ?? 0}%`;
        return h('div', { class: `core ${level(p)}`, title: `Core ${i}: ${fmtPct(p)}` },
          h('span', { class: 'lbl' }, `#${i}`), h('span', { class: 'val num' }, p == null ? '–' : `${Math.round(p)}%`), fill);
      })));
    }
    // Both tabs keep receiving data, so switching tabs shows full charts immediately.
    if (range === 'live') {
      const hist = state.history.get(id) || [];
      for (const chart of [cpuChart, loadChart, memChart, ioChart, netChart]) chart.setData(hist);
      metrics.setData(state.containerHistory.get(id) || emptyContainerHistory());
    } else if (stored) {
      for (const chart of [cpuChart, loadChart, memChart, ioChart, netChart]) chart.setData(stored.host);
      // Stored container CPU is in Docker's % of one core: rescale it like the live values.
      const cores = snap.host?.cores;
      metrics.setData({ t: stored.times, ...Object.fromEntries(CONTAINER_METRICS.map(([k]) => [k, new Map(Object.entries(stored[k] || {})
        .map(([name, vals]) => [name, k === 'cpu' ? vals.map((v) => cpuOfServer(v, cores)) : vals]))])) });
    }
    containers.update(snap);
  };

  setRange(range);
  return {
    update: (sid) => { if (sid === id) update(); },
    onEvents: (sid) => { if (sid === id) loadEvents(); },
    // The app tab is hidden or shown: on-demand polling (top processes) follows it.
    hide: () => topP.setActive(false),
    show: () => topP.setActive(tab === 'server'),
    destroy: () => {
      clearInterval(storedTimer);
      topP.destroy();
      clearInterval(eventsTimer);
      metrics.destroy();
      services.destroy();
      files.destroy();
      network.destroy();
      [cpuChart, loadChart, memChart, ioChart, netChart].forEach((c) => c.destroy());
    },
  };
}

// ---------------------------------------------------------------- Boot
// Theme: System (follows Windows) -> Light -> Dark. The page applies it instantly (theme.js);
// the Go side matches the native title bar and remembers it for the next start.
const THEME_ORDER = ['system', 'light', 'dark'];
const THEME_LABEL = { system: '◐ System', light: '☀ Light', dark: '☾ Dark' };
const THEME_HINT = { system: 'Follows the Windows setting — click for Light', light: 'Light theme — click for Dark', dark: 'Dark theme — click to follow Windows' };
const themeBtn = document.getElementById('theme-toggle');
function currentTheme() {
  try {
    const t = localStorage.getItem('theme');
    return t === 'light' || t === 'dark' ? t : 'system';
  } catch {
    return 'system';
  }
}
function applyTheme(mode, persist) {
  const root = document.documentElement;
  if (mode === 'system') delete root.dataset.theme;
  else root.dataset.theme = mode;
  themeBtn.textContent = THEME_LABEL[mode];
  themeBtn.title = THEME_HINT[mode];
  themeBtn.setAttribute('aria-label', `Theme: ${mode}`);
  try { localStorage.setItem('theme', mode); } catch {}
  if (state.settings) state.settings = { ...state.settings, theme: mode };
  if (persist) call('SetTheme', mode).catch((e) => toast(e.message, true));
}
themeBtn.addEventListener('click', () => applyTheme(THEME_ORDER[(THEME_ORDER.indexOf(currentTheme()) + 1) % THEME_ORDER.length], true));
applyTheme(currentTheme(), false);
notifyToggle.addEventListener('change', () => {
  state.settings = { ...state.settings, notifications: notifyToggle.checked };
  call('SetSettings', state.settings).catch((e) => toast(e.message, true));
});
// Minimized window -> backend polls less often and the UI stops rendering.
document.addEventListener('visibilitychange', () => {
  call('SetVisible', !document.hidden);
  if (!document.hidden) tabs?.flushActive();
});

document.getElementById('alerts-btn').addEventListener('click', openAlertSettings);
const currentServer = () => { const k = keyFromHash(); return k === 'overview' ? '' : k; };
document.getElementById('run-btn').addEventListener('click', () => openRunMany(state.servers, state.snaps, currentServer() ? [currentServer()] : []));
document.getElementById('snippets-btn').addEventListener('click', () => openSnippets(state.servers, currentServer()));
document.getElementById('activity-btn').addEventListener('click', () => openActivity(state.servers, currentServer()));
const termSnippets = document.getElementById('term-snippets');
termSnippets.addEventListener('click', (e) => snippetMenu(e, termSnippets, currentServer(), state.servers, (s) => {
  if (!pasteToTerminal(s.command)) toast('Open a terminal first', true);
}));
// After a task: refresh what it changed.
document.addEventListener('task:exit', (e) => {
  const { key, task } = e.detail;
  if (task.kind.startsWith('compose:')) call('Refresh', key);
  if (task.kind === 'upgrade') call('CheckMaintenance', key).catch(() => {});
});
setAlertSettingsHandler(() => state.settings, (next) => { state.settings = next; });

initLogs();
initTerminals();
initDock({ snapshot: (id) => state.snaps.get(id) });
// Sticky offsets: the tab strip sits under the top bar, a server header under both (heights measured, not guessed).
{
  const topbar = document.querySelector('.topbar');
  const tabbar = document.getElementById('tabs');
  let stickTop = 0;
  const measure = () => {
    stickTop = topbar.offsetHeight + tabbar.offsetHeight;
    document.documentElement.style.setProperty('--topbar-h', `${topbar.offsetHeight}px`);
    document.documentElement.style.setProperty('--stick-top', `${stickTop}px`);
    markStuck();
  };
  // A line under the server header only while it is pinned (content scrolls beneath it).
  const markStuck = () => {
    for (const el of document.querySelectorAll('.detail-head')) {
      el.classList.toggle('stuck', window.scrollY > 0 && el.offsetParent !== null && el.getBoundingClientRect().top <= stickTop + 1);
    }
  };
  const ro = new ResizeObserver(measure);
  ro.observe(topbar);
  ro.observe(tabbar);
  window.addEventListener('scroll', markStuck, { passive: true });
  window.addEventListener('hashchange', () => requestAnimationFrame(markStuck));
}

boot().catch((err) => toast(`Failed to start: ${err.message}`, true));
