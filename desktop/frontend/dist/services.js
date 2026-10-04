// Services tab: systemd services of the host (nginx, docker, ssh, cron…) with start/stop/restart,
// enable/disable, status and the journal (in a terminal tab).
import { h, toast } from './util.js';
import { call, confirmDialog } from './bridge.js';
import { openMenu } from './context-menu.js';
import { modal, panel } from './ui.js';
import { icon } from './icons.js';

// Stopping these can cut the connection or take the whole server down: always say so first.
const CRITICAL = /^(ssh|sshd|systemd-.*|networking|NetworkManager|docker|containerd|dbus)\.service$/;

export function servicesTab(serverId, { openJournal, serverName }) {
  const search = h('input', { type: 'search', placeholder: 'Search services…', 'aria-label': 'Search services' });
  const filters = ['all', 'running', 'failed', 'stopped'];
  const labels = { all: 'All', running: 'Running', failed: 'Failed', stopped: 'Stopped' };
  let filter = 'all';
  const filterBtns = filters.map((k) => h('button', { type: 'button', class: 'tab', 'data-filter': k, onclick: () => { filter = k; render(); } }));
  const refreshBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('refresh'), 'Refresh');
  const status = h('span', { class: 'muted' });
  const p = panel('span-12', 'Services (systemd)', status, refreshBtn);
  const tbody = h('tbody');
  p.body.append(
    h('div', { class: 'toolbar' }, search, h('div', { class: 'tabs', role: 'group', 'aria-label': 'Filter services' }, filterBtns)),
    h('div', { class: 'table-wrap' }, h('table', { class: 'services-table' },
      h('thead', {}, h('tr', {}, ['State', 'Service', 'Description', 'At boot', ''].map((x) => h('th', {}, x)))), tbody)),
    h('p', { class: 'hint' }, 'Start, stop, restart, enable and disable need root (or sudo without a password). Logs open the journal in a terminal tab.'));

  let list = null;
  let loading = false;
  let active = false;

  const isRunning = (s) => s.active === 'active' || s.active === 'reloading' || s.active === 'activating';
  const keep = { all: () => true, running: isRunning, failed: (s) => s.active === 'failed', stopped: (s) => !isRunning(s) && s.active !== 'failed' };

  function stateChip(s) {
    if (s.active === 'failed') return h('span', { class: 'chip critical' }, 'failed');
    if (isRunning(s)) return h('span', { class: 'chip good' }, s.sub === 'exited' ? 'done' : s.sub || 'active');
    return h('span', { class: 'chip' }, s.load === 'not-found' ? 'not found' : 'stopped');
  }

  async function act(s, action) {
    const name = s.name.replace(/\.service$/, '');
    if (['stop', 'restart', 'disable'].includes(action)) {
      const warn = CRITICAL.test(s.name) ? `\n\n⚠ ${name} is essential: stopping it can cut the SSH connection or take services down.` : '';
      if (!(await confirmDialog(`${action[0].toUpperCase()}${action.slice(1)} ${name} on ${serverName()}?${warn}`, `${action[0].toUpperCase()}${action.slice(1)} service`))) return;
    }
    try {
      await call('ServiceAction', serverId, s.name, action);
      toast(`${name}: ${action} done`);
    } catch (err) {
      toast(`${name}: ${err.message}`, true);
    }
    load();
  }

  async function showStatus(s) {
    const m = modal({ title: s.name, sub: `systemctl status · ${serverName()}`, wide: true });
    const pre = h('pre', { class: 'status-pre' }, 'Loading…');
    m.body.append(pre);
    try {
      pre.textContent = await call('ServiceStatus', serverId, s.name);
    } catch (err) {
      pre.textContent = err.message;
    }
  }

  function row(s) {
    const running = isRunning(s);
    const toggle = h('button', { type: 'button', class: `btn sm act ${running ? 'act-stop' : 'act-start'}`, onclick: () => act(s, running ? 'stop' : 'start') },
      icon(running ? 'stop' : 'play'), running ? 'Stop' : 'Start');
    const restart = h('button', { type: 'button', class: 'btn sm act act-restart', onclick: () => act(s, 'restart') }, icon('restart'), 'Restart');
    const logs = h('button', { type: 'button', class: 'btn sm act act-logs', onclick: () => openJournal(s.name) }, icon('logs'), 'Logs');
    const more = h('button', { type: 'button', class: 'btn sm act act-inspect', title: 'More', 'aria-label': `More actions for ${s.name}` }, icon('more'));
    const menu = (e) => openMenu(e, more, [
      { label: 'Status…', action: () => showStatus(s) },
      { label: 'Reload configuration', action: () => act(s, 'reload') },
      s.enabled === 'enabled' ? { label: 'Disable at boot', action: () => act(s, 'disable') } : { label: 'Enable at boot', action: () => act(s, 'enable') },
      { label: 'Restart', action: () => act(s, 'restart') },
      running ? { label: 'Stop', danger: true, action: () => act(s, 'stop') } : { label: 'Start', action: () => act(s, 'start') },
    ]);
    more.addEventListener('click', menu);
    const tr = h('tr', { class: s.active === 'failed' ? 'row-issue' : '' },
      h('td', {}, stateChip(s)),
      h('td', { class: 'svc-name' }, s.name.replace(/\.service$/, '')),
      h('td', { class: 'muted svc-desc', title: s.description }, s.description || ''),
      h('td', {}, s.enabled ? h('span', { class: `chip ${s.enabled === 'enabled' ? 'good' : ''}` }, s.enabled) : h('span', { class: 'muted' }, '–')),
      h('td', {}, h('div', { class: 'c-actions' }, logs, toggle, running ? restart : '', more)));
    tr.addEventListener('contextmenu', menu);
    return tr;
  }

  function render() {
    filterBtns.forEach((b) => {
      const k = b.dataset.filter;
      b.textContent = `${labels[k]}${list ? ` ${list.filter(keep[k]).length}` : ''}`;
      b.classList.toggle('active', k === filter);
      b.classList.toggle('has-issues', k === 'failed' && list?.some(keep.failed));
    });
    if (!list) return;
    const q = search.value.trim().toLowerCase();
    const rows = list.filter((s) => keep[filter](s) && (!q || s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q)))
      // Failed first, then running, then the rest.
      .sort((a, b) => (b.active === 'failed') - (a.active === 'failed') || isRunning(b) - isRunning(a) || a.name.localeCompare(b.name));
    tbody.replaceChildren(...(rows.length ? rows.map(row) : [h('tr', {}, h('td', { colspan: 5, class: 'empty' }, 'No services match.'))]));
  }

  async function load() {
    if (loading) return;
    loading = true;
    status.textContent = 'Loading…';
    try {
      list = await call('Services', serverId);
      status.textContent = `${list.length} services`;
    } catch (err) {
      status.textContent = err.message;
    } finally {
      loading = false;
    }
    render();
  }
  search.addEventListener('input', render);
  refreshBtn.addEventListener('click', load);
  render();

  return {
    el: p.el,
    setActive(v) {
      active = v;
      if (active && !list) load();
    },
    destroy() {},
  };
}
