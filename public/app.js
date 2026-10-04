import { h, api, toast, fmtBytes, fmtRate, fmtPct, fmtUptime, fmtTime, meter, setMeter, statusChip, level } from './util.js';
import { TimeChart, drawSpark } from './chart.js';
import { initLogs, openLogs, onLogsMessage, onLogsSnapshot, onLogsDisconnect } from './logs.js';
import { openServerForm } from './server-form.js';

const state = {
  servers: new Map(), // cấu hình (không có secret)
  snaps: new Map(), // số liệu mới nhất
  history: new Map(), // [{t, cpu, mem, rx, tx}]
};
const HISTORY_MAX = 360;
const view = document.getElementById('view');
let current = null; // view đang hiển thị: { update(id), destroy() }

// ---------------------------------------------------------------- WebSocket
let ws;
let retry = 0;
const connEl = document.getElementById('conn');
function setConn(ok, label) {
  connEl.className = `conn ${ok ? 'ok' : 'bad'}`;
  connEl.querySelector('.conn-label').textContent = label;
}

function send(msg) {
  if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
}

function connect() {
  ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`);
  ws.onopen = () => { retry = 0; setConn(true, 'Realtime'); };
  ws.onclose = async () => {
    setConn(false, 'Mất kết nối');
    onLogsDisconnect();
    // Có thể do hết phiên đăng nhập -> api() sẽ chuyển về trang login.
    await api('/api/me').catch(() => {});
    setTimeout(connect, Math.min(10000, 1000 * 2 ** retry++));
  };
  ws.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    switch (msg.t) {
      case 'init':
        state.servers = new Map(msg.servers.map((s) => [s.id, s]));
        state.snaps = new Map(msg.snapshots.map((s) => [s.id, s]));
        state.history = new Map(Object.entries(msg.history));
        render();
        break;
      case 'servers':
        state.servers = new Map(msg.data.map((s) => [s.id, s]));
        render();
        break;
      case 'removed':
        state.snaps.delete(msg.id);
        state.history.delete(msg.id);
        render();
        break;
      case 'snapshot': {
        const snap = msg.data;
        state.snaps.set(snap.id, snap);
        if (snap.status === 'online' && snap.host?.cpu != null) {
          const hist = state.history.get(snap.id) || [];
          if (!hist.length || hist.at(-1).t < snap.updatedAt) {
            hist.push({ t: snap.updatedAt, cpu: snap.host.cpu, mem: snap.host.mem.pct, rx: snap.host.net.rx, tx: snap.host.net.tx });
            if (hist.length > HISTORY_MAX) hist.splice(0, hist.length - HISTORY_MAX);
          }
          state.history.set(snap.id, hist);
        }
        current?.update(snap.id);
        onLogsSnapshot(snap);
        break;
      }
      case 'logs.data':
      case 'logs.end':
        onLogsMessage(msg);
        break;
    }
  };
}

// ---------------------------------------------------------------- Routing
function render() {
  current?.destroy?.();
  const m = /^#\/server\/([\w-]+)/.exec(location.hash);
  current = m && state.servers.has(m[1]) ? detailView(m[1]) : overviewView();
}
window.addEventListener('hashchange', render);

function setCrumbs(...parts) {
  document.getElementById('crumbs').replaceChildren(...parts.flatMap((p, i) => (i ? [h('span', {}, '/'), p] : [p])));
}

const groupsList = () => [...new Set([...state.servers.values()].map((s) => s.group).filter(Boolean))].sort();
const diskRoot = (host) => host?.disks?.find((d) => d.mount === '/') || host?.disks?.[0];

function seriesLegend(series) {
  return h('div', { class: 'legend' }, series.map((s) => {
    const key = h('i');
    key.style.background = `var(${s.color})`;
    return h('span', {}, key, s.label);
  }));
}

// ---------------------------------------------------------------- Overview
const SPARK_SERIES = [{ key: 'cpu', label: 'CPU', color: '--series-1' }, { key: 'mem', label: 'RAM', color: '--series-2' }];

function serverCard(id) {
  const name = h('div', { class: 'card-title' });
  const sub = h('div', { class: 'card-sub' });
  const chipSlot = h('span');
  const err = h('div', { class: 'card-error', hidden: true });
  const cpuVal = h('span', { class: 'num' });
  const cpuLbl = h('span');
  const memVal = h('span', { class: 'num' });
  const diskVal = h('span', { class: 'num' });
  const cpuM = meter(0);
  const memM = meter(0);
  const diskM = meter(0);
  const spark = h('canvas', { class: 'spark', role: 'img', 'aria-label': 'CPU và RAM theo thời gian' });
  const foot = h('div', { class: 'card-foot' });
  const el = h('a', { class: 'card', href: `#/server/${id}` },
    h('div', { class: 'card-head' }, h('div', { style: 'min-width:0' }, name, sub), chipSlot),
    err,
    h('div', {}, h('div', { class: 'meter-row' }, cpuLbl, cpuVal), cpuM),
    h('div', {}, h('div', { class: 'meter-row' }, h('span', {}, 'RAM'), memVal), memM),
    h('div', {}, h('div', { class: 'meter-row' }, h('span', {}, 'Disk /'), diskVal), diskM),
    h('div', {}, spark, seriesLegend(SPARK_SERIES)),
    foot,
  );

  const update = () => {
    const cfg = state.servers.get(id);
    const snap = state.snaps.get(id) || { status: 'connecting' };
    const host = snap.host;
    el.classList.toggle('offline', snap.status === 'offline');
    name.textContent = cfg.name;
    sub.textContent = [`${cfg.username}@${cfg.host}`, host?.os].filter(Boolean).join(' · ');
    chipSlot.replaceChildren(statusChip(snap.status));
    err.hidden = !snap.error;
    err.textContent = snap.error || '';
    cpuLbl.textContent = host ? `CPU · ${host.cores} core` : 'CPU';
    cpuVal.textContent = fmtPct(host?.cpu);
    setMeter(cpuM, host?.cpu);
    memVal.textContent = host ? `${fmtBytes(host.mem.used)} / ${fmtBytes(host.mem.total)}` : '–';
    setMeter(memM, host?.mem.pct);
    const disk = diskRoot(host);
    diskVal.textContent = disk ? `${fmtBytes(disk.used)} / ${fmtBytes(disk.size)}` : '–';
    setMeter(diskM, disk?.pct);
    drawSpark(spark, state.history.get(id) || [], SPARK_SERIES);
    const d = snap.docker;
    foot.replaceChildren(
      h('span', {}, 'Load ', h('b', { class: 'num' }, host ? host.load.map((v) => v.toFixed(2)).join(' ') : '–')),
      h('span', {}, 'Uptime ', h('b', {}, fmtUptime(host?.uptime))),
      h('span', {}, 'Container ', h('b', { class: 'num' }, d ? (d.available ? `${d.running}/${d.total}` : 'lỗi docker') : '–')),
    );
  };
  return { el, update };
}

function overviewView() {
  setCrumbs(h('span', {}, 'Tổng quan'));
  const search = h('input', { type: 'search', placeholder: 'Tìm server, IP, nhóm…', 'aria-label': 'Tìm server' });
  const statsEl = h('div', { class: 'stats' });
  const list = h('div');
  const cards = new Map();

  const addBtn = h('button', { class: 'btn primary', onclick: () => openServerForm(null, groupsList()) }, '+ Thêm server');
  view.replaceChildren(
    statsEl,
    h('div', { class: 'toolbar' }, search, h('span', { class: 'spacer' }), addBtn),
    list,
  );

  const updateStats = () => {
    const snaps = [...state.servers.keys()].map((id) => state.snaps.get(id)).filter(Boolean);
    const online = snaps.filter((s) => s.status === 'online').length;
    const offline = snaps.filter((s) => s.status === 'offline').length;
    const running = snaps.reduce((a, s) => a + (s.docker?.running || 0), 0);
    const total = snaps.reduce((a, s) => a + (s.docker?.total || 0), 0);
    const tile = (label, value, sub) => h('div', { class: 'stat' }, h('div', { class: 'stat-label' }, label), h('div', { class: 'stat-value' }, value), sub && h('div', { class: 'stat-sub' }, sub));
    statsEl.replaceChildren(
      tile('Server online', `${online} / ${state.servers.size}`),
      tile('Server lỗi kết nối', String(offline), offline ? statusChip('offline') : null),
      tile('Container đang chạy', `${running} / ${total}`),
      tile('Container đã dừng', String(total - running)),
    );
  };

  const build = () => {
    const q = search.value.trim().toLowerCase();
    const servers = [...state.servers.values()]
      .filter((s) => !q || [s.name, s.host, s.group, state.snaps.get(s.id)?.host?.hostname].some((v) => v?.toLowerCase().includes(q)))
      .sort((a, b) => a.name.localeCompare(b.name));
    if (!state.servers.size) {
      list.replaceChildren(h('div', { class: 'panel empty' },
        h('p', {}, 'Chưa có server nào.'),
        h('button', { class: 'btn primary', onclick: () => openServerForm(null, []) }, '+ Thêm server đầu tiên')));
      return;
    }
    const groups = new Map();
    for (const s of servers) {
      const g = s.group || 'Chưa phân nhóm';
      if (!groups.has(g)) groups.set(g, []);
      groups.get(g).push(s);
    }
    const sections = [...groups.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([g, items]) => [
      groups.size > 1 || g !== 'Chưa phân nhóm' ? h('h2', { class: 'group-title' }, `${g} (${items.length})`) : null,
      h('div', { class: 'cards' }, items.map((s) => {
        if (!cards.has(s.id)) cards.set(s.id, serverCard(s.id));
        return cards.get(s.id).el;
      })),
    ]);
    list.replaceChildren(...sections.flat().filter(Boolean));
    if (!servers.length) list.append(h('div', { class: 'empty' }, 'Không có server khớp bộ lọc.'));
    for (const s of servers) cards.get(s.id).update();
  };

  search.addEventListener('input', build);
  build();
  updateStats();
  return {
    update(id) {
      cards.get(id)?.update();
      updateStats();
    },
  };
}

// ---------------------------------------------------------------- Detail
function statPanel(cls, title) {
  const body = h('div');
  const right = h('span', { class: 'right' });
  const el = h('section', { class: `panel ${cls}` }, h('h3', {}, title, right), body);
  return { el, body, right };
}

function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.flatMap(([k, v]) => [h('dt', {}, k), h('dd', { class: 'num' }, v)]));
}

async function doAction(serverId, c, action, buttons) {
  const labels = { start: 'khởi động', stop: 'dừng', restart: 'khởi động lại' };
  if (action !== 'start' && !confirm(`Bạn chắc chắn muốn ${labels[action]} container "${c.name}"?`)) return;
  buttons.forEach((b) => { b.disabled = true; });
  try {
    await api(`/api/servers/${serverId}/containers/${encodeURIComponent(c.name)}/${action}`, { method: 'POST' });
    toast(`Đã ${labels[action]} ${c.name}`);
  } catch (err) {
    toast(`Không thể ${labels[action]} ${c.name}: ${err.message}`, true);
  } finally {
    buttons.forEach((b) => { b.disabled = false; });
  }
}

function containerRow(serverId) {
  const chipCell = h('td');
  const nameEl = h('div', { class: 'c-name' });
  const imageEl = h('div', { class: 'c-image' });
  const statusCell = h('td', { class: 'muted' });
  const cpuCell = h('td', { class: 'num' });
  const memText = h('span');
  const memM = meter(0);
  memM.classList.add('mini-meter');
  const memCell = h('td', { class: 'num' }, memText, memM);
  const netCell = h('td', { class: 'num' });
  const blockCell = h('td', { class: 'num' });
  const pidsCell = h('td', { class: 'num' });
  const logsBtn = h('button', { class: 'btn sm' }, 'Logs');
  const toggleBtn = h('button', { class: 'btn sm' });
  const restartBtn = h('button', { class: 'btn sm' }, 'Restart');
  const buttons = [toggleBtn, restartBtn];
  const tr = h('tr', {},
    chipCell,
    h('td', {}, nameEl, imageEl),
    statusCell, cpuCell, memCell, netCell, blockCell, pidsCell,
    h('td', {}, h('div', { class: 'c-actions' }, logsBtn, toggleBtn, restartBtn)),
  );
  let c = null;
  logsBtn.addEventListener('click', () => openLogs(state.snaps.get(serverId), c));
  toggleBtn.addEventListener('click', () => doAction(serverId, c, c.state === 'running' ? 'stop' : 'start', buttons));
  restartBtn.addEventListener('click', () => doAction(serverId, c, 'restart', buttons));

  const update = (next) => {
    c = next;
    const running = c.state === 'running';
    chipCell.replaceChildren(statusChip(c.state));
    nameEl.textContent = c.name;
    nameEl.title = c.ports ? `Ports: ${c.ports}` : '';
    imageEl.textContent = c.image;
    imageEl.title = c.image;
    statusCell.textContent = c.status;
    cpuCell.textContent = running ? fmtPct(c.cpu, 2) : '–';
    memText.textContent = running ? `${fmtBytes(c.memUsed)} / ${fmtBytes(c.memLimit)}` : '–';
    memM.hidden = !running;
    setMeter(memM, c.memPct);
    memM.classList.add('mini-meter');
    netCell.textContent = running ? c.netIO : '–';
    blockCell.textContent = running ? c.blockIO : '–';
    pidsCell.textContent = running ? c.pids : '–';
    toggleBtn.textContent = running ? 'Stop' : 'Start';
    toggleBtn.classList.toggle('danger', running);
    restartBtn.hidden = !running;
  };
  return { tr, update };
}

function detailView(id) {
  const cfg = () => state.servers.get(id);
  setCrumbs(h('a', { href: '#/' }, 'Tổng quan'), h('span', {}, cfg().name));

  const title = h('h1');
  const chipSlot = h('span');
  const meta = h('div', { class: 'meta' });
  const errBox = h('div', { class: 'panel card-error', hidden: true });
  const resetBtn = h('button', { class: 'btn ghost', hidden: true, onclick: async () => {
    if (!confirm('Chỉ reset khi bạn chắc chắn server vừa được cài lại hoặc đổi SSH host key. Tiếp tục?')) return;
    await api(`/api/servers/${id}/reset-hostkey`, { method: 'POST' }).catch((e) => toast(e.message, true));
  } }, 'Reset host key');
  const head = h('div', { class: 'detail-head' },
    title, chipSlot,
    h('div', { class: 'actions' },
      h('button', { class: 'btn ghost', onclick: () => send({ t: 'refresh', server: id }) }, 'Làm mới'),
      resetBtn,
      h('button', { class: 'btn ghost', onclick: () => openServerForm(cfg(), groupsList()) }, 'Sửa'),
      h('button', { class: 'btn ghost danger', onclick: async () => {
        if (!confirm(`Xoá server "${cfg().name}" khỏi dashboard? (Không ảnh hưởng tới server thật)`)) return;
        await api(`/api/servers/${id}`, { method: 'DELETE' }).catch((e) => toast(e.message, true));
        location.hash = '#/';
      } }, 'Xoá')),
    meta,
  );

  const cpuP = statPanel('span-3', 'CPU');
  const memP = statPanel('span-3', 'RAM');
  const diskP = statPanel('span-6', 'Ổ đĩa');
  const usageP = statPanel('span-6', 'CPU & RAM (%)');
  const netP = statPanel('span-6', 'Network (không tính docker/veth)');
  const coresP = statPanel('span-12', 'CPU theo từng core');
  const usageChart = new TimeChart(usageP.body, { series: SPARK_SERIES, yMax: 100, format: (v) => `${Math.round(v)}%` });
  const netChart = new TimeChart(netP.body, {
    series: [{ key: 'rx', label: 'Nhận (RX)', color: '--series-1' }, { key: 'tx', label: 'Gửi (TX)', color: '--series-3' }],
    format: (v) => fmtRate(v),
    bytes: true,
  });

  // Bảng container
  const cSearch = h('input', { type: 'search', placeholder: 'Lọc container…', 'aria-label': 'Lọc container' });
  const onlyRunning = h('input', { type: 'checkbox' });
  const sortSel = h('select', { 'aria-label': 'Sắp xếp' },
    h('option', { value: 'project' }, 'Nhóm theo compose project'),
    h('option', { value: 'cpu' }, 'CPU cao nhất'),
    h('option', { value: 'mem' }, 'RAM cao nhất'),
    h('option', { value: 'name' }, 'Tên A→Z'));
  const tbody = h('tbody');
  const countEl = h('span', { class: 'right' });
  const dockerErr = h('div', { class: 'card-error', hidden: true });
  const containersP = h('section', { class: 'panel span-12' },
    h('h3', {}, 'Containers', countEl),
    h('div', { class: 'toolbar' }, cSearch, sortSel, h('label', { class: 'check' }, onlyRunning, 'Chỉ đang chạy')),
    dockerErr,
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, 'Trạng thái'), h('th', {}, 'Tên / Image'), h('th', {}, 'Uptime'),
        h('th', { class: 'num', title: '% của 1 core, có thể vượt 100% trên máy nhiều core' }, 'CPU'),
        h('th', { class: 'num' }, 'RAM'), h('th', { class: 'num' }, 'Net I/O'), h('th', { class: 'num' }, 'Block I/O'),
        h('th', { class: 'num' }, 'PIDs'), h('th', {}))),
      tbody)),
  );
  const rows = new Map();
  const projectRows = new Map();
  let lastOrder = null;

  view.replaceChildren(
    head, errBox,
    h('div', { class: 'panels' }, cpuP.el, memP.el, diskP.el, usageP.el, netP.el, coresP.el, containersP),
  );

  const renderContainers = (snap) => {
    const d = snap.docker;
    dockerErr.hidden = !d?.error;
    dockerErr.textContent = d?.error ? `Không đọc được Docker: ${d.error}` : '';
    const q = cSearch.value.trim().toLowerCase();
    let list = (d?.containers || []).filter((c) => (!onlyRunning.checked || c.state === 'running')
      && (!q || [c.name, c.image, c.project].some((v) => v?.toLowerCase().includes(q))));
    const sort = sortSel.value;
    if (sort === 'cpu') list = [...list].sort((a, b) => (b.cpu ?? -1) - (a.cpu ?? -1));
    if (sort === 'mem') list = [...list].sort((a, b) => (b.memUsed ?? -1) - (a.memUsed ?? -1));
    if (sort === 'name') list = [...list].sort((a, b) => a.name.localeCompare(b.name));
    countEl.textContent = d ? `${d.running} đang chạy / ${d.total}` : '';

    const nodes = [];
    let lastProject;
    for (const c of list) {
      if (sort === 'project' && c.project !== lastProject) {
        lastProject = c.project;
        const key = c.project || '—';
        if (!projectRows.has(key)) projectRows.set(key, h('tr', { class: 'project-row' }, h('td', { colspan: 9 })));
        const n = list.filter((x) => x.project === c.project).length;
        projectRows.get(key).firstChild.textContent = c.project ? `Compose: ${c.project} (${n})` : `Container riêng lẻ (${n})`;
        nodes.push(projectRows.get(key));
      }
      if (!rows.has(c.id)) rows.set(c.id, containerRow(id));
      const row = rows.get(c.id);
      row.update(c);
      nodes.push(row.tr);
    }
    // Chỉ sắp xếp lại DOM khi thứ tự đổi, để không mất focus/hover mỗi lần cập nhật.
    const order = nodes.map((n) => n.dataset.key || (n.dataset.key = Math.random().toString(36).slice(2))).join();
    if (order !== lastOrder) {
      lastOrder = order;
      tbody.replaceChildren(...nodes);
      if (!nodes.length) tbody.append(h('tr', {}, h('td', { colspan: 9, class: 'empty' }, d ? 'Không có container nào.' : 'Đang tải…')));
    }
  };
  for (const el of [cSearch, onlyRunning, sortSel]) el.addEventListener('input', () => renderContainers(state.snaps.get(id) || {}));

  const update = () => {
    const c = cfg();
    if (!c) return;
    const snap = state.snaps.get(id) || { status: 'connecting' };
    const host = snap.host;
    title.textContent = c.name;
    chipSlot.replaceChildren(statusChip(snap.status));
    meta.textContent = [`${c.username}@${c.host}:${c.port}`, host?.hostname, host?.os, host && `kernel ${host.kernel}`, host?.cpuModel,
      snap.updatedAt && `cập nhật ${fmtTime(snap.updatedAt)}`].filter(Boolean).join(' · ');
    errBox.hidden = !snap.error;
    errBox.textContent = snap.error ? `Lỗi kết nối: ${snap.error}` : '';
    resetBtn.hidden = !/host key/i.test(snap.error || '');

    if (host) {
      cpuP.right.textContent = `${host.cores} core`;
      cpuP.body.replaceChildren(
        h('div', { class: 'big num' }, fmtPct(host.cpu)), meter(host.cpu),
        kv([['Load 1/5/15', host.load.map((v) => v.toFixed(2)).join(' / ')], ['Uptime', fmtUptime(host.uptime)]]),
      );
      memP.right.textContent = fmtBytes(host.mem.total);
      memP.body.replaceChildren(
        h('div', { class: 'big num' }, fmtPct(host.mem.pct)), meter(host.mem.pct),
        kv([['Đang dùng', fmtBytes(host.mem.used)], ['Swap', host.swap.total ? `${fmtBytes(host.swap.used)} / ${fmtBytes(host.swap.total)}` : 'không có']]),
      );
      diskP.body.replaceChildren(...host.disks.map((d) => h('div', { style: 'margin-bottom:10px' },
        h('div', { class: 'meter-row' }, h('span', {}, `${d.mount} `, h('span', { class: 'muted' }, d.fs)),
          h('span', { class: 'num' }, `${fmtBytes(d.used)} / ${fmtBytes(d.size)} · ${fmtPct(d.pct, 0)}`)),
        meter(d.pct))));
      coresP.body.replaceChildren(h('div', { class: 'cores' }, host.perCore.map((p, i) => {
        const fill = h('span', { class: 'fill' });
        fill.style.width = `${p ?? 0}%`;
        return h('div', { class: `core ${level(p)}`, title: `Core ${i}: ${fmtPct(p)}` },
          h('span', { class: 'lbl' }, `#${i}`), h('span', { class: 'val num' }, p == null ? '–' : `${Math.round(p)}%`), fill);
      })));
    }
    const hist = state.history.get(id) || [];
    usageChart.setData(hist);
    netChart.setData(hist);
    renderContainers(snap);
  };

  update();
  return {
    update: (sid) => { if (sid === id) update(); },
    destroy: () => { usageChart.destroy(); netChart.destroy(); },
  };
}

// ---------------------------------------------------------------- Boot
document.getElementById('logout').addEventListener('click', async () => {
  await api('/api/logout', { method: 'POST' }).catch(() => {});
  location.href = '/login.html';
});
document.getElementById('theme-toggle').addEventListener('click', () => {
  const root = document.documentElement;
  const isDark = root.dataset.theme ? root.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  root.dataset.theme = isDark ? 'light' : 'dark';
  try { localStorage.setItem('theme', root.dataset.theme); } catch {}
});

initLogs(send);
api('/api/me').then(connect).catch(() => {});
