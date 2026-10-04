// Files tab: browse the server over SFTP (same SSH connection), edit text files, upload / download,
// create, rename and delete. Root-owned files are read and saved with sudo when the user may.
import { h, toast, fmtBytes } from './util.js';
import { call, on, confirmDialog, copyText } from './bridge.js';
import { openMenu } from './context-menu.js';
import { panel, promptText, fmtDateTime } from './ui.js';
import { icon } from './icons.js';
import { getPref, setPref } from './prefs.js';

const LAST_KEY = 'filesPath:';
const join = (dir, name) => (dir === '/' ? `/${name}` : `${dir}/${name}`);
const parent = (p) => (p === '/' ? '/' : p.replace(/\/[^/]+$/, '') || '/');

// Progress of uploads and downloads, shown in whichever Files tab started them.
const progressHandlers = new Map(); // serverId -> fn
on('files.progress', (p) => progressHandlers.get(p.serverId)?.(p));

export function filesTab(serverId, { openShellAt, serverName }) {
  const pathInput = h('input', { class: 'mono files-path', spellcheck: 'false', 'aria-label': 'Folder path' });
  const crumbs = h('div', { class: 'crumbs-path' });
  const btn = (name, label, cls, onclick) => h('button', { type: 'button', class: `btn sm act ${cls}`, title: label, onclick }, icon(name), h('span', { class: 'lbl' }, label));
  const upBtn = btn('up', 'Up', 'act-logs', () => go(parent(cwd)));
  const homeBtn = btn('home', 'Home', 'act-logs', () => go(''));
  const refreshBtn = btn('refresh', 'Refresh', 'act-restart', () => go(cwd));
  const newFolderBtn = btn('folder', 'New folder', 'act-exec', newFolder);
  const newFileBtn = btn('file', 'New file', 'act-exec', newFile);
  const uploadBtn = btn('upload', 'Upload', 'act-start', upload);
  const shellBtn = btn('terminal', 'Terminal here', 'act-stop', () => openShellAt(cwd));
  const prog = h('div', { class: 'files-progress', hidden: true }, h('span', {}), h('div', { class: 'meter' }, h('span', {})));
  const filter = h('input', { type: 'search', placeholder: 'Filter this folder…', 'aria-label': 'Filter files' });
  const hidden = h('input', { type: 'checkbox' });
  const tbody = h('tbody');
  const status = h('span', { class: 'muted' });
  const p = panel('span-12', 'Files', status);
  const listView = h('div', {},
    h('div', { class: 'toolbar files-toolbar' }, upBtn, homeBtn, pathInput, refreshBtn),
    crumbs,
    h('div', { class: 'toolbar' }, filter, h('label', { class: 'check' }, hidden, 'Show hidden files'), h('span', { class: 'spacer' }), prog,
      newFolderBtn, newFileBtn, uploadBtn, shellBtn),
    h('div', { class: 'table-wrap files-wrap' }, h('table', { class: 'files-table' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', { class: 'num' }, 'Size'), h('th', {}, 'Modified'), h('th', {}, 'Permissions'), h('th', {}, ''))),
      tbody)),
    h('p', { class: 'hint' }, 'Double-click a folder to open it, a file to edit it. Right-click for more. Files are read and written as the SSH user; root-owned files use sudo when allowed.'));

  // ---- editor
  const edPath = h('div', { class: 'mono ed-path' });
  const edBadge = h('span', { class: 'chip warning', hidden: true, title: 'Read and saved through sudo' }, 'sudo');
  const edState = h('span', { class: 'muted ed-state' });
  const edPos = h('span', { class: 'muted ed-pos' });
  const area = h('textarea', { class: 'editor mono', spellcheck: 'false', wrap: 'off', 'aria-label': 'File content' });
  const saveBtn = btn('save', 'Save (Ctrl+S)', 'act-start', () => save());
  const reloadBtn = btn('refresh', 'Reload', 'act-restart', () => openFile(file.path, true));
  const dlBtn = btn('download', 'Download', 'act-logs', () => download(file.path));
  const closeEdBtn = btn('close', 'Close', 'act-remove', () => closeEditor());
  const editorView = h('div', { class: 'editor-view', hidden: true },
    h('div', { class: 'toolbar' }, edPath, edBadge, edState, h('span', { class: 'spacer' }), saveBtn, reloadBtn, dlBtn, closeEdBtn),
    area,
    h('div', { class: 'ed-foot' }, edPos, h('span', { class: 'muted' }, 'Tab inserts a tab · Ctrl+S saves · Esc closes')));
  p.body.append(listView, editorView);

  let cwd = '';
  let entries = [];
  let file = null; // { path, modTime, sudo, original }
  let active = false;
  let loaded = false;

  function setStatus(t) { status.textContent = t; }

  function renderCrumbs() {
    const parts = cwd.split('/').filter(Boolean);
    const items = [h('button', { type: 'button', class: 'crumb', onclick: () => go('/') }, '/')];
    parts.forEach((part, i) => {
      const target = `/${parts.slice(0, i + 1).join('/')}`;
      items.push(h('span', { class: 'muted' }, '›'), h('button', { type: 'button', class: 'crumb', onclick: () => go(target) }, part));
    });
    crumbs.replaceChildren(...items);
  }

  function entryRow(e) {
    const full = join(cwd, e.name);
    const ic = icon(e.isDir ? 'folder' : e.isLink ? 'link' : 'file');
    ic.classList.add(e.isDir ? 'ic-folder' : 'ic-file');
    const nameBtn = h('button', { type: 'button', class: 'file-name' }, ic, e.name, e.isLink ? h('span', { class: 'muted' }, ' (link)') : '');
    const open = () => (e.isDir ? go(full) : openFile(full));
    nameBtn.addEventListener('dblclick', open);
    nameBtn.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') open(); });
    nameBtn.addEventListener('click', () => { if (e.isDir) go(full); });
    const menuItems = () => [
      { label: e.isDir ? 'Open' : 'Edit', action: open },
      ...(e.isDir ? [{ label: 'Terminal here', action: () => openShellAt(full) }] : [{ label: 'Download', action: () => download(full) }]),
      { label: 'Copy path', action: () => copyText(full).then(() => toast('Path copied')) },
      { label: 'Rename…', action: () => rename(e) },
      { label: e.isDir ? 'Delete folder…' : 'Delete…', danger: true, action: () => remove(e) },
    ];
    const more = h('button', { type: 'button', class: 'btn sm act act-inspect', title: 'Actions', 'aria-label': `Actions for ${e.name}` }, icon('more'));
    more.addEventListener('click', (ev) => openMenu(ev, more, menuItems()));
    const quick = e.isDir
      ? h('button', { type: 'button', class: 'btn sm act act-logs', onclick: () => go(full) }, icon('folder'), 'Open')
      : h('button', { type: 'button', class: 'btn sm act act-logs', onclick: () => openFile(full) }, icon('edit'), 'Edit');
    const tr = h('tr', {},
      h('td', {}, nameBtn),
      h('td', { class: 'num' }, e.isDir ? '' : fmtBytes(e.size)),
      h('td', { class: 'muted nowrap' }, fmtDateTime(e.modTime)),
      h('td', { class: 'mono muted' }, e.mode),
      h('td', {}, h('div', { class: 'c-actions' }, quick, e.isDir ? '' : h('button', { type: 'button', class: 'btn sm act act-start', title: 'Download', 'aria-label': `Download ${e.name}`, onclick: () => download(full) }, icon('download')), more)));
    tr.addEventListener('contextmenu', (ev) => openMenu(ev, tr, menuItems()));
    return tr;
  }

  function render() {
    const q = filter.value.trim().toLowerCase();
    const rows = entries.filter((e) => (hidden.checked || !e.name.startsWith('.')) && (!q || e.name.toLowerCase().includes(q)));
    const hiddenCount = entries.filter((e) => e.name.startsWith('.')).length;
    setStatus(`${entries.length} items${!hidden.checked && hiddenCount ? ` · ${hiddenCount} hidden` : ''}`);
    tbody.replaceChildren(...(rows.length ? rows.map(entryRow) : [h('tr', {}, h('td', { colspan: 5, class: 'empty' }, entries.length ? 'Nothing matches.' : 'This folder is empty.'))]));
  }

  async function go(dir) {
    setStatus('Loading…');
    try {
      const res = await call('ListDir', serverId, dir);
      cwd = res.path;
      entries = res.entries;
      pathInput.value = cwd;
      upBtn.disabled = cwd === '/';
      setPref(LAST_KEY + serverId, cwd);
      renderCrumbs();
      render();
      loaded = true;
    } catch (err) {
      setStatus('');
      toast(err.message, true);
      if (!loaded && dir) go('');
    }
  }
  pathInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(pathInput.value.trim() || '/'); });
  filter.addEventListener('input', render);
  hidden.addEventListener('change', render);

  // ---- actions
  async function newFolder() {
    const name = await promptText('New folder', { label: `Name (in ${cwd})` });
    if (!name) return;
    if (name.includes('/')) { toast('A name cannot contain "/"', true); return; }
    await call('MakeDir', serverId, join(cwd, name)).then(() => go(cwd)).catch((err) => toast(err.message, true));
  }
  async function newFile() {
    const name = await promptText('New file', { label: `Name (in ${cwd})`, placeholder: 'e.g. .env' });
    if (!name) return;
    if (name.includes('/')) { toast('A name cannot contain "/"', true); return; }
    if (entries.some((e) => e.name === name)) { toast(`${name} already exists`, true); return; }
    try {
      await call('WriteFile', serverId, join(cwd, name), '', 0);
      await go(cwd);
      openFile(join(cwd, name));
    } catch (err) {
      toast(err.message, true);
    }
  }
  async function rename(e) {
    const name = await promptText(`Rename ${e.isDir ? 'folder' : 'file'}`, { label: 'New name', value: e.name, okLabel: 'Rename' });
    if (!name || name === e.name) return;
    if (name.includes('/')) { toast('A name cannot contain "/"', true); return; }
    await call('RenamePath', serverId, join(cwd, e.name), join(cwd, name)).then(() => go(cwd)).catch((err) => toast(err.message, true));
  }
  async function remove(e) {
    const what = e.isDir ? `the folder "${e.name}" and EVERYTHING inside it` : `the file "${e.name}"`;
    if (!(await confirmDialog(`Delete ${what} on ${serverName()}?\n\n${join(cwd, e.name)}\n\nThis cannot be undone.`, 'Delete'))) return;
    await call('DeletePath', serverId, join(cwd, e.name)).then(() => { toast(`Deleted ${e.name}`); go(cwd); }).catch((err) => toast(err.message, true));
  }
  async function download(path) {
    try {
      const local = await call('Download', serverId, path);
      if (local) toast(`Saved to ${local}`);
    } catch (err) {
      toast(err.message, true);
    }
    prog.hidden = true;
  }
  async function upload() {
    const locals = await call('PickUploadFiles').catch(() => null);
    if (!locals?.length) return;
    try {
      let res = await call('UploadFiles', serverId, cwd, locals, false);
      if (res.existing.length) {
        if (!(await confirmDialog(`These files already exist in ${cwd}:\n\n${res.existing.join('\n')}\n\nReplace them?`, 'Replace files'))) return;
        res = await call('UploadFiles', serverId, cwd, locals, true);
      }
      toast(`Uploaded ${res.uploaded.length} file${res.uploaded.length > 1 ? 's' : ''}`);
    } catch (err) {
      toast(err.message, true);
    }
    prog.hidden = true;
    go(cwd);
  }
  progressHandlers.set(serverId, (x) => {
    prog.hidden = x.done >= x.total && x.total > 0;
    prog.firstChild.textContent = `${x.name} ${fmtBytes(x.done)} / ${fmtBytes(x.total)}`;
    prog.querySelector('.meter span').style.width = `${x.total ? Math.round((x.done / x.total) * 100) : 0}%`;
  });

  // ---- editor
  const dirty = () => file && area.value !== file.original;
  function updateEdState() {
    edState.textContent = dirty() ? '● unsaved changes' : file?.size != null ? `${fmtBytes(file.size)} · saved ${fmtDateTime(file.modTime)}` : '';
    edState.classList.toggle('dirty', !!dirty());
    saveBtn.disabled = !dirty();
  }
  function updatePos() {
    const before = area.value.slice(0, area.selectionStart);
    const line = before.split('\n').length;
    edPos.textContent = `Line ${line}, column ${before.length - before.lastIndexOf('\n')} · ${area.value.split('\n').length} lines`;
  }
  async function openFile(path, reload = false) {
    if (!reload && dirty() && !(await confirmDialog('Discard the unsaved changes?', 'Unsaved changes'))) return;
    if (reload && dirty() && !(await confirmDialog('Reload from the server and lose your changes?', 'Reload file'))) return;
    setStatus('Opening…');
    try {
      const f = await call('ReadFile', serverId, path);
      file = { path: f.path, modTime: f.modTime, sudo: f.sudo, size: f.size, original: f.content };
      area.value = f.content;
      edPath.textContent = f.path;
      edBadge.hidden = !f.sudo;
      listView.hidden = true;
      editorView.hidden = false;
      updateEdState();
      updatePos();
      area.focus();
      area.setSelectionRange(0, 0);
      area.scrollTop = 0;
    } catch (err) {
      toast(err.message, true);
    }
    render();
  }
  async function save(force = false) {
    if (!file) return;
    saveBtn.disabled = true;
    try {
      const res = await call('WriteFile', serverId, file.path, area.value, force ? 0 : file.modTime);
      file = { ...file, modTime: res.modTime, size: res.size, original: area.value, sudo: res.sudo || file.sudo };
      edBadge.hidden = !file.sudo;
      toast(`Saved ${file.path}`);
    } catch (err) {
      if (err.message.startsWith('CHANGED:')) {
        if (await confirmDialog(`${file.path} was changed on the server since you opened it.\n\nOverwrite it with your version?`, 'File changed')) return save(true);
      } else {
        toast(`Could not save: ${err.message}`, true);
      }
    }
    updateEdState();
  }
  async function closeEditor() {
    if (dirty() && !(await confirmDialog('Close without saving your changes?', 'Unsaved changes'))) return;
    file = null;
    editorView.hidden = true;
    listView.hidden = false;
    go(cwd);
  }
  area.addEventListener('input', () => { updateEdState(); updatePos(); });
  area.addEventListener('click', updatePos);
  area.addEventListener('keyup', updatePos);
  area.addEventListener('keydown', (e) => {
    if (e.key === 's' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); if (dirty()) save(); }
    else if (e.key === 'Escape') { e.preventDefault(); closeEditor(); }
    else if (e.key === 'Tab' && !e.ctrlKey) {
      e.preventDefault();
      document.execCommand('insertText', false, '\t'); // keeps undo history
    }
  });

  return {
    el: p.el,
    setActive(v) {
      active = v;
      if (active && !loaded) {
        let last = '';
        last = getPref(LAST_KEY + serverId, '') || '';
        go(last);
      }
    },
    // Called from the Containers tab: open a folder, or edit a file.
    async openPath(path, asFile = false) {
      if (asFile) {
        await go(parent(path));
        openFile(path);
      } else {
        if (editorView.hidden === false && !(await closeEditorQuiet())) return;
        go(path);
      }
    },
    hasUnsaved: () => !!dirty(),
    destroy() { progressHandlers.delete(serverId); },
  };

  async function closeEditorQuiet() {
    if (dirty() && !(await confirmDialog('Close the file without saving your changes?', 'Unsaved changes'))) return false;
    file = null;
    editorView.hidden = true;
    listView.hidden = false;
    return true;
  }
}
