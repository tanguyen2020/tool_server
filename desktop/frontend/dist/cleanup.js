// Cleanup panels: leftovers of the system (journal, apt, old logs…) and the containers' log files.
import { h, toast, fmtBytes } from './util.js';
import { call, confirmDialog } from './bridge.js';
import { icon } from './icons.js';
import { panel, modal } from './ui.js';

const NO_ROOT = 'Removing needs root: connect as root, or allow this user to use sudo without a password. Sizes only cover what this user can read.';

// ---------------------------------------------------------------- System cleanup (on demand)
const SYSTEM_ITEMS = {
  journal: { label: 'systemd journal', desc: 'System logs kept by journald. Keeps the last 7 days, at most 500 MB.',
    ask: 'Shrink the systemd journal?\n\nOlder entries are deleted: the last 7 days are kept, at most 500 MB.' },
  'apt-cache': { label: 'Downloaded packages', desc: 'The .deb files apt keeps in /var/cache/apt after installing (apt-get clean).',
    ask: 'Delete the downloaded package files (apt-get clean)?\n\nInstalled packages are not touched.' },
  autoremove: { label: 'Unused packages', desc: 'Packages nothing needs any more, such as old kernels (apt-get autoremove). Opens in a terminal tab, where you see the list and confirm.' },
  'rotated-logs': { label: 'Old log files', desc: 'Rotated and compressed logs in /var/log (*.gz, *.1, *.old). The current logs are kept.',
    ask: 'Delete the rotated log files in /var/log?\n\n*.gz, *.xz, *.bz2, *.1 … *.9 and *.old. The current log files are kept.' },
  coredumps: { label: 'Crash dumps', desc: 'Memory dumps of crashed programs in /var/lib/systemd/coredump and /var/crash.',
    ask: 'Delete every crash dump?\n\n/var/lib/systemd/coredump\n/var/crash' },
  tmp: { label: 'Old temporary files', desc: 'Files in /tmp and /var/tmp not changed for 10 days.',
    ask: 'Delete the files in /tmp and /var/tmp not changed for 10 days?\n\n⚠ A program that keeps a file there for a long time loses it.' },
};

export function systemCleanupPanel(serverId, { onAutoremove }) {
  const checkBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('broom'), 'Check');
  const p = panel('span-12', 'System cleanup', checkBtn);
  const intro = h('div', { class: 'du-empty' },
    h('div', {}, h('strong', {}, 'Free up space outside Docker'),
      h('div', { class: 'muted' }, 'Measures the systemd journal, downloaded packages, unused packages, old log files, crash dumps and old temporary files.')),
    h('button', { type: 'button', class: 'btn sm', onclick: () => checkBtn.click() }, 'Check now'));
  p.body.append(intro);
  let busy = false;

  const sizeText = (it) => {
    if (it.kind === 'autoremove') return it.count < 0 ? '–' : `${it.count} package${it.count === 1 ? '' : 's'}`;
    if (it.bytes < 0) return '–';
    return `${fmtBytes(it.bytes)}${it.count > 0 ? ` · ${it.count} file${it.count === 1 ? '' : 's'}` : ''}`;
  };

  function render(info) {
    const rows = info.items.map((it) => {
      const meta = SYSTEM_ITEMS[it.kind];
      const empty = it.kind === 'autoremove' ? it.count === 0 : it.bytes === 0;
      const b = it.kind === 'autoremove'
        ? h('button', { type: 'button', class: 'btn sm act act-stop', disabled: empty || undefined, title: 'Opens apt-get autoremove in a terminal tab' }, icon('terminal'), 'Review…')
        : h('button', { type: 'button', class: 'btn sm act act-stop', disabled: empty || !info.root || undefined }, icon('trash'), 'Clean up');
      b.addEventListener('click', async () => {
        if (it.kind === 'autoremove') {
          onAutoremove();
          return;
        }
        if (busy || !(await confirmDialog(meta.ask, `Clean up: ${meta.label}`))) return;
        busy = true;
        b.disabled = true;
        try {
          const next = await call('RunSystemCleanup', serverId, it.kind);
          const after = next.items.find((x) => x.kind === it.kind);
          const freed = it.bytes >= 0 && after?.bytes >= 0 ? Math.max(0, it.bytes - after.bytes) : null;
          toast(`${meta.label}: ${freed == null ? 'done' : `freed ${fmtBytes(freed)}`}`);
          render(next);
        } catch (err) {
          toast(`${meta.label}: ${err.message}`, true);
          b.disabled = false;
        } finally {
          busy = false;
        }
      });
      return h('tr', {},
        h('td', {}, h('div', {}, meta.label), h('div', { class: 'muted small' }, meta.desc)),
        h('td', { class: `num ${empty ? 'muted' : ''}` }, sizeText(it)),
        h('td', { class: 'num' }, b));
    });
    const total = info.items.reduce((s, x) => s + (x.kind !== 'autoremove' && x.bytes > 0 ? x.bytes : 0), 0);
    p.body.replaceChildren(
      info.root ? '' : h('div', { class: 'confirm-warn' }, icon('alert'), h('span', {}, NO_ROOT)),
      h('div', { class: 'table-wrap' }, h('table', { class: 'cleanup-table' },
        h('thead', {}, h('tr', {}, h('th', {}, 'What'), h('th', { class: 'num' }, 'Size'), h('th', {}))),
        h('tbody', {}, rows))),
      h('div', { class: 'panel-foot' }, `About ${fmtBytes(total)} can be freed (unused packages not counted).`));
  }

  checkBtn.addEventListener('click', async () => {
    if (busy) return;
    busy = true;
    checkBtn.disabled = true;
    p.body.replaceChildren(h('div', { class: 'du-busy' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), 'Measuring…'));
    try {
      render(await call('SystemCleanup', serverId));
    } catch (err) {
      p.body.replaceChildren(h('div', { class: 'du-error' }, icon('alert'), `Could not measure: ${err.message}`));
    } finally {
      busy = false;
      checkBtn.disabled = false;
      checkBtn.lastChild.textContent = 'Check again';
    }
  });
  return { el: p.el };
}

// ---------------------------------------------------------------- Container log files (on demand)
const limitText = (c) => (c.maxSize ? `${c.maxSize} × ${c.maxFile || 1}` : '');

export function containerLogsPanel(serverId) {
  const findBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('logs'), 'Find');
  const limitBtn = h('button', { type: 'button', class: 'btn sm', title: 'Default log size limit for new containers (/etc/docker/daemon.json)' }, icon('cog'), 'Log limit…');
  limitBtn.addEventListener('click', () => openLogLimitDialog(serverId));
  const p = panel('span-12', 'Container log files', limitBtn, findBtn);
  p.body.append(h('div', { class: 'muted' },
    'Docker keeps every line a container prints unless a log limit is set, and docker prune never removes these files. Emptying a log also empties what the Logs view shows for that container.'));
  const sel = new Set();
  let data = null;
  const summary = h('span', { class: 'muted' });
  const emptyBtn = h('button', { type: 'button', class: 'btn sm solid-danger', disabled: true }, icon('trash'), 'Empty selected');

  const canEmpty = (c) => data?.root && c.driver === 'json-file' && c.size > 0;
  function updateSummary() {
    const bytes = data.logs.filter((c) => sel.has(c.id)).reduce((s, c) => s + c.size, 0);
    summary.textContent = sel.size ? `${sel.size} selected · ${fmtBytes(bytes)}` : '';
    emptyBtn.disabled = !sel.size;
  }

  function render() {
    [...sel].forEach((id) => { if (!data.logs.some((c) => c.id === id && canEmpty(c))) sel.delete(id); });
    const boxes = [];
    const selectable = data.logs.filter(canEmpty);
    const all = h('input', { type: 'checkbox', 'aria-label': 'Select all logs', disabled: !selectable.length || undefined });
    const syncAll = () => {
      all.checked = selectable.length > 0 && selectable.every((c) => sel.has(c.id));
      all.indeterminate = !all.checked && selectable.some((c) => sel.has(c.id));
    };
    all.addEventListener('change', () => {
      selectable.forEach((c) => (all.checked ? sel.add(c.id) : sel.delete(c.id)));
      boxes.forEach(([b, c]) => { b.checked = sel.has(c.id); });
      updateSummary();
    });
    const unlimited = data.logs.filter((c) => c.driver === 'json-file' && !c.maxSize).length;
    const rows = data.logs.map((c) => {
      const cb = h('input', { type: 'checkbox', checked: sel.has(c.id), disabled: !canEmpty(c) || undefined });
      boxes.push([cb, c]);
      cb.addEventListener('change', () => { cb.checked ? sel.add(c.id) : sel.delete(c.id); syncAll(); updateSummary(); });
      const tr = h('tr', { class: canEmpty(c) ? '' : 'disabled' },
        h('td', { class: 'cb' }, cb),
        h('td', {}, h('div', { class: 'unused-name' }, h('span', { class: `dot ${c.running ? 'good' : ''}`, title: c.running ? 'Running' : 'Stopped' }), ' ', c.name)),
        h('td', {}, c.driver !== 'json-file' ? h('span', { class: 'muted' }, c.driver || '–')
          : c.maxSize ? h('span', { class: 'chip good' }, limitText(c)) : h('span', { class: 'chip warning', title: 'This log grows until the disk is full' }, 'no limit')),
        h('td', { class: 'num' }, c.size >= 0 ? fmtBytes(c.size) : '–'));
      tr.addEventListener('click', (e) => { if (e.target !== cb && !cb.disabled) { cb.checked = !cb.checked; cb.dispatchEvent(new Event('change')); } });
      return tr;
    });
    syncAll();
    const total = data.logs.reduce((s, c) => s + Math.max(0, c.size), 0);
    p.body.replaceChildren(
      data.root ? '' : h('div', { class: 'confirm-warn' }, icon('alert'), h('span', {}, 'Reading and emptying container logs needs root: connect as root, or allow this user to use sudo without a password.')),
      unlimited ? h('div', { class: 'confirm-warn' }, icon('alert'), h('span', {},
        `${unlimited} container${unlimited === 1 ? ' has' : 's have'} no log limit. Set one with Log limit…, then recreate those containers.`)) : '',
      data.logs.length ? h('div', { class: 'table-wrap' }, h('table', { class: 'unused-table' },
        h('thead', {}, h('tr', {}, h('th', { class: 'cb' }, all), h('th', {}, 'Container'), h('th', {}, 'Limit'), h('th', { class: 'num' }, 'Size'))),
        h('tbody', {}, rows))) : h('div', { class: 'muted' }, 'No containers.'),
      h('div', { class: 'cleanup-foot' }, h('span', { class: 'muted' }, `Total ${fmtBytes(total)}`), summary, h('span', { class: 'spacer' }), emptyBtn));
    updateSummary();
  }

  async function load() {
    findBtn.disabled = true;
    try {
      data = await call('DockerContainerLogs', serverId);
      render();
    } catch (err) {
      toast(`Container logs: ${err.message}`, true);
    } finally {
      findBtn.disabled = false;
      findBtn.lastChild.textContent = 'Refresh';
    }
  }
  findBtn.addEventListener('click', load);

  emptyBtn.addEventListener('click', async () => {
    const list = data.logs.filter((c) => sel.has(c.id));
    const names = list.map((c) => `${c.name}  ${fmtBytes(c.size)}`);
    const msg = `Empty the log files of ${list.length} container${list.length === 1 ? '' : 's'}?\n\n${names.length > 20 ? `${names.slice(0, 20).join('\n')}\n… and ${names.length - 20} more` : names.join('\n')}\n\nThe containers keep running and keep logging. The old lines are gone for good.`;
    if (!(await confirmDialog(msg, 'Delete container logs', { code: true }))) return;
    emptyBtn.disabled = true;
    try {
      const freed = await call('DockerTruncateLogs', serverId, list.map((c) => c.id));
      toast(`Emptied ${list.length} log${list.length === 1 ? '' : 's'} · freed ${fmtBytes(freed)}`);
      sel.clear();
    } catch (err) {
      toast(`Empty logs: ${err.message}`, true);
    }
    await load();
  });
  return { el: p.el };
}

// Default log limit for new containers: log-opts in /etc/docker/daemon.json.
async function openLogLimitDialog(serverId) {
  const m = modal({ title: 'Docker log limit', sub: 'Default for new containers, kept in /etc/docker/daemon.json.' });
  m.body.append(h('div', { class: 'du-busy' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), 'Reading the Docker configuration…'));
  let cfg;
  try {
    cfg = await call('DockerLogConfig', serverId);
  } catch (err) {
    m.body.replaceChildren(h('div', { class: 'du-error' }, icon('alert'), err.message));
    return;
  }
  const sizeIn = h('input', { value: cfg.maxSize || '50m', placeholder: '50m', spellcheck: 'false', 'aria-label': 'Max size' });
  const filesIn = h('input', { value: cfg.maxFile || '3', placeholder: '3', inputmode: 'numeric', 'aria-label': 'Max files' });
  const supported = cfg.driver === 'json-file' || cfg.driver === 'local';
  const total = h('p', { class: 'hint' });
  const updateTotal = () => {
    const mm = /^(\d+)([kmg])$/.exec(sizeIn.value.trim());
    const n = parseInt(filesIn.value, 10);
    total.textContent = mm && n > 0 ? `At most ${fmtBytes(Number(mm[1]) * { k: 1024, m: 1024 ** 2, g: 1024 ** 3 }[mm[2]] * n)} of logs per container.` : '';
  };
  sizeIn.addEventListener('input', updateTotal);
  filesIn.addEventListener('input', updateTotal);
  updateTotal();
  m.body.replaceChildren(
    h('dl', { class: 'kv-grid log-limit-now' },
      h('dt', {}, 'Log driver'), h('dd', {}, cfg.driver || '–'),
      h('dt', {}, 'Current limit'), h('dd', {}, cfg.maxSize ? `${cfg.maxSize} × ${cfg.maxFile || 1} files` : h('span', { class: 'chip warning' }, 'no limit'))),
    supported ? h('form', { class: 'form plain log-limit-form' },
      h('label', {}, 'Max size per file (k, m or g)', sizeIn),
      h('label', {}, 'Files kept', filesIn)) : h('div', { class: 'confirm-warn' }, icon('alert'), h('span', {}, `The ${cfg.driver} log driver is in use: this limit only applies to json-file and local.`)),
    supported ? total : '',
    h('p', { class: 'hint log-limit-note' }, 'Docker reads this file when it starts: restart Docker (systemctl restart docker, which restarts the containers) to apply it. Only containers created after that get the limit; recreate the others (docker compose up -d --force-recreate). The old file is kept as daemon.json.bak.'));
  if (!supported) return;
  const save = h('button', { type: 'button', class: 'btn primary' }, icon('save'), 'Save');
  m.foot.append(h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), save);
  save.addEventListener('click', async () => {
    const size = sizeIn.value.trim().toLowerCase();
    const files = filesIn.value.trim();
    if (!(await confirmDialog(`Set the default log limit to ${size} × ${files} files?\n\n/etc/docker/daemon.json is rewritten; every other setting in it is kept.`, 'Replace Docker log limit'))) return;
    save.disabled = true;
    try {
      await call('DockerSetLogLimit', serverId, size, files);
      toast('Log limit saved. Restart Docker to apply it to new containers.');
      m.close();
    } catch (err) {
      toast(`Log limit: ${err.message}`, true);
      save.disabled = false;
    }
  });
}
