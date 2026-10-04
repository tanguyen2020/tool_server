import { EventEmitter } from 'node:events';
import { config } from './config.js';
import { store } from './store.js';
import { pool, dockerBin } from './ssh-pool.js';

// Một lệnh SSH duy nhất mỗi chu kỳ, các phần cách nhau bằng dòng "@@TÊN".
const script = (docker) => `
export LC_ALL=C
echo @@HOST; hostname
echo @@OS; (. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")
echo @@KERNEL; uname -r
echo @@UPTIME; cat /proc/uptime
echo @@LOAD; cat /proc/loadavg
echo @@CPUMODEL; grep -m1 'model name' /proc/cpuinfo | cut -d: -f2-
echo @@STAT; grep '^cpu' /proc/stat
echo @@MEM; grep -E '^(MemTotal|MemAvailable|SwapTotal|SwapFree):' /proc/meminfo
echo @@DISK; df -PB1 -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs 2>/dev/null
echo @@NET; cat /proc/net/dev
echo @@DOCKER_PS; ${docker} ps -a --no-trunc --format '{{json .}}' 2>&1
echo @@DOCKER_STATS; ${docker} stats --no-stream --no-trunc --format '{{json .}}' 2>&1
`;

function sections(text) {
  const out = {};
  let cur = null;
  for (const line of text.split('\n')) {
    if (line.startsWith('@@')) { cur = line.slice(2).trim(); out[cur] = []; }
    else if (cur) out[cur].push(line);
  }
  for (const k of Object.keys(out)) out[k] = out[k].filter((l) => l.trim() !== '');
  return out;
}

const UNITS = { b: 1, kb: 1e3, mb: 1e6, gb: 1e9, tb: 1e12, kib: 1024, mib: 1024 ** 2, gib: 1024 ** 3, tib: 1024 ** 4 };
function parseSize(str) {
  const m = /([\d.]+)\s*([a-z]*)/i.exec(str || '');
  return m ? Number(m[1]) * (UNITS[m[2].toLowerCase()] ?? 1) : 0;
}

function parseCpuStat(lines) {
  const res = {};
  for (const l of lines) {
    const [name, ...nums] = l.trim().split(/\s+/);
    const v = nums.map(Number);
    const idle = v[3] + (v[4] || 0);
    // steal/guest đã nằm trong user/nice; chỉ cộng 8 cột đầu.
    const total = v.slice(0, 8).reduce((a, b) => a + b, 0);
    res[name] = { idle, total };
  }
  return res;
}

const cpuPct = (prev, cur) => {
  if (!prev || !cur) return null;
  const dt = cur.total - prev.total;
  return dt > 0 ? Math.max(0, Math.min(100, (1 - (cur.idle - prev.idle) / dt) * 100)) : 0;
};

const IGNORED_IFACES = /^(lo|veth|docker|br-|virbr|cni|flannel|cali|tun|tap)/;
function parseNet(lines) {
  let rx = 0, tx = 0;
  for (const l of lines.slice(2)) {
    const [iface, rest] = l.split(':');
    if (!rest || IGNORED_IFACES.test(iface.trim())) continue;
    const v = rest.trim().split(/\s+/).map(Number);
    rx += v[0];
    tx += v[8];
  }
  return { rx, tx };
}

function parseJsonLines(lines) {
  const ok = [];
  let error = null;
  for (const l of lines) {
    try { ok.push(JSON.parse(l)); } catch { error = (error ? `${error}\n` : '') + l; }
  }
  return { rows: ok, error };
}

function parseSnapshot(server, raw, prev) {
  const s = sections(raw);
  const now = Date.now();
  const stat = parseCpuStat(s.STAT || []);
  const coreNames = Object.keys(stat).filter((k) => k !== 'cpu');
  const mem = Object.fromEntries((s.MEM || []).map((l) => {
    const [k, v] = l.split(':');
    return [k, Number.parseInt(v, 10) * 1024];
  }));
  const [l1, l5, l15] = (s.LOAD?.[0] || '').split(' ').map(Number);
  const net = parseNet(s.NET || []);
  const dt = prev ? (now - prev.time) / 1000 : 0;

  const disks = (s.DISK || []).slice(1).map((l) => {
    const p = l.trim().split(/\s+/);
    const size = Number(p[1]), used = Number(p[2]);
    return { fs: p[0], mount: p.slice(5).join(' '), size, used, pct: size ? (used / size) * 100 : 0 };
  }).filter((d) => d.size > 0 && !d.mount.startsWith('/boot/efi'));

  const ps = parseJsonLines(s.DOCKER_PS || []);
  const stats = parseJsonLines(s.DOCKER_STATS || []);
  const statsById = new Map(stats.rows.map((r) => [r.ID, r]));
  const dockerError = ps.rows.length === 0 && ps.error ? ps.error.trim() : null;

  // Compose project trước (A→Z), container lẻ xuống cuối.
  const containers = ps.rows.map((c) => {
    const st = statsById.get(c.ID);
    const labels = Object.fromEntries((c.Labels || '').split(',').filter(Boolean).map((kv) => {
      const i = kv.indexOf('=');
      return [kv.slice(0, i), kv.slice(i + 1)];
    }));
    const [memUsed, memLimit] = (st?.MemUsage || '').split('/');
    const state = c.State || (/^Up/.test(c.Status) ? 'running' : 'exited');
    return {
      id: c.ID,
      shortId: c.ID.slice(0, 12),
      name: c.Names,
      image: c.Image,
      state,
      status: c.Status,
      ports: c.Ports,
      createdAt: c.CreatedAt,
      project: labels['com.docker.compose.project'] || null,
      service: labels['com.docker.compose.service'] || null,
      cpu: st && state === 'running' ? Number.parseFloat(st.CPUPerc) : null,
      memUsed: st && state === 'running' ? parseSize(memUsed) : null,
      memLimit: st && state === 'running' ? parseSize(memLimit) : null,
      memPct: st && state === 'running' ? Number.parseFloat(st.MemPerc) : null,
      netIO: st?.NetIO || null,
      blockIO: st?.BlockIO || null,
      pids: st ? Number(st.PIDs) : null,
    };
  }).sort((a, b) => (!a.project - !b.project) || (a.project || '').localeCompare(b.project || '') || a.name.localeCompare(b.name));

  const memTotal = mem.MemTotal || 0;
  const memAvail = mem.MemAvailable || 0;

  return {
    raw: { time: now, stat, net },
    snapshot: {
      id: server.id,
      name: server.name,
      group: server.group || null,
      status: 'online',
      error: null,
      updatedAt: now,
      host: {
        hostname: s.HOST?.[0] || '',
        os: s.OS?.[0] || '',
        kernel: s.KERNEL?.[0] || '',
        uptime: Number.parseFloat(s.UPTIME?.[0] || '0'),
        load: [l1, l5, l15],
        cpuModel: (s.CPUMODEL?.[0] || '').trim(),
        cores: coreNames.length,
        cpu: cpuPct(prev?.stat.cpu, stat.cpu),
        perCore: coreNames.map((n) => cpuPct(prev?.stat[n], stat[n])),
        mem: { total: memTotal, used: memTotal - memAvail, pct: memTotal ? ((memTotal - memAvail) / memTotal) * 100 : 0 },
        swap: { total: mem.SwapTotal || 0, used: (mem.SwapTotal || 0) - (mem.SwapFree || 0) },
        disks,
        net: dt > 0 ? { rx: Math.max(0, (net.rx - prev.net.rx) / dt), tx: Math.max(0, (net.tx - prev.net.tx) / dt) } : { rx: null, tx: null },
      },
      docker: {
        available: !dockerError,
        error: dockerError,
        running: containers.filter((c) => c.state === 'running').length,
        total: containers.length,
        containers,
      },
    },
  };
}

class Collector extends EventEmitter {
  constructor() {
    super();
    this.snapshots = new Map();
    this.prev = new Map();
    this.history = new Map();
    this.inFlight = new Set();
    this.viewers = 0;
    this.timer = null;
    store.on('change', (id) => {
      this.prev.delete(id);
      if (!store.get(id)) {
        this.snapshots.delete(id);
        this.history.delete(id);
        this.emit('removed', id);
      } else {
        this.collect(id);
      }
    });
  }

  start() {
    const loop = async () => {
      await Promise.allSettled(store.list().map((s) => this.collect(s.id)));
      this.timer = setTimeout(loop, this.viewers > 0 ? config.pollInterval : config.idlePollInterval);
    };
    loop();
  }

  setViewers(n) {
    const wasIdle = this.viewers === 0;
    this.viewers = n;
    // Có người vừa mở dashboard -> lấy số liệu ngay, không đợi chu kỳ idle.
    if (wasIdle && n > 0 && this.timer) {
      clearTimeout(this.timer);
      this.start();
    }
  }

  async collect(id) {
    if (this.inFlight.has(id)) return;
    const server = store.get(id);
    if (!server) return;
    this.inFlight.add(id);
    try {
      const { stdout } = await pool.get(id).exec(script(dockerBin(server)), { timeout: 25000 });
      const { raw, snapshot } = parseSnapshot(server, stdout, this.prev.get(id));
      this.prev.set(id, raw);
      this.push(id, snapshot);
    } catch (err) {
      const last = this.snapshots.get(id);
      this.prev.delete(id);
      this.push(id, {
        ...(last || { id, host: null, docker: null }),
        id,
        name: server.name,
        group: server.group || null,
        status: 'offline',
        error: err.message,
        updatedAt: Date.now(),
      }, false);
    } finally {
      this.inFlight.delete(id);
    }
  }

  push(id, snapshot, record = true) {
    if (!store.get(id)) return;
    this.snapshots.set(id, snapshot);
    if (record && snapshot.host?.cpu != null) {
      const h = this.history.get(id) || [];
      h.push({ t: snapshot.updatedAt, cpu: snapshot.host.cpu, mem: snapshot.host.mem.pct, rx: snapshot.host.net.rx, tx: snapshot.host.net.tx });
      if (h.length > config.historyPoints) h.splice(0, h.length - config.historyPoints);
      this.history.set(id, h);
    }
    this.emit('snapshot', snapshot);
  }

  all() {
    return store.list().map((s) => this.snapshots.get(s.id) || { id: s.id, name: s.name, group: s.group || null, status: 'connecting', host: null, docker: null });
  }
}

export const collector = new Collector();
