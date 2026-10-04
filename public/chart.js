import { h, fmtTime } from './util.js';

const cssVar = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
const FONT = '11px system-ui, -apple-system, "Segoe UI", sans-serif';

function niceMax(v, bytes = false) {
  if (v <= 0) return 1;
  // Dữ liệu byte: làm tròn theo bậc 1024 để trục ra 256 KiB/s, 1 MiB/s… thay vì số lẻ.
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

function setupCanvas(canvas) {
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

// Vẽ lại mọi chart khi đổi theme (màu lấy từ CSS variables).
const charts = new Set();
const redrawAll = () => charts.forEach((c) => c.draw());
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', redrawAll);
new MutationObserver(redrawAll).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

// Biểu đồ đường theo thời gian, một trục Y, có crosshair + tooltip.
export class TimeChart {
  constructor(parent, { series, yMax = null, format = String, bytes = false }) {
    this.series = series;
    this.bytes = bytes;
    this.yMax = yMax;
    this.format = format;
    this.data = [];
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

  setData(points) {
    this.data = points;
    const last = points.at(-1);
    this.series.forEach((s, i) => { this.legendValues[i].textContent = last?.[s.key] != null ? this.format(last[s.key]) : '–'; });
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
    const max = this.yMax ?? niceMax(peak * 1.1, this.bytes);
    const y = (v) => pad.t + ph - (Math.min(v, max) / max) * ph;

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
      ctx.fillText(this.format(v), pad.l - 8, yy);
    }

    if (pts.length < 2) {
      ctx.textAlign = 'center';
      ctx.fillText('Đang thu thập dữ liệu…', pad.l + pw / 2, pad.t + ph / 2);
      this.tip.hidden = true;
      return;
    }

    const t0 = pts[0].t;
    const t1 = pts.at(-1).t;
    const x = (t) => pad.l + ((t - t0) / (t1 - t0 || 1)) * pw;

    ctx.textAlign = 'center';
    ctx.textBaseline = 'top';
    ctx.fillStyle = cssVar('--muted');
    const ticks = Math.max(2, Math.min(6, Math.floor(pw / 110)));
    // Khoảng ngắn thì hiện cả giây để các nhãn không trùng nhau.
    const timeFmt = t1 - t0 < ticks * 60e3 ? { hour: '2-digit', minute: '2-digit', second: '2-digit' } : { hour: '2-digit', minute: '2-digit' };
    let prevLabel = '';
    for (let i = 0; i <= ticks; i++) {
      const t = t0 + ((t1 - t0) * i) / ticks;
      const label = new Date(t).toLocaleTimeString('vi-VN', { ...timeFmt, hour12: false });
      if (label === prevLabel) continue;
      prevLabel = label;
      ctx.textAlign = i === 0 ? 'left' : i === ticks ? 'right' : 'center';
      ctx.fillText(label, x(t), height - pad.b + 6);
    }

    ctx.lineWidth = 2;
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    for (const s of this.series) {
      ctx.strokeStyle = cssVar(s.color);
      strokeSeries(ctx, pts, s.key, x, y);
    }

    if (this.hoverX == null || this.hoverX < pad.l) {
      this.tip.hidden = true;
      return;
    }
    // Crosshair bắt vào điểm dữ liệu gần nhất theo trục X.
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
    );
    this.tip.hidden = false;
    const tw = this.tip.offsetWidth;
    this.tip.style.left = `${hx + 12 + tw > width ? hx - 12 - tw : hx + 12}px`;
    this.tip.style.top = `${pad.t}px`;
  }
}

// Sparkline cho thẻ server: không trục, thang 0–100%.
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
