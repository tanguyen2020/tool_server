// Tạo DOM an toàn: nội dung luôn đi qua text node, không dùng innerHTML với dữ liệu từ server.
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
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
export const fmtTime = (t) => new Date(t).toLocaleTimeString('vi-VN', { hour12: false });

export function fmtUptime(sec) {
  if (!sec) return '–';
  const d = Math.floor(sec / 86400);
  const hr = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d} ngày ${hr} giờ`;
  if (hr > 0) return `${hr} giờ ${m} phút`;
  return `${m} phút`;
}

// Ngưỡng cảnh báo dùng chung cho CPU/RAM/Disk.
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

// Chip trạng thái: luôn có chấm màu + chữ, không dùng màu đơn thuần.
export function statusChip(status) {
  const map = {
    online: ['good', 'Online'],
    offline: ['critical', 'Offline'],
    connecting: ['', 'Đang kết nối'],
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

export async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && path !== '/api/login') {
    location.href = '/login.html';
    throw new Error('Chưa đăng nhập');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Lỗi ${res.status}`);
  return data;
}
