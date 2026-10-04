// Network tab: ports the server listens on (with the process or container behind each one) and
// port forwarding — reach a database or admin page on the server from this computer (like ssh -L).
import { h, toast } from './util.js';
import { call, on, copyText } from './bridge.js';
import { panel } from './ui.js';
import { icon } from './icons.js';

let tunnelList = [];
const tunnelViews = new Set();
on('tunnels', (list) => { tunnelList = list; tunnelViews.forEach((fn) => fn()); });

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
  const startBtn = h('button', { type: 'button', class: 'btn primary' }, icon('plug'), 'Start forwarding');
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
      h('td', {}, p.proto === 'tcp' ? h('button', { type: 'button', class: 'btn sm act act-exec', title: 'Forward this port to this computer', onclick: () => forward(p) }, icon('plug'), 'Forward') : '')))
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
      const t = await call('StartTunnel', serverId, Number(localPort.value || 0), remoteHost.value, rp);
      toast(`Forwarding localhost:${t.localPort} → ${t.remoteHost}:${t.remotePort} via ${serverName()}`);
      localPort.value = '';
    } catch (err) {
      toast(err.message, true);
    }
  });
  search.addEventListener('input', render);
  refreshBtn.addEventListener('click', load);
  call('ListTunnels').then((list) => { tunnelList = list; renderTunnels(); }).catch(() => {});

  return {
    panels: [portsP.el, tunP.el],
    setActive(v) {
      active = v;
      if (active && !ports) load();
    },
    destroy() { tunnelViews.delete(renderTunnels); },
  };
}
