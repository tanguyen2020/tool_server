// "Run on servers": one command on several servers at once, results side by side.
import { h, toast } from './util.js';
import { call, on, confirmDialog, copyText } from './bridge.js';
import { modal } from './ui.js';
import { icon } from './icons.js';
import { snippetMenu } from './snippets.js';

const results = new Map(); // runId -> handler
on('batch.result', (r) => results.get(r.runId)?.result(r));
on('batch.done', (id) => results.get(id)?.done());

export function openRunMany(servers, snaps, preselect = []) {
  const m = modal({ title: 'Run a command on servers', sub: 'Runs without a terminal: commands that ask questions or a sudo password will fail (use sudo -n).', wide: true, cls: 'runmany' });
  const filter = h('input', { type: 'search', placeholder: 'Filter servers…', 'aria-label': 'Filter servers' });
  const listEl = h('div', { class: 'rm-servers' });
  const countEl = h('span', { class: 'muted' });
  const selected = new Set(preselect.length ? preselect : []);
  const command = h('textarea', { rows: 4, class: 'mono', spellcheck: 'false', placeholder: 'df -h /\nuptime', 'aria-label': 'Command' });
  const timeout = h('select', { 'aria-label': 'Timeout' },
    ...[[30, '30 s'], [120, '2 min'], [600, '10 min'], [1800, '30 min']].map(([v, l]) => h('option', { value: v, selected: v === 120 }, `Timeout ${l}`)));
  const runBtn = h('button', { type: 'button', class: 'btn primary' }, icon('zap'), 'Run');
  const cancelBtn = h('button', { type: 'button', class: 'btn act act-remove', hidden: true }, icon('stop'), 'Cancel');
  const snipBtn = h('button', { type: 'button', class: 'btn sm act act-exec' }, icon('bookmark'), 'Snippets');
  const out = h('div', { class: 'rm-results' });
  const summary = h('div', { class: 'rm-summary muted' });

  const groups = () => {
    const q = filter.value.trim().toLowerCase();
    const map = new Map();
    for (const s of [...servers.values()].sort((a, b) => a.name.localeCompare(b.name))) {
      if (q && ![s.name, s.host, s.group].some((v) => v?.toLowerCase().includes(q))) continue;
      const g = s.group || 'No group';
      if (!map.has(g)) map.set(g, []);
      map.get(g).push(s);
    }
    return [...map.entries()].sort((a, b) => (a[0] === 'No group') - (b[0] === 'No group') || a[0].localeCompare(b[0]));
  };

  function renderServers() {
    listEl.replaceChildren(...groups().map(([g, list]) => {
      const all = list.every((s) => selected.has(s.id));
      const box = h('input', { type: 'checkbox', checked: all });
      box.addEventListener('change', () => { list.forEach((s) => (box.checked ? selected.add(s.id) : selected.delete(s.id))); renderServers(); });
      return h('div', { class: 'rm-group' },
        h('label', { class: 'check rm-group-head' }, box, `${g} (${list.length})`),
        ...list.map((s) => {
          const cb = h('input', { type: 'checkbox', checked: selected.has(s.id) });
          cb.addEventListener('change', () => { cb.checked ? selected.add(s.id) : selected.delete(s.id); renderServers(); });
          const st = snaps.get(s.id)?.status || 'connecting';
          return h('label', { class: 'check rm-server' }, cb, h('span', { class: `dot ${st === 'online' ? 'good' : st === 'offline' ? 'critical' : 'warning'}` }), s.name,
            h('span', { class: 'muted' }, ` ${s.username}@${s.host}`));
        }));
    }));
    countEl.textContent = `${selected.size} selected`;
  }
  filter.addEventListener('input', renderServers);

  let runId = null;
  let rows = new Map();
  function resultRow(s) {
    const status = h('span', { class: 'rm-status running' }, 'running…');
    const pre = h('pre', { class: 'rm-out', hidden: true });
    const copy = h('button', { type: 'button', class: 'btn sm ghost', hidden: true, title: 'Copy output' }, 'Copy');
    const head = h('button', { type: 'button', class: 'rm-row-head' }, h('strong', {}, s.name), h('span', { class: 'muted' }, ` ${s.host}`), status);
    head.addEventListener('click', () => { pre.hidden = !pre.hidden; copy.hidden = pre.hidden; });
    copy.addEventListener('click', () => copyText(pre.textContent).then(() => toast('Copied')));
    const el = h('div', { class: 'rm-row' }, h('div', { class: 'rm-row-bar' }, head, copy), pre);
    return { el, status, pre, copy };
  }

  async function run() {
    const ids = [...selected].filter((id) => servers.has(id));
    const cmd = command.value.trim();
    if (!ids.length) { toast('Select at least one server', true); return; }
    if (!cmd) { command.focus(); return; }
    if (!(await confirmDialog(`Run this command on ${ids.length} server${ids.length > 1 ? 's' : ''}?\n\n${cmd.length > 400 ? `${cmd.slice(0, 400)}…` : cmd}`, 'Run command', { code: true }))) return;
    rows = new Map(ids.map((id) => [id, resultRow(servers.get(id))]));
    out.replaceChildren(...[...rows.values()].map((r) => r.el));
    let ok = 0;
    let failed = 0;
    const started = Date.now();
    const showSummary = (doneAll) => {
      const left = ids.length - ok - failed;
      summary.textContent = `${ok} succeeded · ${failed} failed${left ? ` · ${left} running` : ''}${doneAll ? ` · ${((Date.now() - started) / 1000).toFixed(1)} s` : ''}`;
    };
    showSummary(false);
    runBtn.disabled = true;
    cancelBtn.hidden = false;
    try {
      runId = await call('RunMany', ids, cmd, Number(timeout.value));
    } catch (err) {
      toast(err.message, true);
      runBtn.disabled = false;
      cancelBtn.hidden = true;
      return;
    }
    results.set(runId, {
      result(r) {
        const row = rows.get(r.serverId);
        if (!row) return;
        const good = !r.error && r.code === 0;
        good ? ok++ : failed++;
        row.status.className = `rm-status ${good ? 'ok' : 'bad'}`;
        row.status.textContent = r.error ? r.error : `${good ? '✓' : '✗'} exit ${r.code} · ${(r.ms / 1000).toFixed(1)} s`;
        row.pre.textContent = [r.stdout, r.stderr && `--- stderr ---\n${r.stderr}`].filter(Boolean).join('\n') || '(no output)';
        // Failures and short outputs open by themselves.
        if (!good || ids.length <= 3) { row.pre.hidden = false; row.copy.hidden = false; }
        showSummary(false);
      },
      done() {
        results.delete(runId);
        runId = null;
        runBtn.disabled = false;
        cancelBtn.hidden = true;
        showSummary(true);
      },
    });
  }
  runBtn.addEventListener('click', run);
  cancelBtn.addEventListener('click', () => runId && call('CancelRun', runId));
  command.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.ctrlKey) { e.preventDefault(); run(); } });
  snipBtn.addEventListener('click', (e) => snippetMenu(e, snipBtn, '', servers, (s) => { command.value = s.command; command.focus(); }));

  m.body.append(h('div', { class: 'rm-layout' },
    h('div', { class: 'rm-left' }, h('div', { class: 'rm-left-head' }, filter, countEl,
      h('button', { type: 'button', class: 'btn sm ghost', onclick: () => { selected.clear(); renderServers(); } }, 'None')), listEl),
    h('div', { class: 'rm-right' },
      h('label', { class: 'rm-cmd-label' }, h('span', {}, 'Command ', h('span', { class: 'muted' }, '(Ctrl+Enter to run)')), snipBtn),
      command,
      h('div', { class: 'rm-actions' }, timeout, h('span', { class: 'spacer' }), cancelBtn, runBtn),
      summary, out)));
  renderServers();
  command.focus();
}
