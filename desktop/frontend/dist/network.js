// Network tab: ports the server listens on (with the process or container behind each one) and
// port forwarding — reach a database or admin page on the server from this computer (like ssh -L).
import { h, toast } from './util.js';
import { call, on, copyText } from './bridge.js';
import { panel, modal } from './ui.js';
import { icon } from './icons.js';
import { openMenu } from './context-menu.js';

let tunnelList = [];
const tunnelViews = new Set();
on('tunnels', (list) => { tunnelList = list; tunnelViews.forEach((fn) => fn()); });

// Calls fn whenever the forwards change; returns the function that stops watching.
export function watchTunnels(fn) {
  tunnelViews.add(fn);
  return () => tunnelViews.delete(fn);
}

export const containerTunnels = (serverId, name) => tunnelList.filter((t) => t.serverId === serverId && t.container === name);

// TCP ports a container serves, from its `docker ps` port string. Published ports are reached
// through the server's own port; unpublished ones (host: null) through the container's IP.
function forwardTargets(c) {
  const out = [];
  const seen = new Set();
  const add = (host, port, inner, label) => {
    const key = `${host}:${port}`;
    if (!seen.has(key)) { seen.add(key); out.push({ host, port, inner, label }); }
  };
  const range = (lo, hi) => Array.from({ length: Math.min(hi - lo + 1, 10) }, (_, i) => lo + i);
  for (const part of (c.ports || '').split(',').map((s) => s.trim())) {
    const pub = /^(?:(.*):)?(\d+)(?:-(\d+))?->(\d+)(?:-\d+)?\/tcp$/.exec(part);
    if (pub) {
      const bind = (pub[1] || '').replace(/^\[|\]$/g, '');
      const host = !bind || bind === '0.0.0.0' || bind === '::' ? '127.0.0.1' : bind;
      const lo = Number(pub[2]);
      const inner = Number(pub[4]);
      range(lo, Number(pub[3] || lo)).forEach((p) => add(host, p, inner + p - lo, `${p} → ${inner + p - lo}/tcp`));
      continue;
    }
    const internal = /^(\d+)(?:-(\d+))?\/tcp$/.exec(part);
    if (internal) range(Number(internal[1]), Number(internal[2] || internal[1])).forEach((p) => add(null, p, p, `${p}/tcp (not published)`));
  }
  return out;
}

// Where the server reaches a port inside the container: its IP on a Docker network (or the server itself with host networking).
async function containerIP(serverId, name) {
  const d = await call('InspectContainer', serverId, name);
  if (d.HostConfig?.NetworkMode === 'host') return '127.0.0.1';
  const ip = Object.values(d.NetworkSettings?.Networks || {}).map((n) => n.IPAddress).find(Boolean) || d.NetworkSettings?.IPAddress;
  if (!ip) throw new Error(`${name} has no IP address on a Docker network`);
  return ip;
}

// localPort 0: the same port number on this computer when it is free, any free port otherwise.
async function startContainerTunnel(serverId, c, target, serverName, localPort = 0) {
  const host = target.host || await containerIP(serverId, c.name);
  const existing = !localPort && tunnelList.find((t) => t.serverId === serverId && t.remoteHost === host && t.remotePort === target.port);
  if (existing) {
    await copyText(`localhost:${existing.localPort}`).catch(() => {});
    toast(`Already forwarded to localhost:${existing.localPort} (address copied)`);
    return;
  }
  let t;
  if (localPort) {
    t = await call('StartTunnel', serverId, localPort, host, target.port, c.name);
  } else {
    try {
      t = await call('StartTunnel', serverId, target.port, host, target.port, c.name);
    } catch {
      t = await call('StartTunnel', serverId, 0, host, target.port, c.name);
    }
  }
  await copyText(`localhost:${t.localPort}`).catch(() => {});
  toast(`Forwarding localhost:${t.localPort} → ${c.name}:${target.port} via ${serverName()} (address copied)`);
}

// Pick the container port and the port on this computer.
function forwardDialog(serverId, c, serverName, targets) {
  const m = modal({ title: `Forward a port of ${c.name}`, sub: 'Opens a port on this computer (localhost only) that leads to the container through SSH.' });
  const OTHER = 'other';
  const portSel = h('select', { 'aria-label': 'Container port' },
    targets.map((tg, i) => h('option', { value: String(i) }, tg.label)),
    h('option', { value: OTHER }, 'Another port inside the container…'));
  const otherPort = h('input', { type: 'number', min: 1, max: 65535, placeholder: 'e.g. 8080', 'aria-label': 'Port inside the container' });
  const otherLabel = h('label', {}, 'Port inside the container', otherPort);
  const localPort = h('input', { type: 'number', min: 1, max: 65535, 'aria-label': 'Port on this computer' });
  const msg = h('p', { class: 'form-msg error' });
  const form = h('form', { class: 'form plain' },
    targets.length ? h('label', {}, 'Container port', portSel) : '',
    otherLabel,
    h('label', {}, 'Port on this computer', localPort),
    h('p', { class: 'hint' }, 'Leave the port on this computer empty to use the same number, or any free port when it is taken.'),
    msg);
  const target = () => (portSel.value === OTHER || !targets.length ? { host: null, port: Number(otherPort.value) } : targets[Number(portSel.value)]);
  const sync = () => {
    otherLabel.hidden = targets.length > 0 && portSel.value !== OTHER;
    localPort.placeholder = target().port ? `${target().port} or any free port` : 'same, or any free port';
  };
  portSel.addEventListener('change', () => { sync(); (otherLabel.hidden ? localPort : otherPort).focus(); });
  otherPort.addEventListener('input', sync);
  sync();
  const ok = h('button', { type: 'button', class: 'btn primary' }, icon('forward'), 'Forward');
  m.body.append(form);
  m.foot.append(h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), ok);
  const valid = (v) => Number.isInteger(v) && v >= 1 && v <= 65535;
  const submit = async () => {
    const tg = target();
    const lp = localPort.value ? Number(localPort.value) : 0;
    if (!valid(tg.port)) { msg.textContent = 'Enter the port inside the container (1-65535).'; otherPort.focus(); return; }
    if (lp && !valid(lp)) { msg.textContent = 'The port on this computer must be between 1 and 65535.'; localPort.focus(); return; }
    ok.disabled = true;
    msg.textContent = '';
    try {
      await startContainerTunnel(serverId, c, tg, serverName, lp);
      m.close();
    } catch (err) {
      msg.textContent = err.message;
      ok.disabled = false;
    }
  };
  ok.addEventListener('click', submit);
  form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
  (targets.length ? localPort : otherPort).focus();
}

// The container's Forward button: a menu with one-click forwards of its TCP ports, a dialog to choose
// the ports, and its active forwards.
export function forwardContainer(e, anchor, serverId, c, serverName) {
  const run = (fn) => fn().catch((err) => toast(err.message, true));
  const targets = forwardTargets(c);
  const mine = containerTunnels(serverId, c.name);
  if (!mine.length && !targets.length) {
    forwardDialog(serverId, c, serverName, targets);
    return;
  }
  openMenu(e, anchor, [
    ...targets.map((tg) => ({ label: `Forward to localhost:${tg.port} (→ ${tg.inner}/tcp${tg.host ? '' : ', not published'})`, action: () => run(() => startContainerTunnel(serverId, c, tg, serverName)) })),
    { label: 'Forward on another local port…', action: () => forwardDialog(serverId, c, serverName, targets) },
    ...mine.map((t) => ({ label: `Copy localhost:${t.localPort}`, action: () => copyText(`localhost:${t.localPort}`).then(() => toast('Address copied')) })),
    ...mine.map((t) => ({ label: `Stop localhost:${t.localPort} → ${t.remotePort}`, danger: true, action: () => call('StopTunnel', t.id) })),
  ]);
}

// Which container publishes a host port, from `docker ps` port strings like "0.0.0.0:8080->80/tcp".
function containerForPort(containers, port, proto) {
  for (const c of containers || []) {
    for (const part of (c.ports || '').split(',')) {
      const m = /:(\d+)(?:-(\d+))?->(\d+)(?:-\d+)?\/(tcp|udp)/.exec(part.trim());
      if (!m || m[4] !== proto) continue;
      const lo = Number(m[1]);
      const hi = Number(m[2] || m[1]);
      if (port >= lo && port <= hi) return `${c.name} (${m[3]}/${m[4]})`;
    }
  }
  return '';
}

const isPublic = (addrs) => addrs.some((a) => a === '0.0.0.0' || a === '*' || a === '[::]' || a === '::');

export function networkTab(serverId, { containers, serverName }) {
  // ---- listening ports
  const refreshBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('refresh'), 'Refresh');
  const status = h('span', { class: 'muted' });
  const portsP = panel('span-12', 'Listening ports', status, refreshBtn);
  const search = h('input', { type: 'search', placeholder: 'Filter port, process, container…', 'aria-label': 'Filter ports' });
  const tbody = h('tbody');
  const note = h('p', { class: 'hint', hidden: true }, 'Process names of other users need root: connect as root or allow sudo without a password to see them.');
  portsP.body.append(
    h('div', { class: 'toolbar' }, search),
    h('div', { class: 'table-wrap' }, h('table', { class: 'ports-table' },
      h('thead', {}, h('tr', {}, ['Port', 'Proto', 'Listening on', 'Reachable from', 'Process', 'Container', ''].map((x, i) => h('th', { class: i === 0 ? 'num' : '' }, x)))),
      tbody)),
    note);

  // ---- tunnels
  const localPort = h('input', { type: 'number', min: 0, max: 65535, placeholder: 'auto', 'aria-label': 'Local port' });
  const remoteHost = h('input', { value: '127.0.0.1', spellcheck: 'false', 'aria-label': 'Remote host' });
  const remotePort = h('input', { type: 'number', min: 1, max: 65535, placeholder: 'e.g. 5432', 'aria-label': 'Remote port' });
  const startBtn = h('button', { type: 'button', class: 'btn primary' }, icon('forward'), 'Start forwarding');
  const tunnelsEl = h('div', { class: 'tunnels' });
  const tunP = panel('span-12', 'Port forwarding');
  tunP.body.append(
    h('p', { class: 'hint' }, 'Opens a port on this computer (localhost only) that leads to a host and port reachable from the server, through the existing SSH connection. Use it for databases, admin pages or services bound to 127.0.0.1 on the server.'),
    h('div', { class: 'tunnel-form' },
      h('label', {}, 'Local port', localPort),
      h('span', { class: 'arrow' }, '→'),
      h('label', {}, 'Host (seen from the server)', remoteHost),
      h('label', {}, 'Port', remotePort),
      startBtn),
    tunnelsEl);

  let ports = null;
  let active = false;

  function forward(p) {
    remotePort.value = p.port;
    remoteHost.value = p.addrs.find((a) => a !== '0.0.0.0' && a !== '*' && !a.includes(':')) || '127.0.0.1';
    if (remoteHost.value === '0.0.0.0') remoteHost.value = '127.0.0.1';
    localPort.focus();
    tunP.el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }

  function render() {
    if (!ports) return;
    const q = search.value.trim().toLowerCase();
    const list = containers();
    const rows = ports.ports.map((p) => ({ ...p, container: containerForPort(list, p.port, p.proto) }))
      .filter((p) => !q || [String(p.port), p.process, p.container, p.addrs.join(' ')].some((v) => v?.toLowerCase().includes(q)));
    note.hidden = ports.withProcesses;
    status.textContent = `${ports.ports.length} ports`;
    tbody.replaceChildren(...(rows.length ? rows.map((p) => h('tr', {},
      h('td', { class: 'num mono' }, p.port),
      h('td', {}, p.proto),
      h('td', { class: 'mono muted' }, p.addrs.join(', ')),
      h('td', {}, isPublic(p.addrs) ? h('span', { class: 'chip warning', title: 'Listening on every interface: reachable from the network unless a firewall blocks it' }, 'network')
        : h('span', { class: 'chip good', title: 'Only reachable from the server itself' }, 'this server only')),
      h('td', {}, p.process ? `${p.process}${p.pid ? ` (${p.pid})` : ''}` : h('span', { class: 'muted' }, '–')),
      h('td', {}, p.container || h('span', { class: 'muted' }, '–')),
      h('td', {}, p.proto === 'tcp' ? h('button', { type: 'button', class: 'btn sm act act-exec', title: 'Forward this port to this computer', onclick: () => forward(p) }, icon('forward'), 'Forward') : '')))
      : [h('tr', {}, h('td', { colspan: 7, class: 'empty' }, 'No ports match.'))]));
  }

  async function load() {
    status.textContent = 'Loading…';
    try {
      ports = await call('ListeningPorts', serverId);
    } catch (err) {
      status.textContent = err.message;
      return;
    }
    render();
  }

  function renderTunnels() {
    const mine = tunnelList.filter((t) => t.serverId === serverId);
    tunnelsEl.replaceChildren(...(mine.length ? mine.map((t) => h('div', { class: 'tunnel' },
      h('span', { class: 'dot good' }),
      h('strong', { class: 'mono' }, `localhost:${t.localPort}`),
      h('span', { class: 'muted' }, ' → '),
      h('span', { class: 'mono' }, `${t.remoteHost}:${t.remotePort}`),
      t.container ? h('span', {}, ` (${t.container})`) : '',
      h('span', { class: 'muted' }, ` on ${t.server} · ${t.active} open connection${t.active === 1 ? '' : 's'}`),
      t.lastError ? h('span', { class: 'chip critical', title: t.lastError }, 'last connection failed') : '',
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'btn sm act act-logs', onclick: () => copyText(`localhost:${t.localPort}`).then(() => toast('Address copied')) }, 'Copy address'),
      h('button', { type: 'button', class: 'btn sm act act-remove', onclick: () => call('StopTunnel', t.id) }, icon('stop'), 'Stop')))
      : [h('div', { class: 'muted' }, 'No forwarding active for this server. Forwards stop when the app closes.')]));
  }
  tunnelViews.add(renderTunnels);

  startBtn.addEventListener('click', async () => {
    const rp = Number(remotePort.value);
    if (!rp) { remotePort.focus(); return; }
    try {
      const t = await call('StartTunnel', serverId, Number(localPort.value || 0), remoteHost.value, rp, '');
      toast(`Forwarding localhost:${t.localPort} → ${t.remoteHost}:${t.remotePort} via ${serverName()}`);
      localPort.value = '';
    } catch (err) {
      toast(err.message, true);
    }
  });
  search.addEventListener('input', render);
  refreshBtn.addEventListener('click', load);
  call('ListTunnels').then((list) => { tunnelList = list; tunnelViews.forEach((fn) => fn()); }).catch(() => {});

  return {
    panels: [portsP.el, tunP.el],
    setActive(v) {
      active = v;
      if (active && !ports) load();
    },
    destroy() { tunnelViews.delete(renderTunnels); },
  };
}
