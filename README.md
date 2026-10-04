# Server Dashboard

Dashboard web theo dõi nhiều server Debian + Docker qua **SSH, không cần cài agent**.

- Server: trạng thái online/offline, CPU tổng + từng core, RAM, swap, load, uptime, ổ đĩa, network, biểu đồ realtime.
- Container: trạng thái, CPU, RAM, Net/Block I/O, nhóm theo docker compose project, Start / Stop / Restart, xem log realtime (lọc, tạm dừng, tải về, tự nối lại khi container restart).
- Thêm/sửa server ngay trên giao diện, hỗ trợ private key, đường dẫn key, SSH agent hoặc mật khẩu.

## Chạy

```bash
cp .env.example .env      # rồi đặt ADMIN_PASSWORD
npm install
npm start                 # http://localhost:8080
```

Hoặc bằng Docker: `docker compose up -d --build`.

## Chuẩn bị trên mỗi server Debian

Tạo một user riêng cho dashboard, đăng nhập bằng SSH key:

```bash
sudo adduser --disabled-password --gecos "" monitor
sudo usermod -aG docker monitor          # để chạy lệnh docker không cần sudo
sudo -u monitor mkdir -p ~monitor/.ssh
echo "ssh-ed25519 AAAA... dashboard" | sudo tee -a ~monitor/.ssh/authorized_keys
```

> ⚠️ Thuộc group `docker` tương đương quyền root trên server. Nếu không muốn vậy, bỏ bước `usermod`,
> cho phép riêng lệnh docker trong sudoers (`monitor ALL=(root) NOPASSWD: /usr/bin/docker`)
> và tích "Chạy docker bằng sudo -n" khi thêm server.

## Cách hoạt động

Mỗi server giữ 1 kết nối SSH lâu dài. Mỗi chu kỳ (5 giây khi có người xem, 30 giây khi không ai mở dashboard)
tool chạy một lệnh shell duy nhất đọc `/proc`, `df`, `docker ps` và `docker stats --no-stream`.
Log container được stream bằng `docker logs -f` qua WebSocket.

| File | Vai trò |
|---|---|
| `src/ssh-pool.js` | Kết nối SSH, kiểm tra host key (trust-on-first-use) |
| `src/collector.js` | Thu thập & tính số liệu, lưu lịch sử trong RAM |
| `src/docker.js` | Start/stop/restart, stream log |
| `src/store.js` | Lưu danh sách server vào `data/servers.json`, mã hoá AES-256-GCM cho mật khẩu/key |
| `src/server.js` | HTTP API + WebSocket |
| `public/` | Giao diện web (JS thuần, không cần build) |

## Bảo mật

- Đăng nhập bằng `ADMIN_USER`/`ADMIN_PASSWORD`, khoá IP 15 phút sau 5 lần sai.
- Mật khẩu/private key lưu mã hoá; **backup `data/secret.key` cùng `data/servers.json`** (mất key là không giải mã được).
- Host key SSH được ghi nhớ lần đầu; nếu đổi sẽ từ chối kết nối (chống MITM). Dùng nút "Reset host key" khi bạn vừa cài lại server.
- Không public cổng dashboard ra Internet. Dùng VPN (WireGuard/Tailscale) hoặc reverse proxy HTTPS
  (khi đó đặt `COOKIE_SECURE=true`, `TRUST_PROXY=true`).

## Giới hạn hiện tại

- Lịch sử biểu đồ chỉ giữ trong RAM (~30 phút), mất khi restart tool.
- OpenSSH mặc định cho tối đa 10 channel/kết nối, nên mỗi trình duyệt mở tối đa 6 luồng log cùng lúc.
- Chưa có cảnh báo (Telegram/email) và phân quyền nhiều user.
