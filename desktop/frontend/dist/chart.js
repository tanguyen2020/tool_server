import { h, fmtTime } from './util.js';

export const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
export const FONT = '11px system-ui, -apple-system, "Segoe UI", sans-serif';

export function niceMax(v, bytes = false) {
  if (v <= 0) return 1;
  // Byte data: round to powers of 1024 so the axis reads 256 KiB/s, 1 MiB/s… instead of odd numbers.
  if (bytes) {
    const unit = 1024 ** Math.max(0, Math.floor(Math.log(v) / Math.log(1024)));
    const scaled = v / unit;
    for (const m of [1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024]) if (m >= scaled) return m * unit;
    return 1024 * unit;
  }
  const p = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

export function setupCanvas(canvas) {
  const { width, height } = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * dpr);
  canvas.height = Math.round(height * dpr);
  const ctx = canvas.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);
  return { ctx, width, height };
}

function strokeSeries(ctx, pts, key, x, y) {
  ctx.beginPath();
  let pen = false;
  for (const p of pts) {
    if (p[key] == null) { pen = false; continue; }
    if (pen) ctx.lineTo(x(p.t), y(p[key]));
    else ctx.moveTo(x(p.t), y(p[key]));
    pen = true;
  }
  ctx.stroke();
}

// Horizontal grid lines with Y labels (4 steps from 0 to max).
export function drawYGrid(ctx, { pad, width, y, max, format }) {
  ctx.font = FONT;
  ctx.lineWidth = 1;
  ctx.textBaseline = 'middle';
  ctx.textAlign = 'right';
  for (let i = 0; i <= 4; i++) {
    const v = (max * i) / 4;
    const yy = Math.round(y(v)) + 0.5;
    ctx.strokeStyle = i === 0 ? cssVar('--axis') : cssVar('--grid');
    ctx.beginPath(); ctx.moveTo(pad.l, yy); ctx.lineTo(width - pad.r, yy); ctx.stroke();
    ctx.fillStyle = cssVar('--muted');
    ctx.fillText(format(v), pad.l - 8, yy);
  }
}

// Time labels along the X axis.
export function drawTimeTicks(ctx, { pad, height, pw, x, t0, t1 }) {
  ctx.font = FONT;
  ctx.textBaseline = 'top';
  ctx.fillStyle = cssVar('--muted');
  const ticks = Math.max(2, Math.min(6, Math.floor(pw / 110)));
  // Seconds for short ranges so labels don't repeat; the date for ranges spanning days.
  const span = t1 - t0;
  const timeFmt = span < ticks * 60e3 ? { hour: '2-digit', minute: '2-digit', second: '2-digit' }
    : span > 20 * 3600e3 ? { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' }
      : { hour: '2-digit', minute: '2-digit' };
  let prevLabel = '';
  for (let i = 0; i <= ticks; i++) {
    const t = t0 + (span * i) / ticks;
    const label = new Date(t).toLocaleString('en-GB', { ...timeFmt, hour12: false });
    if (label === prevLabel) continue;
    prevLabel = label;
    ctx.textAlign = i === 0 ? 'left' : i === ticks ? 'right' : 'center';
    ctx.fillText(label, x(t), height - pad.b + 6);
  }
}

// Redraw every chart when the theme changes (colors come from CSS variables).
export const charts = new Set();
const redrawAll = () => charts.forEach((c) => c.draw());
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', redrawAll);
new MutationObserver(redrawAll).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

// Time-series line chart: single Y axis, crosshair + tooltip.
// yMax: number or function (null = auto). Auto scale can be bounded: minMax (smallest top, so near-idle noise is
// not blown up) and maxCap (never above, e.g. 100 for percentages).
// ref: optional dashed reference line { value: () => number, label: () => string }.
export class TimeChart {
  constructor(parent, { series, yMax = null, minMax = 0, maxCap = Infinity, format = String, bytes = false, ref = null }) {
    this.series = series;
    this.bytes = bytes;
    this.ref = ref;
    this.yMax = yMax;
    this.minMax = minMax;
    this.maxCap = maxCap;
    this.format = format;
    this.data = [];
    this.events = [];
    this.hoverX = null;

    this.legend = h('div', { class: 'legend' });
    this.legendValues = series.map((s) => {
      const key = h('i');
      key.style.background = `var(${s.color})`;
      const val = h('b', {}, '–');
      this.legend.append(h('span', {}, key, s.label, val));
      return val;
    });
    this.canvas = h('canvas', { role: 'img', 'aria-label': series.map((s) => s.label).join(', ') });
    this.tip = h('div', { class: 'tooltip', hidden: true });
    this.wrap = h('div', { class: 'chart' }, this.canvas, this.tip);
    parent.append(this.legend, this.wrap);

    this.canvas.addEventListener('pointermove', (e) => { this.hoverX = e.offsetX; this.draw(); });
    this.canvas.addEventListener('pointerleave', () => { this.hoverX = null; this.draw(); });
    this.resize = new ResizeObserver(() => this.draw());
    this.resize.observe(this.wrap);
    charts.add(this);
  }

  destroy() {
    this.resize.disconnect();
    charts.delete(this);
  }

  setEvents(events) {
    this.events = events || [];
    this.draw();
  }

  setData(points) {
    this.data = points;
    // Latest available value: stored ranges end with buckets that have no data yet.
    this.series.forEach((s, i) => {
      const last = points.findLast((p) => p[s.key] != null);
      this.legendValues[i].textContent = last ? this.format(last[s.key]) : '–';
    });
    this.draw();
  }

  draw() {
    if (!this.canvas.isConnected) return;
    const { ctx, width, height } = setupCanvas(this.canvas);
    if (!width) return;
    const pad = { l: 74, r: 10, t: 8, b: 22 };
    const pw = width - pad.l - pad.r;
    const ph = height - pad.t - pad.b;
    const pts = this.data;
    const peak = Math.max(0, ...pts.flatMap((p) => this.series.map((s) => p[s.key] ?? 0)));
    const fixed = typeof this.yMax === 'function' ? this.yMax() : this.yMax;
    const refValue = this.ref?.value() ?? null;
    const max = fixed || Math.min(this.maxCap, Math.max(this.minMax, niceMax(Math.max(peak, refValue ?? 0) * 1.1, this.bytes)));
    const y = (v) => pad.t + ph - (Math.min(v, max) / max) * ph;

    drawYGrid(ctx, { pad, width, y, max, format: this.format });

    const hasValue = pts.some((p) => this.series.some((s) => p[s.key] != null));
    if (pts.length < 2 || !hasValue) {
      ctx.textAlign = 'center';
      ctx.fillText(pts.length < 2 ? 'Collecting data…' : 'No data recorded in this range yet', pad.l + pw / 2, pad.t + ph / 2);
      this.tip.hidden = true;
      return;
    }

    const t0 = pts[0].t;
    const t1 = pts.at(-1).t;
    const x = (t) => pad.l + ((t - t0) / (t1 - t0 || 1)) * pw;

    drawTimeTicks(ctx, { pad, height, pw, x, t0, t1 });

    ctx.lineWidth = 2;
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    for (const s of this.series) {
      ctx.strokeStyle = cssVar(s.color);
      strokeSeries(ctx, pts, s.key, x, y);
    }
    drawEventMarkers(ctx, this.events, { x, t0, t1, top: pad.t, bottom: pad.t + ph });
    if (refValue != null && refValue <= max) {
      const ry = Math.round(y(refValue)) + 0.5;
      ctx.save();
      ctx.setLineDash([4, 4]);
      ctx.lineWidth = 1;
      ctx.strokeStyle = cssVar('--ink-2');
      ctx.beginPath(); ctx.moveTo(pad.l, ry); ctx.lineTo(width - pad.r, ry); ctx.stroke();
      ctx.restore();
      ctx.font = FONT;
      ctx.fillStyle = cssVar('--ink-2');
      ctx.textAlign = 'right';
      ctx.textBaseline = 'bottom';
      ctx.fillText(this.ref.label(), width - pad.r - 4, ry - 3);
    }

    if (this.hoverX == null || this.hoverX < pad.l) {
      this.tip.hidden = true;
      return;
    }
    // The crosshair snaps to the nearest data point on the X axis.
    const tHover = t0 + ((this.hoverX - pad.l) / pw) * (t1 - t0);
    let best = pts[0];
    for (const p of pts) if (Math.abs(p.t - tHover) < Math.abs(best.t - tHover)) best = p;
    const hx = Math.round(x(best.t)) + 0.5;
    ctx.lineWidth = 1;
    ctx.strokeStyle = cssVar('--muted');
    ctx.beginPath(); ctx.moveTo(hx, pad.t); ctx.lineTo(hx, pad.t + ph); ctx.stroke();
    for (const s of this.series) {
      if (best[s.key] == null) continue;
      ctx.beginPath();
      ctx.arc(hx, y(best[s.key]), 4, 0, Math.PI * 2);
      ctx.fillStyle = cssVar(s.color);
      ctx.strokeStyle = cssVar('--surface');
      ctx.lineWidth = 2;
      ctx.fill(); ctx.stroke();
    }

    this.tip.replaceChildren(
      h('div', { class: 't' }, fmtTime(best.t)),
      ...this.series.map((s) => {
        const key = h('i');
        key.style.background = `var(${s.color})`;
        return h('div', { class: 'row' }, key, h('b', {}, best[s.key] != null ? this.format(best[s.key]) : '–'), h('span', {}, s.label));
      }),
      ...eventRows(this.events, best.t, Math.max((t1 - t0) / pw * 6, (t1 - t0) / pts.length / 2)),
    );
    this.tip.hidden = false;
    const tw = this.tip.offsetWidth;
    this.tip.style.left = `${hx + 12 + tw > width ? hx - 12 - tw : hx + 12}px`;
    this.tip.style.top = `${pad.t}px`;
  }
}

// Sparkline for server cards: no axes, 0–100% scale.
export function drawSpark(canvas, pts, series) {
  const { ctx, width, height } = setupCanvas(canvas);
  if (!width || pts.length < 2) return;
  const t0 = pts[0].t;
  const t1 = pts.at(-1).t;
  const x = (t) => ((t - t0) / (t1 - t0 || 1)) * (width - 2) + 1;
  const y = (v) => height - 2 - (Math.min(v, 100) / 100) * (height - 4);
  ctx.strokeStyle = cssVar('--grid');
  ctx.lineWidth = 1;
  ctx.beginPath(); ctx.moveTo(0, height - 1.5); ctx.lineTo(width, height - 1.5); ctx.stroke();
  ctx.lineWidth = 1.5;
  ctx.lineJoin = 'round';
  for (const s of series) {
    ctx.strokeStyle = cssVar(s.color);
    strokeSeries(ctx, pts, s.key, x, y);
  }
}

// Tiny single-series trend for table cells, scaled to its own maximum (shape matters, not the scale).
export function drawMini(canvas, values, color) {
  const { ctx, width, height } = setupCanvas(canvas);
  if (!width || !values || values.length < 2) return;
  const max = Math.max(...values.map((v) => v ?? 0)) || 1;
  const x = (i) => (i / (values.length - 1)) * (width - 2) + 1;
  const y = (v) => height - 2 - (v / max) * (height - 4);
  ctx.strokeStyle = cssVar(color);
  ctx.lineWidth = 1.5;
  ctx.lineJoin = 'round';
  ctx.beginPath();
  let pen = false;
  values.forEach((v, i) => {
    if (v == null) { pen = false; return; }
    if (pen) ctx.lineTo(x(i), y(v));
    else ctx.moveTo(x(i), y(v));
    pen = true;
  });
  ctx.stroke();
}

// ---------------------------------------------------------------- Container event markers
const NORMAL_EXITS = new Set(['0', '130', '143']);
const MARKED = new Set(['die', 'oom', 'health', 'start', 'restart']);

export function isCriticalEvent(e) {
  return e.action === 'oom' || (e.action === 'die' && !NORMAL_EXITS.has(e.detail)) || (e.action === 'health' && e.detail === 'unhealthy');
}

export function eventText(e) {
  switch (e.action) {
    case 'die': return `${e.container} exited (code ${e.detail || '?'})`;
    case 'oom': return `${e.container} ran out of memory`;
    case 'health': return `${e.container} ${e.detail}`;
    case 'kill': return `${e.container} killed (signal ${e.detail || '?'})`;
    case 'start': return `${e.container} started`;
    case 'restart': return `${e.container} restarted`;
    case 'stop': return `${e.container} stopped`;
    case 'create': return `${e.container} created`;
    case 'destroy': return `${e.container} removed`;
    default: return `${e.container} ${e.action}`;
  }
}

// Vertical markers for the events worth seeing on a chart; at most one per pixel column.
export function drawEventMarkers(ctx, events, { x, t0, t1, top, bottom }) {
  if (!events?.length) return;
  const seen = new Map();
  for (const e of events) {
    if (e.t < t0 || e.t > t1 || !MARKED.has(e.action) || (e.action === 'health' && e.detail === 'starting')) continue;
    const px = Math.round(x(e.t));
    seen.set(px, seen.get(px) || isCriticalEvent(e));
  }
  ctx.save();
  ctx.lineWidth = 1;
  ctx.setLineDash([2, 3]);
  for (const [px, critical] of seen) {
    ctx.strokeStyle = critical ? cssVar('--critical') : cssVar('--ink-2');
    ctx.globalAlpha = critical ? 0.9 : 0.45;
    ctx.beginPath(); ctx.moveTo(px + 0.5, top); ctx.lineTo(px + 0.5, bottom); ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = ctx.strokeStyle;
    ctx.beginPath(); ctx.moveTo(px - 3.5, top); ctx.lineTo(px + 4.5, top); ctx.lineTo(px + 0.5, top + 5); ctx.closePath(); ctx.fill();
    ctx.setLineDash([2, 3]);
  }
  ctx.restore();
}

// Tooltip rows for the events within `tol` ms of `t`.
export function eventRows(events, t, tol) {
  const near = (events || []).filter((e) => Math.abs(e.t - t) <= tol && e.action !== 'kill' && e.action !== 'create');
  const rows = near.slice(0, 5).map((e) => h('div', { class: `row event ${isCriticalEvent(e) ? 'critical' : ''}` },
    h('i'), h('span', {}, `${fmtTime(e.t)} · ${eventText(e)}`)));
  if (near.length > 5) rows.push(h('div', { class: 't' }, `+${near.length - 5} more events`));
  return rows;
}
