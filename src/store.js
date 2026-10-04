import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { EventEmitter } from 'node:events';
import { config } from './config.js';

const FILE = path.join(config.dataDir, 'servers.json');
const KEY = crypto.createHash('sha256').update(`servers:${config.secret}`).digest();
const SECRET_FIELDS = ['password', 'privateKey', 'passphrase'];

function encrypt(text) {
  const iv = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv('aes-256-gcm', KEY, iv);
  const enc = Buffer.concat([cipher.update(text, 'utf8'), cipher.final()]);
  return `v1:${Buffer.concat([iv, cipher.getAuthTag(), enc]).toString('base64')}`;
}

function decrypt(value) {
  if (!value?.startsWith('v1:')) return value;
  const buf = Buffer.from(value.slice(3), 'base64');
  const decipher = crypto.createDecipheriv('aes-256-gcm', KEY, buf.subarray(0, 12));
  decipher.setAuthTag(buf.subarray(12, 28));
  return Buffer.concat([decipher.update(buf.subarray(28)), decipher.final()]).toString('utf8');
}

class ServerStore extends EventEmitter {
  constructor() {
    super();
    this.servers = fs.existsSync(FILE) ? JSON.parse(fs.readFileSync(FILE, 'utf8')) : [];
  }

  save() {
    const tmp = `${FILE}.tmp`;
    fs.writeFileSync(tmp, JSON.stringify(this.servers, null, 2), { mode: 0o600 });
    fs.renameSync(tmp, FILE);
  }

  list() {
    return this.servers;
  }

  get(id) {
    return this.servers.find((s) => s.id === id);
  }

  // Thông tin an toàn để gửi xuống trình duyệt (không có secret).
  toPublic(s) {
    const { password, privateKey, passphrase, ...rest } = s;
    return { ...rest, hasPassword: !!password, hasPrivateKey: !!privateKey, hasPassphrase: !!passphrase };
  }

  // Cấu hình đã giải mã, chỉ dùng nội bộ để mở kết nối SSH.
  credentials(s) {
    const out = { ...s };
    for (const f of SECRET_FIELDS) if (s[f]) out[f] = decrypt(s[f]);
    if (s.privateKeyPath && !out.privateKey) out.privateKey = fs.readFileSync(s.privateKeyPath, 'utf8');
    return out;
  }

  normalize(input, existing = {}) {
    const s = {
      ...existing,
      name: String(input.name ?? existing.name ?? '').trim(),
      host: String(input.host ?? existing.host ?? '').trim(),
      port: Number(input.port ?? existing.port ?? 22),
      username: String(input.username ?? existing.username ?? '').trim(),
      authType: ['password', 'key', 'keyPath', 'agent'].includes(input.authType) ? input.authType : existing.authType || 'key',
      privateKeyPath: String(input.privateKeyPath ?? existing.privateKeyPath ?? '').trim() || undefined,
      useSudo: input.useSudo !== undefined ? !!input.useSudo : !!existing.useSudo,
      group: String(input.group ?? existing.group ?? '').trim() || undefined,
    };
    if (!s.name) s.name = s.host;
    if (!s.host || !s.username) throw new Error('Host và username là bắt buộc');
    if (!Number.isInteger(s.port) || s.port < 1 || s.port > 65535) throw new Error('Port không hợp lệ');
    // Ô secret để trống khi sửa = giữ giá trị cũ.
    for (const f of SECRET_FIELDS) if (input[f]) s[f] = encrypt(String(input[f]));
    // Bỏ secret không còn dùng tới khi đổi kiểu xác thực.
    if (s.authType !== 'password') delete s.password;
    if (s.authType !== 'key') delete s.privateKey;
    if (s.authType !== 'key' && s.authType !== 'keyPath') delete s.passphrase;
    if (s.authType !== 'keyPath') delete s.privateKeyPath;
    if (s.authType === 'keyPath' && !s.privateKeyPath) throw new Error('Cần đường dẫn private key');
    if (s.authType === 'password' && !s.password) throw new Error('Cần mật khẩu SSH');
    if (s.authType === 'key' && !s.privateKey) throw new Error('Cần nội dung private key');
    // Đổi host/port thì phải tin lại host key mới.
    if (existing.host !== s.host || existing.port !== s.port) delete s.hostKey;
    return s;
  }

  // Tạo bản cấu hình tạm (chưa lưu) để thử kết nối.
  draft(input, id) {
    return this.normalize(input, (id && this.get(id)) || {});
  }

  add(input) {
    const s = { id: crypto.randomUUID(), ...this.normalize(input) };
    this.servers.push(s);
    this.save();
    this.emit('change', s.id);
    return s;
  }

  update(id, input) {
    const i = this.servers.findIndex((s) => s.id === id);
    if (i < 0) throw new Error('Không tìm thấy server');
    this.servers[i] = this.normalize(input, this.servers[i]);
    this.save();
    this.emit('change', id);
    return this.servers[i];
  }

  remove(id) {
    this.servers = this.servers.filter((s) => s.id !== id);
    this.save();
    this.emit('change', id);
  }

  setHostKey(id, hostKey) {
    const s = this.get(id);
    if (!s) return;
    s.hostKey = hostKey;
    this.save();
  }
}

export const store = new ServerStore();
