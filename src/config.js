import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

const dataDir = path.resolve(process.env.DATA_DIR || 'data');
fs.mkdirSync(dataDir, { recursive: true });

// APP_SECRET dùng để ký session và mã hoá mật khẩu/private key lưu trên đĩa.
// Nếu không đặt trong .env thì tự sinh một lần và lưu vào data/secret.key.
function loadSecret() {
  if (process.env.APP_SECRET) return process.env.APP_SECRET;
  const file = path.join(dataDir, 'secret.key');
  if (fs.existsSync(file)) return fs.readFileSync(file, 'utf8').trim();
  const secret = crypto.randomBytes(32).toString('hex');
  fs.writeFileSync(file, secret, { mode: 0o600 });
  return secret;
}

export const config = {
  host: process.env.HOST || '0.0.0.0',
  port: Number(process.env.PORT || 8080),
  adminUser: process.env.ADMIN_USER || 'admin',
  adminPassword: process.env.ADMIN_PASSWORD || '',
  cookieSecure: process.env.COOKIE_SECURE === 'true',
  sessionHours: Number(process.env.SESSION_HOURS || 12),
  // Chu kỳ lấy số liệu khi có người đang xem dashboard / khi không ai xem.
  pollInterval: Number(process.env.POLL_INTERVAL_MS || 5000),
  idlePollInterval: Number(process.env.IDLE_POLL_INTERVAL_MS || 30000),
  historyPoints: Number(process.env.HISTORY_POINTS || 360),
  dataDir,
  secret: loadSecret(),
};

if (!config.adminPassword) {
  console.error('Thiếu ADMIN_PASSWORD. Copy .env.example thành .env và đặt mật khẩu đăng nhập dashboard.');
  process.exit(1);
}
