// Container log viewer, laid out like Grafana's logs panel: a log-volume histogram by level,
// level detection with a colored stripe per line, level filters, search highlight and expandable log details.
// Every server tab has its own log session; streams keep running while their tab is hidden.
// The dock (dock.js) owns the panel; this module renders into the Logs pane while it is shown.
import { h, toast, fmtTime } from './util.js';
import { call, on, copyText } from './bridge.js';
import { cssVar, setupCanvas, niceMax } from './chart.js';

const MAX_LINES = 10000;
const ANSI_RE = /\x1b\[[0-9;?]*[A-Za-z]/g;
const TS_RE = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z) /;

// Severity order: errors are drawn at the bottom of the volume bars so they are always visible.
const LEVELS = ['error', 'warn', 'info', 'debug', 'unknown'];
const LEVEL_LABEL = { error: 'Error', warn: 'Warning', info: 'Info', debug: 'Debug', unknown: 'Other' };
const LEVEL_SHORT = { error: 'ERROR', warn: 'WARN', info: 'INFO', debug: 'DEBUG', unknown: '' };
const LEVEL_COLOR = { error: '--critical', warn: '--warning', info: '--good', debug: '--series-1', unknown: '--lvl-unknown' };
const LEVEL_KEYS = ['level', 'lvl', 'severity', 'loglevel', 'log.level', 'levelname', '@level', 'log_level'];
const WORD_LEVELS = [
  ['error', /\b(emerg|emergency|fatal|panic|alert|crit|critical|error|err|severe)\b/i],
  ['warn', /\b(warn|warning)\b/i],
  ['info', /\b(info|information|informational|notice)\b/i],
  ['debug', /\b(debug|dbug|trace|verbose)\b/i],
];

const $ = (id) => document.getElementById(id);
const panel = $('logs');
const body = $('logs-body');
const filterInput = $('logs-filter');
const levelBox = $('logs-levels');
const pauseBtn = $('logs-pause');
const jumpBtn = $('logs-jump');
const volCanvas = $('logs-volume');
const volTip = $('logs-volume-tip');
const tailSel = $('logs-tail');
const tsBox = $('logs-ts');
const wrapBox = $('logs-wrap');
const jsonBox = $('logs-json');

const sessions = new Map(); // tab key (server id) -> session
let S = null; // the session of the active server tab
let paneShown = false; // the Logs pane is visible in the dock
let counter = 0;
let frame = 0;
let volBuckets = null;

function newSession(serverSnap, container) {
  return {
    key: serverSnap.id, server: serverSnap.id, serverName: serverSnap.name, container: container.name, state: container.state,
    sid: null, ended: true, retries: 0, lines: [], queued: [], pending: '', paused: false, lastTs: null,
    hidden: new Set(), filter: '', tail: tailSel.value, ts: true, wrap: true, json: false, scrollTop: null,
  };
}

const findBySid = (sid) => [...sessions.values()].find((s) => s.sid === sid);
const isShown = (s) => s && s === S && paneShown && !panel.hidden;
const notifyDock = (key) => document.dispatchEvent(new CustomEvent('dock:changed', { detail: { key } }));

// ---------------------------------------------------------------- parsing

// Compare docker timestamps (RFC3339Nano, the fraction may have trailing zeros trimmed).
const normTs = (ts) => ts.replace(/(?:\.(\d+))?Z$/, (_, f = '') => `.${f.padEnd(9, '0')}Z`);

function normLevel(v) {
  if (typeof v === 'number') return v >= 50 ? 'error' : v >= 40 ? 'warn' : v >= 30 ? 'info' : 'debug'; // pino / bunyan
  const s = String(v).trim().toLowerCase();
  if (/^(emerg|emergency|fatal|panic|alert|crit|critical|err|error|e|severe)$/.test(s)) return 'error';
  if (/^(warn|warning|w)$/.test(s)) return 'warn';
  if (/^(info|information|informational|notice|i)$/.test(s)) return 'info';
  if (/^(debug|dbg|dbug|trace|verbose|fine|finer|finest|d|t)$/.test(s)) return 'debug';
  return null;
}

function flatten(obj, prefix = '', out = {}, depth = 0) {
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === 'object' && !Array.isArray(v) && depth < 3) flatten(v, key, out, depth + 1);
    else out[key] = typeof v === 'object' ? JSON.stringify(v) : String(v);
  }
  return out;
}

// JSON or logfmt fields, or null for plain text.
function parseFields(text) {
  const t = text.trim();
  if (t.startsWith('{') && t.endsWith('}')) {
    try {
      const json = JSON.parse(t);
      if (json && typeof json === 'object') return { json, fields: flatten(json) };
    } catch {}
  }
  const pairs = [...t.matchAll(/(?:^|\s)([A-Za-z_][\w.-]*)=("(?:[^"\\]|\\.)*"|[^\s"]*)/g)];
  if (pairs.length >= 2) {
    return { fields: Object.fromEntries(pairs.map((m) => [m[1], m[2].startsWith('"') ? m[2].slice(1, -1).replace(/\\"/g, '"') : m[2]])) };
  }
  return null;
}

function detectLevel(text, parsed) {
  if (parsed) {
    for (const k of LEVEL_KEYS) {
      const raw = parsed.json?.[k] ?? parsed.fields[k];
      if (raw != null) {
        const lvl = normLevel(raw);
        if (lvl) return lvl;
      }
    }
  }
  const head = text.slice(0, 200);
  // A level token as loggers print it; the first one in the line wins (the message may mention "error" later).
  let best = null;
  for (const re of TOKEN_RES) {
    const m = re.exec(head);
    if (m && (!best || m.index < best.index)) best = m;
  }
  if (best) return TOKEN_LEVEL[(best[1] || best[2]).toLowerCase()];
  for (const [lvl, re] of WORD_LEVELS) if (re.test(head)) return lvl;
  return 'unknown';
}

// Level tokens of common loggers: Serilog [INF] [WRN] [ERR] [FTL] [DBG] [VRB], .NET console "info:" "fail:" "trce:",
// log4j / NLog / Python INFO WARN ERROR, nginx [warn], glog-style [I] is too ambiguous and left out.
const TOKEN_LEVEL = {
  inf: 'info', info: 'info', information: 'info', notice: 'info',
  wrn: 'warn', warn: 'warn', warning: 'warn',
  err: 'error', error: 'error', fail: 'error', ftl: 'error', fatal: 'error', crit: 'error', critical: 'error',
  emerg: 'error', alert: 'error', panic: 'error', severe: 'error',
  dbg: 'debug', dbug: 'debug', debug: 'debug', vrb: 'debug', verbose: 'debug', trce: 'debug', trace: 'debug',
};
const TOKEN_NAMES = Object.keys(TOKEN_LEVEL).join('|');
const TOKEN_RES = [
  new RegExp(`[[(<|]\\s*(${TOKEN_NAMES})\\s*[\\])>|]`, 'i'), // [INF] (warn) <error> |ERROR|
  new RegExp(`^\\s*(${TOKEN_NAMES})(?=:\\s)`, 'i'), // .NET console: "info: Microsoft.Hosting…"
  /\b(INF|INFO|NOTICE|WRN|WARN|WARNING|ERR|ERROR|FAIL|FTL|FATAL|CRIT|CRITICAL|DBG|DEBUG|VRB|VERBOSE|TRACE)\b/, // upper case only
];

// Lines that continue the previous entry (exception stack traces, wrapped messages) take its level.
const CONTINUATION_RE = /^(\s+\S|\s*at\s|\s*---|\s*Caused by:|\s*\.\.\. \d+ more|\s*Traceback|\s*File ")/;

function makeLine(ts, text, prevLevel = null) {
  const parsed = parseFields(text);
  let level = detectLevel(text, parsed);
  if (level === 'unknown' && prevLevel && prevLevel !== 'unknown' && CONTINUATION_RE.test(text)) level = prevLevel;
  return { ts, t: ts ? Date.parse(ts) : null, text, parsed, level };
}

// ---------------------------------------------------------------- rendering (active session only)

function atBottom() {
  return body.scrollHeight - body.scrollTop - body.clientHeight < 40;
}

function matches(line) {
  if (line.sys) return true;
  if (S.hidden.has(line.level)) return false;
  const q = S.filter.trim().toLowerCase();
  return !q || line.text.toLowerCase().includes(q);
}

function highlighted(text) {
  const q = S.filter.trim();
  if (!q) return [text];
  const out = [];
  const lower = text.toLowerCase();
  const ql = q.toLowerCase();
  let i = 0;
  let j;
  while ((j = lower.indexOf(ql, i)) !== -1) {
    out.push(text.slice(i, j), h('mark', {}, text.slice(j, j + q.length)));
    i = j + q.length;
  }
  out.push(text.slice(i));
  return out;
}

function fmtLogTime(t) {
  const ms = String(new Date(t).getMilliseconds()).padStart(3, '0');
  return `${fmtTime(t)}.${ms}`;
}

function renderLine(line) {
  if (line.sys) {
    line.el = h('div', { class: 'log-line sys' }, h('span', { class: 'log-msg' }, `── ${line.text}`));
    return line.el;
  }
  const prettify = S.json && line.parsed?.json;
  const msg = prettify ? JSON.stringify(line.parsed.json, null, 2) : line.text;
  const el = h('div', { class: `log-line lvl-${line.level}${prettify ? ' pretty' : ''}`, tabindex: '-1' },
    h('span', { class: 'log-time' }, line.t ? fmtLogTime(line.t) : ''),
    h('span', { class: 'log-level' }, LEVEL_SHORT[line.level]),
    h('span', { class: 'log-msg' }, highlighted(msg)));
  el.addEventListener('click', (e) => {
    if (window.getSelection()?.toString()) return; // selecting text, not opening details
    if (e.target.closest('.log-details')) return;
    toggleDetails(line);
  });
  line.el = el;
  return el;
}

function toggleDetails(line) {
  const open = line.el.nextElementSibling?.classList.contains('log-details');
  if (open) {
    line.el.nextElementSibling.remove();
    line.el.classList.remove('expanded');
    return;
  }
  const fields = line.parsed?.fields || {};
  const keys = Object.keys(fields);
  const details = h('div', { class: 'log-details' },
    h('div', { class: 'log-details-head' },
      h('strong', {}, 'Log details'),
      h('span', { class: 'muted' }, `${LEVEL_LABEL[line.level]} · ${line.ts || 'no timestamp'}`),
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn sm', onclick: async () => {
        try { await copyText(line.text); toast('Log line copied'); } catch (err) { toast(err.message, true); }
      } }, 'Copy line')),
    keys.length
      ? h('table', { class: 'mini-table' }, h('tbody', {}, keys.map((k) => h('tr', {}, h('td', { class: 'field-key' }, k), h('td', { class: 'field-val' }, fields[k])))))
      : h('div', { class: 'muted' }, 'No fields detected (the line is neither JSON nor key=value).'));
  line.el.after(details);
  line.el.classList.add('expanded');
}

function append(session, newLines) {
  if (!newLines.length) return;
  const shown = isShown(session);
  const stick = shown && atBottom();
  session.lines.push(...newLines);
  if (session.lines.length > MAX_LINES) {
    for (const old of session.lines.splice(0, session.lines.length - MAX_LINES)) {
      if (shown && old.el?.nextElementSibling?.classList.contains('log-details')) old.el.nextElementSibling.remove();
      if (shown) old.el?.remove();
    }
  }
  if (!shown) return; // rebuilt when its tab is shown again
  const frag = document.createDocumentFragment();
  for (const l of newLines) if (matches(l)) frag.append(renderLine(l));
  body.append(frag);
  if (stick) body.scrollTop = body.scrollHeight;
  else jumpBtn.hidden = false;
  scheduleSummary();
}

function rerender({ keepScroll = false } = {}) {
  if (!S) return;
  body.replaceChildren(...S.lines.filter(matches).map(renderLine));
  if (keepScroll && S.scrollTop != null) body.scrollTop = S.scrollTop;
  else body.scrollTop = body.scrollHeight;
  jumpBtn.hidden = atBottom();
  scheduleSummary();
}

// ---------------------------------------------------------------- level chips and volume histogram

function scheduleSummary() {
  cancelAnimationFrame(frame);
  frame = requestAnimationFrame(() => {
    if (!S) return;
    renderLevels();
    drawVolume();
  });
}

function renderLevels() {
  const counts = Object.fromEntries(LEVELS.map((l) => [l, 0]));
  for (const l of S.lines) if (!l.sys) counts[l.level]++;
  const shown = S.lines.filter((l) => !l.sys && matches(l)).length;
  const total = S.lines.filter((l) => !l.sys).length;
  $('logs-count').textContent = shown === total ? `${total} lines` : `${shown} of ${total} lines`;
  levelBox.replaceChildren(...LEVELS.filter((l) => counts[l] || S.hidden.has(l)).map((l) => {
    const dot = h('i');
    dot.style.background = `var(${LEVEL_COLOR[l]})`;
    const off = S.hidden.has(l);
    return h('button', {
      type: 'button',
      class: `lvl-chip${off ? ' off' : ''}`,
      'aria-pressed': String(!off),
      title: off ? `Show ${LEVEL_LABEL[l].toLowerCase()} lines` : `Hide ${LEVEL_LABEL[l].toLowerCase()} lines`,
      onclick: () => {
        if (S.hidden.has(l)) S.hidden.delete(l);
        else S.hidden.add(l);
        rerender();
      },
    }, dot, LEVEL_LABEL[l], h('b', {}, counts[l]));
  }));
}

const STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400].map((s) => s * 1000);

function drawVolume() {
  const { ctx, width, height } = setupCanvas(volCanvas);
  volBuckets = null;
  if (!width || !S) return;
  const timed = S.lines.filter((l) => !l.sys && l.t != null && !S.hidden.has(l.level));
  if (timed.length < 2) return;
  const t0 = timed[0].t;
  const t1 = timed.at(-1).t;
  const step = STEPS.find((s) => (t1 - t0) / s <= 60) || STEPS.at(-1);
  const start = t0 - (t0 % step);
  const n = Math.floor((t1 - start) / step) + 1;
  const buckets = Array.from({ length: n }, (_, i) => ({ t: start + i * step, counts: Object.fromEntries(LEVELS.map((l) => [l, 0])) }));
  for (const l of timed) buckets[Math.floor((l.t - start) / step)].counts[l.level]++;
  const peak = Math.max(...buckets.map((b) => LEVELS.reduce((a, l) => a + b.counts[l], 0)));
  const max = niceMax(peak);
  const pad = { l: 34, r: 6, t: 4, b: 16 };
  const pw = width - pad.l - pad.r;
  const ph = height - pad.t - pad.b;
  const bw = pw / n;

  ctx.font = '10px system-ui, -apple-system, "Segoe UI", sans-serif';
  ctx.textAlign = 'right';
  ctx.textBaseline = 'middle';
  ctx.fillStyle = cssVar('--muted');
  ctx.fillText(String(max), pad.l - 6, pad.t + 4);
  ctx.fillText('0', pad.l - 6, pad.t + ph - 2);
  ctx.strokeStyle = cssVar('--axis');
  ctx.beginPath(); ctx.moveTo(pad.l, pad.t + ph + 0.5); ctx.lineTo(width - pad.r, pad.t + ph + 0.5); ctx.stroke();

  buckets.forEach((b, i) => {
    let y = pad.t + ph;
    const x = pad.l + i * bw;
    for (const l of LEVELS) {
      const c = b.counts[l];
      if (!c) continue;
      const hgt = (c / max) * ph;
      ctx.fillStyle = cssVar(LEVEL_COLOR[l]);
      ctx.fillRect(x + 0.5, y - hgt, Math.max(1, bw - 2), hgt);
      y -= hgt;
    }
  });
  // Time labels under the bars (first, middle, last bucket).
  ctx.textBaseline = 'top';
  ctx.fillStyle = cssVar('--muted');
  const fmt = (t) => new Date(t).toLocaleTimeString('en-GB', { hour12: false, ...(t1 - t0 < 3600e3 ? {} : { hour: '2-digit', minute: '2-digit' }) });
  [[0, 'left'], [Math.floor(n / 2), 'center'], [n - 1, 'right']].forEach(([i, align], k, all) => {
    if (k && i === all[k - 1][0]) return;
    ctx.textAlign = align;
    const x = align === 'left' ? pad.l : align === 'right' ? width - pad.r : pad.l + (i + 0.5) * bw;
    ctx.fillText(fmt(buckets[i].t), x, pad.t + ph + 3);
  });
  volBuckets = { buckets, step, pad, bw };
}

volCanvas.addEventListener('pointermove', (e) => {
  if (!volBuckets) return;
  const { buckets, step, pad, bw } = volBuckets;
  const i = Math.floor((e.offsetX - pad.l) / bw);
  const b = buckets[i];
  if (!b) { volTip.hidden = true; return; }
  const rows = LEVELS.filter((l) => b.counts[l]).map((l) => {
    const key = h('i');
    key.style.background = `var(${LEVEL_COLOR[l]})`;
    return h('div', { class: 'row' }, key, h('b', {}, b.counts[l]), h('span', {}, LEVEL_LABEL[l]));
  });
  volTip.replaceChildren(h('div', { class: 't' }, `${fmtTime(b.t)} – ${fmtTime(b.t + step)}`), ...(rows.length ? rows : [h('div', { class: 'muted' }, 'No lines')]));
  volTip.hidden = false;
  const x = pad.l + i * bw;
  const tw = volTip.offsetWidth;
  volTip.style.left = `${x + bw + 8 + tw > volCanvas.clientWidth ? x - tw - 8 : x + bw + 8}px`;
  volTip.style.top = '0px';
});
volCanvas.addEventListener('pointerleave', () => { volTip.hidden = true; });

// ---------------------------------------------------------------- stream

function sysLine(session, text) {
  const line = { sys: true, text, level: 'unknown' };
  if (session.paused) session.queued.push(line);
  else append(session, [line]);
}

function ingest(session, data) {
  session.pending += data;
  const parts = session.pending.split('\n');
  session.pending = parts.pop();
  const out = [];
  for (let raw of parts) {
    raw = raw.replace(ANSI_RE, '').replace(/\r/g, '');
    const m = TS_RE.exec(raw);
    const ts = m?.[1] || null;
    // When resuming with --since, skip lines already received.
    if (ts && session.lastTs && normTs(ts) <= session.lastTs) continue;
    if (ts) session.lastTs = normTs(ts);
    const line = makeLine(ts, m ? raw.slice(m[0].length) : raw, session.lastLevel);
    session.lastLevel = line.level;
    out.push(line);
  }
  if (session.paused) session.queued.push(...out);
  else append(session, out);
}

function start(session, { since = '' } = {}) {
  const sid = String(++counter);
  session.sid = sid;
  session.ended = false;
  call('StartLogs', sid, session.server, session.container, Number(session.tail), since).catch((err) => {
    if (session.sid !== sid) return;
    session.ended = true;
    sysLine(session, err.message);
  });
}

function stop(session) {
  if (session && !session.ended) call('StopLogs', session.sid);
  if (session) session.ended = true;
}

function resetLines(session) {
  session.lines = [];
  session.queued = [];
  session.pending = '';
  session.lastTs = null;
  session.scrollTop = null;
  if (isShown(session)) {
    body.replaceChildren();
    jumpBtn.hidden = true;
    scheduleSummary();
  }
}

// Mirror the session's options into the shared drawer controls.
function syncUI() {
  $('logs-name').textContent = S.container;
  $('logs-server').textContent = `@ ${S.serverName}`;
  $('logs-state').textContent = S.state || '';
  $('logs-state').className = `chip ${S.state === 'running' ? 'good' : S.state ? 'critical' : ''}`;
  filterInput.value = S.filter;
  tailSel.value = S.tail;
  tsBox.checked = S.ts;
  wrapBox.checked = S.wrap;
  jsonBox.checked = S.json;
  body.classList.toggle('hide-ts', !S.ts);
  body.classList.toggle('wrap', S.wrap);
  pauseBtn.textContent = S.paused ? `Resume${S.queued.length ? ` (${S.queued.length})` : ''}` : 'Pause';
}

// Opens the logs of a container in the tab of its server (replacing that tab's previous log session).
export function openLogs(serverSnap, container) {
  const old = sessions.get(serverSnap.id);
  if (old) stop(old);
  S = newSession(serverSnap, container);
  sessions.set(S.key, S);
  start(S);
  document.dispatchEvent(new CustomEvent('dock:show', { detail: { key: S.key, pane: 'logs' } }));
}

export const hasLogs = (key) => sessions.has(key);
export const logsTitle = (key) => sessions.get(key)?.container || '';

// The dock shows or hides the Logs pane.
export function showLogsPane() {
  paneShown = true;
  if (!S) return;
  syncUI();
  rerender({ keepScroll: true });
}
export function hideLogsPane() {
  if (S && paneShown) S.scrollTop = atBottom() ? null : body.scrollTop;
  paneShown = false;
  volTip.hidden = true;
}

// Called when the active app tab changes: select that tab's log session (the dock decides what is shown).
export function setLogsTab(key) {
  hideLogsPane();
  S = sessions.get(key) || null;
}

// The tab was closed: stop its stream.
export function closeTabLogs(key) {
  const s = sessions.get(key);
  if (!s) return;
  stop(s);
  sessions.delete(key);
  if (S === s) S = null;
  notifyDock(key);
}

export function initLogs() {
  on('logs.data', (msg) => {
    const s = findBySid(msg.sid);
    if (!s) return;
    s.retries = 0;
    ingest(s, msg.data);
  });
  on('logs.end', (msg) => {
    const s = findBySid(msg.sid);
    if (!s) return;
    if (s.pending) ingest(s, '\n');
    s.ended = true;
    sysLine(s, msg.reason || 'Stream ended');
  });

  new ResizeObserver(() => scheduleSummary()).observe(volCanvas);
  $('logs-clear').addEventListener('click', () => S && resetLines(S));
  wrapBox.addEventListener('change', () => { S.wrap = wrapBox.checked; body.classList.toggle('wrap', S.wrap); });
  tsBox.addEventListener('change', () => { S.ts = tsBox.checked; body.classList.toggle('hide-ts', !S.ts); });
  jsonBox.addEventListener('change', () => { S.json = jsonBox.checked; rerender(); });
  filterInput.addEventListener('input', () => { S.filter = filterInput.value; rerender(); });
  tailSel.addEventListener('change', () => {
    if (!S) return;
    S.tail = tailSel.value;
    stop(S);
    resetLines(S);
    start(S);
  });
  pauseBtn.addEventListener('click', () => {
    if (!S) return;
    S.paused = !S.paused;
    if (!S.paused) {
      const q = S.queued;
      S.queued = [];
      append(S, q);
    }
    syncUI();
  });
  setInterval(() => { if (S?.paused) pauseBtn.textContent = `Resume (${S.queued.length})`; }, 500);
  jumpBtn.addEventListener('click', () => { body.scrollTop = body.scrollHeight; jumpBtn.hidden = true; });
  body.addEventListener('scroll', () => { if (atBottom()) jumpBtn.hidden = true; });
  $('logs-download').addEventListener('click', async () => {
    if (!S) return;
    const text = S.lines.filter((l) => !l.sys).map((l) => (l.ts ? `${l.ts} ${l.text}` : l.text)).join('\n');
    const name = `${S.container}-${new Date().toISOString().slice(0, 19).replace(/:/g, '')}.log`;
    try {
      const path = await call('SaveLogFile', name, text);
      if (path) toast(`Saved ${path}`);
    } catch (err) {
      toast(`Could not save log: ${err.message}`, true);
    }
  });
}

// Called on every new snapshot: update container state and resume streams of containers that run again.
export function onLogsSnapshot(snap) {
  for (const s of sessions.values()) {
    if (s.server !== snap.id) continue;
    const c = snap.docker?.containers?.find((x) => x.name === s.container);
    s.state = c?.state || 'not found';
    if (isShown(s)) {
      const chip = $('logs-state');
      chip.textContent = s.state;
      chip.className = `chip ${c?.state === 'running' ? 'good' : c ? 'critical' : ''}`;
    }
    if (s.ended && c?.state === 'running' && snap.status === 'online' && s.retries < 5) {
      s.retries++;
      sysLine(s, 'Container is running again, resuming logs');
      start(s, { since: s.lastTs || '' });
    }
  }
}
