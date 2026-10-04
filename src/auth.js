import crypto from 'node:crypto';
import { config } from './config.js';

const COOKIE = 'sd_session';
const hmac = (data) => crypto.createHmac('sha256', config.secret).update(data).digest('base64url');

function safeEqual(a, b) {
  const ha = crypto.createHash('sha256').update(String(a)).digest();
  const hb = crypto.createHash('sha256').update(String(b)).digest();
  return crypto.timingSafeEqual(ha, hb);
}

export function parseCookies(header = '') {
  const out = {};
  for (const part of header.split(';')) {
    const i = part.indexOf('=');
    if (i > 0) out[part.slice(0, i).trim()] = decodeURIComponent(part.slice(i + 1).trim());
  }
  return out;
}

function issueToken(user) {
  const payload = Buffer.from(JSON.stringify({ u: user, exp: Date.now() + config.sessionHours * 3600e3 })).toString('base64url');
  return `${payload}.${hmac(payload)}`;
}

export function verifyToken(token) {
  if (!token) return null;
  const [payload, sig] = token.split('.');
  if (!payload || !sig || !safeEqual(sig, hmac(payload))) return null;
  try {
    const data = JSON.parse(Buffer.from(payload, 'base64url').toString());
    return data.exp > Date.now() ? data : null;
  } catch {
    return null;
  }
}

export const userFromRequest = (req) => verifyToken(parseCookies(req.headers.cookie)[COOKIE]);

// Chống dò mật khẩu: khoá IP 15 phút sau 5 lần sai.
const failures = new Map();
function isLocked(ip) {
  const f = failures.get(ip);
  if (!f) return false;
  if (Date.now() - f.first > 15 * 60e3) { failures.delete(ip); return false; }
  return f.count >= 5;
}

export function login(req, res) {
  const ip = req.ip;
  if (isLocked(ip)) return res.status(429).json({ error: 'Sai quá nhiều lần, thử lại sau 15 phút' });
  const { username, password } = req.body || {};
  if (safeEqual(username, config.adminUser) && safeEqual(password, config.adminPassword)) {
    failures.delete(ip);
    res.cookie(COOKIE, issueToken(username), {
      httpOnly: true, sameSite: 'strict', secure: config.cookieSecure, maxAge: config.sessionHours * 3600e3,
    });
    return res.json({ ok: true });
  }
  const f = failures.get(ip) || { count: 0, first: Date.now() };
  f.count++;
  failures.set(ip, f);
  res.status(401).json({ error: 'Sai tài khoản hoặc mật khẩu' });
}

export function logout(_req, res) {
  res.clearCookie(COOKIE);
  res.json({ ok: true });
}

export function requireAuth(req, res, next) {
  const user = userFromRequest(req);
  if (!user) return res.status(401).json({ error: 'Chưa đăng nhập' });
  req.user = user;
  next();
}
