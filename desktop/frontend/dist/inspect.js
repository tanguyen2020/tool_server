import { h, toast, fmtBytes, statusChip } from './util.js';
import { call, copyText } from './bridge.js';

const $ = (id) => document.getElementById(id);
const dialog = $('inspect-dialog');
const body = $('inspect-body');
const reveal = $('inspect-reveal');
const tabs = [...dialog.querySelectorAll('.tab')];

let current = null; // { serverId, serverName, name, data }
let tab = 'overview';
let loadSeq = 0;

// Environment variables whose name suggests a credential are masked until "Show secret values" is ticked.
const SECRET_RE = /pass|secret|token|key|credential|auth|private|cert|salt/i;
const MASK = '••••••••';

const isZeroTime = (s) => !s || s.startsWith('0001-01-01');
const fmtDate = (s) => (isZeroTime(s) ? '–' : new Date(s).toLocaleString('en-GB', { hour12: false }));
const join = (v) => (Array.isArray(v) ? v.join(' ') : v || '');

function maskEnv(env = []) {
  return env.map((line) => {
    const i = line.indexOf('=');
    const key = i < 0 ? line : line.slice(0, i);
    return !reveal.checked && i >= 0 && SECRET_RE.test(key) ? `${key}=${MASK}` : line;
  });
}

// ---------------------------------------------------------------- building blocks
function section(title, content, { full = false, right = '' } = {}) {
  return h('section', { class: `insp-section${full ? ' full' : ''}` },
    h('h3', {}, title, right ? h('span', { class: 'right' }, right) : null), content);
}

// rows: [label, value, { mono }]; empty values are skipped.
function kvGrid(rows) {
  const items = rows.filter(([, v]) => v !== '' && v != null && v !== false);
  if (!items.length) return h('div', { class: 'empty' }, 'None');
  return h('dl', { class: 'kv-grid' }, items.flatMap(([k, v, opts = {}]) => [
    h('dt', {}, k),
    h('dd', { class: opts.mono ? 'mono' : '' }, v instanceof Node ? v : String(v)),
  ]));
}

function table(headers, rows, empty) {
  if (!rows.length) return h('div', { class: 'empty' }, empty);
  return h('div', { class: 'table-wrap' }, h('table', {},
    h('thead', {}, h('tr', {}, headers.map((x) => h('th', {}, x)))),
    h('tbody', {}, rows.map((r) => h('tr', {}, r.map((cell) => h('td', {}, cell instanceof Node ? cell : String(cell ?? '')))))),
  ));
}

const mono = (text) => h('span', { class: 'mono' }, text);

// ---------------------------------------------------------------- overview
function overview(d) {
  const cfg = d.Config || {};
  const hc = d.HostConfig || {};
  const st = d.State || {};
  const ns = d.NetworkSettings || {};
  const restart = hc.RestartPolicy?.Name
    ? `${hc.RestartPolicy.Name}${hc.RestartPolicy.MaximumRetryCount ? ` (max ${hc.RestartPolicy.MaximumRetryCount})` : ''}`
    : 'no';

  const general = kvGrid([
    ['ID', d.Id, { mono: true }],
    ['Image', cfg.Image],
    ['Image ID', d.Image, { mono: true }],
    ['Created', fmtDate(d.Created)],
    ['Command', [d.Path, ...(d.Args || [])].join(' '), { mono: true }],
    ['Entrypoint', join(cfg.Entrypoint), { mono: true }],
    ['Working dir', cfg.WorkingDir, { mono: true }],
    ['User', cfg.User],
    ['Hostname', cfg.Hostname],
    ['Restart policy', restart],
    ['Network mode', hc.NetworkMode],
    ['Log driver', hc.LogConfig?.Type],
    ['Privileged', hc.Privileged ? 'yes' : ''],
    ['Compose project', cfg.Labels?.['com.docker.compose.project']],
    ['Compose service', cfg.Labels?.['com.docker.compose.service']],
  ]);

  const health = st.Health;
  const state = kvGrid([
    ['Status', statusChip(st.Status)],
    ['Health', health ? `${health.Status}${health.FailingStreak ? ` (${health.FailingStreak} failing)` : ''}` : ''],
    ['Started', fmtDate(st.StartedAt)],
    ['Finished', st.Running ? '' : fmtDate(st.FinishedAt)],
    ['Exit code', st.Running ? '' : String(st.ExitCode ?? '')],
    ['Restart count', String(d.RestartCount ?? 0)],
    ['PID', st.Pid ? String(st.Pid) : ''],
    ['OOM killed', st.OOMKilled ? 'yes' : ''],
    ['Error', st.Error],
  ]);

  const resources = kvGrid([
    ['Memory limit', hc.Memory ? fmtBytes(hc.Memory) : 'unlimited'],
    ['Memory reservation', hc.MemoryReservation ? fmtBytes(hc.MemoryReservation) : ''],
    ['Memory + swap', hc.MemorySwap > 0 ? fmtBytes(hc.MemorySwap) : ''],
    ['CPUs', hc.NanoCpus ? String(hc.NanoCpus / 1e9) : 'unlimited'],
    ['CPU shares', hc.CpuShares ? String(hc.CpuShares) : ''],
    ['CPU set', hc.CpusetCpus],
    ['PIDs limit', hc.PidsLimit > 0 ? String(hc.PidsLimit) : ''],
  ]);

  const ports = Object.entries(ns.Ports || {}).map(([port, binds]) => [
    mono(port),
    binds?.length ? mono(binds.map((b) => `${b.HostIp || '0.0.0.0'}:${b.HostPort}`).join(', ')) : h('span', { class: 'muted' }, 'not published'),
  ]);

  const networks = Object.entries(ns.Networks || {}).map(([name, n]) => [
    name,
    mono(n.IPAddress ? `${n.IPAddress}/${n.IPPrefixLen}` : '–'),
    mono(n.Gateway || '–'),
    mono(n.MacAddress || '–'),
    (n.Aliases || []).join(', '),
  ]);

  const mounts = (d.Mounts || []).map((m) => [
    m.Type,
    mono(m.Type === 'volume' && m.Name ? m.Name : m.Source),
    mono(m.Destination),
    m.RW ? 'rw' : 'ro',
  ]);

  const env = maskEnv(cfg.Env).map((line) => {
    const i = line.indexOf('=');
    const val = i < 0 ? '' : line.slice(i + 1);
    return [mono(i < 0 ? line : line.slice(0, i)), val === MASK ? h('span', { class: 'masked' }, MASK) : mono(val)];
  });

  const labels = Object.entries(cfg.Labels || {}).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => [mono(k), mono(v)]);

  const healthLog = (health?.Log || []).slice(-5).reverse().map((l) => [
    fmtDate(l.Start), String(l.ExitCode), mono((l.Output || '').trim()),
  ]);

  return h('div', { class: 'insp-grid' },
    section('General', general),
    section('State', state),
    section('Resources', resources),
    section('Ports', table(['Container', 'Host'], ports, 'No exposed ports')),
    section('Networks', table(['Network', 'IP', 'Gateway', 'MAC', 'Aliases'], networks, 'No networks'), { full: true }),
    section('Mounts', table(['Type', 'Source', 'Destination', 'Mode'], mounts, 'No mounts'), { full: true }),
    healthLog.length ? section('Health checks', table(['Time', 'Exit', 'Output'], healthLog, ''), { full: true, right: 'last 5' }) : null,
    section('Environment', table(['Name', 'Value'], env, 'No environment variables'), { full: true, right: `${env.length}` }),
    section('Labels', table(['Key', 'Value'], labels, 'No labels'), { full: true, right: `${labels.length}` }),
  );
}

// Raw JSON with the same masking as the overview.
function rawJSON(d) {
  const copy = structuredClone(d);
  if (copy.Config?.Env) copy.Config.Env = maskEnv(copy.Config.Env);
  return JSON.stringify(copy, null, 2);
}

// ---------------------------------------------------------------- dialog
function render() {
  tabs.forEach((t) => {
    t.classList.toggle('active', t.dataset.tab === tab);
    t.setAttribute('aria-selected', String(t.dataset.tab === tab));
  });
  if (!current?.data) return;
  const scroll = body.scrollTop;
  body.replaceChildren(tab === 'overview' ? overview(current.data) : h('pre', { class: 'json' }, rawJSON(current.data)));
  body.scrollTop = scroll;
}

async function load() {
  const seq = ++loadSeq;
  body.replaceChildren(h('div', { class: 'empty' }, 'Loading…'));
  try {
    const data = await call('InspectContainer', current.serverId, current.name);
    if (seq !== loadSeq) return;
    current.data = data;
    $('inspect-sub').textContent = `${current.serverName} · ${data.Config?.Image || ''} · ${data.Id?.slice(0, 12) || ''}`;
    render();
  } catch (err) {
    if (seq !== loadSeq) return;
    body.replaceChildren(h('div', { class: 'card-error' }, `Could not inspect ${current.name}: ${err.message}`));
  }
}

export function openInspect(serverSnap, container) {
  current = { serverId: serverSnap.id, serverName: serverSnap.name, name: container.name, data: null };
  tab = 'overview';
  reveal.checked = false;
  $('inspect-title').textContent = container.name;
  $('inspect-sub').textContent = serverSnap.name;
  render();
  dialog.showModal();
  load();
}

tabs.forEach((t) => t.addEventListener('click', () => { tab = t.dataset.tab; body.scrollTop = 0; render(); }));
reveal.addEventListener('change', render);
$('inspect-refresh').addEventListener('click', () => current && load());
$('inspect-close').addEventListener('click', () => dialog.close());
$('inspect-copy').addEventListener('click', async () => {
  if (!current?.data) return;
  try {
    await copyText(rawJSON(current.data));
    toast(reveal.checked ? 'Inspect JSON copied' : 'Inspect JSON copied (secret values masked)');
  } catch (err) {
    toast(`Could not copy: ${err.message}`, true);
  }
});
// Clicking the backdrop closes the dialog.
dialog.addEventListener('click', (e) => { if (e.target === dialog) dialog.close(); });
