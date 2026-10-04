// Saved commands: for every server or one server. Inserted into a terminal (not run until Enter)
// or used in "Run on servers".
import { h, toast } from './util.js';
import { call, confirmDialog } from './bridge.js';
import { openMenu } from './context-menu.js';
import { modal } from './ui.js';
import { icon } from './icons.js';

let cache = null;

export async function loadSnippets(force = false) {
  if (!cache || force) cache = await call('Snippets').catch(() => []);
  return cache;
}

// Snippets that apply to a server (shared ones first).
export async function snippetsFor(serverId) {
  const list = await loadSnippets();
  return list.filter((s) => !s.serverId || s.serverId === serverId)
    .sort((a, b) => (!!a.serverId - !!b.serverId) || a.name.localeCompare(b.name));
}

// Menu of snippets under a button; onPick(snippet).
export async function snippetMenu(e, anchor, serverId, servers, onPick) {
  const list = serverId ? await snippetsFor(serverId) : await loadSnippets();
  openMenu(e, anchor, [
    ...list.map((s) => ({ label: `${s.name}${s.serverId && !serverId ? ` · ${servers.get(s.serverId)?.name || 'server'}` : ''}`, action: () => onPick(s) })),
    ...(list.length ? [] : [{ label: 'No snippets yet', action: () => {} }]),
    { label: 'Manage snippets…', action: () => openSnippets(servers, serverId) },
  ]);
}

// The manager: list, add, edit, delete.
export function openSnippets(servers, preferServer = '') {
  const m = modal({ title: 'Snippets', sub: 'Saved commands. In a terminal they are typed in for you; press Enter to run them.', wide: true });
  const list = h('div', { class: 'snippet-list' });
  const name = h('input', { placeholder: 'e.g. Disk space', 'aria-label': 'Name' });
  const scope = h('select', { 'aria-label': 'Applies to' },
    h('option', { value: '' }, 'Every server'),
    ...[...servers.values()].sort((a, b) => a.name.localeCompare(b.name)).map((s) => h('option', { value: s.id }, `Only ${s.name}`)));
  const command = h('textarea', { rows: 5, spellcheck: 'false', placeholder: 'df -h\ndocker ps --format "table {{.Names}}\\t{{.Status}}"', class: 'mono', 'aria-label': 'Command' });
  const msg = h('p', { class: 'form-msg', role: 'status' });
  const saveBtn = h('button', { type: 'button', class: 'btn primary' }, icon('save'), 'Save snippet');
  const newBtn = h('button', { type: 'button', class: 'btn ghost' }, icon('plus'), 'New');
  let editing = null;

  function edit(s) {
    editing = s;
    name.value = s?.name || '';
    command.value = s?.command || '';
    scope.value = s ? s.serverId || '' : preferServer;
    msg.textContent = '';
    saveBtn.lastChild.textContent = s ? 'Save changes' : 'Save snippet';
    render();
    name.focus();
  }

  async function render() {
    const all = await loadSnippets();
    list.replaceChildren(...(all.length ? all.slice().sort((a, b) => a.name.localeCompare(b.name)).map((s) => h('div', {
      class: `snippet-item${editing?.id === s.id ? ' active' : ''}`,
      onclick: () => edit(s),
    },
    h('div', { class: 'snippet-name' }, s.name, h('span', { class: 'muted' }, s.serverId ? ` · ${servers.get(s.serverId)?.name || 'removed server'}` : ' · every server')),
    h('pre', { class: 'snippet-cmd' }, s.command),
    h('button', { type: 'button', class: 'btn sm act act-remove', title: 'Delete', 'aria-label': `Delete ${s.name}`, onclick: async (e) => {
      e.stopPropagation();
      if (!(await confirmDialog(`Delete the snippet "${s.name}"?`, 'Delete snippet'))) return;
      await call('DeleteSnippet', s.id).catch((err) => toast(err.message, true));
      await loadSnippets(true);
      if (editing?.id === s.id) edit(null);
      else render();
    } }, icon('trash')))) : [h('div', { class: 'muted' }, 'No snippets yet. Add the commands you type often.')]));
  }

  saveBtn.addEventListener('click', async () => {
    try {
      const saved = await call('SaveSnippet', { id: editing?.id || '', name: name.value, command: command.value, serverId: scope.value });
      await loadSnippets(true);
      toast(`Saved "${saved.name}"`);
      edit(saved);
    } catch (err) {
      msg.textContent = err.message;
      msg.className = 'form-msg error';
    }
  });
  newBtn.addEventListener('click', () => edit(null));

  m.body.append(h('div', { class: 'snippets-layout' },
    list,
    h('div', { class: 'form plain' },
      h('div', { class: 'grid-2' }, h('label', {}, 'Name', name), h('label', {}, 'Applies to', scope)),
      h('label', {}, 'Command (several lines are fine)', command),
      msg)));
  m.foot.append(newBtn, h('span', { class: 'spacer' }), saveBtn);
  edit(null);
}
