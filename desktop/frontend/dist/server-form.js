import { toast, h } from './util.js';
import { call } from './bridge.js';

const dialog = document.getElementById('server-dialog');
const form = document.getElementById('server-form');
const msg = document.getElementById('server-form-msg');
const testBtn = document.getElementById('server-test');
let editing = null;

function setMsg(text, kind = '') {
  msg.textContent = text;
  msg.className = `form-msg ${kind}`;
}

function syncAuthFields() {
  const type = form.authType.value;
  for (const el of form.querySelectorAll('[data-auth]')) el.hidden = !el.dataset.auth.split(' ').includes(type);
}

function values() {
  const data = Object.fromEntries(new FormData(form));
  data.jumpId = form.jumpId.value;
  data.useSudo = form.useSudo.checked;
  data.port = Number(data.port || 22);
  data.id = editing?.id || '';
  return data;
}

// servers: the saved servers, offered as jump hosts (not the server itself or ones that go through it).
export function openServerForm(server = null, groups = [], servers = []) {
  editing = server;
  form.reset();
  const goesThrough = (s) => {
    for (let x = s, n = 0; x && n < 6; x = servers.find((y) => y.id === x.jumpId), n++) if (x.jumpId === server?.id) return true;
    return false;
  };
  form.jumpId.replaceChildren(h('option', { value: '' }, 'Direct connection'),
    ...servers.filter((s) => s.id !== server?.id && !(server && goesThrough(s)))
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((s) => h('option', { value: s.id }, `Through ${s.name} (${s.username}@${s.host})`)));
  setMsg('');
  document.getElementById('server-dialog-title').textContent = server ? `Edit server: ${server.name}` : 'Add server';
  document.getElementById('secret-hint').hidden = !server;
  document.getElementById('group-list').replaceChildren(...groups.map((g) => h('option', { value: g })));
  form.privateKey.placeholder = '-----BEGIN OPENSSH PRIVATE KEY-----';
  form.password.placeholder = '';
  if (server) {
    for (const k of ['name', 'group', 'host', 'port', 'username', 'authType', 'privateKeyPath']) form[k].value = server[k] ?? '';
    form.useSudo.checked = !!server.useSudo;
    form.jumpId.value = server.jumpId || '';
    if (server.hasPrivateKey) form.privateKey.placeholder = '(saved — leave empty to keep)';
    if (server.hasPassword) form.password.placeholder = '(saved)';
  }
  syncAuthFields();
  dialog.showModal();
}

form.authType.addEventListener('change', syncAuthFields);
document.getElementById('server-cancel').addEventListener('click', () => dialog.close());
document.getElementById('pick-key').addEventListener('click', async () => {
  const path = await call('PickKeyFile').catch((e) => { setMsg(e.message, 'error'); return ''; });
  if (path) form.privateKeyPath.value = path;
});

testBtn.addEventListener('click', async () => {
  setMsg('Testing connection…');
  testBtn.disabled = true;
  try {
    const r = await call('TestServer', values());
    const docker = /^\d+\.\d+/.test(r.docker) ? `Docker ${r.docker}` : `Docker: ${r.docker || 'not available'}`;
    setMsg(`Connected — hostname: ${r.hostname}\n${docker}\nHost key: ${r.hostKey}`, 'ok');
  } catch (err) {
    setMsg(err.message, 'error');
  } finally {
    testBtn.disabled = false;
  }
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  setMsg('Saving…');
  try {
    await call('SaveServer', values());
    dialog.close();
    toast(editing ? 'Server updated' : 'Server added');
  } catch (err) {
    setMsg(err.message, 'error');
  }
});
