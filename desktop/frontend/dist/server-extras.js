import { h, toast, fmtBytes, fmtTime } from './util.js';
import { call, confirmDialog } from './bridge.js';
import { icon } from './icons.js';

function fmtAgo(ms) {
  if (!ms) return 'unknown';
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 90) return 'just now';
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 172800) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} days ago`;
}

function panel(cls, title, right) {
  const body = h('div');
  return { el: h('section', { class: `panel ${cls}` }, h('h3', {}, title, h('span', { class: 'right' }, right)), body), body };
}

// ---------------------------------------------------------------- Top processes (on demand)
// Runs `top` only while the panel is open, the Server tab is active and the window is visible.
export function topProcessesPanel(serverId, containerName) {
  const sortSel = h('select', { 'aria-label': 'Sort processes by' }, h('option', { value: 'cpu' }, 'By CPU'), h('option', { value: 'mem' }, 'By memory'));
  const toggle = h('button', { type: 'button', class: 'btn sm' }, 'Show');
  const status = h('span', { class: 'muted' });
  const p = panel('span-12', 'Top processes', [status, sortSel, toggle]);
  const tbody = h('tbody');
  const table = h('div', { class: 'table-wrap', hidden: true }, h('table', { class: 'proc-table' },
    h('thead', {}, h('tr', {},
      h('th', { class: 'num' }, 'PID'), h('th', {}, 'User'), h('th', {}, 'Container'),
      h('th', { class: 'num', title: '% of one core over the last second' }, 'CPU'), h('th', { class: 'num' }, 'Mem'),
      h('th', { class: 'num' }, 'RSS'), h('th', { class: 'num' }, 'CPU time'), h('th', {}, 'Command'))),
    tbody));
  const hint = h('div', { class: 'muted' }, 'Lists the busiest processes on the host, including ones outside Docker. Measured over 1 second, refreshed every 5 seconds while shown.');
  p.body.append(hint, table);

  let open = false;
  let active = true;
  let busy = false;
  let timer = null;

  async function refresh() {
    if (!open || !active || busy || document.hidden) return;
    busy = true;
    try {
      const procs = await call('TopProcesses', serverId, sortSel.value);
      tbody.replaceChildren(...procs.map((x) => h('tr', {},
        h('td', { class: 'num' }, x.pid), h('td', {}, x.user),
        h('td', {}, x.containerId ? containerName(x.containerId) || x.containerId.slice(0, 12) : h('span', { class: 'muted' }, 'host')),
        h('td', { class: 'num' }, `${x.cpu.toFixed(1)}%`), h('td', { class: 'num' }, `${x.mem.toFixed(1)}%`),
        h('td', { class: 'num' }, fmtBytes(x.rss)), h('td', { class: 'num' }, x.time),
        h('td', { class: 'cmd', title: x.command }, x.command))));
      status.textContent = `updated ${fmtTime(Date.now())}`;
    } catch (err) {
      status.textContent = err.message;
    } finally {
      busy = false;
    }
  }

  function setOpen(next) {
    open = next;
    toggle.textContent = open ? 'Hide' : 'Show';
    table.hidden = !open;
    hint.hidden = open;
    clearInterval(timer);
    if (open) {
      status.textContent = 'Loading…';
      refresh();
      timer = setInterval(refresh, 5000);
    } else {
      status.textContent = '';
    }
  }
  toggle.addEventListener('click', () => setOpen(!open));
  sortSel.addEventListener('change', refresh);

  return {
    el: p.el,
    setActive(v) { active = v; if (v) refresh(); },
    destroy() { clearInterval(timer); open = false; },
  };
}

// ---------------------------------------------------------------- Maintenance (hourly)
export function maintenancePanel(serverId, { onReboot, onUpgrade } = {}) {
  const checkBtn = h('button', { type: 'button', class: 'btn sm' }, 'Check now');
  const p = panel('span-6', 'Maintenance', checkBtn);
  checkBtn.addEventListener('click', async () => {
    checkBtn.disabled = true;
    checkBtn.textContent = 'Checking…';
    try {
      await call('CheckMaintenance', serverId);
    } catch (err) {
      toast(`Maintenance check failed: ${err.message}`, true);
    } finally {
      checkBtn.disabled = false;
      checkBtn.textContent = 'Check now';
    }
  });
  let shownAt = -1;

  function update(m) {
    if (!m) {
      p.body.replaceChildren(h('div', { class: 'muted' }, 'Not checked yet — runs automatically once the server is online.'));
      return;
    }
    if (m.checkedAt === shownAt) return; // keep an expanded package list open between refreshes
    shownAt = m.checkedAt;
    const listsAge = m.listsUpdated ? Date.now() - m.listsUpdated : null;
    const stale = listsAge != null && listsAge > 7 * 86400e3;
    p.body.replaceChildren(h('dl', { class: 'kv-grid' },
      h('dt', {}, 'Reboot'),
      h('dd', {}, m.rebootRequired
        ? [h('span', { class: 'chip warning' }, 'Required'), ' ', h('span', { class: 'muted' }, m.rebootReason),
          onReboot ? [' ', h('button', { type: 'button', class: 'btn sm act act-remove', onclick: onReboot }, icon('power'), 'Reboot now…')] : '']
        : h('span', { class: 'chip good' }, 'Not required')),
      h('dt', {}, 'Updates'),
      h('dd', {}, m.updates
        ? [h('span', { class: `chip ${m.securityUpdates ? 'warning' : ''}` }, `${m.updates} pending`),
          m.securityUpdates ? ` including ${m.securityUpdates} security` : '',
          onUpgrade ? [' ', h('button', { type: 'button', class: 'btn sm act act-start', title: 'Runs apt-get update and upgrade in a terminal tab, where you answer the prompts', onclick: onUpgrade }, icon('upload'), 'Upgrade…')] : '']
        : h('span', { class: 'chip good' }, 'Up to date')),
      h('dt', {}, 'Certificates'),
      h('dd', {}, certSummary(m.certs)),
      h('dt', {}, 'Package lists'),
      h('dd', {}, `updated ${fmtAgo(m.listsUpdated)}`, stale ? h('span', { class: 'muted' }, ' — counts may be outdated; run apt update') : ''),
      h('dt', {}, 'Checked'),
      h('dd', {}, `${fmtTime(m.checkedAt)} · every hour`),
    ),
    m.packages?.length ? h('details', { class: 'pkg-list' },
      h('summary', {}, `Show packages${m.updates > m.packages.length ? ` (first ${m.packages.length})` : ''}`),
      h('ul', {}, m.packages.map((x) => h('li', { class: 'mono' }, x)))) : '',
    m.certs?.length ? h('details', { class: 'pkg-list', open: m.certs.some((c) => daysLeft(c) <= 14) },
      h('summary', {}, `Show certificates (${m.certs.length})`),
      h('table', { class: 'mini-table' },
        h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Expires'), h('th', {}, 'File'))),
        h('tbody', {}, m.certs.map((c) => h('tr', {},
          h('td', {}, c.subject || '–'),
          h('td', {}, certChip(c)),
          h('td', { class: 'mono muted' }, c.path)))))) : '');
  }
  return { el: p.el, update };
}

// ---------------------------------------------------------------- Docker disk usage (on demand)
const CLEANUPS = [
  { kind: 'containers', label: 'Stopped containers', type: 'Containers', ask: 'Remove every stopped container?\n\nTheir images and volumes are kept.' },
  { kind: 'images-dangling', label: 'Dangling images', type: 'Images', ask: 'Remove dangling images (untagged layers left by rebuilds and pulls)?' },
  { kind: 'images-unused', label: 'Unused images', type: 'Images', ask: 'Remove every image not used by a container (running or stopped)?\n\nThey will be downloaded again when needed.' },
  { kind: 'build-cache', label: 'Build cache', type: 'Build Cache', ask: 'Remove the build cache?\n\nThe next image builds will be slower.' },
  { kind: 'networks', label: 'Unused networks', type: '', ask: 'Remove networks not used by any container?' },
  { kind: 'volumes', label: 'Unused volumes', type: 'Local Volumes', danger: true, ask: 'Remove unused anonymous volumes?\n\n⚠ Volumes hold data (databases, uploads). Anything in a volume no container uses is deleted for good.' },
];

export function dockerDiskPanel(serverId) {
  // Cleanup buttons, each with the space docker reports as reclaimable for its type.
  const cleanupBox = (rows) => {
    const reclaim = (type) => rows.find((r) => r.type === type)?.reclaimable || '';
    return h('div', { class: 'cleanup' },
      h('div', { class: 'cleanup-title' }, icon('broom'), 'Free up space'),
      h('div', { class: 'cleanup-actions' }, CLEANUPS.map((c) => {
        const b = h('button', { type: 'button', class: `btn sm act ${c.danger ? 'act-remove' : 'act-stop'}` },
          icon('trash'), c.label, c.type && reclaim(c.type) ? h('span', { class: 'muted' }, ` · ${reclaim(c.type).replace(/\s*\(.*\)/, '')}`) : '');
        b.addEventListener('click', async () => {
          if (!(await confirmDialog(c.ask, `Clean up: ${c.label}`))) return;
          b.disabled = true;
          try {
            const out = await call('DockerPrune', serverId, c.kind);
            toast(`${c.label}: ${out}`);
            btn.click(); // recalculate
          } catch (err) {
            toast(`${c.label}: ${err.message}`, true);
          } finally {
            b.disabled = false;
          }
        });
        return b;
      })));
  };
  const btn = h('button', { type: 'button', class: 'btn sm' }, 'Calculate');
  const p = panel('span-12', 'Docker disk usage', btn);
  p.body.append(h('div', { class: 'muted' }, 'Space used by images, containers, volumes and the build cache (`docker system df`). Can take a while on busy hosts.'));
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    btn.textContent = 'Calculating…';
    try {
      const rows = await call('DockerDiskUsage', serverId);
      p.body.replaceChildren(
        h('div', { class: 'table-wrap' }, h('table', {},
          h('thead', {}, h('tr', {}, ['Type', 'Total', 'Active', 'Size', 'Reclaimable'].map((x, i) => h('th', { class: i ? 'num' : '' }, x)))),
          h('tbody', {}, rows.map((r) => h('tr', {}, h('td', {}, r.type), h('td', { class: 'num' }, r.total), h('td', { class: 'num' }, r.active),
            h('td', { class: 'num' }, r.size), h('td', { class: 'num' }, r.reclaimable)))))),
        h('div', { class: 'panel-foot' }, `Calculated ${fmtTime(Date.now())}.`),
        cleanupBox(rows),
      );
    } catch (err) {
      toast(`docker system df failed: ${err.message}`, true);
    } finally {
      btn.disabled = false;
      btn.textContent = 'Recalculate';
    }
  });
  return { el: p.el };
}

const daysLeft = (c) => Math.floor((c.notAfter - Date.now()) / 86400e3);

function certChip(c) {
  const d = daysLeft(c);
  const date = new Date(c.notAfter).toLocaleDateString('en-GB', { day: '2-digit', month: 'short', year: 'numeric' });
  if (d < 0) return h('span', { class: 'chip critical' }, `expired ${date}`);
  if (d <= 14) return h('span', { class: 'chip critical' }, `${d} days · ${date}`);
  if (d <= 30) return h('span', { class: 'chip warning' }, `${d} days · ${date}`);
  return h('span', { class: 'chip good' }, `${d} days · ${date}`);
}

function certSummary(certs) {
  if (!certs?.length) return h('span', { class: 'muted', title: 'Looked in /etc/letsencrypt/live and the nginx / Apache configuration' }, 'none found');
  const soonest = certs[0];
  return [certChip(soonest), ' ', h('span', { class: 'muted' }, `${soonest.subject || soonest.path}${certs.length > 1 ? ` (soonest of ${certs.length})` : ''}`)];
}

// ---------------------------------------------------------------- Disk usage by folder (on demand)
export function diskExplorerPanel(serverId, { mounts }) {
  const mountSel = h('select', { 'aria-label': 'File system' });
  const analyzeBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('disk'), 'Analyze');
  const status = h('span', { class: 'muted' });
  const p = panel('span-12', 'Disk usage by folder', [status, mountSel, analyzeBtn]);
  const crumbs = h('div', { class: 'crumbs-path', hidden: true });
  const view = h('div', { class: 'du-view' });
  p.body.append(crumbs, view);
  let current = '';
  let busy = false;
  let mountsKey = '';

  function syncMounts() {
    const ms = mounts() || [];
    const key = ms.map((d) => `${d.mount}:${Math.round(d.pct)}`).join(',');
    if (key === mountsKey) return; // rebuilding the list would close it while open
    mountsKey = key;
    const keep = mountSel.value;
    mountSel.replaceChildren(...ms.map((d) => h('option', { value: d.mount }, `${d.mount} · ${Math.round(d.pct)}% of ${fmtBytes(d.size)}`)));
    if (ms.some((d) => d.mount === keep)) mountSel.value = keep;
    else if (ms.length) mountSel.value = ms.reduce((a, b) => (b.pct > a.pct ? b : a)).mount; // the fullest first
    if (!current && !busy) showStart();
  }

  // Before the first run: say what it does and offer one clear button.
  function showStart() {
    const target = mountSel.value || '/';
    view.replaceChildren(h('div', { class: 'du-empty' },
      h('div', {}, h('strong', {}, 'What fills the disk?'),
        h('div', { class: 'muted' }, `Measures the folders of ${target} (one file system, like du). Click a folder to look inside. Uses root rights when allowed; otherwise folders the SSH user cannot read are skipped.`)),
      h('button', { type: 'button', class: 'btn primary', onclick: () => analyze(target) }, icon('disk'), `Analyze ${target}`)));
  }

  async function analyze(path) {
    if (busy) return;
    busy = true;
    analyzeBtn.disabled = true;
    mountSel.disabled = true;
    status.textContent = '';
    const started = Date.now();
    const elapsed = h('span', { class: 'muted' });
    const tick = () => { elapsed.textContent = `${Math.round((Date.now() - started) / 1000)} s`; };
    tick();
    const timer = setInterval(tick, 1000);
    view.replaceChildren(h('div', { class: 'du-busy' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }),
      h('span', {}, `Measuring ${path}… `), elapsed,
      h('span', { class: 'muted' }, ' · large disks can take a minute or two; the scan runs at low priority on the server')));
    try {
      const res = await call('DiskUsageAt', serverId, path);
      current = res.path;
      const parts = current.split('/').filter(Boolean);
      crumbs.hidden = false;
      crumbs.replaceChildren(
        h('button', { type: 'button', class: 'crumb', onclick: () => analyze('/') }, '/'),
        ...parts.flatMap((part, i) => [h('span', { class: 'muted' }, '›'),
          h('button', { type: 'button', class: 'crumb', onclick: () => analyze(`/${parts.slice(0, i + 1).join('/')}`) }, part)]),
        h('span', { class: 'muted' }, ` · ${fmtBytes(res.total)} in total`),
        current !== '/' ? h('button', { type: 'button', class: 'btn sm act act-logs du-up', onclick: () => analyze(current.replace(/\/[^/]+$/, '') || '/') }, icon('up'), 'Up') : '');
      const inFolders = res.dirs.reduce((a, d) => a + d.size, 0);
      const rows = res.dirs.map((d) => ({ name: d.path.slice(current === '/' ? 1 : current.length + 1) || d.path, size: d.size, path: d.path }));
      // What sits directly in this folder (files, not sub-folders).
      if (res.total - inFolders > 1024 * 1024) rows.push({ name: '(files in this folder)', size: res.total - inFolders, path: null });
      rows.sort((a, b) => b.size - a.size);
      const max = rows[0]?.size || 1;
      view.replaceChildren(h('div', { class: 'du-list' }, ...(rows.length ? rows.map((d) => {
        const bar = h('span', { class: 'du-bar', style: `width:${Math.max(1, (d.size / max) * 100).toFixed(1)}%` });
        const cells = [h('span', { class: `du-name mono${d.path ? '' : ' muted'}` }, d.name), h('span', { class: 'du-track' }, bar),
          h('span', { class: 'du-size num' }, fmtBytes(d.size)),
          h('span', { class: 'du-pct muted num' }, res.total ? `${Math.round((d.size / res.total) * 100)}%` : '')];
        return d.path
          ? h('button', { type: 'button', class: 'du-row', title: `Open ${d.path}`, onclick: () => analyze(d.path) }, ...cells)
          : h('div', { class: 'du-row static' }, ...cells);
      }) : [h('div', { class: 'muted' }, 'This folder is empty.')])));
      status.textContent = `measured in ${Math.round((Date.now() - started) / 1000)} s · ${fmtTime(Date.now())}`;
    } catch (err) {
      view.replaceChildren(h('div', { class: 'du-error' },
        h('span', { class: 'chip critical' }, 'Failed'), h('span', {}, err.message),
        h('button', { type: 'button', class: 'btn sm act act-restart', onclick: () => analyze(path) }, icon('refresh'), 'Try again')));
    } finally {
      clearInterval(timer);
      busy = false;
      analyzeBtn.disabled = false;
      mountSel.disabled = false;
    }
  }
  analyzeBtn.addEventListener('click', () => analyze(mountSel.value || '/'));
  // Picking another disk measures it right away.
  mountSel.addEventListener('change', () => analyze(mountSel.value || '/'));
  showStart();
  return { el: p.el, update: syncMounts };
}

// Chips for the overview card: reboot needed / pending security updates.
export function maintenanceChips(m) {
  if (!m) return [];
  const chips = [];
  if (m.rebootRequired) chips.push(h('span', { class: 'chip warning', title: m.rebootReason }, 'Reboot required'));
  const expiring = (m.certs || []).filter((c) => daysLeft(c) <= 14);
  if (expiring.length) {
    const d = daysLeft(expiring[0]);
    chips.push(h('span', { class: 'chip critical', title: expiring.map((c) => c.subject || c.path).join('\n') }, d < 0 ? 'Certificate expired' : `Certificate expires in ${d} d`));
  }
  if (m.securityUpdates) chips.push(h('span', { class: 'chip warning' }, `${m.securityUpdates} security update${m.securityUpdates > 1 ? 's' : ''}`));
  else if (m.updates) chips.push(h('span', { class: 'chip' }, `${m.updates} update${m.updates > 1 ? 's' : ''}`));
  return chips;
}
