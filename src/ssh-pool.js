import { Client } from 'ssh2';
import { store } from './store.js';

export const dockerBin = (server) => (server.useSudo ? 'sudo -n docker' : 'docker');

// Mỗi server giữ 1 kết nối SSH lâu dài; mọi lệnh chạy trên các channel của kết nối đó.
// Lưu ý: OpenSSH mặc định MaxSessions=10 channel/kết nối.
class SshConnection {
  constructor(server) {
    this.server = server;
    this.client = null;
    this.connecting = null;
    this.closed = false;
  }

  connect() {
    if (this.client) return Promise.resolve(this.client);
    if (this.connecting) return this.connecting;
    this.connecting = new Promise((resolve, reject) => {
      const s = store.credentials(this.server);
      const client = new Client();
      let hostKeyError = null;
      client
        .on('ready', () => {
          this.client = client;
          this.connecting = null;
          resolve(client);
        })
        .on('error', (err) => {
          this.connecting = null;
          reject(hostKeyError || err);
        })
        .on('close', () => {
          if (this.client === client) this.client = null;
          this.connecting = null;
          reject(hostKeyError || new Error('Kết nối SSH bị đóng'));
        });
      try {
        client.connect({
          host: s.host,
          port: s.port,
          username: s.username,
          password: s.authType === 'password' ? s.password : undefined,
          privateKey: s.authType === 'key' || s.authType === 'keyPath' ? s.privateKey : undefined,
          passphrase: s.passphrase,
          agent: s.authType === 'agent' ? process.env.SSH_AUTH_SOCK || (process.platform === 'win32' ? 'pageant' : undefined) : undefined,
          readyTimeout: 10000,
          keepaliveInterval: 15000,
          keepaliveCountMax: 3,
          hostHash: 'sha256',
          // Trust-on-first-use: lần đầu lưu fingerprint, các lần sau phải khớp (chống MITM).
          hostVerifier: (fingerprint) => {
            if (!this.server.hostKey) {
              if (this.server.id && store.get(this.server.id)) store.setHostKey(this.server.id, fingerprint);
              this.server.hostKey = fingerprint;
              return true;
            }
            if (this.server.hostKey === fingerprint) return true;
            hostKeyError = new Error(`Host key đã thay đổi (SHA256 ${fingerprint.slice(0, 16)}…). Nếu bạn vừa cài lại server, bấm "Reset host key".`);
            return false;
          },
        });
      } catch (err) {
        this.connecting = null;
        reject(err);
      }
    });
    return this.connecting;
  }

  async exec(command, { timeout = 30000 } = {}) {
    const client = await this.connect();
    return new Promise((resolve, reject) => {
      client.exec(command, (err, stream) => {
        if (err) return reject(err);
        let stdout = '';
        let stderr = '';
        const timer = setTimeout(() => {
          stream.close();
          reject(new Error(`Lệnh quá thời gian ${timeout / 1000}s`));
        }, timeout);
        stream.on('data', (d) => { stdout += d; });
        stream.stderr.on('data', (d) => { stderr += d; });
        stream.on('close', (code) => {
          clearTimeout(timer);
          resolve({ stdout, stderr, code });
        });
      });
    });
  }

  // Trả về channel để stream (dùng cho log realtime).
  async stream(command) {
    const client = await this.connect();
    return new Promise((resolve, reject) => {
      client.exec(command, (err, stream) => (err ? reject(err) : resolve(stream)));
    });
  }

  close() {
    this.closed = true;
    this.client?.end();
    this.client = null;
  }
}

class SshPool {
  constructor() {
    this.conns = new Map();
    // Cấu hình server đổi hoặc bị xoá -> bỏ kết nối cũ, lần sau tự kết nối lại.
    store.on('change', (id) => this.drop(id));
  }

  get(id) {
    let conn = this.conns.get(id);
    if (!conn) {
      const server = store.get(id);
      if (!server) throw new Error('Không tìm thấy server');
      conn = new SshConnection(server);
      this.conns.set(id, conn);
    }
    return conn;
  }

  drop(id) {
    this.conns.get(id)?.close();
    this.conns.delete(id);
  }

  // Thử kết nối với cấu hình chưa lưu (form "Thêm server").
  async test(server) {
    const conn = new SshConnection({ ...server });
    try {
      const { stdout } = await conn.exec(`hostname; ${dockerBin(server)} version --format "{{.Server.Version}}" 2>&1 | head -1`, { timeout: 15000 });
      const [hostname, ...docker] = stdout.trim().split('\n');
      return { hostname, docker: docker.join(' ').trim(), hostKey: conn.server.hostKey };
    } finally {
      conn.close();
    }
  }
}

export const pool = new SshPool();
