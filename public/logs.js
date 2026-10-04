import { h } from './util.js';

const MAX_LINES = 10000;
const ANSI_RE = /\x1b\[[0-9;?]*[A-Za-z]/g;
const TS_RE = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z) /;

const $ = (id) => document.getElementById(id);
const panel = $('logs');
const body = $('logs-body');
const filterInput = $('logs-filter');
const pauseBtn = $('logs-pause');
const jumpBtn = $('logs-jump');

let send = () => {};
let counter = 0;
let current = null; // { sid, server, container, name, ended }
let lines = [];
let queued = [];
let pending = '';
let paused = false;
let lastTs = null;

// So sánh timestamp của docker (RFC3339Nano, phần thập phân có thể bị cắt số 0).
const normTs = (ts) => ts.replace(/(?:\.(\d+))?Z$/, (_, f = '') => `.${f.padEnd(9, '0')}Z`);

function atBottom() {
  return body.scrollHeight - body.scrollTop - body.clientHeight < 40;
}

function matches(line) {
  const q = filterInput.value.trim().toLowerCase();
  return !q || line.text.toLowerCase().includes(q) || (line.ts || '').includes(q);
}

function renderLine(line) {
  const el = h('div', { class: line.sys ? 'sys' : '' });
  if (line.ts) el.append(h('span', { class: 'ts' }, line.ts.replace('T', ' ').replace(/\.\d+Z$/, '')));
  const q = filterInput.value.trim();
  if (!q || line.sys) {
    el.append(line.text);
  } else {
    // Tô sáng phần khớp bộ lọc.
    const lower = line.text.toLowerCase();
    const ql = q.toLowerCase();
    let i = 0;
    let j;
    while ((j = lower.indexOf(ql, i)) !== -1) {
      el.append(line.text.slice(i, j), h('mark', {}, line.text.slice(j, j + q.length)));
      i = j + q.length;
    }
    el.append(line.text.slice(i));
  }
  line.el = el;
  return el;
}

function append(newLines) {
  if (!newLines.length) return;
  const stick = atBottom();
  lines.push(...newLines);
  if (lines.length > MAX_LINES) {
    for (const old of lines.splice(0, lines.length - MAX_LINES)) old.el?.remove();
  }
  const frag = document.createDocumentFragment();
  for (const l of newLines) if (matches(l)) frag.append(renderLine(l));
  body.append(frag);
  if (stick) body.scrollTop = body.scrollHeight;
  else jumpBtn.hidden = false;
}

function rerender() {
  body.replaceChildren(...lines.filter(matches).map(renderLine));
  body.scrollTop = body.scrollHeight;
  jumpBtn.hidden = true;
}

function sysLine(text) {
  const line = { sys: true, text: `── ${text}` };
  if (paused) queued.push(line);
  else append([line]);
}

function ingest(data) {
  pending += data;
  const parts = pending.split('\n');
  pending = parts.pop();
  const out = [];
  for (let raw of parts) {
    raw = raw.replace(ANSI_RE, '').replace(/\r/g, '');
    const m = TS_RE.exec(raw);
    const ts = m?.[1] || null;
    // Khi nối lại bằng --since, bỏ các dòng đã nhận rồi.
    if (ts && lastTs && normTs(ts) <= lastTs) continue;
    if (ts) lastTs = normTs(ts);
    out.push({ ts, text: m ? raw.slice(m[0].length) : raw });
  }
  if (paused) queued.push(...out);
  else append(out);
}

function start({ since } = {}) {
  current.sid = String(++counter);
  current.ended = false;
  send({ t: 'logs.start', sid: current.sid, server: current.server, container: current.container, tail: $('logs-tail').value, since });
}

function stop() {
  if (current && !current.ended) send({ t: 'logs.stop', sid: current.sid });
}

export function openLogs(serverSnap, container) {
  stop();
  current = { server: serverSnap.id, container: container.name, name: container.name, ended: true, retries: 0 };
  lines = [];
  queued = [];
  pending = '';
  lastTs = null;
  paused = false;
  pauseBtn.textContent = 'Tạm dừng';
  body.replaceChildren();
  jumpBtn.hidden = true;
  $('logs-name').textContent = container.name;
  $('logs-server').textContent = `@ ${serverSnap.name}`;
  $('logs-state').textContent = container.state;
  $('logs-state').className = 'chip';
  panel.hidden = false;
  start();
}

function close() {
  stop();
  current = null;
  panel.hidden = true;
}

export function initLogs(sendFn) {
  send = sendFn;
  $('logs-close').addEventListener('click', close);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !panel.hidden && !document.querySelector('dialog[open]')) close(); });
  $('logs-clear').addEventListener('click', () => { lines = []; body.replaceChildren(); });
  $('logs-wrap').addEventListener('change', (e) => body.classList.toggle('wrap', e.target.checked));
  const tsToggle = $('logs-ts');
  const applyTs = () => body.classList.toggle('hide-ts', !tsToggle.checked);
  tsToggle.addEventListener('change', applyTs);
  applyTs();
  filterInput.addEventListener('input', rerender);
  $('logs-tail').addEventListener('change', () => {
    if (!current) return;
    stop();
    lines = []; queued = []; pending = ''; lastTs = null;
    body.replaceChildren();
    start();
  });
  pauseBtn.addEventListener('click', () => {
    paused = !paused;
    pauseBtn.textContent = paused ? `Tiếp tục${queued.length ? ` (${queued.length})` : ''}` : 'Tạm dừng';
    if (!paused) { append(queued); queued = []; }
  });
  setInterval(() => { if (paused) pauseBtn.textContent = `Tiếp tục (${queued.length})`; }, 500);
  jumpBtn.addEventListener('click', () => { body.scrollTop = body.scrollHeight; jumpBtn.hidden = true; });
  body.addEventListener('scroll', () => { if (atBottom()) jumpBtn.hidden = true; });
  $('logs-download').addEventListener('click', () => {
    if (!current) return;
    const text = lines.map((l) => (l.ts ? `${l.ts} ${l.text}` : l.text)).join('\n');
    const a = h('a', { href: URL.createObjectURL(new Blob([text], { type: 'text/plain' })), download: `${current.name}-${new Date().toISOString().slice(0, 19).replace(/:/g, '')}.log` });
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  });
}

export function onLogsMessage(msg) {
  if (!current || msg.sid !== current.sid) return;
  if (msg.t === 'logs.data') { current.retries = 0; ingest(msg.data); }
  else if (msg.t === 'logs.end') {
    if (pending) ingest('\n');
    current.ended = true;
    sysLine(msg.reason || 'Stream kết thúc');
  }
}

// Gọi mỗi khi có snapshot mới: cập nhật trạng thái và tự nối lại khi container chạy lại.
export function onLogsSnapshot(snap) {
  if (!current || snap.id !== current.server) return;
  const c = snap.docker?.containers?.find((x) => x.name === current.container);
  const chip = $('logs-state');
  chip.textContent = c?.state || 'không tìm thấy';
  chip.className = `chip ${c?.state === 'running' ? 'good' : c ? 'critical' : ''}`;
  if (current.ended && c?.state === 'running' && snap.status === 'online' && current.retries < 5) {
    current.retries++;
    sysLine('Container đang chạy lại, tiếp tục theo dõi log');
    start({ since: lastTs || undefined });
  }
}

// WebSocket rớt -> server đã huỷ stream; đánh dấu để nối lại khi có snapshot kế tiếp.
export function onLogsDisconnect() {
  if (current && !current.ended) {
    current.ended = true;
    sysLine('Mất kết nối tới dashboard, đang thử lại…');
  }
}
