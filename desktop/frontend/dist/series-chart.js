import { h, fmtTime, kbd } from './util.js';
import { cssVar, niceMax, setupCanvas, drawYGrid, drawTimeTicks, charts, drawEventMarkers, eventRows, leftPad, fillUnder, drawCrosshair, FONT } from './chart.js';

// The 8 validated categorical slots. More hues would be indistinguishable, so only the
// 8 largest series get a color; the rest are drawn as gray context lines.
const SLOTS = 8;
const PAD = { r: 12, t: 10, b: 24 }; // the left side fits the widest Y label
const TOOLTIP_ROWS = 8;

const lastValue = (arr) => {
  for (let i = arr.length - 1; i >= 0; i--) if (arr[i] != null) return arr[i];
  return null;
};
const firstValue = (arr) => arr.find((v) => v != null) ?? null;

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];

// An axis fitted to lo..hi instead of starting at zero, in round steps (1, 2, 5 × 10ⁿ), so the rise and fall of
// one series stays visible. Byte axes get their own labels with enough decimals for the step.
function zoomRange(lo, hi, bytes) {
  const span = Math.max(hi - lo, Math.abs(hi) * 0.04, bytes ? 4096 : 0.4);
  if (bytes) {
    let u = Math.max(0, Math.floor(Math.log(Math.max(hi, 1)) / Math.log(1024)));
    if (span / 4 / 1024 ** u < 0.01 && u > 0) u--;
    const unit = 1024 ** u;
    const step = niceStep(span / 4 / unit) * unit;
    const min = Math.max(0, Math.floor(lo / step) * step);
    const max = Math.max(Math.ceil(hi / step) * step, min + step);
    const digits = Math.max(0, Math.min(2, -Math.floor(Math.log10(step / unit) + 1e-9)));
    return { min, max, step, format: (v) => `${(v / unit).toFixed(u ? digits : 0)} ${BYTE_UNITS[u]}` };
  }
  const step = niceStep(span / 4);
  const min = Math.max(0, Math.floor(lo / step) * step);
  return { min, max: Math.max(Math.ceil(hi / step) * step, min + step), step, format: null };
}
function niceStep(raw) {
  const p = 10 ** Math.floor(Math.log10(raw));
  return [1, 2, 5, 10].find((m) => m * p >= raw * 0.999) * p;
}

// Multi-series time chart with a sortable legend table (Name / Last / Max / Min), Grafana style.
// minMax / maxCap bound the auto scale like TimeChart; emptyText replaces "No running containers".
// Like Grafana, a click on a series shows only that one (Ctrl/Shift+click adds or removes series), and the
// axis follows the series shown. trend adds Mean and Diff (last minus first value in the range) and fits the
// axis to the shown series instead of starting at zero, to spot growth such as a memory leak.
export class SeriesPanel {
  constructor(parent, { format, bytes = false, label, minMax = 0, maxCap = Infinity, emptyText = 'No running containers', trend = false }) {
    this.format = format;
    this.trend = trend;
    this.bytes = bytes;
    this.minMax = minMax;
    this.maxCap = maxCap;
    this.emptyText = emptyText;
    this.times = [];
    this.events = [];
    this.series = new Map(); // name -> values aligned with times
    this.stats = []; // [{ name, last, max, min, mean, diff }]
    this.colorOf = new Map(); // name -> slot 1..8, kept while the series stays in the top 8
    this.sort = { key: 'last', dir: -1 };
    this.chartHover = null;
    this.legendHover = null;
    this.only = new Set(); // the series shown; empty = all of them
    this.hoverX = null;
    this.hoverY = null;
    this.rows = new Map();
    this.lastOrder = '';
    this.frame = 0;

    this.canvas = h('canvas', { role: 'img', 'aria-label': label });
    this.tip = h('div', { class: 'tooltip', hidden: true });
    this.wrap = h('div', { class: 'chart series-chart' }, this.canvas, this.tip);
    this.tbody = h('tbody');
    const cols = [['name', 'Name'], ['last', 'Last'], ...(trend ? [['diff', 'Diff'], ['mean', 'Mean']] : []), ['max', 'Max'], ['min', 'Min']];
    this.cols = cols.map(([key]) => key);
    const tips = { diff: 'Difference between the last and the first value in the range', mean: 'Average over the range' };
    this.heads = cols.map(([key, text]) =>
      h('th', { class: key === 'name' ? 'sortable' : 'sortable num', 'data-key': key, onclick: () => this.setSort(key), title: tips[key] }, text));
    this.showing = h('span');
    this.note = h('div', { class: 'series-note', hidden: true }, this.showing,
      h('button', { type: 'button', class: 'link', onclick: () => this.showAll() }, 'Show all'));
    this.legend = h('div', { class: 'series-legend' }, this.note,
      h('table', {}, h('thead', {}, h('tr', {}, h('th', { 'aria-label': 'Color' }), this.heads)), this.tbody));
    this.empty = h('div', { class: 'empty', hidden: true });
    parent.append(h('div', { class: trend ? 'series-panel wide' : 'series-panel' }, this.wrap, this.legend), this.empty);

    this.canvas.addEventListener('pointermove', (e) => { this.hoverX = e.offsetX; this.hoverY = e.offsetY; this.schedule(); });
    this.canvas.addEventListener('pointerleave', () => { this.hoverX = this.hoverY = null; this.chartHover = null; this.schedule(); this.syncRows(); });
    this.canvas.addEventListener('click', (e) => { if (this.chartHover) this.isolate(this.chartHover, e.ctrlKey || e.metaKey || e.shiftKey); });
    this.resize = new ResizeObserver(() => this.draw());
    this.resize.observe(this.wrap);
    charts.add(this);
    this.renderHeads();
  }

  destroy() {
    this.resize.disconnect();
    charts.delete(this);
    cancelAnimationFrame(this.frame);
  }

  get focus() {
    return this.legendHover ?? this.chartHover ?? (this.only.size === 1 ? [...this.only][0] : null);
  }

  visible(name) {
    return !this.only.size || this.only.has(name);
  }

  schedule() {
    cancelAnimationFrame(this.frame);
    this.frame = requestAnimationFrame(() => this.draw());
  }

  setEvents(events) {
    this.events = events || [];
    this.schedule();
  }

  setData(times, series) {
    this.times = times;
    this.series = series;
    this.stats = [];
    for (const [name, values] of series) {
      let max = -Infinity;
      let min = Infinity;
      let sum = 0;
      let n = 0;
      for (const v of values) {
        if (v == null) continue;
        if (v > max) max = v;
        if (v < min) min = v;
        sum += v;
        n++;
      }
      if (max === -Infinity) continue;
      const last = lastValue(values);
      this.stats.push({ name, last, max, min, mean: sum / n, diff: last - firstValue(values) });
    }
    for (const name of this.only) if (!this.stats.some((s) => s.name === name)) this.only.delete(name);
    this.assignColors();
    this.renderLegend();
    this.draw();
  }

  // Color follows the container, not its rank: series already holding a slot keep it
  // while they remain in the top 8 (by max); freed slots go to newcomers.
  assignColors() {
    const top = [...this.stats].sort((a, b) => b.max - a.max).slice(0, SLOTS).map((s) => s.name);
    const keep = new Set(top);
    for (const name of this.colorOf.keys()) if (!keep.has(name)) this.colorOf.delete(name);
    const used = new Set(this.colorOf.values());
    for (const name of top) {
      if (this.colorOf.has(name)) continue;
      let slot = 1;
      while (used.has(slot)) slot++;
      this.colorOf.set(name, slot);
      used.add(slot);
    }
  }

  colorVar(name) {
    const slot = this.colorOf.get(name);
    return slot ? `--cat-${slot}` : '--muted';
  }

  setSort(key) {
    this.sort = this.sort.key === key ? { key, dir: -this.sort.dir } : { key, dir: key === 'name' ? 1 : -1 };
    this.renderHeads();
    this.renderLegend();
  }

  // A click shows only this series (again: all of them); with add, the series joins or leaves the ones shown.
  isolate(name, add) {
    if (!add) this.only = this.only.size === 1 && this.only.has(name) ? new Set() : new Set([name]);
    else if (!this.only.size) this.only = new Set(this.stats.map((s) => s.name).filter((n) => n !== name));
    else if (this.only.has(name)) this.only.delete(name);
    else this.only.add(name);
    if (this.only.size >= this.stats.length) this.only.clear();
    this.syncRows();
    this.schedule();
  }

  showAll() {
    this.only.clear();
    this.syncRows();
    this.schedule();
  }

  renderHeads() {
    for (const th of this.heads) {
      const active = th.dataset.key === this.sort.key;
      th.classList.toggle('active', active);
      th.setAttribute('aria-sort', active ? (this.sort.dir > 0 ? 'ascending' : 'descending') : 'none');
      th.dataset.arrow = active ? (this.sort.dir > 0 ? '▲' : '▼') : '';
    }
  }

  renderLegend() {
    const { key, dir } = this.sort;
    const sorted = [...this.stats].sort((a, b) =>
      (key === 'name' ? a.name.localeCompare(b.name) : (a[key] ?? -Infinity) - (b[key] ?? -Infinity)) * dir || a.name.localeCompare(b.name));
    for (const s of sorted) {
      let row = this.rows.get(s.name);
      if (!row) {
        const sw = h('i', { class: 'key' });
        const cells = [h('td', {}, sw), h('td', { class: 'name' }, s.name), ...this.heads.slice(1).map(() => h('td', { class: 'num' }))];
        const tr = h('tr', {
          title: kbd('Click to show only this one · Ctrl+click to add or remove'),
          onmouseenter: () => { this.legendHover = s.name; this.syncRows(); this.schedule(); },
          onmouseleave: () => { this.legendHover = null; this.syncRows(); this.schedule(); },
          onclick: (e) => this.isolate(s.name, e.ctrlKey || e.metaKey || e.shiftKey),
        }, cells);
        row = { tr, sw, cells };
        this.rows.set(s.name, row);
      }
      row.sw.style.background = `var(${this.colorVar(s.name)})`;
      row.cells.slice(2).forEach((td, i) => {
        const key = this.cols[i + 1];
        if (key === 'diff') this.paintDiff(td, s);
        else td.textContent = s[key] == null ? '–' : this.format(s[key]);
      });
    }
    for (const name of this.rows.keys()) if (!this.stats.some((s) => s.name === name)) this.rows.delete(name);
    // Only reorder the DOM when the order changes, so hover state survives updates.
    const order = sorted.map((s) => s.name).join('\n');
    if (order !== this.lastOrder) {
      this.lastOrder = order;
      this.tbody.replaceChildren(...sorted.map((s) => this.rows.get(s.name).tr));
    }
    this.syncRows();
  }

  // ▲ grew, ▼ shrank; a change under 2% of the peak counts as steady.
  paintDiff(td, s) {
    const steady = Math.abs(s.diff) < Math.max(s.max, 1e-9) * 0.02;
    td.className = `num trend ${steady ? 'flat' : s.diff > 0 ? 'up' : 'down'}`;
    td.textContent = steady ? '≈ 0' : `${s.diff > 0 ? '▲' : '▼'} ${this.format(Math.abs(s.diff))}`;
    td.title = steady ? 'Steady over this range' : `${s.diff > 0 ? 'Up' : 'Down'} ${this.format(Math.abs(s.diff))} since the start of the range`;
  }

  // The focused series against where it started: a dashed line at its first value and a ▲/▼ badge with the
  // difference at its last one, so growth reads at a glance even when the line wiggles.
  drawTrend(ctx, name, { x, y, left, right, top, bottom }) {
    const values = this.series.get(name);
    const s = this.stats.find((st) => st.name === name);
    const i0 = values.findIndex((v) => v != null);
    let i1 = values.length - 1;
    while (i1 > i0 && values[i1] == null) i1--;
    if (!s || i0 < 0 || i1 <= i0) return;
    const steady = Math.abs(s.diff) < Math.max(s.max, 1e-9) * 0.02;
    const color = cssVar(steady ? '--muted' : s.diff > 0 ? '--critical-ink' : '--good');
    const sx = x(this.times[i0]);
    const sy = Math.round(y(values[i0])) + 0.5;
    ctx.save();
    ctx.setLineDash([4, 4]);
    ctx.lineWidth = 1;
    ctx.strokeStyle = cssVar('--ink-2');
    ctx.globalAlpha = 0.6;
    ctx.beginPath(); ctx.moveTo(sx, sy); ctx.lineTo(right, sy); ctx.stroke();
    ctx.restore();
    // The start label sits at the right end of the dashed line, on the side away from where the series ended.
    const startText = `start ${this.format(values[i0])}`;
    ctx.font = FONT;
    const lw = ctx.measureText(startText).width + 8;
    const below = s.diff >= 0 ? sy + 16 <= bottom : sy - 16 < top;
    const ly = below ? sy + 2 : sy - 16;
    ctx.save();
    ctx.globalAlpha = 0.85;
    ctx.fillStyle = cssVar('--surface');
    ctx.fillRect(right - lw - 2, ly, lw, 14);
    ctx.restore();
    ctx.fillStyle = cssVar('--ink-2');
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    ctx.fillText(startText, right - 6, ly + 7.5);

    const ex = x(this.times[i1]);
    const ey = y(values[i1]);
    const text = steady ? '≈ steady' : `${s.diff > 0 ? '▲ +' : '▼ −'}${this.format(Math.abs(s.diff))}`;
    ctx.font = `600 ${FONT}`;
    const bw = ctx.measureText(text).width + 14;
    const bh = 20;
    const bx = Math.max(left, Math.min(ex - bw - 8, right - bw));
    const by = Math.max(top, Math.min(bottom - bh, s.diff >= 0 ? ey - bh - 8 : ey + 8));
    ctx.fillStyle = cssVar('--surface');
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.25;
    ctx.beginPath(); ctx.roundRect(bx + 0.5, by + 0.5, bw, bh, 6); ctx.fill(); ctx.stroke();
    ctx.fillStyle = color;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(text, bx + bw / 2 + 0.5, by + bh / 2 + 1);
    ctx.beginPath(); ctx.arc(ex, ey, 3.5, 0, Math.PI * 2); ctx.fill();
  }

  syncRows() {
    const focus = this.focus;
    for (const [name, row] of this.rows) {
      const shown = this.visible(name);
      row.tr.classList.toggle('focus', name === focus);
      row.tr.classList.toggle('dim', shown && focus != null && name !== focus);
      row.tr.classList.toggle('off', !shown);
      row.tr.classList.toggle('pinned', this.only.has(name));
    }
    this.note.hidden = !this.only.size;
    this.showing.textContent = `Showing ${this.only.size} of ${this.stats.length}`;
  }

  draw() {
    if (!this.canvas.isConnected) return;
    const hasData = this.times.length >= 2 && this.stats.length > 0;
    this.empty.hidden = hasData;
    this.empty.textContent = this.times.length < 2 ? 'Collecting data…'
      : this.series.size || this.emptyText !== 'No running containers' ? this.emptyText : 'No data recorded in this range yet';
    this.wrap.parentElement.hidden = !hasData;
    if (!hasData) return;

    const { ctx, width, height } = setupCanvas(this.canvas);
    if (!width) return;
    const shown = this.stats.filter((s) => this.visible(s.name));
    // The axis follows the series shown: from zero, or fitted to them (trend) when some are picked.
    let min = 0;
    let max;
    let step = 0;
    let axisFormat = this.format;
    if (this.trend && this.only.size) {
      const r = zoomRange(Math.min(...shown.map((s) => s.min)), Math.max(...shown.map((s) => s.max)), this.bytes);
      ({ min, max, step } = r);
      if (r.format) axisFormat = r.format;
    } else {
      const peak = Math.max(0, ...shown.map((s) => s.max));
      max = Math.min(this.maxCap, Math.max(this.minMax, niceMax(peak * 1.1, this.bytes)));
    }
    const pad = { ...PAD, l: leftPad(ctx, max, axisFormat, this.bytes, min, step) };
    const pw = width - pad.l - pad.r;
    const ph = height - pad.t - pad.b;
    const y = (v) => pad.t + ph - ((Math.min(Math.max(v, min), max) - min) / (max - min)) * ph;
    const t0 = this.times[0];
    const t1 = this.times.at(-1);
    const x = (t) => pad.l + ((t - t0) / (t1 - t0 || 1)) * pw;

    drawYGrid(ctx, { pad, width, y, max, min, step, format: axisFormat, bytes: this.bytes });
    drawTimeTicks(ctx, { pad, height, pw, x, t0, t1 });

    // Crosshair index and the series closest to the pointer (hover highlight).
    let idx = -1;
    this.chartHover = null;
    if (this.hoverX != null && this.hoverX >= pad.l) {
      const tHover = t0 + ((this.hoverX - pad.l) / pw) * (t1 - t0);
      let best = Infinity;
      this.times.forEach((t, i) => { if (Math.abs(t - tHover) < best) { best = Math.abs(t - tHover); idx = i; } });
      let nearest = 12; // px
      for (const s of shown) {
        const v = this.series.get(s.name)[idx];
        if (v == null) continue;
        const d = Math.abs(y(v) - this.hoverY);
        if (d < nearest) { nearest = d; this.chartHover = s.name; }
      }
      this.syncRows();
    }
    const focus = this.focus && this.visible(this.focus) ? this.focus : null; // a hidden series is not drawn, even on hover

    const stroke = (name, width, alpha, color) => {
      const values = this.series.get(name);
      ctx.globalAlpha = alpha;
      ctx.lineWidth = width;
      ctx.strokeStyle = color;
      ctx.beginPath();
      let pen = false;
      for (let i = 0; i < values.length; i++) {
        const v = values[i];
        if (v == null) { pen = false; continue; }
        if (pen) ctx.lineTo(x(this.times[i]), y(v));
        else ctx.moveTo(x(this.times[i]), y(v));
        pen = true;
      }
      ctx.stroke();
    };
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    const muted = cssVar('--muted');
    const area = (name, alpha) => {
      const values = this.series.get(name);
      const color = this.colorOf.has(name) ? cssVar(this.colorVar(name)) : muted;
      fillUnder(ctx, this.times.map((t) => x(t)), values.map((v) => (v == null ? null : y(v))), { top: pad.t, base: pad.t + ph, color, alpha });
    };
    if (focus && this.series.has(focus)) area(focus, 0.2);
    else if (shown.length <= 2) for (const s of shown) area(s.name, 0.14);
    const gray = shown.filter((s) => !this.colorOf.has(s.name));
    const colored = shown.filter((s) => this.colorOf.has(s.name)).sort((a, b) => a.max - b.max);
    for (const s of gray) if (s.name !== focus) stroke(s.name, 1, focus ? 0.15 : 0.35, muted);
    for (const s of colored) if (s.name !== focus) stroke(s.name, 1.75, focus ? 0.2 : 1, cssVar(this.colorVar(s.name)));
    if (focus && this.series.has(focus)) {
      stroke(focus, 2.5, 1, this.colorOf.has(focus) ? cssVar(this.colorVar(focus)) : cssVar('--ink'));
    }
    ctx.globalAlpha = 1;
    if (this.trend && focus && this.series.has(focus)) this.drawTrend(ctx, focus, { x, y, left: pad.l, right: width - pad.r, top: pad.t, bottom: pad.t + ph });
    drawEventMarkers(ctx, this.events, { x, t0, t1, top: pad.t, bottom: pad.t + ph });

    if (idx < 0) {
      this.tip.hidden = true;
      return;
    }
    const hx = Math.round(x(this.times[idx])) + 0.5;
    drawCrosshair(ctx, hx, pad.t, pad.t + ph);

    // Tooltip: the focused series first, then the largest values at this time.
    const at = shown
      .map((s) => ({ name: s.name, v: this.series.get(s.name)[idx] }))
      .filter((r) => r.v != null)
      .sort((a, b) => (b.name === focus) - (a.name === focus) || b.v - a.v);
    if (focus) {
      const fv = this.series.get(focus)?.[idx];
      if (fv != null) {
        ctx.beginPath();
        ctx.arc(hx, y(fv), 4, 0, Math.PI * 2);
        ctx.fillStyle = this.colorOf.has(focus) ? cssVar(this.colorVar(focus)) : cssVar('--ink');
        ctx.strokeStyle = cssVar('--surface');
        ctx.lineWidth = 2;
        ctx.fill(); ctx.stroke();
      }
    }
    const rows = at.slice(0, TOOLTIP_ROWS).map((r) => {
      const key = h('i');
      key.style.background = `var(${this.colorVar(r.name)})`;
      return h('div', { class: `row${r.name === focus ? ' focus' : ''}` }, key, h('span', {}, r.name), h('b', {}, this.format(r.v)));
    });
    if (at.length > TOOLTIP_ROWS) rows.push(h('div', { class: 't' }, `+${at.length - TOOLTIP_ROWS} more`));
    rows.push(...eventRows(this.events, this.times[idx], Math.max((t1 - t0) / pw * 6, (t1 - t0) / this.times.length / 2)));
    this.tip.replaceChildren(h('div', { class: 't' }, fmtTime(this.times[idx])), ...rows);
    this.tip.hidden = false;
    const tw = this.tip.offsetWidth;
    this.tip.style.left = `${hx + 14 + tw > width ? hx - 14 - tw : hx + 14}px`;
    this.tip.style.top = `${pad.t + 4}px`;
  }
}
