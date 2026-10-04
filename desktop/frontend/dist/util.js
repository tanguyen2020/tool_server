// Safe DOM builder: content always goes through text nodes, never innerHTML with server data.
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c != null && c !== false) el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

export function fmtBytes(b, digits = 1) {
  if (b == null || Number.isNaN(b)) return '–';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (Math.abs(b) >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(i === 0 ? 0 : digits)} ${units[i]}`;
}

export const fmtRate = (b) => (b == null ? '–' : `${fmtBytes(b)}/s`);
export const fmtPct = (p, digits = 1) => (p == null || Number.isNaN(p) ? '–' : `${p.toFixed(digits)}%`);
// Time of day, with the date when it is not today.
export function fmtTime(t) {
  const d = new Date(t);
  const time = d.toLocaleTimeString('en-GB', { hour12: false });
  return d.toDateString() === new Date().toDateString() ? time : `${d.toLocaleDateString('en-GB', { day: '2-digit', month: '2-digit' })} ${time}`;
}

export function fmtUptime(sec) {
  if (!sec) return '–';
  const d = Math.floor(sec / 86400);
  const hr = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${hr}h`;
  if (hr > 0) return `${hr}h ${m}m`;
  return `${m}m`;
}

// Shared warning thresholds for CPU/RAM/Disk.
export const level = (p) => (p == null ? '' : p >= 90 ? 'critical' : p >= 75 ? 'warning' : '');

export function meter(pct) {
  const el = h('div', { class: 'meter', role: 'presentation' }, h('span'));
  setMeter(el, pct);
  return el;
}

export function setMeter(el, pct) {
  el.className = `meter ${level(pct)}`;
  el.firstChild.style.width = `${Math.max(0, Math.min(100, pct || 0))}%`;
}

// Status chip: always a colored dot + label, never color alone.
export function statusChip(status) {
  const map = {
    online: ['good', 'Online'],
    offline: ['critical', 'Offline'],
    connecting: ['', 'Connecting'],
    rebooting: ['warning', 'Rebooting…'],
    running: ['good', 'Running'],
    restarting: ['warning', 'Restarting'],
    paused: ['warning', 'Paused'],
    created: ['', 'Created'],
    exited: ['critical', 'Exited'],
    dead: ['critical', 'Dead'],
  };
  const [cls, label] = map[status] || ['', status || '–'];
  return h('span', { class: `chip ${cls}` }, label);
}

export function toast(message, isError = false) {
  const box = document.getElementById('toast');
  const el = h('div', { class: isError ? 'error' : '' }, message);
  box.append(el);
  setTimeout(() => el.remove(), isError ? 7000 : 3500);
}
