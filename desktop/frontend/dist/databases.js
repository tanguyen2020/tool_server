// Backup & Restore tab: copy a PostgreSQL database to another server, back it up to this computer, restore a backup.
// The work runs in the database containers (pg_dump / pg_restore in parallel); this page only follows it.
// Header with the figures of the day; left: the new-job form in numbered steps; right: live jobs, then the history.
import { h, toast, fmtBytes } from './util.js';
import { call, on, confirmDialog } from './bridge.js';
import { modal, fmtDateTime } from './ui.js';
import { icon } from './icons.js';
import { combo } from './combo.js';

export const TAB_TITLE = 'Postgres Backup & Restore';

const jobs = new Map(); // id -> latest view
const listeners = new Set();
let loaded = false;
let stripRefresh = () => {};

on('pgjob', (v) => {
  const was = jobs.get(v.id)?.state;
  jobs.set(v.id, v);
  listeners.forEach((fn) => fn(v, was !== v.state));
  updateBadge();
  if (was !== v.state) stripRefresh();
});

async function loadJobs() {
  if (loaded) return;
  try {
    for (const v of await call('PgJobs')) if (!jobs.has(v.id)) jobs.set(v.id, v);
    loaded = true;
  } catch (err) {
    toast(`Backup jobs: ${err.message}`, true);
  }
}

const PG_IMAGE = /postgres|postgis|timescale|pgvector|paradedb|supabase\/postgres|pgautoupgrade|citus|bitnami\/postgresql/i;
export const isPostgresImage = (image) => PG_IMAGE.test(image || '');

const KINDS = {
  copy: { title: 'Copy', label: 'Copy to another server', icon: 'forward', desc: 'Move a database from one server to another' },
  backup: { title: 'Back up', label: 'Back up to a file', icon: 'download', desc: 'Save a compressed backup on this computer' },
  restore: { title: 'Restore', label: 'Restore a backup', icon: 'upload', desc: 'Put a backup file back onto a server' },
};
const STATE = {
  running: ['info', 'In progress'], queued: ['neutral', 'Queued'], done: ['good', 'Completed'],
  warning: ['warning', 'Completed with warnings'], failed: ['critical', 'Failed'], cancelled: ['neutral', 'Cancelled'],
};
const STEP = { prepare: 'Prepare', dump: 'Back up', transfer: 'Transfer', restore: 'Restore', indexes: 'Indexes', finish: 'Finish' };
const DOING = { prepare: 'Preparing', dump: 'Backing up', transfer: 'Transferring', restore: 'Restoring data', indexes: 'Building indexes', finish: 'Finishing' };
const versionText = (v) => (v >= 100000 ? `${Math.floor(v / 10000)}.${v % 10000}` : v > 0 ? `${Math.floor(v / 10000)}.${Math.floor(v / 100) % 100}` : '?');
const baseName = (p) => (p || '').split(/[\\/]/).pop();
const plural = (n, one, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
const pill = (tone, text) => h('span', { class: `bk-pill ${tone}` }, text);

function fmtDuration(sec) {
  if (sec == null || sec < 0 || !Number.isFinite(sec)) return '–';
  sec = Math.round(sec);
  const hr = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (hr) return `${hr}h ${String(m).padStart(2, '0')}m`;
  if (m) return `${m}m ${String(s).padStart(2, '0')}s`;
  return `${s}s`;
}

// "just now", "12 min ago", "today 14:03", "yesterday 09:10", else the date.
function fmtWhen(ms) {
  if (!ms) return '–';
  const d = new Date(ms);
  const diff = (Date.now() - ms) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)} min ago`;
  const hm = d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' });
  const day = new Date(); day.setHours(0, 0, 0, 0);
  if (ms >= day.getTime()) return `today ${hm}`;
  if (ms >= day.getTime() - 86400000) return `yesterday ${hm}`;
  return fmtDateTime(ms);
}

// What a job moves: name (database), and the route between the two ends.
const jobName = (v) => {
  const s = v.spec;
  if (s.kind === 'restore') return s.target.database;
  if (s.kind === 'copy' && s.target.database !== s.source.database) return `${s.source.database} → ${s.target.database}`;
  return s.source.database;
};
const jobRoute = (v) => {
  const s = v.spec;
  if (s.kind === 'copy') return [s.source.server, s.target.server];
  if (s.kind === 'backup') return [s.source.server, 'This computer'];
  return [baseName(s.file), s.target.server];
};

// ---------------------------------------------------------------- top bar button and tab strip

let badgeBtn = null;
export function initBackupButton(btn, open, refreshStrip) {
  badgeBtn = btn;
  stripRefresh = refreshStrip;
  btn.addEventListener('click', () => open());
  loadJobs().then(updateBadge);
}

export function runningSummary() {
  const running = [...jobs.values()].filter((v) => v.state === 'running');
  if (!running.length) return null;
  return { count: running.length, percent: running.reduce((s, v) => s + v.percent, 0) / running.length };
}

function updateBadge() {
  if (!badgeBtn) return;
  const r = runningSummary();
  let badge = badgeBtn.querySelector('.pg-badge');
  if (!r) { badge?.remove(); badgeBtn.title = 'Back up, restore and copy PostgreSQL databases'; return; }
  if (!badge) { badge = h('span', { class: 'pg-badge' }); badgeBtn.append(badge); }
  badge.textContent = r.count > 1 ? `${r.count} · ${r.percent.toFixed(0)}%` : `${r.percent.toFixed(0)}%`;
  badgeBtn.title = `${plural(r.count, 'job')} in progress`;
}

// ---------------------------------------------------------------- the tab

let current = null; // the mounted page

// Shows the new-job form with a server / container chosen (from a container's menu), or a job to run again.
export function backupNewJob(preset) {
  current?.newJob(preset);
}

export function backupServersChanged() {
  current?.serversChanged();
}

// The page of the tab. getServers / getSnaps return the app's current maps.
export function backupView(root, { getServers, getSnaps }) {
  const stat = (label, iconName, tone) => {
    const value = h('div', { class: 'bk-stat-value' }, '0');
    const sub = h('div', { class: 'bk-stat-sub' });
    return { value, sub, el: h('div', { class: `bk-stat t-${tone}` }, h('span', { class: 'bk-stat-icon' }, icon(iconName)), h('div', { class: 'bk-stat-body' }, h('div', { class: 'bk-stat-label' }, label), value, sub)) };
  };
  const stRunning = stat('In progress', 'clock', 'blue');
  const stQueued = stat('Queued', 'more', 'gray');
  const stDone = stat('Completed today', 'check', 'green');
  const stData = stat('Data moved today', 'disk', 'violet');
  const formHost = h('div', { class: 'bk-form-host' });
  const liveEl = h('div', { class: 'bk-live' });
  const liveEmpty = h('div', { class: 'bk-live-empty' }, icon('check'), h('span', {}, 'Nothing is running. Set up a job on the left; its progress shows here.'));
  const historyEl = h('div', { class: 'bk-history' });
  const historyEmpty = h('div', { class: 'bk-empty' },
    h('div', { class: 'bk-empty-art' }, icon('disk')),
    h('div', { class: 'bk-empty-title' }, 'No jobs yet'),
    h('div', { class: 'bk-empty-text' }, 'Copies, backups and restores you run are listed here, with their size, duration and result.'));
  const clearBtn = h('button', { type: 'button', class: 'btn ghost sm' }, icon('trash'), 'Clear history');
  const liveCount = h('span', { class: 'bk-count' });
  const historyCount = h('span', { class: 'bk-count' });
  root.append(h('div', { class: 'bk-page' },
    h('header', { class: 'bk-hero' },
      h('span', { class: 'bk-hero-icon' }, icon('disk')),
      h('div', { class: 'bk-hero-text' },
        h('h2', {}, TAB_TITLE),
        h('p', {}, 'Copy, back up and restore PostgreSQL databases running in Docker, with live progress.'))),
    h('div', { class: 'bk-stats' }, stRunning.el, stQueued.el, stDone.el, stData.el),
    h('div', { class: 'bk-layout' },
      h('section', { class: 'bk-panel bk-new' },
        h('div', { class: 'bk-panel-head' }, h('h3', {}, 'New job'), h('span', { class: 'bk-panel-sub' }, 'Copy, back up or restore')),
        formHost),
      h('div', { class: 'bk-activity' },
        h('section', { class: 'bk-section' },
          h('div', { class: 'bk-section-head' }, h('h3', {}, 'In progress'), liveCount),
          liveEl, liveEmpty),
        h('section', { class: 'bk-section' },
          h('div', { class: 'bk-section-head' }, h('h3', {}, 'History'), historyCount, h('span', { class: 'spacer' }), clearBtn),
          historyEl, historyEmpty)))));

  const instances = new Map(); // serverId -> Promise<Instance[]>
  const loadInstances = (id, fresh = false) => {
    if (fresh || !instances.has(id)) {
      const p = call('PgInstances', id);
      p.catch(() => instances.delete(id));
      instances.set(id, p);
    }
    return instances.get(id);
  };

  let form = null;
  function newJob(preset = {}) {
    form = newJobForm({ servers: getServers(), snaps: getSnaps(), loadInstances, preset, onStarted: (ids) => { if (ids.length === 1) expandNext.add(ids[0]); } });
    formHost.replaceChildren(form.el);
  }

  const cards = new Map(); // id -> card
  const expandNext = new Set(); // a single job started from this page opens its details
  function cardFor(v) {
    let c = cards.get(v.id);
    if (!c) {
      c = jobCard(v, {
        expanded: expandNext.delete(v.id),
        onAgain: (spec) => { newJob({ spec }); formHost.scrollIntoView({ behavior: 'smooth', block: 'start' }); },
        onForget: async (job) => {
          await call('PgForget', job.id);
          jobs.delete(job.id);
          cards.get(job.id)?.el.remove();
          cards.delete(job.id);
          layout();
        },
      });
      cards.set(v.id, c);
    }
    return c;
  }

  const isLive = (v) => v.state === 'running' || v.state === 'queued';
  // Running first, then the queue in the order it starts; the history newest first.
  function layout() {
    const list = [...jobs.values()].sort((a, b) => b.started - a.started);
    const running = list.filter((v) => v.state === 'running');
    const queued = list.filter((v) => v.state === 'queued').reverse();
    const finished = list.filter((v) => !isLive(v));
    liveEl.replaceChildren(...[...running, ...queued].map((v) => cardFor(v).el));
    historyEl.replaceChildren(...finished.map((v) => cardFor(v).el));
    liveEmpty.hidden = running.length + queued.length > 0;
    historyEmpty.hidden = finished.length > 0;
    clearBtn.hidden = !finished.length;
    liveCount.textContent = String(running.length + queued.length);
    historyCount.textContent = String(finished.length);
    stats();
  }

  function stats() {
    const today = new Date(); today.setHours(0, 0, 0, 0);
    const list = [...jobs.values()];
    const running = list.filter((v) => v.state === 'running');
    const doneToday = list.filter((v) => (v.state === 'done' || v.state === 'warning') && v.ended >= today.getTime());
    const failedToday = list.filter((v) => v.state === 'failed' && v.ended >= today.getTime()).length;
    stRunning.value.textContent = String(running.length);
    stRunning.sub.textContent = running.length ? `${(running.reduce((s, v) => s + v.percent, 0) / running.length).toFixed(0)}% on average` : 'idle';
    const q = list.filter((v) => v.state === 'queued').length;
    stQueued.value.textContent = String(q);
    stQueued.sub.textContent = q ? 'waiting for a free slot' : 'none waiting';
    stDone.value.textContent = String(doneToday.length);
    stDone.sub.textContent = failedToday ? `${failedToday} failed` : 'no failures';
    stDone.sub.classList.toggle('bad', failedToday > 0);
    const bytes = doneToday.reduce((s, v) => s + (v.summary?.databaseBytes || v.summary?.backupBytes || 0), 0);
    stData.value.textContent = bytes ? fmtBytes(bytes) : '0';
    stData.sub.textContent = `${plural(doneToday.length, 'database')}`;
  }

  const onJob = (v, stateChanged) => {
    const isNew = !cards.has(v.id);
    cardFor(v).update(v);
    if (isNew || stateChanged) layout();
    else if (v.state === 'running') stats();
  };

  clearBtn.addEventListener('click', async () => {
    const done = [...jobs.values()].filter((v) => !isLive(v));
    if (!done.length || !(await confirmDialog(`Clear the history of ${plural(done.length, 'job')}?\n\nOnly the list is cleared: backup files and databases are not touched.`, 'Remove finished jobs'))) return;
    for (const v of done) {
      await call('PgForget', v.id).catch(() => {});
      jobs.delete(v.id);
      cards.get(v.id)?.el.remove();
      cards.delete(v.id);
    }
    layout();
  });

  listeners.add(onJob);
  current = {
    newJob,
    serversChanged: () => form?.serversChanged(getServers(), getSnaps()),
  };
  newJob();
  loadJobs().then(() => { for (const v of jobs.values()) cardFor(v).update(v); layout(); });
  layout();
  const tick = setInterval(stats, 60000); // "today" moves on

  return {
    update() {}, // server snapshots do not concern this page
    destroy() {
      clearInterval(tick);
      listeners.delete(onJob);
      current = null;
    },
  };
}

// ---------------------------------------------------------------- small controls

// A switch with a label and a line of help.
function toggle(label, help = '', checked = false) {
  const input = h('input', { type: 'checkbox', role: 'switch', checked });
  const el = h('label', { class: 'bk-toggle' }, input, h('span', { class: 'bk-switch', 'aria-hidden': 'true' }),
    h('span', { class: 'bk-toggle-text' }, h('span', { class: 'bk-toggle-label' }, label), help ? h('span', { class: 'bk-toggle-help' }, help) : ''));
  return { el, input };
}

// A row of buttons where one is chosen.
function segmented(options, value, onChange) {
  const el = h('div', { class: 'bk-seg', role: 'radiogroup' });
  const render = () => el.replaceChildren(...options.map((o) => h('button', {
    type: 'button', role: 'radio', 'aria-checked': String(o.value === value), class: `bk-seg-btn${o.value === value ? ' on' : ''}`, title: o.title || null,
    onclick: () => { if (o.disabled) return; value = o.value; render(); onChange(value); }, disabled: o.disabled || undefined,
  }, o.icon ? icon(o.icon) : '', h('span', {}, o.label), o.badge ? h('span', { class: 'bk-seg-badge' }, o.badge) : '')));
  render();
  return { el, get value() { return value; }, set(v) { value = v; render(); }, setOptions(next) { options = next; render(); } };
}

// Clicking or tabbing into a name field selects it, so typing replaces the suggested name at once.
function selectOnFocus(input) {
  let byMouse = false;
  input.addEventListener('mousedown', () => { byMouse = document.activeElement !== input; });
  input.addEventListener('focus', () => input.select());
  input.addEventListener('mouseup', (e) => { if (byMouse) { e.preventDefault(); byMouse = false; } }); // keep the selection the click would undo
}

// − [n] + within min..max.
function stepper(value, min, max, onChange) {
  const input = h('input', { type: 'number', min, max, step: 1, value, class: 'bk-stepper-in', 'aria-label': 'Number' });
  const clamp = (n) => Math.max(min, Math.min(max, Math.round(Number(n) || min)));
  const set = (n) => { input.value = clamp(n); onChange(Number(input.value)); };
  const el = h('div', { class: 'bk-stepper' },
    h('button', { type: 'button', class: 'bk-stepper-btn', 'aria-label': 'Fewer', onclick: () => set(Number(input.value) - 1) }, '−'),
    input,
    h('button', { type: 'button', class: 'bk-stepper-btn', 'aria-label': 'More', onclick: () => set(Number(input.value) + 1) }, '+'));
  input.addEventListener('change', () => set(input.value));
  return { el, input, get value() { return clamp(input.value); }, set };
}

// ---------------------------------------------------------------- new job

function newJobForm({ servers, snaps, loadInstances, preset, onStarted }) {
  const spec0 = preset.spec || null;
  let kind = spec0?.kind || preset.kind || 'copy';
  const el = h('div', { class: 'bk-form' });

  const source = endpointPicker({ role: 'source', servers, snaps, loadInstances,
    preset: spec0 ? spec0.source : preset.serverId ? { serverId: preset.serverId, container: preset.container } : null });
  const target = endpointPicker({ role: 'target', servers, snaps, loadInstances, preset: spec0 ? spec0.target : null });

  // Step 1: the kind of job, as three choice cards.
  const kindCards = h('div', { class: 'bk-kinds', role: 'radiogroup' });
  function renderKinds() {
    kindCards.replaceChildren(...Object.entries(KINDS).map(([k, d]) => h('button', {
      type: 'button', role: 'radio', 'aria-checked': String(k === kind), class: `bk-kind k-${k}${k === kind ? ' on' : ''}`,
      onclick: () => { if (k === kind) return; kind = k; setFile(''); files = []; fileInfoEl.replaceChildren(); keepCopy.input.checked = false; checksEl.replaceChildren(); renderKind(); },
    }, h('span', { class: 'bk-kind-icon' }, icon(d.icon)), h('span', { class: 'bk-kind-text' }, h('span', { class: 'bk-kind-title' }, d.title), h('span', { class: 'bk-kind-desc' }, d.desc)))));
  }

  // Files: the backup to restore, or where backups are saved.
  let file = ''; // asked every time, even for "Run again": a backup must not overwrite the last one by accident
  const filePath = h('div', { class: 'bk-file-path' });
  const fileInfoEl = h('div', { class: 'bk-file-info' });
  const chooseBtn = h('button', { type: 'button', class: 'btn' }, icon('folder'), 'Choose…');
  const fileBox = h('div', { class: 'bk-file' }, h('span', { class: 'bk-file-icon' }, icon('file')), filePath, chooseBtn);
  const keepCopy = toggle('Also keep a backup file on this computer', 'A copy of the transferred backup is saved as it goes by');
  const fileArea = h('div', { class: 'bk-file-area' }, fileBox, fileInfoEl);

  let files = []; // restore: the chosen backups ({ path, bytes, format, meta })
  const setFile = (p) => {
    file = p || '';
    paintFile();
  };
  // The name a backup is restored as by default: the database it was made from.
  const defaultName = (b) => (b.meta?.database || baseName(b.path).replace(/\.(pgbackup\.tar|tar|dump|backup)$/i, '').replace(/_\d{8}-\d{4}$/, '').replace(/[^A-Za-z0-9_]/g, '_')).slice(0, 50);
  function paintFiles() {
    const total = files.reduce((t, b) => t + (b.bytes || 0), 0);
    filePath.replaceChildren(files.length
      ? h('span', { class: 'bk-file-name' }, files.length === 1 ? baseName(files[0].path) : `${files.length} backup files`)
      : h('span', { class: 'bk-file-name muted' }, 'No backup chosen'),
    files.length ? h('span', { class: 'bk-file-dir' }, files.length === 1 ? files[0].path : `${fmtBytes(total)} in total`) : '');
    chooseBtn.lastChild.textContent = files.length ? 'Add files…' : 'Choose backups…';
    fileInfoEl.replaceChildren(...(files.length ? [h('div', { class: 'bk-files' }, ...files.map((b) => {
      const mt = b.meta;
      const sub = [baseName(b.path), fmtBytes(b.bytes), mt && `from ${mt.server}`, mt && fmtWhen(mt.created), !mt && (b.format === 'custom' ? 'pg_dump archive' : 'pg_dump folder')].filter(Boolean).join(' · ');
      return h('div', { class: 'bk-files-row' },
        h('span', { class: 'bk-files-icon' }, icon('file')),
        h('div', { class: 'bk-files-text' }, h('span', { class: 'bk-files-name' }, mt?.database || baseName(b.path)), h('span', { class: 'bk-files-sub', title: b.path }, sub)),
        h('button', { type: 'button', class: 'btn ghost icon sm', title: 'Remove', 'aria-label': `Remove ${baseName(b.path)}`, onclick: () => { files = files.filter((x) => x !== b); paintFiles(); syncTargets(); updateSummary(); } }, icon('close')));
    }))] : []));
  }
  function paintFile() {
    if (kind === 'restore') { paintFiles(); return; }
    const multi = multiFile();
    const empty = kind === 'restore' ? 'No backup chosen' : multi ? 'No folder chosen' : 'No file chosen';
    filePath.replaceChildren(file ? h('span', { class: 'bk-file-name' }, baseName(file)) : h('span', { class: 'bk-file-name muted' }, empty),
      file ? h('span', { class: 'bk-file-dir' }, file) : '');
    chooseBtn.lastChild.textContent = file ? 'Change…' : kind === 'restore' ? 'Choose a backup…' : multi ? 'Choose a folder…' : 'Choose where to save…';
  }
  chooseBtn.addEventListener('click', async () => {
    try {
      if (kind === 'restore') {
        const got = await call('PgChooseBackupFiles');
        if (!got?.length) return;
        const bad = got.filter((b) => b.error);
        if (bad.length) toast(`Cannot restore ${bad.map((b) => `${baseName(b.path)}: ${b.error}`).join(' · ')}`, true);
        const known = new Set(files.map((b) => b.path));
        files = [...files, ...got.filter((b) => !b.error && !known.has(b.path))];
        paintFiles();
        syncTargets();
        updateSummary();
      } else {
        const sv = source.value();
        const p = multiFile() ? await call('PgChooseFolder', 'Folder for the backup files')
          : await call('PgChooseSaveFile', source.serverName() || 'server', sv.database || 'database');
        if (!p) { if (kind === 'copy' && !file) keepCopy.input.checked = false; syncFile(); return; }
        setFile(p);
        if (kind === 'copy') keepCopy.input.checked = true;
        syncFile();
        syncFileMode();
      }
    } catch (err) {
      toast(err.message, true);
    }
  });
  keepCopy.input.addEventListener('change', () => {
    if (keepCopy.input.checked && !file) chooseBtn.click();
    syncFile();
  });
  function syncFile() {
    fileArea.hidden = kind === 'copy' && (!keepCopy.input.checked || directOn);
    updateSummary();
  }

  // Transfer (copy): directly between the servers when the target can reach the source, else through this computer.
  let directOn = !!spec0?.direct;
  let directTouched = !!spec0; // the user decided (or runs a job again): detection no longer changes it
  let detectSeq = 0;
  let detectTimer = null;
  let detectKey = '';
  let found = null; // address the detection found
  let reachable = null; // null unknown, true / false after detection
  const modeSeg = segmented([
    { value: true, label: 'Direct', icon: 'zap', title: 'The destination server reads the source over the network' },
    { value: false, label: 'Through this computer', icon: 'forward', title: 'The data passes through this computer' },
  ], directOn, (v) => { directOn = v; directTouched = true; syncDirect(); });
  const directStatus = h('div', { class: 'bk-status' });
  const directHost = h('input', { spellcheck: 'false', value: spec0?.directHost || '', 'aria-label': 'Source address as seen from the destination' });
  const directPort = h('input', { inputmode: 'numeric', value: spec0?.directPort || '', placeholder: 'auto', 'aria-label': 'Source port' });
  const connBox = h('details', { class: 'bk-disclosure bk-conn' }, h('summary', {}, 'Connection settings'),
    h('div', { class: 'bk-conn-grid' },
      h('label', { class: 'bk-field' }, h('span', { class: 'bk-label' }, 'Source address, as the destination sees it'), directHost),
      h('label', { class: 'bk-field' }, h('span', { class: 'bk-label' }, 'Port'), directPort)),
    h('p', { class: 'bk-help' }, 'Leave empty to find them automatically. The source must accept connections from the destination.'));
  const transferArea = h('div', { class: 'bk-transfer' }, modeSeg.el, directStatus, connBox, keepCopy.el);

  const status = (tone, iconName, text, detail = '') => directStatus.replaceChildren(h('div', { class: `bk-status-line ${tone}`, title: detail || null },
    iconName === 'spin' ? h('span', { class: 'spinner', 'aria-hidden': 'true' }) : icon(iconName), h('span', {}, text)));
  function scheduleDetect() {
    if (kind !== 'copy') return;
    const sv = source.value();
    const tv = target.value();
    const ready = sv.serverId && sv.container && sv.database && tv.serverId && tv.container && !(sv.serverId === tv.serverId && sv.container === tv.container);
    const key = ready ? [sv.serverId, sv.container, sv.database, tv.serverId, tv.container, directHost.value.trim(), directPort.value.trim()].join('|') : '';
    if (key === detectKey) return;
    detectKey = key;
    found = null;
    reachable = null;
    clearTimeout(detectTimer);
    detectSeq++;
    if (!ready) { directStatus.replaceChildren(); syncDirect(); return; }
    const seq = detectSeq;
    status('busy', 'spin', `Checking whether ${target.serverName()} can reach ${source.serverName()}…`);
    detectTimer = setTimeout(async () => {
      let r;
      try {
        r = await call('PgDetectDirect', { kind: 'copy', source: sv, target: { ...tv, database: tv.database || sv.database },
          directHost: directHost.value.trim(), directPort: Number(directPort.value) || 0 });
      } catch (err) {
        r = { ok: false, message: err.message };
      }
      if (seq !== detectSeq) return; // the choice changed meanwhile
      reachable = r.ok;
      found = r.ok ? r.address : null;
      if (!directTouched) { directOn = r.ok; modeSeg.set(directOn); }
      syncDirect(r.message);
    }, 400);
  }
  directHost.addEventListener('change', scheduleDetect);
  directPort.addEventListener('change', scheduleDetect);
  function syncDirect(message = '') {
    connBox.hidden = !directOn;
    keepCopy.el.hidden = kind !== 'copy' || directOn;
    syncFile();
    modeSeg.setOptions([
      { value: true, label: 'Direct', icon: 'zap', badge: reachable ? 'Fastest' : '', title: 'The destination server reads the source over the network' },
      { value: false, label: 'Through this computer', icon: 'forward', title: 'The data passes through this computer' },
    ]);
    const host = servers.get(source.value().serverId)?.host;
    directHost.placeholder = found ? `${found.host} (found automatically)` : host || 'IP or host name';
    directPort.placeholder = found ? String(found.port) : 'auto';
    if (reachable === true) {
      status('good', 'check', directOn ? `Direct connection ready: ${target.serverName()} reads the source at ${found.host}:${found.port}` : 'A direct connection is available and would be faster', message);
    } else if (reachable === false) {
      status(directOn ? 'bad' : 'muted', 'alert', directOn ? 'The destination cannot reach the source directly: this job would fail' : 'The servers cannot reach each other, so the data passes through this computer', message);
    }
    updateSummary();
  }

  // Advanced options
  const compSel = h('select', { class: 'bk-select' },
    h('option', { value: 'auto' }, 'Automatic (recommended)'),
    h('option', { value: 'zstd' }, 'Smaller files (zstd)'),
    h('option', { value: 'lz4' }, 'Fastest (lz4)'),
    h('option', { value: 'gzip' }, 'Most compatible (gzip)'),
    h('option', { value: 'none' }, 'No compression'));
  const exclude = h('input', { class: 'bk-input', placeholder: 'e.g. public.audit_log, logs.*', spellcheck: 'false' });
  const analyze = toggle('Optimize after restore', 'Refresh query statistics so the database is fast right away', true);
  const verify = toggle('Verify row counts', 'Compare every table on both servers once the copy is done');
  const owners = toggle('Keep owners and permissions', 'The same roles must exist on the destination');
  const schemaOnly = toggle('Structure only', 'Copy tables, views and functions without their rows');
  const optCompression = h('label', { class: 'bk-field' }, h('span', { class: 'bk-label' }, 'Compression'), compSel);
  const optExclude = h('label', { class: 'bk-field' }, h('span', { class: 'bk-label' }, 'Skip the rows of these tables'), exclude);
  const advanced = h('details', { class: 'bk-disclosure bk-adv' }, h('summary', {}, icon('cog'), 'Advanced options'),
    h('div', { class: 'bk-adv-body' },
      h('div', { class: 'bk-adv-fields' }, optCompression, optExclude),
      h('div', { class: 'bk-toggles' }, analyze.el, verify.el, owners.el, schemaOnly.el)));
  if (spec0) {
    compSel.value = spec0.compression || 'auto';
    analyze.input.checked = spec0.analyze;
    verify.input.checked = spec0.verify;
    owners.input.checked = spec0.keepOwners;
    schemaOnly.input.checked = spec0.schemaOnly;
    exclude.value = (spec0.excludeData || []).join(', ');
  }

  // One page: numbered sections, then Check / Start at the bottom.
  const checksEl = h('div', { class: 'bk-checks' });
  const summaryEl = h('div', { class: 'bk-summary' });
  const startBtn = h('button', { type: 'button', class: 'btn primary bk-go' }, icon('play'), 'Start');
  const checkBtn = h('button', { type: 'button', class: 'btn bk-check-btn' }, icon('check'), 'Check');
  const steps = h('div', { class: 'bk-steps-form' });
  const step = (n, title, ...content) => h('section', { class: 'bk-fstep' },
    h('div', { class: 'bk-fstep-head' }, h('span', { class: 'bk-num' }, String(n)), h('span', { class: 'bk-fstep-title' }, title)), ...content);

  function renderPage() {
    const parts = [step(1, 'What do you want to do?', kindCards)];
    if (kind === 'copy') parts.push(step(2, 'Source', source.el), step(3, 'Destination', target.el), step(4, 'Transfer', transferArea, fileArea));
    else if (kind === 'backup') parts.push(step(2, 'Source', source.el), step(3, 'Save to', fileArea));
    else parts.push(step(2, 'Backup files', fileArea), step(3, 'Destination', target.el));
    steps.replaceChildren(...parts);
  }

  function renderKind() {
    renderKinds();
    syncFile();
    syncFileMode();
    syncTargets();
    verify.el.hidden = kind !== 'copy';
    analyze.el.hidden = owners.el.hidden = kind === 'backup';
    schemaOnly.el.hidden = optExclude.hidden = optCompression.hidden = kind === 'restore';
    renderPage();
    syncDirect();
    detectKey = '';
    scheduleDetect();
    updateSummary();
  }

  // Several databases: their backup files go into one folder, one file each.
  const multiFile = () => kind !== 'restore' && source.value().databases.length > 1;
  let lastMulti = false;
  function syncFileMode() {
    const multi = multiFile();
    if (multi !== lastMulti) { // a file name and a folder do not mix
      lastMulti = multi;
      file = '';
      fileInfoEl.replaceChildren();
      if (kind === 'copy') keepCopy.input.checked = false;
      syncFile();
    }
    keepCopy.el.querySelector('.bk-toggle-label').textContent = multi ? 'Also keep the backup files on this computer' : 'Also keep a backup file on this computer';
    if (multi && file && kind !== 'restore') fileInfoEl.replaceChildren(h('div', { class: 'bk-help' }, 'One file per database, named after the database, the server and the date.'));
    paintFile();
  }

  // The destination names follow the source databases until they are edited; a copy within one container gets "_copy".
  function syncTargets() {
    const sv = source.value();
    const tv = target.value();
    const same = sv.serverId && sv.serverId === tv.serverId && sv.container === tv.container;
    if (kind === 'restore') {
      target.setSources(files.map((b) => b.path), '', {
        labels: new Map(files.map((b) => [b.path, b.meta?.database ? `${b.meta.database} · ${baseName(b.path)}` : baseName(b.path)])),
        defaults: new Map(files.map((b) => [b.path, defaultName(b)])),
        head: 'Backup file',
      });
      return;
    }
    target.setSources(kind === 'copy' ? sv.databases : [], same ? '_copy' : '');
  }
  source.onDatabase(() => { syncTargets(); syncFileMode(); syncDirect(); });
  source.onChange(() => { scheduleDetect(); updateSummary(); });
  target.onChange(() => { scheduleDetect(); syncTargets(); updateSummary(); });

  // One line above the buttons says what is about to happen.
  function updateSummary() {
    const sv = source.value();
    const n = sv.databases.length;
    const what = kind === 'restore' ? (files.length > 1 ? plural(files.length, 'backup') : files.length ? baseName(files[0].path) : 'a backup') : n ? (n === 1 ? sv.databases[0] : plural(n, 'database')) : 'a database';
    const bytes = kind === 'restore' ? files.reduce((t, b) => t + (b.bytes || 0), 0) : source.selectedBytes();
    const size = bytes > 0 ? ` (${fmtBytes(bytes)})` : '';
    let text;
    if (kind === 'copy') text = `Copy ${what}${size}${sv.serverId ? ` from ${source.serverName()}` : ''}${target.value().serverId ? ` to ${target.serverName()}` : ''}${target.value().serverId && sv.serverId ? (directOn ? ' · direct' : ' · through this computer') : ''}`;
    else if (kind === 'backup') text = `Back up ${what}${size}${sv.serverId ? ` from ${source.serverName()}` : ''} to this computer`;
    else text = `Restore ${what}${size}${target.value().serverId ? ` to ${target.serverName()}` : ''}`;
    summaryEl.textContent = text;
    startBtn.lastChild.textContent = kind === 'copy' ? (n > 1 ? `Start ${n} copies` : 'Start copy') : kind === 'backup' ? (n > 1 ? `Back up ${n} databases` : 'Start backup') : files.length > 1 ? `Restore ${files.length} backups` : 'Start restore';
  }

  // The jobs to start: one per database. Files are named once everything else is valid.
  function collect() {
    const sv = source.value();
    const tv = target.value();
    const base = {
      kind,
      direct: kind === 'copy' && directOn,
      directHost: directHost.value.trim(),
      directPort: Number(directPort.value) || 0,
      ifExists: target.ifExists(),
      jobs: 0,
      restoreJobs: 0,
      compression: compSel.value,
      keepOwners: owners.input.checked,
      schemaOnly: schemaOnly.input.checked,
      excludeData: exclude.value.split(',').map((s) => s.trim()).filter(Boolean),
      analyze: analyze.input.checked,
      verify: verify.input.checked,
    };
    const src = { serverId: sv.serverId, container: sv.container };
    const dst = { serverId: tv.serverId, container: tv.container };
    if (kind === 'restore') return target.targets().map((t) => ({ ...base, source: {}, target: { ...dst, database: t.target }, file: t.source }));
    const pairs = kind === 'copy' ? target.targets() : sv.databases.map((d) => ({ source: d, target: '' }));
    return pairs.map((p) => ({ ...base, source: { ...src, database: p.source }, target: kind === 'copy' ? { ...dst, database: p.target } : {}, file: '' }));
  }
  const wantsFile = () => kind === 'backup' || (kind === 'copy' && keepCopy.input.checked && !directOn);

  function missing(list) {
    const s = list[0];
    if (kind !== 'restore' && (!s.source.serverId || !s.source.container || !s.source.database)) return 'Choose the source server and at least one database';
    if (kind !== 'backup' && (!s.target.serverId || !s.target.container)) return 'Choose the destination server';
    if (kind !== 'backup') {
      const empty = list.find((x) => !x.target.database);
      if (empty) return list.length > 1 ? `Name the destination database for ${empty.source.database}` : 'Name the destination database';
      const seen = new Set();
      for (const x of list) {
        if (seen.has(x.target.database)) return `Two databases would be named ${x.target.database}: give them different names`;
        seen.add(x.target.database);
        if (kind === 'copy' && x.source.serverId === x.target.serverId && x.source.container === x.target.container && x.source.database === x.target.database) {
          return `${x.source.database} would be copied onto itself: give the copy another name`;
        }
      }
    }
    if (kind === 'restore' && !files.length) return 'Choose the backup files to restore';
    if (wantsFile() && !file) return multiFile() ? 'Choose a folder for the backup files' : kind === 'backup' ? 'Choose where to save the backup' : 'Choose where to keep the backup file';
    const port = Number(directPort.value) || 0;
    if (kind === 'copy' && directOn && directPort.value.trim() && !(port > 0 && port < 65536)) return 'Enter a port between 1 and 65535';
    return '';
  }

  // Runs fn on the items, at most n at a time.
  async function pool(items, n, fn) {
    const out = new Array(items.length);
    let next = 0;
    await Promise.all(Array.from({ length: Math.min(n, items.length) }, async () => {
      while (next < items.length) {
        const i = next++;
        out[i] = await fn(items[i], i);
      }
    }));
    return out;
  }

  const checkRow = (level, text, title = '') => h('div', { class: `bk-check ${level}` }, icon(level === 'ok' ? 'check' : 'alert'), h('div', {}, title ? h('strong', {}, title) : '', title ? ' ' : '', text));
  const label = (s) => (kind === 'copy' ? (s.source.database === s.target.database ? s.source.database : `${s.source.database} → ${s.target.database}`)
    : kind === 'backup' ? s.source.database : s.target.database);

  // Checks every job. Resolves to [{ s, pf }] or null.
  async function preflight() {
    const list = collect();
    const miss = missing(list);
    if (miss) { checksEl.replaceChildren(checkRow('error', miss)); return null; }
    if (wantsFile()) {
      for (const s of list) s.file = multiFile() ? await call('PgBackupFileIn', file, source.serverName(), s.source.database) : file;
    }
    const busy = h('div', { class: 'bk-check busy' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), h('div', {}, list.length > 1 ? `Checking ${list.length} databases…` : 'Checking the servers…'));
    checksEl.replaceChildren(busy);
    let done = 0;
    const results = await pool(list, 3, async (s) => {
      let pf;
      try {
        pf = await call('PgPreflight', s);
      } catch (err) {
        pf = { ok: false, checks: [{ level: 'error', text: err.message }] };
      }
      done++;
      if (list.length > 1) busy.lastChild.textContent = `Checking ${list.length} databases… ${done} done`;
      return { s, pf };
    });
    // What needs attention is spelled out; a clean result is one line.
    const rows = [];
    for (const { s, pf } of results) {
      const notes = pf.checks.filter((c) => c.level !== 'ok');
      if (!notes.length) continue;
      for (const c of notes) rows.push(checkRow(c.level, c.text, list.length > 1 ? label(s) : ''));
    }
    const allOk = results.every((r) => r.pf.ok);
    if (allOk) rows.unshift(checkRow('ok', list.length > 1 ? `All ${list.length} databases are ready` : 'Everything is ready'));
    checksEl.replaceChildren(...rows);
    return results;
  }

  checkBtn.addEventListener('click', async () => {
    checkBtn.disabled = startBtn.disabled = true;
    await preflight();
    checkBtn.disabled = startBtn.disabled = false;
  });
  startBtn.addEventListener('click', async () => {
    checkBtn.disabled = startBtn.disabled = true;
    try {
      const items = await preflight();
      if (!items || items.some((r) => !r.pf.ok)) return;
      const choice = await confirmStart(items, { source: source.serverName(), target: target.serverName(), folder: multiFile() && wantsFile() ? file : '',
        bytes: kind === 'restore' ? files.reduce((t, b) => t + (b.bytes || 0), 0) : source.selectedBytes() });
      if (!choice) return;
      const ids = await call('PgStartMany', items.map((r) => ({ ...r.s, jobs: choice.jobs, restoreJobs: choice.restoreJobs })), choice.parallel);
      onStarted(ids);
      checksEl.replaceChildren(checkRow('ok', ids.length > 1 ? `Started ${ids.length} jobs, ${choice.parallel} at a time. Follow them on the right.` : 'Started. Follow it on the right; you can set up another job meanwhile.'));
      if (kind === 'backup') setFile(''); // a new backup needs a new file name
      if (kind === 'restore') { files = []; paintFiles(); syncTargets(); updateSummary(); }
    } catch (err) {
      toast(err.message, true);
    } finally {
      checkBtn.disabled = startBtn.disabled = false;
    }
  });

  el.append(h('div', { class: 'bk-scroll' }, steps, advanced, checksEl), h('div', { class: 'bk-footer' }, summaryEl, h('span', { class: 'spacer' }), checkBtn, startBtn));
  renderKind();
  return {
    el,
    serversChanged(next, nextSnaps) {
      servers = next;
      snaps = nextSnaps;
      source.serversChanged(next, nextSnaps);
      target.serversChanged(next, nextSnaps);
    },
  };
}

const fact = (k, v) => h('div', { class: 'bk-fact' }, h('span', { class: 'bk-fact-k' }, k), h('span', { class: 'bk-fact-v' }, v));

// Start: the review in a dialog (route, databases, total size, speed). Resolves to the speed choice or null.
function confirmStart(items, names) {
  return new Promise((resolve) => {
    let result = null;
    const review = reviewPanel(items, names);
    const m = modal({ title: review.title, sub: KINDS[items[0].s.kind].label, cls: 'bk-start', onClose: () => resolve(result) });
    const warns = [...new Set(items.flatMap((r) => r.pf.checks.filter((c) => c.level === 'warn').map((c) => c.text)))];
    m.body.append(review.el, ...(warns.length ? [h('div', { class: 'bk-warns' }, ...warns.slice(0, 6).map((w) => h('div', { class: 'bk-check warn' }, icon('alert'), h('div', {}, w))),
      warns.length > 6 ? h('div', { class: 'bk-help' }, `and ${warns.length - 6} more`) : '')] : []));
    const ok = h('button', { type: 'button', class: `btn ${review.replacing ? 'solid-danger' : 'primary'} bk-start-ok` }, icon('play'), review.title);
    m.foot.append(h('span', { class: 'bk-help' }, 'You can cancel at any time; nothing is left behind.'), h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), ok);
    ok.addEventListener('click', () => { result = review.value(); m.close(); });
    ok.focus();
  });
}

// The review page: the route, the databases and how hard the jobs may work.
// Returns { el, value() → { jobs, restoreJobs, parallel }, title, replacing }.
function reviewPanel(items, names) {
  const { s, pf } = items[0];
  const n = items.length;
  const multi = n > 1;
  const replacing = items.some((r) => r.pf.targetExists && r.s.ifExists === 'replace');
  const noun = { copy: ['copy', 'copies'], backup: ['backup', 'backups'], restore: ['restore', 'restores'] }[s.kind];
  const title = replacing ? 'Replace and start' : multi ? `Start ${n} ${noun[1]}` : `Start ${noun[0]}`;

  // The route: source → destination, with the transfer mode on the arrow.
  const end = (label, iconName, name, sub) => h('div', { class: 'bk-end' }, h('span', { class: 'bk-end-label' }, label),
    h('span', { class: 'bk-end-main' }, h('span', { class: 'bk-end-icon' }, icon(iconName)), h('span', { class: 'bk-end-text' }, h('span', { class: 'bk-end-name' }, name), h('span', { class: 'bk-end-sub' }, sub))));
  const pgText = (inst) => (inst?.version ? `PostgreSQL ${versionText(inst.version)}` : '');
  const from = s.kind === 'restore' ? end('From', 'file', multi ? `${n} backup files` : baseName(s.file), 'on this computer')
    : end('From', 'disk', names.source, [s.source.container, pgText(pf.source)].filter(Boolean).join(' · '));
  const to = s.kind === 'backup' ? end('To', 'monitor', 'This computer', names.folder ? 'one file per database' : baseName(s.file))
    : end('To', 'disk', names.target, [s.target.container, pgText(pf.target)].filter(Boolean).join(' · '));
  const via = s.kind === 'copy' ? (s.direct ? 'Direct' : 'Via this computer') : s.kind === 'backup' ? 'Download' : 'Upload';
  const dbs = items.slice(0, 24).map((r) => h('span', { class: 'bk-chip' },
    s.kind === 'copy' && r.s.source.database !== r.s.target.database ? `${r.s.source.database} → ${r.s.target.database}`
      : s.kind === 'restore' ? r.s.target.database : r.s.source.database));

  // Speed presets: the share of the CPU cores the jobs may use on each server.
  const dumpOn = s.direct ? pf.target : pf.source;
  const dumpCores = dumpOn?.cores || 0;
  const restCores = pf.target?.cores || 0;
  const hasDump = s.kind !== 'restore';
  const hasRest = s.kind !== 'backup';
  let parallel = multi ? Math.min(n, 2) : 1;
  let speed = 'balanced';
  const share = { gentle: 0.25, balanced: 0.5, fast: 1 };
  const per = (cores, fallback) => Math.max(1, Math.min(16, Math.floor((cores ? cores * share[speed] : fallback * 2 * share[speed]) / parallel)));
  const speedSeg = segmented([
    { value: 'gentle', label: 'Gentle', title: 'About a quarter of the CPU: the servers stay responsive' },
    { value: 'balanced', label: 'Balanced', badge: 'Recommended', title: 'About half of the CPU' },
    { value: 'fast', label: 'Fast', title: 'All CPU cores: the servers slow down meanwhile' },
    { value: 'custom', label: 'Custom' },
  ], speed, (v) => { speed = v; sync(); });
  const speedNote = h('div', { class: 'bk-help' });
  const dumpStep = stepper(1, 1, 32, () => sync(true));
  const restStep = stepper(1, 1, 32, () => sync(true));
  const customBox = h('div', { class: 'bk-custom' },
    hasDump ? h('div', { class: 'bk-field' }, h('span', { class: 'bk-label' }, multi ? 'Backup workers per database' : 'Backup workers'), dumpStep.el) : '',
    hasRest ? h('div', { class: 'bk-field' }, h('span', { class: 'bk-label' }, multi ? 'Restore workers per database' : 'Restore workers'), restStep.el) : '');
  const parStep = multi ? stepper(parallel, 1, Math.min(n, 10), (v) => { parallel = v; sync(); }) : null;
  const parNote = h('div', { class: 'bk-help' });

  function sync(fromCustom = false) {
    if (!fromCustom && speed !== 'custom') {
      if (hasDump) dumpStep.input.value = per(dumpCores, pf.dumpJobs || 1);
      if (hasRest) restStep.input.value = per(restCores, pf.restoreJobs || 1);
    }
    customBox.hidden = speed !== 'custom';
    const parts = [];
    const use = (cores, w, server) => {
      const total = w * parallel;
      parts.push(cores ? `${server}: ${Math.min(total, cores)} of ${cores} cores` : `${server}: ${total} workers`);
      return cores && total > cores;
    };
    let over = false;
    if (hasDump) over = use(dumpCores, dumpStep.value, s.direct ? names.target : names.source) || over;
    if (hasRest && !(hasDump && s.direct)) over = use(restCores, restStep.value, names.target) || over;
    speedNote.textContent = `Uses about ${parts.join(' · ')}${over ? '. More than the server has: other applications will slow down.' : ''}`;
    speedNote.classList.toggle('warn', over || speed === 'fast');
    if (parStep) parNote.textContent = parallel >= n ? 'All of them start right away' : n - parallel === 1 ? 'The other one waits in the queue and starts as soon as one finishes' : `The other ${n - parallel} wait in the queue and start as soon as one finishes`;
  }
  sync();

  const el = h('div', { class: 'bk-review-panel' },
    h('div', { class: 'bk-route' }, from, h('div', { class: 'bk-route-arrow' }, h('span', { class: 'bk-route-via' }, via), h('span', { class: 'bk-route-line' })), to),
    h('div', { class: 'bk-start-dbs' }, h('span', { class: 'bk-label' }, `${multi ? `${n} databases` : 'Database'}${names.bytes > 0 ? ` · ${fmtBytes(names.bytes)} in total` : ''}`), h('div', { class: 'bk-chips' }, ...dbs, n > 24 ? h('span', { class: 'muted' }, `+${n - 24} more`) : '')),
    h('div', { class: 'bk-start-section' },
      h('div', { class: 'bk-start-row' }, h('div', {}, h('div', { class: 'bk-start-title' }, 'Speed'), h('div', { class: 'bk-help' }, 'How much of the servers’ CPU the jobs may use')), speedSeg.el),
      speedNote, customBox),
    parStep ? h('div', { class: 'bk-start-section' },
      h('div', { class: 'bk-start-row' }, h('div', {}, h('div', { class: 'bk-start-title' }, 'Databases at a time'), parNote), parStep.el)) : '');
  return {
    el,
    title,
    replacing,
    value() {
      const restore = hasRest ? restStep.value : 0;
      return { jobs: hasDump ? dumpStep.value : restore, restoreJobs: restore, parallel };
    },
  };
}

// Server → container → database, each a searchable list. The source ticks one or more databases; the destination
// names the database each one is restored into.
function endpointPicker({ role, servers, snaps, loadInstances, preset }) {
  const isSource = role === 'source';
  const serverSel = combo({ ariaLabel: `${role} server`, placeholder: 'Choose a server…', searchPlaceholder: 'Search servers, groups, IPs…', empty: 'No server matches' });
  const contSel = combo({ ariaLabel: `${role} PostgreSQL`, placeholder: '–', searchPlaceholder: 'Search…', empty: 'Nothing matches' });
  const dbSel = combo({ ariaLabel: 'source databases', placeholder: '–', searchPlaceholder: 'Search databases…', empty: 'No database matches', multiple: true, noun: 'databases' });
  contSel.disabled = true;
  dbSel.disabled = true;
  const dbIn = h('input', { class: 'bk-input', 'aria-label': 'Destination database name', placeholder: 'database name', spellcheck: 'false', autocomplete: 'off' });
  selectOnFocus(dbIn);
  const manyNote = h('div', { class: 'bk-many', hidden: true });
  const mapEl = h('div', { class: 'bk-map', hidden: true });
  const meta = h('div', { class: 'bk-meta' });
  const selInfo = h('div', { class: 'bk-selinfo', hidden: true });
  const refresh = h('button', { type: 'button', class: 'btn ghost icon sm bk-refresh', title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh'));
  const exists = h('div', { class: 'bk-exists', hidden: true });
  let ifExists = 'stop';
  let list = [];
  let touched = false;
  let onDb = () => {};
  let onChg = () => {};
  // Destination only: the source databases and the name each one gets here.
  let sources = [];
  let suffix = '';
  const names = new Map();
  let mapLabels = new Map();
  let mapHead = 'Database';
  let mapKey = ''; // what the rows were last drawn for
  let takenNames = []; // destination names that already exist

  function fillServers() {
    const sorted = [...servers.values()].sort((a, b) => a.name.localeCompare(b.name));
    serverSel.setOptions(sorted.map((s) => {
      const st = snaps.get(s.id)?.status;
      return { value: s.id, label: s.name, sub: [s.group, s.host, st === 'offline' && 'unreachable'].filter(Boolean).join(' · ') };
    }), 'Choose a server…');
  }
  fillServers();

  const instance = () => list.find((i) => i.container === contSel.value) || null;

  function setMeta(inst, err) {
    if (err) { meta.replaceChildren(h('span', { class: 'bk-meta-err' }, icon('alert'), err)); return; }
    if (!inst) { meta.replaceChildren(); return; }
    meta.replaceChildren(
      h('span', { class: 'bk-tag' }, `PostgreSQL ${versionText(inst.version)}`),
      h('span', { class: 'bk-tag' }, `${inst.cores} CPU`),
      h('span', { class: 'bk-tag', title: 'Free space for temporary backup files' }, `${fmtBytes(inst.tmpFreeKb * 1024)} free`),
      h('span', { class: 'bk-tag subtle', title: 'PostgreSQL login read from the container' }, `login ${inst.user}`));
  }

  function fillDatabases() {
    fillDb();
    onChg();
  }

  function fillDb() {
    const inst = instance();
    if (!inst) { dbSel.setOptions([], '–'); dbSel.disabled = true; setMeta(null); renderExists(); if (isSource) onDb([]); return; }
    if (inst.error) {
      setMeta(null, inst.error);
      dbSel.setOptions([], '–');
      dbSel.disabled = true;
      renderExists();
      if (isSource) onDb([]);
      return;
    }
    setMeta(inst);
    const prev = dbSel.values.length ? dbSel.values : preset?.databases || (preset?.database ? [preset.database] : []);
    dbSel.setOptions(inst.databases.map((d) => ({ value: d.name, label: d.name, sub: d.bytes >= 0 ? fmtBytes(d.bytes) : 'no access' })),
      inst.databases.length ? 'Choose databases…' : 'No database');
    dbSel.disabled = !inst.databases.length;
    const keep = prev.filter((x) => dbSel.has(x));
    if (keep.length) dbSel.values = keep;
    else {
      // The biggest user database is the likely pick, not "postgres".
      const user = inst.databases.filter((d) => d.name !== 'postgres').sort((a, b) => b.bytes - a.bytes)[0];
      dbSel.values = user ? [user.name] : [];
    }
    if (isSource) onDb(dbSel.values);
    paintSelection();
    renderExists();
    renderMap();
  }

  async function loadServer(fresh = false) {
    const id = serverSel.value;
    list = [];
    contSel.setOptions([], id ? 'Looking for PostgreSQL…' : '–');
    contSel.disabled = true;
    fillDatabases();
    if (!id) return;
    try {
      const got = await loadInstances(id, fresh);
      if (serverSel.value !== id) return;
      list = got || [];
      if (!list.length) {
        contSel.setOptions([], 'No PostgreSQL found');
        setMeta(null, 'No PostgreSQL container is running on this server');
        return;
      }
      contSel.setOptions(list.map((i) => ({ value: i.container, label: i.container, sub: i.version ? `PostgreSQL ${versionText(i.version)}` : 'cannot log in' })), 'Choose PostgreSQL…');
      contSel.disabled = false;
      const want = preset?.container;
      contSel.value = want && contSel.has(want) ? want : list.length === 1 ? list[0].container : contSel.value;
      fillDatabases();
    } catch (err) {
      if (serverSel.value !== id) return;
      contSel.setOptions([], 'Could not connect');
      setMeta(null, err.message);
    }
  }

  const many = () => !isSource && sources.length > 1;
  // The destination databases: [{ source, target }] (source is '' for a restore).
  function targets() {
    if (!many()) return [{ source: sources[0] || '', target: dbIn.value.trim() }];
    return sources.map((s) => ({ source: s, target: (names.get(s) || '').trim() }));
  }

  // Several databases: one row per database with the name it gets on the destination.
  function renderMap() {
    const on = many();
    dbIn.hidden = on;
    manyNote.hidden = !on;
    mapEl.hidden = !on;
    if (!on) return;
    manyNote.textContent = `${sources.length} ${mapHead === 'Backup file' ? 'backups' : 'databases'} (names below)`;
    const inst = instance();
    mapEl.replaceChildren(h('div', { class: 'bk-map-head' }, h('span', {}, mapHead), h('span', {}), h('span', {}, 'Name on the destination'), h('span', {})),
      ...sources.map((s) => {
        const input = h('input', { class: 'bk-input', value: names.get(s) || '', spellcheck: 'false', autocomplete: 'off', 'aria-label': `Name for ${mapLabels.get(s) || s}` });
        selectOnFocus(input);
        const mark = h('span', { class: 'bk-map-mark' });
        const paintMark = () => {
          const v = input.value.trim();
          const ex = inst?.databases?.some((d) => d.name === v);
          mark.className = `bk-pill ${!v ? 'critical' : ex ? 'warning' : 'good'}`;
          mark.textContent = !v ? 'name it' : ex ? 'exists' : 'new';
        };
        input.addEventListener('input', () => { names.set(s, input.value); paintMark(); renderExists(); onChg(); });
        paintMark();
        return h('div', { class: 'bk-map-row' }, h('span', { class: 'bk-map-src', title: s }, mapLabels.get(s) || s), h('span', { class: 'bk-map-arrow' }, '→'), input, mark);
      }));
  }

  function renderExists() {
    if (isSource) return;
    const inst = instance();
    const taken = [...new Set(targets().map((t) => t.target).filter((x) => x && inst?.databases?.some((d) => d.name === x)))];
    takenNames = taken;
    exists.hidden = !taken.length;
    if (!taken.length) return;
    const one = taken.length === 1;
    const seg = segmented([
      { value: 'stop', label: 'Don’t overwrite' },
      { value: 'keep', label: one ? 'Keep old as backup' : 'Keep old ones as backup', title: 'Renamed to <name>_old_<date> once the new copy is complete' },
      { value: 'replace', label: 'Replace', title: 'Dropped once the new copy is complete; usable until then' },
    ], ifExists, (v) => { ifExists = v; help(); });
    const helpEl = h('div', { class: 'bk-help' });
    const help = () => {
      helpEl.textContent = { stop: 'The job will not start while the name is taken.', keep: 'The current database is renamed to <name>_old_<date> once the new one is complete.', replace: 'The current database is dropped once the new one is complete. It stays usable until then.' }[ifExists];
    };
    help();
    exists.replaceChildren(
      h('div', { class: 'bk-exists-head' }, icon('alert'), one ? `${taken[0]} already exists on this server` : `${taken.length} of these names already exist: ${taken.join(', ')}`),
      seg.el, helpEl);
  }

  serverSel.el.addEventListener('change', () => { preset = null; loadServer(); });
  contSel.el.addEventListener('change', () => { fillDatabases(); renderMap(); });
  // The databases ticked and their total size.
  function selectedBytes() {
    const inst = instance();
    const picked = new Set(dbSel.values);
    return (inst?.databases || []).filter((d) => picked.has(d.name) && d.bytes > 0).reduce((t, d) => t + d.bytes, 0);
  }
  function paintSelection() {
    if (!isSource) return;
    const inst = instance();
    const picked = (inst?.databases || []).filter((d) => dbSel.values.includes(d.name));
    selInfo.hidden = !picked.length;
    if (!picked.length) return;
    const unknown = picked.filter((d) => d.bytes < 0).length;
    selInfo.replaceChildren(icon('disk'),
      h('span', {}, h('strong', {}, plural(picked.length, 'database')), ' selected · ', h('strong', {}, fmtBytes(selectedBytes())), ' in total',
        unknown ? h('span', { class: 'muted' }, ` (size unknown for ${unknown})`) : ''));
  }
  dbSel.el.addEventListener('change', () => { paintSelection(); onDb(dbSel.values); onChg(); });
  dbIn.addEventListener('input', () => { touched = true; renderExists(); onChg(); });
  refresh.addEventListener('click', () => loadServer(true));

  const field = (label, ...control) => h('div', { class: 'bk-field' }, h('span', { class: 'bk-label' }, label), ...control);
  const el = h('div', { class: 'bk-endpoint' },
    h('div', { class: 'bk-grid3' },
      field('Server', serverSel.el),
      field('PostgreSQL', contSel.el),
      field(isSource ? 'Databases' : 'Database name', isSource ? dbSel.el : dbIn, manyNote)),
    selInfo,
    h('div', { class: 'bk-endpoint-foot' }, meta, refresh),
    mapEl, exists);

  if (preset?.serverId && servers.has(preset.serverId)) {
    serverSel.value = preset.serverId;
    if (!isSource && preset.database) { dbIn.value = preset.database; touched = true; }
    loadServer();
  }
  return {
    el,
    value: () => ({ serverId: serverSel.value, container: contSel.value, database: isSource ? dbSel.value : dbIn.value.trim(), databases: isSource ? dbSel.values : [] }),
    targets,
    selectedBytes,
    ifExists: () => (exists.hidden ? 'stop' : ifExists),
    taken: () => takenNames,
    onDatabase: (fn) => { onDb = fn; },
    onChange: (fn) => { onChg = fn; },
    serverName: () => servers.get(serverSel.value)?.name || '',
    setDatabase: (name) => { dbIn.value = name || ''; renderExists(); },
    dbTouched: () => touched,
    // Destination: the source databases to name (a copy within one container gets a "_copy" suffix by default).
    // opts: labels (key -> what the row shows), defaults (key -> name), head (first column title).
    setSources(next, sfx = '', opts = {}) {
      if (sfx !== suffix) { names.clear(); suffix = sfx; }
      sources = next;
      mapLabels = opts.labels || new Map();
      mapHead = opts.head || 'Database';
      const dflt = (s) => opts.defaults?.get(s) ?? `${s}${sfx}`;
      for (const s of next) if (!names.has(s)) names.set(s, dflt(s));
      if (next.length === 1 && !touched) dbIn.value = dflt(next[0]);
      // Redraw the rows only when the list itself changed: typing in a row must keep its focus.
      const key = JSON.stringify([next, sfx, mapHead]);
      if (key !== mapKey) {
        mapKey = key;
        renderMap();
      }
      renderExists();
    },
    serversChanged(next, nextSnaps) {
      servers = next;
      snaps = nextSnaps;
      const had = serverSel.value;
      fillServers();
      if (had && !serverSel.value) loadServer(); // the chosen server was removed
    },
  };
}

// ---------------------------------------------------------------- job card

// A job. Live: progress, figures and steps; finished: one row with its result. Details open below.
function jobCard(v0, { expanded = false, onAgain, onForget }) {
  const kindIcon = h('span', { class: 'jc-icon' });
  const name = h('div', { class: 'jc-name' });
  const route = h('div', { class: 'jc-route' });
  const statusEl = h('span', { class: 'bk-pill' });
  const actions = h('div', { class: 'jc-actions' });
  const more = h('button', { type: 'button', class: 'jc-more', 'aria-expanded': 'false', title: 'Details' }, icon('down'));
  const facts = h('div', { class: 'jc-facts' });
  const bar = h('div', { class: 'jc-bar' }, h('span'));
  const pct = h('span', { class: 'jc-pct' });
  const nowEl = h('div', { class: 'jc-now' });
  const metrics = h('div', { class: 'jc-metrics' });
  const stepsEl = h('ol', { class: 'jc-steps' });
  const live = h('div', { class: 'jc-live' }, h('div', { class: 'jc-progress' }, bar, pct), nowEl, metrics, stepsEl);
  const phasesEl = h('div', { class: 'jc-phases' });
  const tablesEl = h('div', { class: 'jc-tables' });
  const warnEl = h('div', { class: 'jc-warnings' });
  const summaryEl = h('div', { class: 'jc-summary' });
  const logPre = h('pre', { class: 'jc-log' });
  const log = h('details', { class: 'bk-disclosure jc-log-box' }, h('summary', {}, icon('logs'), 'Technical log'), logPre);
  const body = h('div', { class: 'jc-body' }, phasesEl, tablesEl, warnEl, summaryEl, log);
  const el = h('article', { class: 'jc' },
    h('div', { class: 'jc-head' }, kindIcon, h('div', { class: 'jc-title' }, name, route), facts, statusEl, actions, more),
    live, body);
  let lastState = null;
  let logLen = -1;
  let last = v0;

  const setExpanded = (on) => {
    expanded = on;
    body.hidden = !on;
    el.classList.toggle('open', on);
    more.setAttribute('aria-expanded', String(on));
    more.title = on ? 'Hide details' : 'Details';
    if (on) update(last);
  };
  more.addEventListener('click', () => setExpanded(!expanded));
  el.addEventListener('click', (e) => { // a click on a finished row opens it too
    if (!el.classList.contains('done-row') || e.target.closest('button, a, input, details, pre')) return;
    setExpanded(!expanded);
  });

  const metric = (label, value, title = '') => h('div', { class: 'jc-metric', title: title || null }, h('span', { class: 'jc-metric-label' }, label), h('span', { class: 'jc-metric-value' }, value));

  function update(v) {
    last = v;
    const s = v.spec;
    const isRunning = v.state === 'running';
    const isLive = isRunning || v.state === 'queued';
    el.className = `jc k-${s.kind} st-${v.state}${isLive ? '' : ' done-row'}${expanded ? ' open' : ''}`;
    kindIcon.replaceChildren(icon(KINDS[s.kind]?.icon || 'disk'));
    name.textContent = jobName(v);
    const [a, b] = jobRoute(v);
    route.replaceChildren(h('span', {}, a), h('span', { class: 'jc-route-arrow' }, '→'), h('span', {}, b),
      s.kind === 'copy' ? h('span', { class: 'jc-route-mode' }, s.direct ? 'Direct' : 'Via this computer') : '');
    const [tone, label] = STATE[v.state] || ['neutral', v.state];
    statusEl.className = `bk-pill ${tone}`;
    statusEl.textContent = label;
    live.hidden = !isLive;

    // A finished row: size, duration and when, at a glance.
    facts.hidden = isLive;
    if (!isLive) {
      const sm = v.summary || {};
      facts.replaceChildren(
        h('span', { title: 'Size' }, sm.backupBytes ? fmtBytes(sm.backupBytes) : sm.databaseBytes ? fmtBytes(sm.databaseBytes) : '–'),
        h('span', { title: 'Duration' }, fmtDuration((v.ended - v.started) / 1000)),
        h('span', { title: fmtDateTime(v.ended || v.started) }, fmtWhen(v.ended || v.started)));
    }

    if (isLive) {
      pct.textContent = `${Math.floor(v.percent)}%`;
      bar.className = `jc-bar${isRunning ? ' running' : ''}`;
      bar.firstChild.style.width = `${v.percent.toFixed(2)}%`;
      const cur = v.phases.find((p) => p.state === 'running');
      if (v.state === 'queued') nowEl.textContent = 'Waiting in the queue: starts as soon as another database finishes';
      else nowEl.textContent = cur ? `${DOING[cur.key] || cur.label}${cur.detail && /parallel/.test(cur.detail) ? ` · ${cur.detail.split('·').pop().trim().replace('parallel jobs', 'workers')}` : ''}` : 'Starting…';
      const bytesPhase = cur && cur.unit === 'bytes' && cur.total > 0 ? cur : null;
      metrics.replaceChildren(
        metric('Speed', bytesPhase && cur.rate > 0 ? `${fmtBytes(cur.rate)}/s` : '–'),
        metric('Time left', cur && cur.eta >= 0 ? fmtDuration(cur.eta) : '–', 'For the current step'),
        metric('Processed', bytesPhase ? `${fmtBytes(cur.done)} / ${fmtBytes(cur.total)}` : cur && cur.total > 0 ? `${cur.done} / ${cur.total}` : '–'),
        metric('Tables', v.tablesTotal ? `${v.tablesDone} / ${v.tablesTotal}` : '–'),
        metric('Elapsed', v.state === 'queued' ? '–' : fmtDuration((Date.now() - v.started) / 1000)));
      stepsEl.replaceChildren(...v.phases.map((p) => h('li', { class: `jc-step st-${p.state}`, title: p.detail || p.label },
        h('span', { class: 'jc-dot' }, p.state === 'done' ? icon('check') : p.state === 'failed' ? icon('close') : ''),
        h('span', { class: 'jc-step-label' }, p.key === 'transfer' ? p.label : STEP[p.key] || p.label,
          p.state === 'running' && p.total > 0 ? h('span', { class: 'jc-step-pct' }, ` ${Math.floor(100 * Math.min(1, p.done / p.total))}%`) : ''))));
    }

    if (v.state !== lastState) {
      lastState = v.state;
      renderActions(v);
    }
    if (!expanded) return; // the details are drawn only when shown

    phasesEl.replaceChildren(...v.phases.map(phaseRow));
    if (isRunning && v.active.length) {
      tablesEl.hidden = false;
      tablesEl.replaceChildren(h('div', { class: 'jc-sub' }, `Tables in progress · ${v.tablesDone} of ${v.tablesTotal} done`),
        ...v.active.map((t) => h('div', { class: 'jc-table' },
          h('span', { class: 'jc-table-name', title: t.name }, t.name),
          h('span', { class: 'jc-mini' }, h('span', { style: `width:${t.total ? (100 * t.done) / t.total : 0}%` })),
          h('span', { class: 'jc-table-size' }, fmtBytes(t.total)))));
    } else {
      tablesEl.hidden = true;
    }
    warnEl.hidden = !v.warnings.length && !(v.state === 'failed' || v.state === 'cancelled');
    warnEl.replaceChildren(
      v.state === 'failed' || v.state === 'cancelled' ? h('div', { class: `bk-check ${v.state === 'failed' ? 'error' : 'muted'}` }, icon('alert'), h('div', {}, v.error)) : '',
      ...v.warnings.slice(0, 30).map((w) => h('div', { class: 'bk-check warn' }, icon('alert'), h('div', {}, w))));
    renderSummary(v);
    if (v.state === 'failed') log.open = true;
    if (v.log.length !== logLen || !isRunning) {
      logLen = v.log.length;
      const atEnd = logPre.scrollTop + logPre.clientHeight >= logPre.scrollHeight - 30;
      logPre.textContent = v.log.join('\n');
      if (atEnd) logPre.scrollTop = logPre.scrollHeight;
    }
  }

  function phaseRow(p) {
    const frac = p.state === 'done' ? 1 : p.total > 0 ? Math.min(1, p.done / p.total) : 0;
    let nums = '';
    if (p.state === 'running' || p.state === 'done' || p.state === 'failed') {
      if (p.unit === 'bytes' && p.total > 0) nums = `${fmtBytes(p.done)} of ${fmtBytes(p.total)}`;
      else if (p.unit === 'items' && p.total > 0 && p.key !== 'prepare' && p.key !== 'finish') nums = `${p.done} of ${p.total}`;
      if (p.state === 'running' && p.unit === 'bytes' && p.rate > 0) nums += ` · ${fmtBytes(p.rate)}/s`;
      if (p.state === 'running' && p.eta >= 0) nums += ` · ${fmtDuration(p.eta)} left`;
      if (p.state !== 'running' && p.ended && p.started) nums += `${nums ? ' · ' : ''}${fmtDuration((p.ended - p.started) / 1000)}`;
    } else if (p.state === 'skipped') nums = 'not needed';
    return h('div', { class: `jc-phase st-${p.state}` },
      h('span', { class: 'jc-dot' }, p.state === 'done' ? icon('check') : p.state === 'failed' ? icon('close') : p.state === 'running' ? h('span', { class: 'spinner', 'aria-hidden': 'true' }) : ''),
      h('div', { class: 'jc-phase-main' },
        h('div', { class: 'jc-phase-top' }, h('span', { class: 'jc-phase-label' }, p.label), h('span', { class: 'jc-phase-nums' }, nums)),
        h('div', { class: 'jc-mini' }, h('span', { style: `width:${(frac * 100).toFixed(1)}%` })),
        p.detail && p.state === 'running' ? h('div', { class: 'jc-phase-detail' }, p.detail.replace('parallel jobs', 'workers')) : ''));
  }

  function renderActions(v) {
    const btns = [];
    if (v.state === 'queued') {
      btns.push(h('button', { type: 'button', class: 'btn ghost sm', title: 'Take it out of the queue', onclick: (e) => { e.currentTarget.disabled = true; call('PgCancel', v.id); } }, 'Remove'));
    } else if (v.state === 'running') {
      const cancel = h('button', { type: 'button', class: 'btn ghost sm jc-cancel' }, icon('stop'), 'Cancel');
      cancel.addEventListener('click', async () => {
        const what = v.spec.kind === 'backup' ? 'The backup file is not saved.' : 'The partially restored database is removed; an existing database with the same name is not touched.';
        if (!(await confirmDialog(`Cancel this job?\n\nEverything it started on the servers is stopped and cleaned up. ${what}`, 'Stop job', { confirmLabel: 'Cancel job', cancelLabel: 'Keep running' }))) return;
        cancel.disabled = true;
        call('PgCancel', v.id);
      });
      btns.push(cancel);
    } else {
      if (v.spec.file && v.state !== 'failed' && v.state !== 'cancelled') {
        btns.push(h('button', { type: 'button', class: 'btn ghost icon sm', title: 'Show the backup file', 'aria-label': 'Show the backup file', onclick: () => call('PgShowFile', v.spec.file).catch((e) => toast(e.message, true)) }, icon('folder')));
      }
      btns.push(h('button', { type: 'button', class: 'btn ghost icon sm', title: 'Run again (fills the form)', 'aria-label': 'Run again', onclick: () => onAgain(v.spec) }, icon('restart')));
      btns.push(h('button', { type: 'button', class: 'btn ghost icon sm', title: 'Remove from the history', 'aria-label': 'Remove from the history', onclick: () => onForget(v) }, icon('trash')));
    }
    actions.replaceChildren(...btns);
  }

  function renderSummary(v) {
    if (v.state === 'running' || v.state === 'queued') { summaryEl.hidden = true; return; }
    const sm = v.summary || {};
    const s = v.spec;
    const items = [];
    const add = (k, val) => { if (val !== '' && val != null) items.push(fact(k, val)); };
    add('Duration', fmtDuration((v.ended - v.started) / 1000));
    if (sm.databaseBytes) add('Database size', fmtBytes(sm.databaseBytes));
    if (sm.backupBytes) add('Backup size', `${fmtBytes(sm.backupBytes)}${sm.databaseBytes ? ` (${(sm.backupBytes / sm.databaseBytes * 100).toFixed(0)}%)` : ''}`);
    if (sm.tables) add('Tables', String(sm.tables));
    if (sm.sourceVersion) add('PostgreSQL', `${versionText(sm.sourceVersion)}${sm.targetVersion ? ` → ${versionText(sm.targetVersion)}` : ''}`);
    if (sm.finalDatabase) add('Result', `${sm.finalDatabase} on ${s.target.server}`);
    if (sm.oldDatabase) add('Previous database', `kept as ${sm.oldDatabase}`);
    if (s.kind === 'copy') add('Transfer', s.direct ? 'Direct between the servers' : 'Through this computer');
    const par = [sm.dumpJobs && `${sm.dumpJobs} backup`, sm.restoreJobs && `${sm.restoreJobs} restore`].filter(Boolean).join(' · ');
    if (par) add('Workers', par);
    if (sm.compression) add('Compression', sm.compression);
    if (s.file && s.kind !== 'restore') add('File', h('span', { class: 'jc-path', title: s.file }, s.file));
    if (sm.verified) add('Row counts', sm.mismatches?.length ? h('span', { class: 'bk-text-bad' }, `${sm.mismatches.length} of ${sm.verified} tables differ`) : h('span', { class: 'bk-text-good' }, `all ${sm.verified} tables match`));
    const parts = [h('div', { class: 'bk-facts' }, ...items)];
    if (sm.mismatches?.length) {
      parts.push(h('div', { class: 'table-wrap' }, h('table', { class: 'jc-mismatch' },
        h('thead', {}, h('tr', {}, h('th', {}, 'Table'), h('th', { class: 'num' }, 'Source rows'), h('th', { class: 'num' }, 'Destination rows'))),
        h('tbody', {}, ...sm.mismatches.slice(0, 200).map((x) => h('tr', {}, h('td', {}, x.table), h('td', { class: 'num' }, x.source.toLocaleString()), h('td', { class: 'num' }, x.target.toLocaleString())))))));
    }
    summaryEl.hidden = v.state === 'failed' || v.state === 'cancelled';
    summaryEl.replaceChildren(...parts);
  }

  setExpanded(expanded);
  update(v0);
  return { el, update };
}
