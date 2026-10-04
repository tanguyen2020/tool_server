import { h, fmtBytes, statusChip } from './util.js';
import { drawMini } from './chart.js';
import { openMenu } from './context-menu.js';
import { icon } from './icons.js';
import { getJSONPref, setJSONPref } from './prefs.js';

// Row action button: icon + label, tinted with its own color. The label hides when the table must be compact.
const actBtn = (kind, iconName, label, title = label) => h('button', { type: 'button', class: `btn sm act act-${kind}`, title, 'aria-label': label },
  icon(iconName), h('span', { class: 'lbl' }, label));

// Low-priority details hidden one step at a time until the table fits without horizontal scrolling.
const FIT_STEPS = ['no-spark', 'no-pids', 'no-block', 'no-net', 'no-ports', 'narrow-name', 'compact-actions', 'no-restarts', 'no-health', 'no-status'];

// Common exit codes: why did the container stop?
export const EXIT_MEANING = {
  0: 'clean exit',
  1: 'application error',
  2: 'misused shell command',
  125: 'docker could not run it',
  126: 'command not executable',
  127: 'command not found',
  130: 'interrupted (SIGINT)',
  137: 'killed (SIGKILL: out of memory or stop timeout)',
  139: 'crashed (segmentation fault)',
  143: 'stopped (SIGTERM)',
};
const NORMAL_EXIT = new Set([0, 130, 143]);

// Something needs attention: failing healthcheck, restart loop, restarting, or an abnormal exit.
export function hasIssue(c) {
  return c.health === 'unhealthy' || c.looping || c.state === 'restarting' || c.state === 'dead'
    || (c.state === 'exited' && c.exitCode != null && !NORMAL_EXIT.has(c.exitCode));
}

// "0.0.0.0:8080->80/tcp, :::8080->80/tcp, 6379/tcp" -> ["8080→80/tcp"] (+ internal-only ports)
function parsePorts(raw) {
  const published = new Set();
  const internal = new Set();
  for (const part of (raw || '').split(',').map((s) => s.trim()).filter(Boolean)) {
    const [host, inner] = part.split('->');
    if (inner) published.add(`${host.slice(host.lastIndexOf(':') + 1)}→${inner}`);
    else internal.add(part);
  }
  return { published: [...published], internal: [...internal] };
}

const fmtCpu = (v) => `${v >= 10 ? v.toFixed(1) : v.toFixed(2)}%`;

const SORTS = {
  name: (a, b) => a.name.localeCompare(b.name),
  cpu: (a, b) => (a.cpu ?? -1) - (b.cpu ?? -1),
  mem: (a, b) => (a.memUsed ?? -1) - (b.memUsed ?? -1),
  restarts: (a, b) => (a.restartCount ?? -1) - (b.restartCount ?? -1),
  state: (a, b) => Number(hasIssue(a)) - Number(hasIssue(b)) || Number(a.state === 'running') - Number(b.state === 'running'),
};

function loadPrefs() {
  try {
    return { filter: 'all', group: true, sort: 'name', dir: 1, ...getJSONPref('containerTable') };
  } catch {
    return { filter: 'all', group: true, sort: 'name', dir: 1 };
  }
}

function containerRow({ onLogs, onInspect, onExec, onRemove, onAction, sparkline, hostMemTotal }) {
  const chipCell = h('td');
  const nameEl = h('div', { class: 'c-name' });
  const imageEl = h('div', { class: 'c-image' });
  const healthCell = h('td');
  const statusCell = h('td', { class: 'c-status' });
  const portsCell = h('td', { class: 'c-ports' });
  const cpuText = h('span');
  const cpuSpark = h('canvas', { class: 'cell-spark', 'aria-hidden': 'true' });
  const memText = h('span');
  const memSpark = h('canvas', { class: 'cell-spark', 'aria-hidden': 'true' });
  const restartCell = h('td', { class: 'num' });
  const netCell = h('td', { class: 'num muted' });
  const blockCell = h('td', { class: 'num muted' });
  const pidsCell = h('td', { class: 'num' });
  const logsBtn = actBtn('logs', 'logs', 'Logs', 'Follow the logs (docker logs -f)');
  const execBtn = actBtn('exec', 'terminal', 'Exec', 'Open a shell inside the container (docker exec)');
  const inspectBtn = actBtn('inspect', 'info', 'Inspect', 'Details (docker inspect)');
  const toggleBtn = actBtn('stop', 'stop', 'Stop');
  const restartBtn = actBtn('restart', 'restart', 'Restart');
  const removeBtn = actBtn('remove', 'trash', 'Remove', 'Remove the container (docker rm); image and volumes are kept');
  const buttons = [toggleBtn, restartBtn];
  const tr = h('tr', {},
    chipCell, h('td', {}, nameEl, imageEl), healthCell, statusCell, portsCell,
    h('td', { class: 'num' }, h('div', { class: 'cell-metric' }, cpuText, cpuSpark)),
    h('td', { class: 'num' }, h('div', { class: 'cell-metric' }, memText, memSpark)),
    restartCell, netCell, blockCell, pidsCell,
    h('td', { class: 'c-actions-cell' }, h('div', { class: 'c-actions' }, logsBtn, execBtn, inspectBtn, toggleBtn, restartBtn, removeBtn)),
  );
  let c = null;
  logsBtn.addEventListener('click', () => onLogs(c));
  inspectBtn.addEventListener('click', () => onInspect(c));
  execBtn.addEventListener('click', () => onExec(c));
  toggleBtn.addEventListener('click', () => onAction(c, c.state === 'running' ? 'stop' : 'start', buttons));
  restartBtn.addEventListener('click', () => onAction(c, 'restart', buttons));
  removeBtn.addEventListener('click', () => onRemove(c));
  tr.addEventListener('contextmenu', (e) => {
    const running = c.state === 'running';
    openMenu(e, tr, [
      { label: 'Logs', action: () => onLogs(c) },
      ...(running ? [{ label: 'Exec (shell)', action: () => onExec(c) }] : []),
      { label: 'Inspect', action: () => onInspect(c) },
      running ? { label: 'Stop', action: () => onAction(c, 'stop', buttons) } : { label: 'Start', action: () => onAction(c, 'start', buttons) },
      ...(running ? [{ label: 'Restart', action: () => onAction(c, 'restart', buttons) }] : []),
      { label: running ? 'Stop and remove…' : 'Remove…', danger: true, action: () => onRemove(c) },
    ]);
  });

  const update = (next) => {
    c = next;
    const running = c.state === 'running';
    tr.classList.toggle('row-issue', hasIssue(c));
    chipCell.replaceChildren(statusChip(c.state));
    nameEl.textContent = c.name;
    imageEl.textContent = c.image;
    imageEl.title = c.image;

    healthCell.replaceChildren(
      c.health === 'healthy' ? h('span', { class: 'chip good' }, 'healthy')
        : c.health === 'unhealthy' ? h('span', { class: 'chip critical' }, 'unhealthy')
          : c.health === 'starting' ? h('span', { class: 'chip warning' }, 'starting')
            : h('span', { class: 'muted', title: 'The image defines no healthcheck' }, '–'),
    );

    // Health already has its own column, so drop "(healthy)" from the status text.
    const status = (c.status || '').replace(/\s*\((healthy|unhealthy|health: starting)\)/, '');
    if (c.state === 'exited' && c.exitCode != null) {
      const meaning = EXIT_MEANING[c.exitCode] || 'non-zero exit';
      statusCell.replaceChildren(
        h('div', {}, status.replace(/^Exited \(-?\d+\)\s*/, 'Exited ')),
        h('div', { class: `exit-code ${NORMAL_EXIT.has(c.exitCode) ? 'muted' : 'bad'}` }, `code ${c.exitCode} · ${meaning}`),
      );
    } else {
      statusCell.replaceChildren(status);
    }

    const ports = parsePorts(c.ports);
    portsCell.title = c.ports || '';
    portsCell.replaceChildren(
      ...ports.published.slice(0, 3).map((p) => h('div', { class: 'mono' }, p)),
      ports.published.length > 3 ? h('div', { class: 'muted' }, `+${ports.published.length - 3} more`) : '',
      !ports.published.length && ports.internal.length ? h('div', { class: 'muted' }, `${ports.internal.length} internal`) : '',
      !ports.published.length && !ports.internal.length ? h('span', { class: 'muted' }, '–') : '',
    );

    cpuText.textContent = running && c.cpu != null ? fmtCpu(c.cpu) : '–';
    // Docker reports the host's RAM as the limit when none is set: only show real limits.
    const total = hostMemTotal();
    const limited = c.memLimit && total && c.memLimit < total * 0.98;
    memText.textContent = running && c.memUsed != null ? `${fmtBytes(c.memUsed)}${limited ? ` / ${fmtBytes(c.memLimit)}` : ''}` : '–';
    memText.title = limited ? `${c.memPct?.toFixed(1)}% of its memory limit` : 'No memory limit';
    const series = sparkline(c.name);
    drawMini(cpuSpark, running ? series?.cpu : null, '--series-1');
    drawMini(memSpark, running ? series?.mem : null, '--series-2');

    restartCell.replaceChildren(
      c.looping ? h('span', { class: 'chip critical', title: 'Restarted 3+ times in the last 10 minutes' }, `loop · ${c.restartCount}`)
        : c.restartCount == null ? h('span', { class: 'muted' }, '…')
          : c.restartCount === 0 ? h('span', { class: 'muted' }, '0') : String(c.restartCount),
    );
    restartCell.title = 'Restarts since the container was created';
    netCell.textContent = running ? c.netIO : '–';
    blockCell.textContent = running ? c.blockIO : '–';
    pidsCell.textContent = running ? c.pids : '–';
    if (toggleBtn.dataset.running !== String(running)) {
      toggleBtn.dataset.running = String(running);
      const label = running ? 'Stop' : 'Start';
      toggleBtn.replaceChildren(icon(running ? 'stop' : 'play'), h('span', { class: 'lbl' }, label));
      toggleBtn.title = label;
      toggleBtn.setAttribute('aria-label', label);
      toggleBtn.classList.toggle('act-stop', running);
      toggleBtn.classList.toggle('act-start', !running);
    }
    restartBtn.hidden = !running;
    execBtn.hidden = !running;
    removeBtn.hidden = running; // running containers: right-click → "Stop and remove…"
  };
  return { tr, update };
}

// Group header row; for a compose project it carries the project actions (update, restart, stop/start…).
function projectRow(project, colspan, { onCompose, onComposeOpen }) {
  const label = h('span', { class: 'proj-label' });
  const td = h('td', { colspan }, h('div', { class: 'proj-head' }, label));
  const tr = h('tr', { class: 'project-row' }, td);
  if (!project || !onCompose) return { tr, set: (text) => { label.textContent = text; } };
  const b = (kind, iconName, text, title, onclick) => h('button', { type: 'button', class: `btn sm act act-${kind}`, title, onclick }, icon(iconName), h('span', { class: 'lbl' }, text));
  const update = b('start', 'download', 'Update', 'Pull newer images and recreate the containers that changed (docker compose pull && up -d)', () => onCompose(project, 'update'));
  const restart = b('restart', 'restart', 'Restart', 'docker compose restart', () => onCompose(project, 'restart'));
  const toggle = b('stop', 'stop', 'Stop', '', () => onCompose(project, toggle.dataset.action));
  const more = h('button', { type: 'button', class: 'btn sm act act-inspect', title: 'More project actions', 'aria-label': `More actions for ${project}` }, icon('more'));
  more.addEventListener('click', (e) => openMenu(e, more, [
    { label: 'Edit compose file', action: () => onComposeOpen(project, 'file') },
    { label: 'Open project folder', action: () => onComposeOpen(project, 'folder') },
    { label: 'Up (create missing, start all)', action: () => onCompose(project, 'up') },
    { label: 'Down (remove containers)…', danger: true, action: () => onCompose(project, 'down') },
  ]));
  td.firstChild.append(h('span', { class: 'proj-actions' }, update, restart, toggle, more));
  return {
    tr,
    set(text, members) {
      label.textContent = text;
      const anyRunning = members.some((c) => c.state === 'running');
      toggle.dataset.action = anyRunning ? 'stop' : 'start';
      toggle.className = `btn sm act act-${anyRunning ? 'stop' : 'start'}`;
      toggle.title = anyRunning ? 'docker compose stop' : 'docker compose start';
      toggle.replaceChildren(icon(anyRunning ? 'stop' : 'play'), h('span', { class: 'lbl' }, anyRunning ? 'Stop' : 'Start'));
    },
  };
}

// The Containers tab: one table with search, quick filters, grouping, sortable columns and per-row trends.
export function containersPanel(opts) {
  const prefs = loadPrefs();
  const save = () => setJSONPref('containerTable', prefs);

  const search = h('input', { type: 'search', placeholder: 'Search name, image or project…', 'aria-label': 'Search containers' });
  const filterBtns = ['all', 'running', 'stopped', 'issues'].map((key) => h('button', {
    type: 'button', class: 'tab', 'data-filter': key,
    onclick: () => { prefs.filter = key; save(); render(); },
  }));
  const groupBox = h('input', { type: 'checkbox' });
  groupBox.checked = prefs.group;
  groupBox.addEventListener('change', () => { prefs.group = groupBox.checked; save(); render(); });
  const countEl = h('span', { class: 'right' });
  const dockerErr = h('div', { class: 'card-error', hidden: true });

  const COLS = [
    ['state', 'State'], ['name', 'Name / Image'], [null, 'Health'], [null, 'Status'], [null, 'Ports'],
    ['cpu', 'CPU', '% of one core; the line shows the last 30 minutes'], ['mem', 'RAM', 'Usage; the line shows the last 30 minutes'],
    ['restarts', 'Restarts'], [null, 'Net I/O'], [null, 'Block I/O'], [null, 'PIDs'], [null, ''],
  ];
  const heads = COLS.map(([key, label, title], i) => {
    const th = h('th', { class: `${i >= 5 && i <= 10 ? 'num' : ''} ${key ? 'sortable' : ''}`, title });
    th.textContent = label;
    if (key) {
      th.dataset.key = key;
      th.addEventListener('click', () => {
        prefs.dir = prefs.sort === key ? -prefs.dir : key === 'name' ? 1 : -1;
        prefs.sort = key;
        save();
        render();
      });
    }
    return th;
  });
  const tbody = h('tbody');
  const table = h('table', { class: 'containers-table' }, h('thead', {}, h('tr', {}, heads)), tbody);
  const wrap = h('div', { class: 'table-wrap' }, table);
  // Fit the table to the panel: start from every detail, then drop the least important ones while it overflows.
  let fitQueued = false;
  function fit() {
    fitQueued = false;
    if (!wrap.clientWidth) return; // hidden tab: fitted again when it is shown (resize)
    table.classList.remove(...FIT_STEPS);
    for (const step of FIT_STEPS) {
      if (wrap.scrollWidth <= wrap.clientWidth) break;
      table.classList.add(step);
    }
  }
  const queueFit = () => {
    if (!fitQueued) { fitQueued = true; requestAnimationFrame(fit); }
  };
  new ResizeObserver(queueFit).observe(wrap);
  const el = h('section', { class: 'panel span-12' },
    h('h3', {}, 'Containers', countEl),
    h('div', { class: 'toolbar' }, search, h('div', { class: 'tabs', role: 'group', 'aria-label': 'Filter containers' }, filterBtns),
      h('label', { class: 'check' }, groupBox, 'Group by compose project')),
    dockerErr,
    wrap,
  );
  search.addEventListener('input', () => render());

  const rows = new Map();
  const projectRows = new Map();
  let lastOrder = null;
  let snap = {};

  const sparkline = (name) => {
    const ch = opts.history();
    if (!ch) return null;
    return { cpu: ch.cpu.get(name), mem: ch.mem.get(name) };
  };

  function render() {
    const d = snap.docker;
    dockerErr.hidden = !d?.error;
    dockerErr.textContent = d?.error ? `Cannot read Docker: ${d.error}` : '';
    const all = d?.containers || [];
    const counts = {
      all: all.length,
      running: all.filter((c) => c.state === 'running').length,
      stopped: all.filter((c) => c.state !== 'running').length,
      issues: all.filter(hasIssue).length,
    };
    const labels = { all: 'All', running: 'Running', stopped: 'Stopped', issues: 'Issues' };
    filterBtns.forEach((b) => {
      const key = b.dataset.filter;
      b.textContent = `${labels[key]} ${counts[key]}`;
      b.classList.toggle('active', prefs.filter === key);
      b.classList.toggle('has-issues', key === 'issues' && counts.issues > 0);
      b.setAttribute('aria-pressed', String(prefs.filter === key));
    });
    heads.forEach((th) => {
      th.classList.toggle('active', th.dataset.key === prefs.sort);
      th.dataset.arrow = th.dataset.key === prefs.sort ? (prefs.dir > 0 ? '▲' : '▼') : '';
    });
    countEl.textContent = d ? `${d.running} running / ${d.total}${counts.issues ? ` · ${counts.issues} with issues` : ''}` : '';

    const q = search.value.trim().toLowerCase();
    const keep = {
      all: () => true,
      running: (c) => c.state === 'running',
      stopped: (c) => c.state !== 'running',
      issues: hasIssue,
    }[prefs.filter] || (() => true);
    const cmp = SORTS[prefs.sort] || SORTS.name;
    const list = all
      .filter((c) => keep(c) && (!q || [c.name, c.image, c.project].some((v) => v?.toLowerCase().includes(q))))
      .sort((a, b) => {
        // Grouping keeps compose projects together (A→Z, standalone last); the column sort applies inside each group.
        if (prefs.group && a.project !== b.project) {
          if (!a.project !== !b.project) return a.project ? -1 : 1;
          return a.project.localeCompare(b.project);
        }
        return cmp(a, b) * prefs.dir || a.name.localeCompare(b.name);
      });

    const nodes = [];
    let lastProject;
    for (const c of list) {
      if (prefs.group && c.project !== lastProject) {
        lastProject = c.project;
        const key = c.project || '—';
        if (!projectRows.has(key)) projectRows.set(key, projectRow(c.project, COLS.length, opts));
        const members = list.filter((x) => x.project === c.project);
        const allMembers = all.filter((x) => x.project === c.project);
        const issues = members.filter(hasIssue).length;
        const pr = projectRows.get(key);
        pr.set(`${c.project ? `Compose: ${c.project}` : 'Standalone containers'} (${members.length})${issues ? ` · ${issues} with issues` : ''}`, allMembers);
        nodes.push(pr.tr);
      }
      if (!rows.has(c.id)) rows.set(c.id, containerRow({ ...opts, sparkline }));
      const row = rows.get(c.id);
      row.update(c);
      nodes.push(row.tr);
    }
    // Only reorder the DOM when the order changes, so focus/hover survive updates.
    const order = nodes.map((n) => n.dataset.key || (n.dataset.key = Math.random().toString(36).slice(2))).join();
    if (order !== lastOrder) {
      lastOrder = order;
      tbody.replaceChildren(...nodes);
      if (!nodes.length) {
        tbody.append(h('tr', {}, h('td', { colspan: COLS.length, class: 'empty' },
          !d ? 'Loading…' : prefs.filter === 'issues' && !q ? 'No container needs attention.' : 'No containers match.')));
      }
    }
    queueFit();
  }

  return {
    el,
    update(next) {
      snap = next;
      render();
    },
  };
}
