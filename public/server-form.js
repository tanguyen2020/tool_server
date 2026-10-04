import { api, toast, h } from './util.js';

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
  data.useSudo = form.useSudo.checked;
  data.port = Number(data.port || 22);
  if (editing) data.id = editing.id;
  return data;
}

export function openServerForm(server = null, groups = []) {
  editing = server;
  form.reset();
  setMsg('');
  document.getElementById('server-dialog-title').textContent = server ? `Sửa server: ${server.name}` : 'Thêm server';
  document.getElementById('secret-hint').hidden = !server;
  document.getElementById('group-list').replaceChildren(...groups.map((g) => h('option', { value: g })));
  if (server) {
    for (const k of ['name', 'group', 'host', 'port', 'username', 'authType', 'privateKeyPath']) form[k].value = server[k] ?? '';
    form.useSudo.checked = !!server.useSudo;
    form.privateKey.placeholder = server.hasPrivateKey ? '(đã lưu — để trống để giữ nguyên)' : '-----BEGIN OPENSSH PRIVATE KEY-----';
    form.password.placeholder = server.hasPassword ? '(đã lưu)' : '';
  }
  syncAuthFields();
  dialog.showModal();
}

form.authType.addEventListener('change', syncAuthFields);
document.getElementById('server-cancel').addEventListener('click', () => dialog.close());

testBtn.addEventListener('click', async () => {
  setMsg('Đang thử kết nối…');
  testBtn.disabled = true;
  try {
    const r = await api('/api/servers/test', { method: 'POST', body: values() });
    const docker = /^\d+\.\d+/.test(r.docker) ? `Docker ${r.docker}` : `Docker: ${r.docker || 'không chạy được'}`;
    setMsg(`Kết nối OK — hostname: ${r.hostname}\n${docker}\nHost key SHA256: ${r.hostKey?.slice(0, 32)}…`, 'ok');
  } catch (err) {
    setMsg(err.message, 'error');
  } finally {
    testBtn.disabled = false;
  }
});

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  setMsg('Đang lưu…');
  try {
    if (editing) await api(`/api/servers/${editing.id}`, { method: 'PUT', body: values() });
    else await api('/api/servers', { method: 'POST', body: values() });
    dialog.close();
    toast(editing ? 'Đã cập nhật server' : 'Đã thêm server');
  } catch (err) {
    setMsg(err.message, 'error');
  }
});
