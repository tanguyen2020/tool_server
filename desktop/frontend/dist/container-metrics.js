import { h, fmtBytes, fmtRate, fmtTime } from './util.js';
import { SeriesPanel } from './series-chart.js';
import { eventText, isCriticalEvent } from './chart.js';
import { EXIT_MEANING } from './containers-table.js';
import { getJSONPref, setJSONPref } from './prefs.js';

const PREF_KEY = 'containerMetrics';
const DEFAULTS = { group: false, markers: true, net: 'total', io: 'total' };

function loadPrefs() {
  try {
    return { ...DEFAULTS, ...getJSONPref(PREF_KEY) };
  } catch {
    return { ...DEFAULTS };
  }
}

const fmtCpu = (v) => `${v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)}%`;
const fmtMem = (v) => fmtBytes(v).replace('.0 ', ' ');
const fmtRateShort = (v) => fmtRate(v).replace('.0 ', ' ');

function panel(title) {
  const right = h('span', { class: 'right' });
  const body = h('div');
  return { el: h('section', { class: 'panel span-12' }, h('h3', {}, title, right), body), right, body };
}

function segmented(label, options, value, onChange) {
  const buttons = options.map(([key, text]) => h('button', { type: 'button', class: 'tab', 'data-key': key }, text));
  const sync = (v) => buttons.forEach((b) => {
    b.classList.toggle('active', b.dataset.key === v);
    b.setAttribute('aria-pressed', String(b.dataset.key === v));
  });
  buttons.forEach((b) => b.addEventListener('click', () => { sync(b.dataset.key); onChange(b.dataset.key); }));
  sync(value);
  return h('div', { class: 'tabs', role: 'group', 'aria-label': label }, buttons);
}

// Total = a + b per sample; a missing side counts as 0 unless both are missing.
function combine(a, b, mode) {
  if (mode === 'a') return a;
  if (mode === 'b') return b;
  const out = new Map();
  for (const name of new Set([...a.keys(), ...b.keys()])) {
    const x = a.get(name);
    const y = b.get(name);
    const n = (x || y).length;
    out.set(name, Array.from({ length: n }, (_, i) => (x?.[i] == null && y?.[i] == null ? null : (x?.[i] ?? 0) + (y?.[i] ?? 0))));
  }
  return out;
}

// Sums the containers of each compose project into one series; standalone containers stay as they are.
function groupByProject(map, projectOf) {
  const out = new Map();
  for (const [name, values] of map) {
    const project = projectOf(name);
    const key = project ? `${project} (project)` : name;
    const acc = out.get(key);
    if (!acc) {
      out.set(key, values.slice());
      continue;
    }
    values.forEach((v, i) => { if (v != null) acc[i] = (acc[i] ?? 0) + v; });
  }
  return out;
}

const ACTION_LABEL = { die: 'exited', oom: 'out of memory', start: 'start', restart: 'restart', stop: 'stop', kill: 'kill', create: 'create', destroy: 'remove', pause: 'pause', unpause: 'unpause' };

function eventDetail(e) {
  if (e.action === 'die') return `code ${e.detail} · ${EXIT_MEANING[e.detail] || 'non-zero exit'}`;
  if (e.action === 'kill') return `signal ${e.detail}`;
  return '';
}

// The Container metrics tab. projectOf(name) maps a container to its compose project (or '').
export function containerMetricsTab({ projectOf }) {
  const prefs = loadPrefs();
  const save = () => setJSONPref(PREF_KEY, prefs);
  let src = null;
  let events = [];

  const groupBox = h('input', { type: 'checkbox' });
  groupBox.checked = prefs.group;
  const markerBox = h('input', { type: 'checkbox' });
  markerBox.checked = prefs.markers;
  const toolbar = h('div', { class: 'span-12 metrics-toolbar' },
    h('label', { class: 'check' }, groupBox, 'Group by compose project'),
    h('label', { class: 'check', title: 'Exits, OOM kills, health changes, starts and restarts' }, markerBox, 'Show container events on charts'));

  const cpuP = panel('Container CPU usage');
  const memP = panel('Container memory usage');
  const netP = panel('Container network');
  const ioP = panel('Container disk I/O');
  const cpu = new SeriesPanel(cpuP.body, { format: fmtCpu, label: 'CPU usage per container over time' });
  const mem = new SeriesPanel(memP.body, { format: fmtMem, bytes: true, label: 'Memory usage per container over time' });
  const net = new SeriesPanel(netP.body, { format: fmtRateShort, bytes: true, label: 'Network throughput per container over time' });
  const io = new SeriesPanel(ioP.body, { format: fmtRateShort, bytes: true, label: 'Disk throughput per container over time' });
  const netNote = h('span');
  const ioNote = h('span');
  netP.right.append(netNote, segmented('Network direction', [['total', 'Total'], ['a', 'Received'], ['b', 'Sent']], prefs.net, (v) => { prefs.net = v; save(); render(); }));
  ioP.right.append(ioNote, segmented('Disk direction', [['total', 'Total'], ['a', 'Read'], ['b', 'Write']], prefs.io, (v) => { prefs.io = v; save(); render(); }));

  // Events list
  const evP = panel('Container events');
  const evSearch = h('input', { type: 'search', placeholder: 'Filter by container or event…', 'aria-label': 'Filter events' });
  const evOnlyIssues = h('input', { type: 'checkbox' });
  const evBody = h('tbody');
  evP.body.append(
    h('div', { class: 'toolbar' }, evSearch, h('label', { class: 'check' }, evOnlyIssues, 'Problems only')),
    h('div', { class: 'table-wrap events-wrap' }, h('table', { class: 'events-table' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Time'), h('th', {}, 'Container'), h('th', {}, 'Event'), h('th', {}, 'Details'))), evBody)),
  );
  function renderEvents() {
    const q = evSearch.value.trim().toLowerCase();
    const list = events.filter((e) => (!evOnlyIssues.checked || isCriticalEvent(e))
      && (!q || `${e.container} ${e.action} ${e.detail} ${eventText(e)}`.toLowerCase().includes(q))).reverse();
    evP.right.textContent = `${events.length} in this range · newest first`;
    evBody.replaceChildren(...list.slice(0, 300).map((e) => h('tr', { class: isCriticalEvent(e) ? 'row-issue' : '' },
      h('td', { class: 'num' }, fmtTime(e.t)),
      h('td', {}, e.container),
      h('td', {}, h('span', { class: `chip ${isCriticalEvent(e) ? 'critical' : e.action === 'health' && e.detail === 'healthy' ? 'good' : ''}` },
        e.action === 'health' ? e.detail : ACTION_LABEL[e.action] || e.action)),
      h('td', { class: 'muted' }, eventDetail(e)))));
    if (!list.length) evBody.append(h('tr', {}, h('td', { colspan: 4, class: 'empty' }, events.length ? 'No events match.' : 'No container events in this range.')));
    if (list.length > 300) evBody.append(h('tr', {}, h('td', { colspan: 4, class: 'empty' }, `${list.length - 300} older events not shown — narrow the filter or the range.`)));
  }
  evSearch.addEventListener('input', renderEvents);
  evOnlyIssues.addEventListener('change', renderEvents);

  function render() {
    if (!src) return;
    const grouped = (m) => (prefs.group ? groupByProject(m, projectOf) : m);
    cpu.setData(src.t, grouped(src.cpu));
    mem.setData(src.t, grouped(src.mem));
    net.setData(src.t, grouped(combine(src.netRx, src.netTx, prefs.net)));
    io.setData(src.t, grouped(combine(src.blkRead, src.blkWrite, prefs.io)));
    const what = prefs.group ? 'series' : 'containers';
    cpuP.right.textContent = `${cpu.stats.length} ${what} · % of one core · top 8 colored`;
    memP.right.textContent = `${mem.stats.length} ${what} · top 8 colored`;
    netNote.textContent = `${net.stats.length} ${what} · host-network containers excluded`;
    ioNote.textContent = `${io.stats.length} ${what}`;
  }
  function applyMarkers() {
    for (const chart of [cpu, mem, net, io]) chart.setEvents(prefs.markers ? events : []);
  }
  groupBox.addEventListener('change', () => { prefs.group = groupBox.checked; save(); render(); });
  markerBox.addEventListener('change', () => { prefs.markers = markerBox.checked; save(); applyMarkers(); });

  return {
    panels: [toolbar, cpuP.el, memP.el, netP.el, ioP.el, evP.el],
    // next: { t: [], cpu, mem, netRx, netTx, blkRead, blkWrite } with Map(name -> values aligned with t)
    setData(next) {
      src = next;
      render();
    },
    setEvents(next) {
      events = next || [];
      applyMarkers();
      renderEvents();
    },
    destroy() {
      for (const chart of [cpu, mem, net, io]) chart.destroy();
    },
  };
}
