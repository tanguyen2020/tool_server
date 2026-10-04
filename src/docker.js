import { store } from './store.js';
import { pool, dockerBin } from './ssh-pool.js';

// Tên/ID container chỉ cho phép ký tự an toàn -> không thể chèn lệnh shell.
const CONTAINER_RE = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/;
const ACTIONS = new Set(['start', 'stop', 'restart']);
const TS_RE = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,9})?Z$/;

function resolve(serverId, container) {
  const server = store.get(serverId);
  if (!server) throw new Error('Không tìm thấy server');
  if (typeof container !== 'string' || !CONTAINER_RE.test(container)) throw new Error('Tên container không hợp lệ');
  return server;
}

export async function containerAction(serverId, container, action) {
  if (!ACTIONS.has(action)) throw new Error('Thao tác không hợp lệ');
  const server = resolve(serverId, container);
  const { code, stdout, stderr } = await pool.get(serverId).exec(`${dockerBin(server)} ${action} ${container}`, { timeout: 90000 });
  if (code !== 0) throw new Error((stderr || stdout).trim() || `docker ${action} thất bại (exit ${code})`);
  return { ok: true };
}

// Stream `docker logs -f`. Gom dữ liệu 100ms/lần để không làm nghẽn WebSocket khi log ồ ạt.
export async function streamLogs(serverId, container, { tail = 200, since, onData, onEnd }) {
  const server = resolve(serverId, container);
  const n = Math.max(0, Math.min(5000, Number.parseInt(tail, 10) || 0));
  // Nối lại sau khi container restart: lấy tiếp từ timestamp cuối cùng đã nhận.
  const from = typeof since === 'string' && TS_RE.test(since) ? `--since ${since}` : `--tail ${n}`;
  // `exec` để docker thay thế shell -> tín hiệu dừng đến thẳng tiến trình docker logs.
  const stream = await pool.get(serverId).stream(`exec ${dockerBin(server)} logs -f --timestamps ${from} ${container} 2>&1`);
  const MAX_BUFFER = 512 * 1024;
  let buffer = '';
  let dropped = 0;
  let ended = false;

  const flush = () => {
    if (dropped) {
      onData(`\n[dashboard] Bỏ qua ${dropped} byte log do quá nhiều dữ liệu\n`);
      dropped = 0;
    }
    if (buffer) {
      onData(buffer);
      buffer = '';
    }
  };
  const timer = setInterval(flush, 100);

  stream.on('data', (chunk) => {
    if (buffer.length > MAX_BUFFER) dropped += chunk.length;
    else buffer += chunk.toString('utf8');
  });
  stream.on('close', (code) => {
    if (ended) return;
    ended = true;
    clearInterval(timer);
    flush();
    onEnd(code === 0 || code == null ? 'Container đã dừng hoặc stream kết thúc' : `docker logs thoát với mã ${code}`);
  });

  return () => {
    if (ended) return;
    ended = true;
    clearInterval(timer);
    try { stream.signal('TERM'); } catch {}
    stream.close();
  };
}
