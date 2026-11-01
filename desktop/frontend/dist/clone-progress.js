// Progress of a clone. The backend follows the script running on the target server and sends "clone.job"
// events; this module keeps them, shows a badge on the Clone button, and draws the progress page of the dialog:
// overall bar, phases, counters, one live row per container (with its log and the reason of a failure).
import { h, toast, fmtBytes } from './util.js';
import { call, on, confirmDialog } from './bridge.js';
import { icon } from './icons.js';

export const jobs = new Map(); // id -> view
const listeners = new Set();
let badgeBtn = null;
let shown = null; // the job id an open dialog shows (no toast for it)

export function initCloneJobs(btn) {
  badgeBtn = btn;
  call('CloneJobs').then((list) => { for (const v of list) jobs.set(v.id, v); updateBadge(); }).catch(() => {});
  on('clone.job', (v) => {
    const before = jobs.get(v.id);
    jobs.set(v.id, v);
    updateBadge();
    listeners.forEach((fn) => fn(v));
    if (before?.state === 'running' && v.state !== 'running' && shown !== v.id) toast(resultText(v), v.state === 'failed');
  });
}

// Calls fn on every job update; returns the function that stops it.
export function onJobChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

let onBadge = () => {};
// Called when the running progress changes (the tab strip shows it on the Clone tab).
export function setBadgeListener(fn) { onBadge = fn; }

export const runningSummary = () => {
  const running = [...jobs.values()].filter((v) => v.state === 'running');
  if (!running.length) return null;
  return { count: running.length, percent: running.reduce((s, v) => s + v.percent, 0) / running.length };
};

export const latestJob = () => [...jobs.values()].sort((a, b) => b.started - a.started)[0] || null;

let lastBadge = '';
function updateBadge() {
  const r = runningSummary();
  const key = r ? `${r.count}|${Math.floor(r.percent)}` : '';
  if (key !== lastBadge) { lastBadge = key; onBadge(); }
  if (!badgeBtn) return;
  const running = [...jobs.values()].filter((v) => v.state === 'running');
  let badge = badgeBtn.querySelector('.clone-btn-badge');
  if (!running.length) { badge?.remove(); badgeBtn.dataset.tip = 'Clone containers to another server'; return; }
  if (!badge) { badge = h('span', { class: 'clone-btn-badge' }); badgeBtn.append(badge); }
  const pct = running.reduce((s, v) => s + v.percent, 0) / running.length;
  badge.textContent = `${Math.floor(pct)}%`;
  badgeBtn.dataset.tip = `Cloning ${running.map((v) => `to ${v.target}`).join(', ')} · ${Math.floor(pct)}%`;
}

// ---------------------------------------------------------------- wording

export function counts(v) {
  const c = { total: v.containers.length, started: 0, running: 0, attention: 0, failed: 0, skipped: 0, cancelled: 0, waiting: 0 };
  for (const x of v.containers) {
    if (x.status === 'ok') {
      c.started++;
      if (x.state && x.state !== 'running') c.attention++; else c.running++;
    } else if (x.status === 'failed' || x.status === 'unknown') c.failed++;
    else if (x.status === 'skipped') c.skipped++;
    else if (x.status === 'cancelled') c.cancelled++;
    else c.waiting++;
  }
  return c;
}

function resultText(v) {
  const c = counts(v);
  const where = `${v.source} → ${v.target}`;
  switch (v.state) {
    case 'done': return `Clone ${where} completed: ${c.running} container${c.running === 1 ? '' : 's'} running`;
    case 'warning': return `Clone ${where} finished: ${c.running} running, ${c.failed + c.attention} need attention`;
    case 'cancelled': return `Clone ${where} cancelled`;
    default: return `Clone ${where} failed${v.error ? `: ${v.error}` : ''}`;
  }
}

const STATE_LABEL = { running: 'In progress', done: 'Completed', warning: 'Completed with issues', failed: 'Failed', cancelled: 'Cancelled' };
const PHASE_LABEL = { images: 'Images', files: 'Config files', networks: 'Networks', containers: 'Containers', verify: 'Health check' };
const STEP_LABEL = { removing: 'Removing the old container…', creating: 'Creating…', starting: 'Starting…' };

function fmtDur(ms) {
  if (ms == null || ms < 0) return '';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

// What a container row says: [tone, icon, text, detail].
function containerStatus(x, v) {
  switch (x.status) {
    case 'pending': return v.state === 'running' ? ['muted', 'clock', 'Waiting', ''] : ['muted', 'clock', 'Not run', ''];
    case 'working': return ['busy', null, STEP_LABEL[x.step] || 'Preparing…', ''];
    case 'skipped': return ['muted', 'arrowRight', 'Skipped', 'A container with this name already exists on the target'];
    case 'cancelled': return ['muted', 'close', 'Cancelled', ''];
    case 'unknown': return ['warn', 'question', 'Unknown', 'The connection was lost before it finished'];
    case 'failed': return ['bad', 'close', 'Failed', x.error];
    case 'ok':
      if (x.state === 'running') return ['good', 'check', 'Running', ''];
      if (x.state === 'restarting' || x.restarts > 0) return ['bad', 'restart', 'Restarting', `It keeps crashing (${x.restarts} restart${x.restarts === 1 ? '' : 's'}): often a wrong variable. Open its log.`];
      if (x.state) return ['bad', 'alert', x.state === 'exited' ? `Exited (code ${x.exitCode})` : x.state, 'It stopped right after starting: often a wrong variable. Open its log.'];
      return v.phase === 'verify' ? ['busy', null, 'Started · checking…', ''] : ['good', 'check', 'Started', ''];
    default: return ['muted', null, x.status, ''];
  }
}

const statusIcon = (tone, name) => h('span', { class: `cp-ic ${tone}` }, tone === 'busy' ? h('span', { class: 'cp-spin' }) : icon(name || 'clock'));

// ---------------------------------------------------------------- the page

// Draws the progress of job id into content/foot. actions: { editAgain (or null), newClone, openServer(id),
// openLogs(serverId, name), close }. Returns a function that detaches it.
export function progressView(id, { content, foot, actions }) {
  let v = jobs.get(id);
  shown = id;
  const filters = ['all', 'problems', 'running'];
  let filter = 'all';
  const open = new Set(); // rows with their log shown

  const title = h('div', { class: 'cp-title' });
  const route = h('div', { class: 'cp-route' });
  const pill = h('span', { class: 'cp-pill' });
  const elapsed = h('span', { class: 'cp-elapsed' });
  const pct = h('div', { class: 'cp-pct' });
  const bar = h('div', { class: 'jc-bar' }, h('span'));
  const now = h('div', { class: 'cp-now' });
  const err = h('div', { class: 'cp-error', hidden: true });
  const phases = h('ol', { class: 'jc-steps cp-phases' });
  const metrics = h('div', { class: 'jc-metrics cp-metrics' });
  const filterBtns = filters.map((f) => h('button', { type: 'button', class: 'tab', onclick: () => { filter = f; render(); } }));
  const tbody = h('tbody');
  const others = h('div', { class: 'cp-others' });

  content.append(
    h('section', { class: 'cp-head' },
      h('div', { class: 'cp-head-top' },
        h('div', { class: 'cp-icon' }, icon('copy')),
        h('div', { class: 'cp-head-text' }, title, route),
        h('div', { class: 'cp-head-side' }, pill, elapsed)),
      h('div', { class: 'cp-progress' }, bar, pct),
      now, err),
    phases,
    metrics,
    h('div', { class: 'cp-list-head' }, h('h3', {}, 'Containers'), h('div', { class: 'tabs', role: 'group', 'aria-label': 'Show' }, filterBtns)),
    h('div', { class: 'cp-table-wrap' }, h('table', { class: 'cp-table' },
      h('colgroup', {}, h('col', { class: 'st' }), h('col', { class: 'nm' }), h('col'), h('col', { class: 'du' }), h('col', { class: 'ac' })),
      h('thead', {}, h('tr', {}, h('th', {}), h('th', {}, 'Container'), h('th', {}, 'Status'), h('th', { class: 'num' }, 'Time'), h('th', {}))),
      tbody)),
    others);

  // One row per container, made once and updated in place (keeps an open log and the scroll position).
  const rows = v.containers.map((_, i) => {
    const ic = h('td', { class: 'cp-st' });
    const name = h('div', { class: 'cp-name' });
    const image = h('div', { class: 'cp-image' });
    const text = h('div', { class: 'cp-text' });
    const detail = h('div', { class: 'cp-detail' });
    const dur = h('td', { class: 'num cp-dur' });
    const logBtn = h('button', { type: 'button', class: 'btn sm ghost', title: 'What the copy did for this container' }, icon('logs'), h('span', { class: 'lbl' }, 'Output'));
    const liveBtn = h('button', { type: 'button', class: 'btn sm ghost', title: 'Open the container’s own log on the target server' }, icon('terminal'), h('span', { class: 'lbl' }, 'Logs'));
    const pre = h('pre', { class: 'cp-log mono' });
    const logRow = h('tr', { class: 'cp-log-row', hidden: true }, h('td', { colspan: 5 }, pre));
    const tr = h('tr', { class: 'cp-row' }, ic, h('td', {}, name, image), h('td', {}, text, detail), dur, h('td', { class: 'cp-acts' }, logBtn, liveBtn));
    const loadLog = async () => {
      try { pre.textContent = (await call('CloneLog', id, `c:${i}`)) || 'No output yet.'; } catch (e) { pre.textContent = e.message; }
    };
    logBtn.addEventListener('click', () => {
      if (open.has(i)) open.delete(i); else { open.add(i); loadLog(); }
      logRow.hidden = !open.has(i);
      logBtn.classList.toggle('active', open.has(i));
    });
    liveBtn.addEventListener('click', () => actions.openLogs(v.targetId, v.containers[i].name));
    let last = '';
    return {
      nodes: [tr, logRow],
      update(x) {
        const [tone, iconName, label, more] = containerStatus(x, v);
        const key = `${x.status}|${x.step}|${x.state}|${x.exitCode}|${x.restarts}|${x.error}|${v.phase}`;
        tr.className = `cp-row t-${tone}`;
        if (key !== last) {
          ic.replaceChildren(statusIcon(tone, iconName));
          name.textContent = x.name;
          image.textContent = x.image;
          text.textContent = label;
          detail.textContent = more || '';
          detail.hidden = !more;
          liveBtn.hidden = x.status !== 'ok';
          logBtn.disabled = x.status === 'pending';
          if (open.has(i)) loadLog();
          last = key;
        } else if (open.has(i) && x.status === 'working') loadLog();
        const end = x.ended || (x.status === 'working' ? Date.now() : 0);
        dur.textContent = x.started && end ? fmtDur(end - x.started) : '';
      },
      matches(x) {
        const [tone] = containerStatus(x, v);
        return filter === 'all' || (filter === 'problems' ? tone === 'bad' || tone === 'warn' : tone === 'good');
      },
    };
  });

  function renderPhases() {
    const at = v.phases.indexOf(v.phase === 'done' ? v.phases[v.phases.length - 1] : v.phase);
    const doneAll = v.phase === 'done' || v.state !== 'running';
    const sub = {
      files: () => `${v.files.filter((x) => x.status !== 'pending' && x.status !== 'working').length}/${v.files.length}`,
      networks: () => `${v.networks.filter((x) => x.status !== 'pending' && x.status !== 'working').length}/${v.networks.length}`,
      images: () => `${v.images.filter((x) => x.status !== 'pending' && x.status !== 'working').length}/${v.images.length}`,
      containers: () => `${v.containers.filter((x) => x.status !== 'pending' && x.status !== 'working').length}/${v.containers.length}`,
      verify: () => {
        const c = counts(v);
        if (v.phase === 'verify') return `${Math.max(0, Math.ceil(v.verifyWait - (Date.now() - v.phaseAt) / 1000))}s`;
        return v.phase === 'done' && c.started ? `${c.running}/${c.started} up` : '';
      },
    };
    const failedIn = {
      files: () => v.files.some((x) => x.status === 'failed'),
      networks: () => v.networks.some((x) => x.status === 'failed'),
      images: () => v.images.some((x) => x.status === 'failed'),
      containers: () => v.containers.some((x) => x.status === 'failed'),
      verify: () => v.phase === 'done' && counts(v).attention > 0,
    };
    phases.replaceChildren(...v.phases.map((p, i) => {
      let st = i < at || (doneAll && i <= at) ? 'done' : i === at ? 'running' : 'pending';
      if (st === 'done' && failedIn[p]()) st = 'failed';
      if (v.state !== 'running' && st === 'running') st = 'failed';
      if (v.state === 'cancelled' && i > at) st = 'skipped';
      return h('li', { class: `jc-step st-${st}` },
        h('span', { class: 'jc-dot' }, st === 'done' ? icon('check') : st === 'failed' ? icon('close') : ''),
        h('span', { class: 'cp-phase-label' }, PHASE_LABEL[p] || p),
        h('span', { class: 'cp-phase-sub' }, sub[p]?.() || ''));
    }));
  }

  function renderNow() {
    if (v.state !== 'running') { now.hidden = true; return; }
    now.hidden = false;
    let what = '';
    const img = v.images.find((x) => x.status === 'working');
    const c = v.containers.find((x) => x.status === 'working');
    if (v.cancelling) what = 'Cancelling after the current container…';
    else if (v.phase === 'files') what = 'Writing the config files';
    else if (v.phase === 'networks') what = 'Creating the networks';
    else if (v.phase === 'images' && img) what = img.bytes ? `Copying ${img.name} from ${v.source} · ${copyText(img)}` : `Getting ${img.name}`;
    else if (v.phase === 'containers' && c) what = `${c.name}: ${(STEP_LABEL[c.step] || 'preparing…').toLowerCase()}`;
    else if (v.phase === 'verify') what = 'Checking that the started containers keep running';
    else what = 'Working…';
    now.replaceChildren(h('span', { class: 'cp-spin' }), h('b', {}, what), v.now && v.phase !== 'verify' ? h('span', { class: 'cp-now-line mono' }, v.now) : '');
  }

  // Copy speed per image, from the byte counts of the last events.
  const speed = new Map(); // image name -> { bytes, t, rate }
  function copyText(x) {
    const s = speed.get(x.name);
    const now = Date.now();
    if (!s || x.bytes < s.bytes) speed.set(x.name, { bytes: x.bytes, t: now, rate: 0 });
    else if (now - s.t >= 1000) speed.set(x.name, { bytes: x.bytes, t: now, rate: (x.bytes - s.bytes) / ((now - s.t) / 1000) });
    const rate = speed.get(x.name).rate;
    return `${fmtBytes(x.bytes)} sent${rate ? ` · ${fmtBytes(rate)}/s` : ''}${x.size ? ` · image ${fmtBytes(x.size)}` : ''}`;
  }

  const openGroups = new Set();
  function renderOthers() {
    const groups = [['Images', v.images], ['Networks', v.networks], ['Config files', v.files]].filter(([, l]) => l.length);
    const imageNote = (x) => {
      if (x.status === 'working') return x.bytes ? copyText(x) : 'getting it…';
      if (x.status === 'ok') return { pull: 'pulled', copy: `copied from ${v.source}${x.bytes ? ` (${fmtBytes(x.bytes)} sent)` : ''}`, local: 'already on the target' }[x.via] || 'ready';
      return { cancelled: 'cancelled', pending: 'waiting', failed: 'not available' }[x.status] || '';
    };
    others.replaceChildren(...groups.map(([label, list]) => {
      const d = h('details', { class: 'cp-group', open: openGroups.has(label) || (label === 'Images' && v.phase === 'images') },
        h('summary', {}, label, h('span', { class: 'muted' }, ` ${list.filter((x) => ['ok', 'exists', 'local'].includes(x.status)).length}/${list.length}`),
          list.some((x) => x.status === 'failed') ? h('span', { class: 'cp-group-bad' }, icon('alert'), `${list.filter((x) => x.status === 'failed').length} failed`) : ''),
        h('ul', {}, ...list.map((x) => {
          const tone = x.status === 'working' ? 'busy' : x.status === 'failed' ? 'bad' : x.status === 'pending' || x.status === 'cancelled' ? 'muted' : 'good';
          const note = label === 'Images' ? imageNote(x) : { exists: 'already there', cancelled: 'cancelled', pending: 'waiting' }[x.status] || '';
          return h('li', {}, statusIcon(tone, tone === 'bad' ? 'close' : tone === 'good' ? 'check' : 'clock'),
            h('span', { class: 'mono' }, x.name), note ? h('span', { class: 'muted' }, note) : '', x.error ? h('span', { class: 'cp-group-err' }, x.error) : '');
        })));
      d.addEventListener('toggle', () => { d.open ? openGroups.add(label) : openGroups.delete(label); });
      return d;
    }));
  }

  function renderFoot() {
    const running = v.state === 'running';
    const failed = v.containers.filter((x) => x.status === 'failed' || x.status === 'unknown');
    const btn = (cls, iconName, label, onclick, attrs = {}) => h('button', { type: 'button', class: `btn ${cls}`, onclick, ...attrs }, iconName ? icon(iconName) : '', label);
    const left = [btn('ghost', 'logs', 'Full output', async () => {
      const text = await call('CloneLog', id, 'all').catch((e) => e.message);
      const w = h('pre', { class: 'cp-log mono cp-full' }, text || 'No output yet.');
      others.replaceChildren(w);
      w.scrollIntoView({ block: 'nearest' });
    })];
    const right = running
      ? [actions.close ? btn('ghost', null, 'Run in background', actions.close, { title: 'Close this window: the copy goes on, the Clone button shows its progress' }) : '',
        btn('act act-remove', 'stop', v.cancelling ? 'Cancelling…' : 'Cancel', async () => {
          if (!await confirmDialog('Cancel the clone?\n\nThe container in progress is finished first; the ones after it are not created. Containers already created stay.', 'Stop clone', { confirmLabel: 'Cancel clone', cancelLabel: 'Keep going' })) return;
          call('CloneCancel', id).catch((e) => toast(e.message, true));
        }, { disabled: v.cancelling })]
      : [
        failed.length ? btn('', 'restart', `Retry ${failed.length} failed`, async () => {
          const plan = { ...v.plan, specs: v.plan.specs.filter((s) => failed.some((f) => f.name === s.name)), files: [], replace: true, images: v.plan.images || 'auto' };
          try {
            const next = await call('CloneStart', v.targetId, plan);
            actions.show(next);
          } catch (e) { toast(e.message, true); }
        }, { title: 'Create the failed containers again (a half-created one is replaced)' }) : '',
        actions.editAgain ? btn('ghost', 'edit', 'Edit and run again', actions.editAgain, { title: 'Back to the variables of this clone' }) : '',
        btn('ghost', 'server', `Open ${v.target}`, () => actions.openServer(v.targetId)),
        btn('primary', 'plus', 'New clone', actions.newClone),
      ];
    foot.replaceChildren(...left, h('span', { class: 'spacer' }), ...right);
  }

  let lastState = '';
  function render() {
    const c = counts(v);
    const running = v.state === 'running';
    title.textContent = running ? `Cloning ${c.total} container${c.total === 1 ? '' : 's'}`
      : { done: 'Clone completed', warning: 'Clone completed with issues', failed: 'Clone failed', cancelled: 'Clone cancelled' }[v.state] || 'Clone';
    route.replaceChildren(h('span', {}, v.source), icon('arrowRight'), h('b', {}, v.target));
    pill.textContent = v.cancelling ? 'Cancelling' : STATE_LABEL[v.state] || v.state;
    pill.className = `cp-pill s-${v.state}`;
    elapsed.textContent = fmtDur((v.ended || Date.now()) - v.started);
    bar.className = `jc-bar cp-bar s-${v.state}${running ? ' running' : ''}`;
    bar.firstChild.style.width = `${v.percent}%`;
    pct.textContent = `${Math.floor(v.percent)}%`;
    err.hidden = !v.error;
    err.textContent = v.error || '';
    renderPhases();
    renderNow();
    const live = v.phase === 'done' || !running ? 'Running' : 'Started';
    const metric = (label, value, tone = '') => h('div', { class: `jc-metric ${tone}` }, h('span', { class: 'jc-metric-label' }, label), h('span', { class: 'jc-metric-value' }, value));
    metrics.replaceChildren(
      metric('Containers', c.total),
      metric(live, c.running, c.running ? 'good' : ''),
      metric('Need attention', c.failed + c.attention, c.failed + c.attention ? 'bad' : ''),
      metric('Skipped', c.skipped),
      metric(running ? 'Remaining' : 'Not run', c.waiting + c.cancelled));
    const problems = c.failed + c.attention;
    const labels = { all: `All ${c.total}`, problems: `Problems ${problems}`, running: `${live} ${c.running}` };
    filterBtns.forEach((b, i) => { b.textContent = labels[filters[i]]; b.classList.toggle('active', filter === filters[i]); b.classList.toggle('has-issues', filters[i] === 'problems' && problems > 0); });
    const nodes = [];
    v.containers.forEach((x, i) => { rows[i].update(x); if (rows[i].matches(x)) nodes.push(...rows[i].nodes); });
    if (nodes.length !== tbody.childNodes.length || nodes.some((n, i) => tbody.childNodes[i] !== n)) {
      tbody.replaceChildren(...(nodes.length ? nodes : [h('tr', {}, h('td', { colspan: 5, class: 'empty' }, 'Nothing here.'))]));
    }
    if (!others.querySelector('.cp-full')) renderOthers();
    // The footer only changes with the state (redrawing it on every event would steal the focus of its buttons).
    const footKey = `${v.state}|${v.cancelling}`;
    if (footKey !== lastState) { renderFoot(); lastState = footKey; }
  }

  const onJob = (next) => { if (next.id === id) { v = next; render(); } };
  listeners.add(onJob);
  // The timers (elapsed time, health check countdown) move between events.
  const timer = setInterval(() => { if (v.state === 'running') { elapsed.textContent = fmtDur(Date.now() - v.started); renderPhases(); } }, 1000);
  render();
  // A problem is the first thing to see once it is over.
  if (v.state !== 'running' && counts(v).failed + counts(v).attention > 0) { filter = 'problems'; render(); }
  return () => { listeners.delete(onJob); clearInterval(timer); if (shown === id) shown = null; };
}
