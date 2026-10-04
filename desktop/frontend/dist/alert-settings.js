import { toast } from './util.js';
import { call } from './bridge.js';

const DEFAULTS = { cpu: 90, memory: 90, disk: 90, iowait: 30, sustainMinutes: 5 };
const FIELDS = Object.keys(DEFAULTS);

const dialog = document.getElementById('alerts-dialog');
const form = document.getElementById('alerts-form');
const msg = document.getElementById('alerts-msg');
let getSettings = () => null;
let onSaved = () => {};

// app.js owns the settings object; this module only edits its `alerts` part.
export function setAlertSettingsHandler(get, saved) {
  getSettings = get;
  onSaved = saved;
}

function fill(values) {
  for (const f of FIELDS) form[f].value = values[f];
}

export function openAlertSettings() {
  msg.textContent = '';
  msg.className = 'form-msg';
  fill({ ...DEFAULTS, ...getSettings()?.alerts });
  dialog.showModal();
}

document.getElementById('alerts-defaults').addEventListener('click', () => fill(DEFAULTS));
document.getElementById('alerts-cancel').addEventListener('click', () => dialog.close());
form.addEventListener('submit', async (e) => {
  e.preventDefault();
  const alerts = Object.fromEntries(FIELDS.map((f) => [f, Number(form[f].value)]));
  const next = { ...getSettings(), alerts };
  try {
    await call('SetSettings', next);
    onSaved(next);
    dialog.close();
    toast('Alert thresholds saved');
  } catch (err) {
    msg.textContent = err.message;
    msg.className = 'form-msg error';
  }
});
