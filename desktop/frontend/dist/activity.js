// Activity: what was done from this app, when, by whom and whether it worked (kept locally).
import { h } from './util.js';
import { call } from './bridge.js';
import { modal, fmtDateTime } from './ui.js';
import { icon } from './icons.js';

export async function openActivity(servers, serverId = '') {
  const m = modal({ title: 'Activity', sub: 'Actions done from this app on this computer (newest first, last 5000 kept).', wide: true });
  const serverSel = h('select', { 'aria-label': 'Server' },
    h('option', { value: '' }, 'All servers'),
    ...[...servers.values()].sort((a, b) => a.name.localeCompare(b.name)).map((s) => h('option', { value: s.id, selected: s.id === serverId }, s.name)));
  const search = h('input', { type: 'search', placeholder: 'Search action, target, error…', 'aria-label': 'Search activity' });
  const onlyFailed = h('input', { type: 'checkbox' });
  const tbody = h('tbody');
  const count = h('span', { class: 'muted' });
  let entries = [];

  function render() {
    const q = search.value.trim().toLowerCase();
    const rows = entries.filter((e) => (!onlyFailed.checked || !e.ok)
      && (!q || [e.action, e.target, e.error, e.server, e.user].some((v) => v?.toLowerCase().includes(q))));
    count.textContent = `${rows.length} of ${entries.length}`;
    tbody.replaceChildren(...(rows.length ? rows.map((e) => h('tr', { class: e.ok ? '' : 'row-issue' },
      h('td', { class: 'nowrap' }, fmtDateTime(e.time)),
      h('td', {}, e.server || h('span', { class: 'muted' }, '–')),
      h('td', {}, e.action),
      h('td', { class: 'mono act-target', title: e.target || '' }, e.target || ''),
      h('td', {}, e.ok ? h('span', { class: 'chip good' }, 'OK') : h('span', { class: 'chip critical', title: e.error }, 'Failed'), e.ok ? '' : h('div', { class: 'muted small' }, e.error)),
      h('td', { class: 'muted' }, e.user || ''))) : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, 'Nothing recorded yet.'))]));
  }

  async function load() {
    entries = await call('Activity', serverSel.value, 5000).catch(() => []);
    render();
  }
  serverSel.addEventListener('change', load);
  search.addEventListener('input', render);
  onlyFailed.addEventListener('change', render);
  m.body.append(
    h('div', { class: 'toolbar' }, serverSel, search, h('label', { class: 'check' }, onlyFailed, 'Failed only'), h('span', { class: 'spacer' }), count,
      h('button', { type: 'button', class: 'btn sm act act-restart', onclick: load }, icon('refresh'), 'Refresh')),
    h('div', { class: 'table-wrap activity-wrap' }, h('table', { class: 'activity-table' },
      h('thead', {}, h('tr', {}, ['Time', 'Server', 'Action', 'Target', 'Result', 'User'].map((x) => h('th', {}, x)))), tbody)));
  await load();
}
