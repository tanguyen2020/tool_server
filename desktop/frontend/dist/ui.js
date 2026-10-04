// Small UI building blocks shared by the newer screens: a modal dialog, a text prompt and a panel.
import { h } from './util.js';
import { icon } from './icons.js';

// Opens a modal. Returns { dialog, body, foot, close }. The dialog is removed from the page when closed.
export function modal({ title, sub = '', wide = false, cls = '', onClose } = {}) {
  const body = h('div', { class: 'modal-body' });
  const foot = h('div', { class: 'modal-foot' });
  const closeBtn = h('button', { type: 'button', class: 'btn ghost icon', 'aria-label': 'Close', title: 'Close (Esc)' }, icon('close'));
  const titleEl = h('h2', {}, title);
  const subEl = h('div', { class: 'muted modal-sub' }, sub);
  const dialog = h('dialog', { class: `dialog modal ${wide ? 'wide' : ''} ${cls}` },
    h('div', { class: 'modal-head' }, h('div', { class: 'modal-title' }, titleEl, subEl), closeBtn),
    body, foot);
  document.body.append(dialog);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    dialog.close();
    dialog.remove();
    onClose?.();
  };
  closeBtn.addEventListener('click', close);
  dialog.addEventListener('cancel', (e) => { e.preventDefault(); close(); });
  dialog.addEventListener('close', close); // closed by the browser (Esc) without a cancel event
  dialog.showModal();
  return { dialog, body, foot, close, setTitle: (t) => { titleEl.textContent = t; }, setSub: (t) => { subEl.textContent = t; } };
}

// Asks for one line of text. Resolves to the text, or null when cancelled.
export function promptText(title, { label = '', value = '', placeholder = '', okLabel = 'OK', hint = '' } = {}) {
  return new Promise((resolve) => {
    let result = null;
    const m = modal({ title, onClose: () => resolve(result) });
    const input = h('input', { value, placeholder, spellcheck: 'false' });
    const form = h('form', { class: 'form plain' },
      h('label', {}, label, input),
      hint ? h('p', { class: 'hint' }, hint) : '',
    );
    m.body.append(form);
    const ok = h('button', { type: 'button', class: 'btn primary' }, okLabel);
    m.foot.append(h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), ok);
    const submit = () => {
      if (!input.value.trim()) { input.focus(); return; }
      result = input.value.trim();
      m.close();
    };
    ok.addEventListener('click', submit);
    form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
    input.focus();
    // Select the name without its extension, like a file manager does.
    const dot = value.lastIndexOf('.');
    input.setSelectionRange(0, dot > 0 ? dot : value.length);
  });
}

// A titled panel in the page grid (same look as the other panels).
export function panel(cls, title, ...right) {
  const body = h('div');
  const rightEl = h('span', { class: 'right' }, ...right);
  const el = h('section', { class: `panel ${cls}` }, h('h3', {}, title, rightEl), body);
  return { el, body, right: rightEl };
}

// Button with an icon and a label (label optional for icon-only buttons with a tooltip).
export function iconButton(name, label, { cls = 'btn sm', title = label, onclick, hideLabel = false } = {}) {
  return h('button', { type: 'button', class: cls, title, 'aria-label': label, onclick },
    icon(name), hideLabel ? '' : h('span', { class: 'lbl' }, label));
}

// Formats a unix-ms time as "2026-10-04 14:03".
export function fmtDateTime(ms) {
  if (!ms) return '–';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// ---------------------------------------------------------------- confirmation
// The tone, icon and button label follow from the title unless given: "Remove container" → red, trash icon,
// a "Remove" button. Paragraphs are separated by a blank line; the first one is the question, a paragraph
// starting with ⚠ becomes a warning box, and multi-line paragraphs or paths are shown in monospace.
const DANGER_RE = /^(remove|delete|reboot|clean up|down|compose: down|disable|reset)/i;
const WARN_RE = /^(stop|restart|close|unsaved|file changed|replace|run|reload|compose: (stop|restart|pull))/i;
const VERB_LABEL = { remove: 'Remove', delete: 'Delete', reboot: 'Reboot', close: 'Close', stop: 'Stop', restart: 'Restart', start: 'Start',
  run: 'Run', upgrade: 'Upgrade', replace: 'Replace', reset: 'Reset', disable: 'Disable', enable: 'Enable', reload: 'Reload' };

function confirmLabelFor(title) {
  const t = title.toLowerCase();
  if (t.startsWith('compose:')) return t.slice(8).trim().replace(/^./, (c) => c.toUpperCase());
  if (t.startsWith('clean up')) return 'Clean up';
  if (t === 'unsaved changes') return 'Discard changes';
  if (t === 'file changed') return 'Overwrite';
  return VERB_LABEL[t.split(/\s+/)[0]] || 'Continue';
}

function iconFor(title, tone) {
  const t = title.toLowerCase();
  if (/remove|delete|clean up/.test(t)) return 'trash';
  if (/reboot/.test(t)) return 'power';
  if (/^run/.test(t)) return 'zap';
  if (/upgrade/.test(t)) return 'upload';
  if (/restart|reload/.test(t)) return 'restart';
  return tone === 'primary' ? 'question' : 'alert';
}

export function confirmModal(message, title = 'Confirm', opts = {}) {
  const tone = opts.tone || (DANGER_RE.test(title) ? 'danger' : WARN_RE.test(title) ? 'warning' : 'primary');
  const okLabel = opts.confirmLabel || confirmLabelFor(title);
  return new Promise((resolve) => {
    let result = false;
    const paragraphs = String(message).split(/\n\s*\n/).map((p) => p.trim()).filter(Boolean);
    const [lead, ...rest] = paragraphs;
    const body = h('div', { class: 'confirm-text' },
      h('p', { class: 'confirm-lead' }, lead || ''),
      ...rest.map((p) => {
        if (p.startsWith('⚠')) return h('div', { class: 'confirm-warn' }, icon('alert'), h('span', {}, p.replace(/^⚠\s*/, '')));
        if (p.includes('\n') || p.startsWith('/') || opts.code) return h('pre', { class: 'confirm-code' }, p);
        return h('p', {}, p);
      }));
    const cancelBtn = h('button', { type: 'button', class: 'btn ghost' }, opts.cancelLabel || 'Cancel');
    const okBtn = h('button', { type: 'button', class: `btn ${tone === 'danger' ? 'solid-danger' : tone === 'warning' ? 'solid-warning' : 'primary'}` },
      icon(iconFor(title, tone)), okLabel);
    const dialog = h('dialog', { class: `dialog confirm tone-${tone}`, 'aria-labelledby': 'confirm-title' },
      h('div', { class: 'confirm-main' },
        h('div', { class: 'confirm-icon' }, icon(iconFor(title, tone))),
        h('div', { class: 'confirm-content' }, h('h2', { id: 'confirm-title' }, title), body)),
      h('div', { class: 'confirm-foot' }, cancelBtn, okBtn));
    document.body.append(dialog);
    let closing = false;
    const close = (v) => {
      if (closing) return;
      closing = true;
      result = v;
      dialog.classList.add('closing');
      setTimeout(() => { dialog.close(); dialog.remove(); resolve(result); }, 120);
    };
    cancelBtn.addEventListener('click', () => close(false));
    okBtn.addEventListener('click', () => close(true));
    dialog.addEventListener('cancel', (e) => { e.preventDefault(); close(false); });
    // Chromium may close a dialog on Esc without a cancel event: settle anyway.
    dialog.addEventListener('close', () => {
      if (closing) return;
      closing = true;
      dialog.remove();
      resolve(false);
    });
    // A click on the dimmed backdrop cancels.
    dialog.addEventListener('mousedown', (e) => { if (e.target === dialog) close(false); });
    dialog.showModal();
    // Destructive actions start on Cancel so a stray Enter does not delete anything.
    (tone === 'danger' ? cancelBtn : okBtn).focus();
  });
}
