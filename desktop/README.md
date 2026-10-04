# Server Dashboard (desktop)

A desktop app for monitoring many Debian + Docker servers over **SSH, with no agent to install**.
Built with Go + Wails; the plain-JS UI is embedded in a single `.exe` (~12 MB) and uses the WebView2 runtime that ships with Windows 10/11.

## Features

- **Overview** in two views, switched with *Cards / List*: compact cards of equal height (issues on one line with "+N"),
  or a dense sortable list (one row per server: status, CPU / RAM / fullest disk, load, uptime, containers, issues).
  Sort by *Problems first* (unreachable servers, alerts, container issues, expiring certificates, then reboot / updates),
  name, CPU, RAM, disk or load; group by group; the stat tiles (Online, Unreachable, Need attention, Reboot required)
  filter the list. The choices are saved in settings.json, like the other view preferences.
- **Tabs, one per opened server** (like a browser): Overview is always the first tab; click a server card to open/switch,
  Ctrl+Click, middle click or right-click → *Open in new tab* to open it in the background. Each tab keeps its own sub-tab,
  time range, filters, scroll position and log panel (log streams keep running in hidden tabs). Tabs show a status dot
  and a count of alerts/container issues; hidden tabs don't render. Ctrl+Tab / Ctrl+Shift+Tab, Ctrl+W, Ctrl+1…9;
  at most 10 server tabs (the least recently used one without terminals closes). Closing a tab that has open terminals
  asks first. Every start begins with the Overview tab only, in a maximized window.
- **Server page** with an always-visible summary (CPU, RAM, fullest disk, containers), a **Refresh** button that collects
  new metrics at once (shows *Refreshing…*, then *✓ Updated* and highlights the updated time) and three tabs:
  **Server**, **Containers** (the table, for day-to-day operations) and **Container metrics** (charts and Docker disk usage).
- **Servers:** online/offline; CPU split into user / system / I/O wait / steal; load average vs core count; memory used / cache / available,
  swap activity and OOM kills; disk I/O (throughput, IOPS, utilization per disk); disk space and inode usage; network; per-core CPU;
  charts with a time range picker: **Live 30m** (5-second samples) or **1h / 6h / 24h / 7d** from the on-disk history.
- **History** stored in `%AppData%\ServerDashboard\history.db` (bbolt): 1-minute averages, kept 7 days, survives restarts.
  Data is only recorded while the app is running; gaps show as breaks in the charts.
- **Maintenance** (checked hourly): reboot required (newer kernel installed or `/var/run/reboot-required`), pending updates
  and security updates (`apt-get -s`, read-only), age of the package lists. Shown on the server card and the Server tab.
- **Reboot server** (`⏻ Reboot` in the server header, right-click a server card, or *Reboot now…* when a reboot is required):
  asks first, then runs `systemctl reboot` as root or through passwordless `sudo -n` (otherwise it explains what is missing).
  The server shows as **Rebooting…** (no "unreachable" or "container stopped" alerts meanwhile) until it answers with a fresh
  uptime; then one notification says how long it took and lists the containers that were running before but did not start
  again. No answer within 15 minutes raises an alert. The maintenance check runs again right after the reboot.
- **Top processes** (on demand): busiest processes measured over 1 second with `top`, mapped to their container.
- **Docker disk usage** (on demand): `docker system df` on the Docker tab.
- **Threshold alerts** (button *Alerts*): CPU, memory and I/O wait sustained above a threshold, disk or inode usage above a threshold,
  and OOM kills. Defaults: 90 / 90 / 90 / 30% sustained for 5 minutes; an alert resolves 5 points below its threshold.
- **Container metrics (Grafana style):** CPU, memory, **network** (total / received / sent) and **disk I/O** (total / read / write)
  per container over time, with a sortable Last / Max / Min legend; the 8 largest series get a color, the rest are gray context lines;
  hover or click a line/row to highlight it; optional **grouping by compose project**. Network and disk numbers come from exact
  counters (`/proc/<pid>/net/dev`, cgroup v2 `io.stat`), falling back to `docker stats`; host-network containers are excluded from network.
- **Container events timeline:** start, restart, stop, exit (with exit code), OOM kill and health changes from `docker events`,
  stored 7 days, listed on the Container metrics tab and drawn as markers on every chart (red = exit with an error, OOM, unhealthy).
- **Containers:** state, **health**, status with **exit code meaning**, published ports, CPU and RAM with 30-minute trend lines,
  **restart count** (with restart-loop detection: 3+ restarts in 10 minutes), net/block I/O; quick filters All / Running / Stopped /
  **Issues** (unhealthy, restart loop, restarting, abnormal exit); sortable columns; grouping by compose project; Start / Stop / Restart;
  **Remove** (`docker rm`, after a confirmation; image and volumes are kept) for stopped containers, and right-click a row
  for every action including *Stop and remove…* (`docker rm -f`) on a running one;
  **Grafana-style logs**: log-volume histogram by level, level detection (JSON incl. pino numeric levels, logfmt, keywords)
  with a colored stripe per line, level filter chips with counts, search highlight, click a line for its parsed fields,
  Prettify JSON, wrap/time toggles, resizable or maximized panel, pause, save to file, auto-resume after a restart;
  **Inspect** view of `docker inspect` (general info, state/health, resources, ports, networks, mounts, env, labels, raw JSON),
  with env values that look like credentials masked until revealed.
- **Windows notifications** when a server becomes unreachable / comes back, a container stops without you stopping it, becomes unhealthy / healthy again, starts restarting in a loop or is killed for lack of memory, or a threshold alert fires / resolves.
- **Built-in terminal** (xterm.js): `>_ Terminal` opens a shell on the server and **Exec** opens one inside a container
  (`docker exec -it`, bash or sh), both on the SSH connection the dashboard already holds — no second login.
  Logs and terminals share the bottom panel as tabs (like VS Code); they keep running while collapsed or in another server tab.
  Copy/paste with Ctrl+Shift+C / Ctrl+Shift+V or right-click, search (Ctrl+Shift+F), font size (Ctrl+= / Ctrl+-),
  colors follow the theme, Enter reconnects an ended session. Up to 4 terminals per server, numbered (`web-01`, `web-01 #2`).
  Close one panel tab with its ✕, middle click, right-click (*Close / Close other tabs / Close all terminals*) or
  **Ctrl+Shift+W** (works inside a terminal); Ctrl+W closes the panel tab when the focus is in the panel outside a shell.
  Inside a terminal, other app shortcuts such as Ctrl+W go to the shell.
- **Theme switch** System (follows Windows) / Light / Dark; the native title bar and window background follow it.
  The choice is saved in `settings.json` and restored on the next start.
- Authentication with a private key (pasted or picked from a file), the Windows OpenSSH agent, or a password.
- **Jump host (bastion)**: a server can be reached *through* another saved server ("Connect through" in the server form),
  like `ssh -J`. Loops are refused, and a jump host that other servers use cannot be removed.

### Working on the servers

Everything runs on the SSH connection the dashboard already holds; commands are built by the app from validated input.

- **Compose projects** (Containers tab, on each project row): **Update** (`docker compose pull && up -d`), Restart,
  Stop / Start, Up, Down, *Edit compose file* and *Open project folder* (in the Files tab). The project folder and files
  come from the labels Compose puts on its containers; the plugin (`docker compose`) and `docker-compose` both work.
  Actions run in a terminal tab so pull progress and errors are visible, and the table refreshes when they finish.
- **Docker cleanup** (Container metrics → Docker disk usage): stopped containers, dangling images, unused images,
  build cache, unused networks and unused volumes, each with the reclaimable size and a confirmation.
- **Services tab** (systemd): state, description and boot setting of every service, failed ones first; Start, Stop,
  Restart, Reload, Enable / Disable (root or passwordless sudo), `systemctl status`, and **Logs** that follow the
  journal in a terminal tab. Stopping essential services (ssh, docker, networking…) warns first.
- **Files tab** (SFTP): browse with breadcrumbs, show hidden files, edit text files (Ctrl+S, Tab, unsaved marker,
  detection of changes made on the server meanwhile), new file / folder, rename, delete, upload (with overwrite check)
  and download with progress, *Terminal here*. Root-owned files are read and saved through `sudo -n` when allowed
  (marked "sudo"). The last folder is remembered per server.
- **Network tab**: listening TCP/UDP ports with the process (needs root for other users' processes), the container that
  publishes the port, and whether it is reachable from the network or only from the server.
  **Port forwarding** (like `ssh -L`): opens `localhost:<port>` on this computer leading to a host:port reachable from
  the server (databases, admin pages bound to 127.0.0.1); one click from a port row. Forwards stop when the app closes.
- **Disk usage by folder** (Server tab): `du` on one file system, starting with the fullest disk; click to drill down.
- **Package upgrade**: *Upgrade…* in Maintenance runs `apt-get update && apt-get upgrade` in a terminal tab where you
  answer apt's prompts (and sudo's password if asked); the maintenance status is checked again afterwards.
- **TLS certificates** (checked hourly with maintenance): Let's Encrypt certificates and the ones referenced by
  nginx / Apache, with days left. An alert fires when one expires within 14 days; the server card shows it too.
- **Run on servers** (top bar): one command on several servers at once (pick by group, filter), results side by side
  with exit code, duration and output (stdout / stderr); cancel while running. Runs without a terminal: use `sudo -n`.
- **Snippets** (top bar, and *Snippets* in the terminal panel): saved commands for every server or one server. In a
  terminal they are typed in but not run until you press Enter; also usable in *Run on servers*.
- **Activity** (top bar): what was done from the app — container, compose, service, file, cleanup, reboot, tunnel,
  run-command and server-list actions — with time, server, Windows user and result. Last 5000 entries, kept locally.
- **Export / Import** (Overview): the server list (groups, jump hosts, key file paths) and snippets as JSON.
  No passwords or keys are exported; servers using a password or pasted key ask for it again after import.

## Running

Open `build/bin/ServerDashboard.exe`. It is self-contained: no Go, Node or other runtime needed.

Data lives in `%AppData%\ServerDashboard\`:

| File | Contents |
|---|---|
| `servers.json` | Server list. Passwords/private keys are encrypted with **Windows DPAPI** (only your Windows account can decrypt them) |
| `settings.json` | Settings (notifications, alert thresholds, theme) |
| `snippets.json` | Saved commands |
| `activity.log` | Actions done from the app (JSON lines, last 5000) |
| `history.db` | Metric history, 1-minute averages for 7 days (roughly 0.5–1 MB per server per day) |
| `webview/` | WebView2 cache |

> Because of DPAPI, copying `servers.json` to another machine or account leaves pasted passwords/keys undecryptable
> (servers using a key file or the agent keep working). Just re-enter the secrets there.

## Building

```powershell
go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0   # once
./build.ps1
```

Local builds report the version `dev` and never update themselves.

## Releases and automatic updates

GitHub Actions (`.github/workflows/build.yml`) builds Windows, macOS (universal) and Linux on every push.
**Pushing a version tag publishes a release:**

```bash
git tag v1.2.0 -m "Server Dashboard 1.2.0"
git push origin v1.2.0
```

The release contains the builds and `latest.json` (version, notes, size and SHA-256 of each platform's
executable) with `latest.json.sig`, an **Ed25519 signature** made in CI with the private release key.
The app embeds the matching public key (`internal/updater/updater.go`). It checks for a newer release
20 seconds after start and then every 6 hours, downloads it in the background, verifies the signature and
the checksum, and installs it when the app closes (or right away with *Restart now*). Nothing that is not
signed with the project's key is ever installed. Turn it off in the Updates dialog (click the version in
the top bar).

Release files:

| File | For |
|---|---|
| `ServerDashboard-windows-amd64-setup.exe` | **Windows installer**: installs for your account in `%LOCALAPPDATA%\Programs\ServerDashboard` (no admin rights), Start menu and desktop shortcuts, uninstall from *Apps & features*. Updates itself. |
| `ServerDashboard-windows-amd64.exe` | Windows without installing: run it from any folder you can write to (it updates itself in place) |
| `ServerDashboard-macos-universal.dmg` | **macOS** (Intel and Apple Silicon): open, drag the app to Applications. Updates itself. |
| `ServerDashboard-linux-amd64.deb` | **Debian / Ubuntu**: `sudo apt install ./ServerDashboard-linux-amd64.deb` (menu entry and `serverdash` command). Installed in /opt, so new versions are announced in the app and installed the same way. |
| `ServerDashboard-linux-amd64.tar.gz` | Other Linux: unpack anywhere you can write to; needs `libwebkit2gtk-4.1-0` and `libgtk-3-0`. Updates itself. |
| `ServerDashboard-macos-universal.zip`, `ServerDashboard-macos-universal`, `ServerDashboard-linux-amd64` | Plain app / executables (the last two are used by the updater) |
| `latest.json`, `latest.json.sig` | Signed update manifest |

CI installs and uninstalls each package on its runner (silent Windows install, `apt install` of the .deb, mounting the .dmg) before publishing.

**Download warnings on Windows.** The executables are not signed with a code-signing certificate, so
SmartScreen and browsers show "unknown publisher" / "not commonly downloaded" warnings for new versions (Defender
finds nothing in them). To check a download, compare it with `SHA256SUMS.txt` of the release
(`Get-FileHash <file>` in PowerShell). To remove the warnings, add a code-signing certificate as the
repository secrets `WINDOWS_SIGN_PFX` (the .pfx file in base64) and `WINDOWS_SIGN_PASSWORD`: CI then signs the
executable and the setup with signtool. Until then you can submit a release file to Microsoft
(https://www.microsoft.com/wdsi/filesubmission, "Software developer") to clear its reputation.

**Signing key setup (once):** the private key lives only in the repository secret `UPDATE_SIGNING_KEY`
(Settings → Secrets and variables → Actions). Keep an offline backup: if it is lost, generate a new pair with
`go run ./tools/updatesign genkey -out <file>`, put the new public key in `updater.PublicKey`, and users of
older versions must download the next version by hand once. The repository must be **public** for the app to
download releases.

> macOS builds are not notarized: the first time, right-click the app and choose *Open* (or run
> `xattr -dr com.apple.quarantine "/Applications/ServerDashboard.app"`). Updates after that need nothing.

## Performance

- **One long-lived SSH connection** per server and **one command per cycle** (reads `/proc`, `df`, `docker ps`, `docker stats`).
- Polls every 5 seconds while the window is visible, **every 30 seconds when minimized**; the UI stops rendering while hidden.
- Servers are collected independently, so a slow or hung server never delays the others.
- WebView2 runs without GPU acceleration (`WebviewGpuIsDisabled`) to save memory.
- Measured with one server: about **160 MB** private memory in total (Go ~55 MB, WebView2 ~105 MB).

## Layout

| Path | Purpose |
|---|---|
| `main.go`, `app.go` | Wails setup and the methods the UI calls via `window.go.main.App.*` |
| `internal/sshpool` | SSH connections, host key checking (trust-on-first-use), keepalive, SSH agent |
| `internal/monitor` | Metric collection and parsing, in-memory history, incident detection |
| `internal/docker` | Start/stop/restart, inspect, log streaming |
| `internal/ops` | Compose projects, Docker cleanup, systemd, listening ports, disk usage, apt upgrade |
| `internal/hostinfo` | Top processes, maintenance (updates, reboot, certificates), reboot |
| `internal/activity` | Activity log |
| `internal/updater`, `app_update.go`, `tools/updatesign` | Signed automatic updates from GitHub releases |
| `app_ops.go`, `app_files.go`, `app_tools.go`, `terminal.go` | Server operations, SFTP files, tunnels / run-on-servers / snippets / import-export, terminals and tasks |
| `internal/store`, `internal/secret` | Config storage, DPAPI encryption (AES-GCM outside Windows) |
| `frontend/dist` | UI (no build step) |

## Preparing a Debian server

```bash
sudo adduser --disabled-password --gecos "" monitor
sudo usermod -aG docker monitor
# add your public key to ~monitor/.ssh/authorized_keys
```

> Membership in the `docker` group is equivalent to root. If you don't want that, allow only the docker command in sudoers
> (`monitor ALL=(root) NOPASSWD: /usr/bin/docker`) and tick "Run docker with sudo -n" when adding the server.

Actions that need root (reboot, service start/stop, editing root-owned files, process names of listening ports) use
`sudo -n`, so they work as root or when sudo needs no password. To allow only some of them, list the commands in
sudoers, for example:

```
monitor ALL=(root) NOPASSWD: /usr/bin/systemctl reboot, /usr/bin/systemctl restart *, /usr/bin/systemctl start *, /usr/bin/systemctl stop *
```
