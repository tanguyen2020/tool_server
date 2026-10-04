import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import express from 'express';
import { WebSocketServer } from 'ws';
import { config } from './config.js';
import { login, logout, requireAuth, userFromRequest } from './auth.js';
import { store } from './store.js';
import { pool } from './ssh-pool.js';
import { collector } from './collector.js';
import { containerAction, streamLogs } from './docker.js';

const app = express();
app.set('trust proxy', process.env.TRUST_PROXY === 'true');
app.disable('x-powered-by');
app.use(express.json({ limit: '64kb' }));
app.use((_req, res, next) => {
  res.set({
    'X-Frame-Options': 'DENY',
    'X-Content-Type-Options': 'nosniff',
    'Referrer-Policy': 'no-referrer',
    'Content-Security-Policy': "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'",
  });
  next();
});

const wrap = (fn) => async (req, res) => {
  try {
    res.json(await fn(req, res));
  } catch (err) {
    res.status(400).json({ error: err.message });
  }
};

app.post('/api/login', login);
app.post('/api/logout', logout);
app.use('/api', requireAuth);
app.get('/api/me', (req, res) => res.json({ user: req.user.u }));

app.get('/api/servers', (_req, res) => res.json(store.list().map((s) => store.toPublic(s))));
app.post('/api/servers', wrap((req) => store.toPublic(store.add(req.body))));
app.put('/api/servers/:id', wrap((req) => store.toPublic(store.update(req.params.id, req.body))));
app.delete('/api/servers/:id', wrap((req) => { store.remove(req.params.id); return { ok: true }; }));
app.post('/api/servers/test', wrap((req) => pool.test(store.draft(req.body, req.body.id))));
app.post('/api/servers/:id/reset-hostkey', wrap((req) => {
  const s = store.get(req.params.id);
  if (!s) throw new Error('Không tìm thấy server');
  store.setHostKey(s.id, undefined);
  pool.drop(s.id);
  collector.collect(s.id);
  return { ok: true };
}));
app.post('/api/servers/:id/containers/:container/:action', wrap(async (req) => {
  const { id, container, action } = req.params;
  const result = await containerAction(id, container, action);
  collector.collect(id);
  return result;
}));

const publicDir = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'public');
app.use(express.static(publicDir));

const server = http.createServer(app);
const wss = new WebSocketServer({ noServer: true, maxPayload: 16 * 1024 });

server.on('upgrade', (req, socket, head) => {
  // Chặn cross-site WebSocket hijacking: Origin phải trùng host đang phục vụ.
  const origin = req.headers.origin;
  let okOrigin = !origin;
  try { okOrigin ||= new URL(origin).host === req.headers.host; } catch {}
  if (req.url !== '/ws' || !okOrigin || !userFromRequest(req)) {
    socket.write('HTTP/1.1 401 Unauthorized\r\n\r\n');
    return socket.destroy();
  }
  wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws, req));
});

const send = (ws, msg) => ws.readyState === ws.OPEN && ws.send(JSON.stringify(msg));
const broadcast = (msg) => {
  const data = JSON.stringify(msg);
  for (const ws of wss.clients) if (ws.readyState === ws.OPEN) ws.send(data);
};

collector.on('snapshot', (snapshot) => broadcast({ t: 'snapshot', data: snapshot }));
collector.on('removed', (id) => broadcast({ t: 'removed', id }));
store.on('change', () => broadcast({ t: 'servers', data: store.list().map((s) => store.toPublic(s)) }));

wss.on('connection', (ws) => {
  const logStreams = new Map();
  collector.setViewers(wss.clients.size);
  send(ws, {
    t: 'init',
    servers: store.list().map((s) => store.toPublic(s)),
    snapshots: collector.all(),
    history: Object.fromEntries(collector.history),
  });

  ws.on('message', async (raw) => {
    let msg;
    try { msg = JSON.parse(raw); } catch { return; }
    if (msg.t === 'logs.start') {
      const sid = String(msg.sid);
      if (logStreams.has(sid) || logStreams.size >= 6) return send(ws, { t: 'logs.end', sid, reason: 'Đang mở quá nhiều luồng log' });
      logStreams.set(sid, null);
      try {
        const stop = await streamLogs(msg.server, msg.container, {
          tail: msg.tail,
          since: msg.since,
          onData: (data) => send(ws, { t: 'logs.data', sid, data }),
          onEnd: (reason) => { logStreams.delete(sid); send(ws, { t: 'logs.end', sid, reason }); },
        });
        // Client đã đóng log trong lúc đang mở stream.
        if (!logStreams.has(sid)) stop();
        else logStreams.set(sid, stop);
      } catch (err) {
        logStreams.delete(sid);
        send(ws, { t: 'logs.end', sid, reason: err.message });
      }
    } else if (msg.t === 'logs.stop') {
      const sid = String(msg.sid);
      logStreams.get(sid)?.();
      logStreams.delete(sid);
    } else if (msg.t === 'refresh' && store.get(msg.server)) {
      collector.collect(msg.server);
    }
  });

  ws.on('close', () => {
    for (const stop of logStreams.values()) stop?.();
    logStreams.clear();
    collector.setViewers(wss.clients.size);
  });
});

server.listen(config.port, config.host, () => {
  console.log(`Dashboard chạy tại http://${config.host === '0.0.0.0' ? 'localhost' : config.host}:${config.port}`);
  collector.start();
});
