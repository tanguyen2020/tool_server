// App updates: the version in the top bar, the Updates dialog, and a small notice while an update
// downloads / once it is ready (it installs when the app closes, or right away with "Restart now").
import { h, toast } from './util.js';
import { call, on, confirmDialog } from './bridge.js';
import { modal } from './ui.js';
import { icon } from './icons.js';

let current = '';
let readyInfo = null;
const notice = h('div', { class: 'update-notice', hidden: true, role: 'status' });

function showNotice(...children) {
  notice.replaceChildren(...children);
  notice.hidden = false;
}

function readyNotice(info) {
  readyInfo = info;
  showNotice(
    h('span', { class: 'update-dot' }),
    h('span', {}, h('strong', {}, `Version ${info.latest}`), ' is ready. It installs when you close the app.'),
    h('button', { type: 'button', class: 'btn sm primary', onclick: install }, icon('restart'), 'Restart now'),
    h('button', { type: 'button', class: 'btn sm ghost', onclick: () => openUpdates() }, "What's new"),
    h('button', { type: 'button', class: 'btn sm ghost icon', 'aria-label': 'Hide', title: 'Hide (installs when you close the app)', onclick: () => { notice.hidden = true; } }, icon('close')));
}

async function install() {
  if (!(await confirmDialog(`Restart now to install version ${readyInfo?.latest || ''}?\n\nOpen terminals, log streams and port forwards close; the app opens again by itself.`, 'Restart to update', { tone: 'primary', confirmLabel: 'Restart now' }))) return;
  try {
    await call('InstallUpdate');
  } catch (err) {
    toast(`Could not install the update: ${err.message}`, true);
  }
}

export function initUpdates(settings, versionBtn) {
  document.body.append(notice);
  call('AppVersion').then((v) => {
    current = v;
    versionBtn.textContent = v === 'dev' || v.includes('dev') ? 'dev build' : `v${v}`;
  });
  versionBtn.addEventListener('click', () => openUpdates(settings));
  on('update.progress', (p) => {
    const pct = p.total ? Math.round((p.done / p.total) * 100) : 0;
    showNotice(h('span', { class: 'spinner' }), h('span', {}, `Downloading version ${p.version}… ${pct}%`));
  });
  on('update.ready', readyNotice);
  on('update.available', (info) => showNotice(
    h('span', { class: 'update-dot' }),
    h('span', {}, h('strong', {}, `Version ${info.latest}`), ' is available.'),
    h('button', { type: 'button', class: 'btn sm primary', onclick: () => window.runtime.BrowserOpenURL(info.page) }, icon('download'), 'Download'),
    h('button', { type: 'button', class: 'btn sm ghost icon', 'aria-label': 'Hide', onclick: () => { notice.hidden = true; } }, icon('close'))));
}

export async function openUpdates(settings) {
  const m = modal({ title: 'Updates', sub: `Server Dashboard ${current === 'dev' ? '(development build)' : `version ${current}`}` });
  const status = h('div', { class: 'upd-status' }, h('span', { class: 'spinner' }), 'Checking GitHub for a newer version…');
  const notes = h('div', { class: 'upd-notes', hidden: true });
  const auto = h('input', { type: 'checkbox', checked: settings?.autoUpdate !== false });
  auto.addEventListener('change', async () => {
    try {
      await call('SetAutoUpdate', auto.checked);
      if (settings) settings.autoUpdate = auto.checked;
    } catch (err) {
      toast(err.message, true);
    }
  });
  const actions = h('div', { class: 'upd-actions' });
  m.body.append(status, notes, actions,
    h('label', { class: 'check upd-auto' }, auto, 'Download and install new versions automatically'),
    h('p', { class: 'hint' }, 'Updates come from the project\'s GitHub releases and are installed only when their signature matches the key built into this app.'));
  try {
    const info = await call('CheckUpdate');
    if (info.dev) {
      status.replaceChildren(icon('info'), `This is a development build: it is never replaced automatically. Latest release: ${info.latest || '–'}.`);
    } else if (!info.available) {
      status.replaceChildren(icon('check'), `You have the latest version (${info.current}).`);
      status.classList.add('ok');
    } else {
      status.replaceChildren(icon('download'), h('span', {}, 'Version ', h('strong', {}, info.latest), ` is available (you have ${info.current}).`));
      status.classList.add('new');
      if (!info.canInstall) {
        actions.replaceChildren(
          h('button', { type: 'button', class: 'btn primary', onclick: () => window.runtime.BrowserOpenURL(info.page) }, icon('download'), 'Download from GitHub'),
          h('span', { class: 'muted small' }, 'This copy is installed in a protected folder, so it cannot replace itself.'));
      } else if (info.staged === info.latest || readyInfo?.latest === info.latest) {
        actions.replaceChildren(h('button', { type: 'button', class: 'btn primary', onclick: install }, icon('restart'), 'Restart and install'));
      } else {
        const dl = h('button', { type: 'button', class: 'btn primary' }, icon('download'), 'Download and install');
        dl.addEventListener('click', async () => {
          dl.disabled = true;
          dl.lastChild.textContent = 'Downloading…';
          try {
            await call('DownloadUpdate');
            m.close();
            if (readyInfo) install();
          } catch (err) {
            toast(`Update failed: ${err.message}`, true);
            dl.disabled = false;
            dl.lastChild.textContent = 'Download and install';
          }
        });
        actions.replaceChildren(dl);
      }
    }
    if (info.latest) {
      notes.hidden = false;
      notes.replaceChildren(h('div', { class: 'upd-notes-title' }, `What's new in ${info.latest}`),
        h('pre', {}, info.notes || 'No release notes.'),
        h('a', { href: '#', onclick: (e) => { e.preventDefault(); window.runtime.BrowserOpenURL(info.page); } }, 'Release page on GitHub'));
    }
  } catch (err) {
    status.replaceChildren(icon('alert'), `Could not check for updates: ${err.message}`);
    status.classList.add('bad');
  }
}
