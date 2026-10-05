import { h, fmtTime } from './util.js';
import { cssVar, niceMax, setupCanvas, drawYGrid, drawTimeTicks, charts, drawEventMarkers, eventRows } from './chart.js';

// The 8 validated categorical slots. More hues would be indistinguishable, so only the
// 8 largest series get a color; the rest are drawn as gray context lines.
const SLOTS = 8;
const PAD = { l: 74, r: 10, t: 8, b: 22 };
const TOOLTIP_ROWS = 8;

const lastValue = (arr) => {
  for (let i = arr.length - 1; i >= 0; i--) if (arr[i] != null) return arr[i];
  return null;
};

// Multi-series time chart with a sortable legend table (Name / Last / Max / Min), Grafana style.
// minMax / maxCap bound the auto scale like TimeChart; emptyText replaces "No running containers".
export class SeriesPanel {
  constructor(parent, { format, bytes = false, label, minMax = 0, maxCap = Infinity, emptyText = 'No running containers' }) {
    this.format = format;
    this.bytes = bytes;
    this.minMax = minMax;
    this.maxCap = maxCap;
    this.emptyText = emptyText;
    this.times = [];
    this.events = [];
    this.series = new Map(); // name -> values aligned with times
    this.stats = []; // [{ name, last, max, min }]
    this.colorOf = new Map(); // name -> slot 1..8, kept while the series stays in the top 8
    this.sort = { key: 'last', dir: -1 };
    this.chartHover = null;
    this.legendHover = null;
    this.pinned = null;
    this.hoverX = null;
    this.hoverY = null;
    this.rows = new Map();
    this.lastOrder = '';
    this.frame = 0;

    this.canvas = h('canvas', { role: 'img', 'aria-label': label });
    this.tip = h('div', { class: 'tooltip', hidden: true });
    this.wrap = h('div', { class: 'chart series-chart' }, this.canvas, this.tip);
    this.tbody = h('tbody');
    this.heads = [['name', 'Name'], ['last', 'Last'], ['max', 'Max'], ['min', 'Min']].map(([key, text]) =>
      h('th', { class: key === 'name' ? 'sortable' : 'sortable num', 'data-key': key, onclick: () => this.setSort(key) }, text));
    this.legend = h('div', { class: 'series-legend' },
      h('table', {}, h('thead', {}, h('tr', {}, h('th', { 'aria-label': 'Color' }), this.heads)), this.tbody));
    this.empty = h('div', { class: 'empty', hidden: true });
    parent.append(h('div', { class: 'series-panel' }, this.wrap, this.legend), this.empty);

    this.canvas.addEventListener('pointermove', (e) => { this.hoverX = e.offsetX; this.hoverY = e.offsetY; this.schedule(); });
    this.canvas.addEventListener('pointerleave', () => { this.hoverX = this.hoverY = null; this.chartHover = null; this.schedule(); this.syncRows(); });
    this.canvas.addEventListener('click', () => { if (this.chartHover) this.togglePin(this.chartHover); });
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
    return this.legendHover ?? this.chartHover ?? this.pinned;
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
      for (const v of values) {
        if (v == null) continue;
        if (v > max) max = v;
        if (v < min) min = v;
      }
      if (max === -Infinity) continue;
      this.stats.push({ name, last: lastValue(values), max, min });
    }
    if (this.pinned && !series.has(this.pinned)) this.pinned = null;
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

  togglePin(name) {
    this.pinned = this.pinned === name ? null : name;
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
        const cells = [h('td', {}, sw), h('td', { class: 'name' }, s.name), h('td', { class: 'num' }), h('td', { class: 'num' }), h('td', { class: 'num' })];
        const tr = h('tr', {
          onmouseenter: () => { this.legendHover = s.name; this.syncRows(); this.schedule(); },
          onmouseleave: () => { this.legendHover = null; this.syncRows(); this.schedule(); },
          onclick: () => this.togglePin(s.name),
        }, cells);
        row = { tr, sw, cells };
        this.rows.set(s.name, row);
      }
      row.sw.style.background = `var(${this.colorVar(s.name)})`;
      row.cells[2].textContent = s.last == null ? '–' : this.format(s.last);
      row.cells[3].textContent = this.format(s.max);
      row.cells[4].textContent = this.format(s.min);
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

  syncRows() {
    const focus = this.focus;
    for (const [name, row] of this.rows) {
      row.tr.classList.toggle('focus', name === focus);
      row.tr.classList.toggle('dim', focus != null && name !== focus);
      row.tr.classList.toggle('pinned', name === this.pinned);
    }
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
    const pw = width - PAD.l - PAD.r;
    const ph = height - PAD.t - PAD.b;
    const peak = Math.max(0, ...this.stats.map((s) => s.max));
    const max = Math.min(this.maxCap, Math.max(this.minMax, niceMax(peak * 1.1, this.bytes)));
    const y = (v) => PAD.t + ph - (Math.min(v, max) / max) * ph;
    const t0 = this.times[0];
    const t1 = this.times.at(-1);
    const x = (t) => PAD.l + ((t - t0) / (t1 - t0 || 1)) * pw;

    drawYGrid(ctx, { pad: PAD, width, y, max, format: this.format });
    drawTimeTicks(ctx, { pad: PAD, height, pw, x, t0, t1 });

    // Crosshair index and the series closest to the pointer (hover highlight).
    let idx = -1;
    this.chartHover = null;
    if (this.hoverX != null && this.hoverX >= PAD.l) {
      const tHover = t0 + ((this.hoverX - PAD.l) / pw) * (t1 - t0);
      let best = Infinity;
      this.times.forEach((t, i) => { if (Math.abs(t - tHover) < best) { best = Math.abs(t - tHover); idx = i; } });
      let nearest = 12; // px
      for (const s of this.stats) {
        const v = this.series.get(s.name)[idx];
        if (v == null) continue;
        const d = Math.abs(y(v) - this.hoverY);
        if (d < nearest) { nearest = d; this.chartHover = s.name; }
      }
      this.syncRows();
    }
    const focus = this.focus;

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
    const gray = this.stats.filter((s) => !this.colorOf.has(s.name));
    const colored = this.stats.filter((s) => this.colorOf.has(s.name)).sort((a, b) => a.max - b.max);
    for (const s of gray) if (s.name !== focus) stroke(s.name, 1, focus ? 0.15 : 0.35, muted);
    for (const s of colored) if (s.name !== focus) stroke(s.name, 1.5, focus ? 0.2 : 1, cssVar(this.colorVar(s.name)));
    if (focus && this.series.has(focus)) {
      stroke(focus, 2.5, 1, this.colorOf.has(focus) ? cssVar(this.colorVar(focus)) : cssVar('--ink'));
    }
    ctx.globalAlpha = 1;
    drawEventMarkers(ctx, this.events, { x, t0, t1, top: PAD.t, bottom: PAD.t + ph });

    if (idx < 0) {
      this.tip.hidden = true;
      return;
    }
    const hx = Math.round(x(this.times[idx])) + 0.5;
    ctx.lineWidth = 1;
    ctx.strokeStyle = muted;
    ctx.beginPath(); ctx.moveTo(hx, PAD.t); ctx.lineTo(hx, PAD.t + ph); ctx.stroke();

    // Tooltip: the focused series first, then the largest values at this time.
    const at = this.stats
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
      return h('div', { class: `row${r.name === focus ? ' focus' : ''}` }, key, h('b', {}, this.format(r.v)), h('span', {}, r.name));
    });
    if (at.length > TOOLTIP_ROWS) rows.push(h('div', { class: 't' }, `+${at.length - TOOLTIP_ROWS} more`));
    rows.push(...eventRows(this.events, this.times[idx], Math.max((t1 - t0) / pw * 6, (t1 - t0) / this.times.length / 2)));
    this.tip.replaceChildren(h('div', { class: 't' }, fmtTime(this.times[idx])), ...rows);
    this.tip.hidden = false;
    const tw = this.tip.offsetWidth;
    this.tip.style.left = `${hx + 12 + tw > width ? hx - 12 - tw : hx + 12}px`;
    this.tip.style.top = `${PAD.t}px`;
  }
}
