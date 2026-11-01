// "Clone containers": copies containers started with docker run from one server to another. Their settings are
// read with docker inspect; the environment variables are edited on the way (find and replace, one value for every
// service that uses it, or per service), then a script creates and starts them on the target, in a terminal.
// Edits are kept as a list of changes, so they can be undone one by one and saved as a profile for the next time.
import { h, toast } from './util.js';
import { call, confirmDialog } from './bridge.js';
import { promptText } from './ui.js';
import { icon } from './icons.js';
import { combo } from './combo.js';
import { getPref, setPref } from './prefs.js';
import { jobs, progressView, onJobChange } from './clone-progress.js';

const SECRET_RE = /pass|pwd|secret|token|key|credential|private|auth|connectionstring/i;
const isSecret = (key) => SECRET_RE.test(key);

// The clone in progress survives closing the dialog (e.g. to log in to Docker Hub on the target).
// { sourceId, targetId, names, source: { specs, networks, hubLogin, arch }, changes: [], options, ui }
let draft = null;

// ---------------------------------------------------------------- changes

// ---------------------------------------------------------------- .env files mounted into containers

// A .env file as lines; variable lines keep their layout (export, spaces, quotes, comment) so only values change.
const ENV_LINE = /^(\s*(?:export\s+)?)([A-Za-z_][A-Za-z0-9_.-]*)(\s*=\s*)(.*)$/;
function parseEnv(text) {
  return text.split('\n').map((raw) => {
    const cr = raw.endsWith('\r') ? '\r' : '';
    const body = cr ? raw.slice(0, -1) : raw;
    const m = ENV_LINE.exec(body);
    if (!m) return { raw };
    const [, pre, key, eq, rest] = m;
    let value = rest;
    let quote = '';
    let post = '';
    if ((rest[0] === '"' || rest[0] === "'") && rest.lastIndexOf(rest[0]) > 0) {
      quote = rest[0];
      const end = rest.lastIndexOf(quote);
      value = rest.slice(1, end);
      post = rest.slice(end + 1);
    } else {
      const c = rest.search(/\s+#/);
      if (c >= 0) { value = rest.slice(0, c); post = rest.slice(c); }
    }
    return { pre, key, eq, value, quote, post, cr };
  });
}
const lineText = (l) => (l.key === undefined ? l.raw : `${l.pre}${l.key}${l.eq}${l.quote}${l.value}${l.quote}${l.post}${l.cr}`);
const envText = (lines) => lines.map(lineText).join('\n');

// A new value keeps the line's quotes; an unquoted one gets quotes when it needs them.
function setLine(l, value) {
  l.value = value;
  if (!l.quote && /[\s#"'`$\\]/.test(value)) l.quote = value.includes('"') ? "'" : '"';
}
function addLine(lines, key, value) {
  const l = { pre: '', key, eq: '=', value: '', quote: '', post: '', cr: lines.some((x) => x.cr) ? '\r' : '' };
  setLine(l, value);
  // Before the empty last line of a file that ends with a newline.
  const last = lines[lines.length - 1];
  if (last && last.key === undefined && last.raw === '') lines.splice(lines.length - 1, 0, l); else lines.push(l);
}
const fileVars = (f) => f.lines.filter((l) => l.key !== undefined);

// Files whose variables the containers got through docker run --env-file (found by content, or added by path).
const SYNCED = new Set(['env-file', 'manual']);
// docker --env-file keeps quotes: a container whose value had them gets them again.
const envValueFor = (l, e) => (l.quote && e && /^["']/.test(e.value) ? `${l.quote}${l.value}${l.quote}` : l.value);
const USAGE_TEXT = {
  mount: 'mounted into the container',
  folder: 'inside a folder mounted into the container',
  'env-file': 'given to docker run --env-file (found by its content)',
  manual: 'added by path',
};
// The .env file each variable of a container comes from, when it comes from one.
function keysFromFiles(files, name) {
  const out = new Map();
  for (const f of files) {
    if (!SYNCED.has(f.usage) || f.format === 'json' || !f.containers.includes(name)) continue;
    for (const l of fileVars(f)) out.set(l.key, f.path);
  }
  return out;
}
const baseName = (p) => p.slice(p.lastIndexOf('/') + 1);

// A change is one of:
//   { type: 'replace', find, to, images }  text replaced inside every value (and image names when images)
//   { type: 'value', find, to }            every value equal to find becomes to
//   { type: 'set', service, key, value }   one variable of one container (added when missing)
//   { type: 'unset', service, key }        one variable removed
//   { type: 'image', service, value }      the image of one container
//   { type: 'ports', service, ports }      the published ports of one container ([{ hostIp, hostPort, port }])
//   { type: 'fileset', path, key, value }  one variable of a mounted .env file (added when missing)
//   { type: 'fileunset', path, key }       one variable of a .env file removed
//   { type: 'filetext', path, text }       a .env file rewritten as a whole
// Replace and value also change the .env files. Applied in order to what was read; hits counts what each
// change touched (0: nothing matches).
function applyChanges(src, changes, opts = {}) {
  const out = src.specs.map((s) => ({ ...s, key: s.name, env: s.env.map((e) => ({ ...e })), ports: (s.ports || []).map((p) => ({ ...p })) }));
  const files = (src.files || []).map(asFile);
  const hits = changes.map((c) => {
    let n = 0;
    if (c.type === 'replace' || c.type === 'value') {
      const hit = (v) => (c.type === 'replace' ? !!c.find && v.includes(c.find) : v === c.find);
      const next = (v) => (c.type === 'replace' ? v.split(c.find).join(c.to) : c.to);
      for (const f of files) {
        if (f.format === 'json') {
          // String values only, last first so the earlier places stay valid.
          const leaves = jsonLeaves(f.text).leaves.filter((l) => l.type === 'string' && hit(l.value));
          for (const l of leaves.reverse()) { f.text = setJsonLeaf(f.text, l, next(l.value)); n++; }
          continue;
        }
        for (const l of fileVars(f)) if (hit(l.value)) { setLine(l, next(l.value)); n++; }
      }
    } else if (c.type.startsWith('file') || c.type === 'jsonset') {
      const f = files.find((x) => x.path === c.path);
      if (!f) return 0;
      if (c.type === 'filetext') {
        if (f.format === 'json') f.text = c.text; else f.lines = parseEnv(c.text);
        return 1;
      }
      if (c.type === 'jsonset') {
        const l = jsonLeaves(f.text).leaves.find((x) => x.key === c.key);
        if (l) { f.text = setJsonLeaf(f.text, l, c.value); n++; }
        return n;
      }
      const l = fileVars(f).find((x) => x.key === c.key);
      if (c.type === 'fileset') { if (l) setLine(l, c.value); else addLine(f.lines, c.key, c.value); n++; }
      else if (c.type === 'fileunset' && l) { f.lines.splice(f.lines.indexOf(l), 1); n++; }
      return n;
    }
    for (const s of out) {
      if (c.service && c.service !== s.key) continue;
      if (c.type === 'replace' && c.find) {
        for (const e of s.env) if (e.value.includes(c.find)) { e.value = e.value.split(c.find).join(c.to); n++; }
        if (c.images && s.image.includes(c.find)) { s.image = s.image.split(c.find).join(c.to); n++; }
      } else if (c.type === 'value') {
        for (const e of s.env) if (e.value === c.find) { e.value = c.to; n++; }
      } else if (c.type === 'rename') {
        s.name = c.to;
        n++;
      } else if (c.type === 'ports') {
        s.ports = c.ports.map((p) => ({ ...p }));
        n++;
      } else if (c.type === 'set') {
        const e = s.env.find((x) => x.key === c.key);
        if (e) e.value = c.value; else s.env.push({ key: c.key, value: c.value });
        n++;
      } else if (c.type === 'unset') {
        const i = s.env.findIndex((x) => x.key === c.key);
        if (i >= 0) { s.env.splice(i, 1); n++; }
      } else if (c.type === 'image') {
        s.image = c.value;
        n++;
      }
    }
    return n;
  });
  // A file given to docker run --env-file is where the containers' variables come from: they follow its edits
  // (a variable removed from the file is removed from them too).
  for (const f of files) {
    if (!SYNCED.has(f.usage) || f.format === 'json') continue;
    const before = fileVars({ lines: parseEnv(f.content) }).map((l) => l.key);
    const now = fileVars(f);
    for (const s of out) {
      if (!f.containers.includes(s.key)) continue;
      for (const l of now) {
        const e = s.env.find((x) => x.key === l.key);
        const v = envValueFor(l, e);
        if (e) e.value = v; else s.env.push({ key: l.key, value: v });
      }
      for (const k of before) {
        if (!now.some((l) => l.key === k)) s.env = s.env.filter((e) => e.key !== k);
      }
    }
  }
  for (const f of files) f.content = f.format === 'json' ? f.text : envText(f.lines);
  // A renamed container keeps its old name as a network alias (others may call it by that name), unless that is
  // turned off; a container sharing another's network (--network container:x) follows its new name.
  const renamed = new Map(out.filter((s) => s.name !== s.key).map((s) => [s.key, s.name]));
  if (renamed.size) {
    for (const s of out) {
      s.flags = (s.flags || []).map((f) => (f.startsWith('container:') && renamed.has(f.slice(10)) ? `container:${renamed.get(f.slice(10))}` : f));
      if (s.name !== s.key && opts.keepAliases !== false) {
        s.networks = (s.networks || []).map((nw) => ({ ...nw, aliases: nw.aliases.includes(s.key) ? nw.aliases : [...nw.aliases, s.key] }));
      }
    }
  }
  // A changed image name is not the build the source runs: the target gets it by that name.
  out.forEach((s, i) => { if (s.image !== src.specs[i].image) s.imageId = ''; });
  return { specs: out, files, hits };
}

// A file as the editor works on it: env files as lines, JSON as text.
function asFile(f) {
  const format = f.format || 'env';
  return format === 'json' ? { ...f, format, text: f.content } : { ...f, format, lines: parseEnv(f.content) };
}

// The values of a file, as { key, value, type } (JSON keys as paths like ConnectionStrings:Default).
function fileEntries(f) {
  if (f.format === 'json') {
    return jsonLeaves(f.text ?? f.content).leaves.map((l) => ({ key: l.key, value: l.value, type: l.type }));
  }
  return fileVars(f.lines ? f : asFile(f)).map((l) => ({ key: l.key, value: l.value, type: 'string' }));
}

// ---------------------------------------------------------------- JSON files (appsettings.json…)

// The leaf values of a JSON text with their place in it, so one can be changed without touching the rest
// (comments, order, indentation). Comments and trailing commas are allowed, as in .NET's appsettings.json.
// Keys are paths joined with ":" like .NET configuration (ConnectionStrings:Default, Serilog:WriteTo:0:Name).
function jsonLeaves(text) {
  const n = text.length;
  const leaves = [];
  let i = 0;
  const fail = (what) => { throw new Error(`${what} at line ${text.slice(0, i).split('\n').length}`); };
  const ws = () => {
    for (;;) {
      while (i < n && /\s/.test(text[i])) i++;
      if (text.startsWith('//', i)) { while (i < n && text[i] !== '\n') i++; } else if (text.startsWith('/*', i)) { const e = text.indexOf('*/', i + 2); i = e < 0 ? n : e + 2; } else return;
    }
  };
  const str = () => {
    const s = i++;
    while (i < n && text[i] !== '"') { if (text[i] === '\\') i++; i++; }
    if (i >= n) fail('Unclosed string');
    i++;
    return JSON.parse(text.slice(s, i));
  };
  const value = (path) => {
    ws();
    const c = text[i];
    if (c === '{' || c === '[') {
      const close = c === '{' ? '}' : ']';
      let idx = 0;
      i++;
      ws();
      while (text[i] !== close) {
        if (i >= n) fail('Unexpected end');
        let key = String(idx++);
        if (c === '{') {
          if (text[i] !== '"') fail('Expected a "key"');
          key = str();
          ws();
          if (text[i] !== ':') fail('Expected ":"');
          i++;
        }
        value(path ? `${path}:${key}` : key);
        ws();
        if (text[i] === ',') { i++; ws(); } else if (text[i] !== close) fail(`Expected "," or "${close}"`);
      }
      i++;
      return;
    }
    const start = i;
    if (c === '"') { leaves.push({ key: path, start, type: 'string', value: str(), end: i }); return; }
    const m = /^(-?\d+(\.\d+)?([eE][+-]?\d+)?|true|false|null)/.exec(text.slice(i, i + 80));
    if (!m) fail(`Unexpected "${c ?? 'end'}"`);
    i += m[0].length;
    leaves.push({ key: path, start, end: i, type: m[0] === 'null' ? 'null' : /^[tf]/.test(m[0]) ? 'bool' : 'number', value: m[0] });
  };
  try {
    value('');
    ws();
    if (i < n) fail('Text after the end');
    return { leaves };
  } catch (e) {
    return { leaves: [], error: e.message };
  }
}

// Writes a new value in place: a string stays a string; a number, true, false or null stays one when the new
// value is one too (otherwise it becomes a string).
function setJsonLeaf(text, leaf, value) {
  const literal = /^(-?\d+(\.\d+)?([eE][+-]?\d+)?|true|false|null)$/.test(value.trim());
  const enc = leaf.type !== 'string' && literal ? value.trim() : JSON.stringify(value);
  return text.slice(0, leaf.start) + enc + text.slice(leaf.end);
}

// A later edit of the same thing (variable, image, ports, file) replaces the earlier one.
function addChange(c) {
  const list = draft.changes;
  const same = {
    set: (x) => (x.type === 'set' || x.type === 'unset') && x.service === c.service && x.key === c.key,
    image: (x) => x.type === 'image' && x.service === c.service,
    ports: (x) => x.type === 'ports' && x.service === c.service,
    rename: (x) => x.type === 'rename' && x.service === c.service,
    fileset: (x) => (x.type === 'fileset' || x.type === 'fileunset') && x.path === c.path && x.key === c.key,
    filetext: (x) => (x.type.startsWith('file') || x.type === 'jsonset') && x.path === c.path,
    jsonset: (x) => x.type === 'jsonset' && x.path === c.path && x.key === c.key,
  }[c.type === 'unset' ? 'set' : c.type === 'fileunset' ? 'fileset' : c.type];
  if (same) for (let i = list.length - 1; i >= 0; i--) if (same(list[i])) list.splice(i, 1);
  list.push(c);
}

const derived = () => applyChanges(draft.source, draft.changes, draft.options);

const portText = (p) => `${p.hostIp ? `${p.hostIp}:` : ''}${p.hostPort || '(random)'} → ${p.port}`;

function describeChange(c, showSecrets) {
  const val = (v, key) => (!showSecrets && key && isSecret(key) ? '••••••' : `“${v}”`);
  switch (c.type) {
    case 'replace': return `Replace “${c.find}” with “${c.to}” in every value${c.images ? ' and image' : ''}`;
    case 'value': return `Every value ${showSecrets ? `“${c.find}”` : '…'} → “${c.to}”`;
    case 'set': return `${c.service} · ${c.key} = ${val(c.value, c.key)}`;
    case 'unset': return `${c.service} · remove ${c.key}`;
    case 'image': return `${c.service} · image → ${c.value}`;
    case 'rename': return `${c.service} · renamed to ${c.to}`;
    case 'ports': return `${c.service} · ports ${c.ports.length ? c.ports.map(portText).join(', ') : 'none'}`;
    case 'fileset': return `${c.path} · ${c.key} = ${val(c.value, c.key)}`;
    case 'fileunset': return `${c.path} · remove ${c.key}`;
    case 'filetext': return `${c.path} · rewritten`;
    case 'jsonset': return `${c.path} · ${c.key} = ${val(c.value, c.key)}`;
    default: return JSON.stringify(c);
  }
}

// ---------------------------------------------------------------- the tab

export const TAB_TITLE = 'Clone containers';
const LEAD = 'Copy containers to another server, change their environment variables, ports and .env files, then start them there with live progress.';

// The Clone tab. ctx: { servers, snaps, runTask(serverId, task), openServer(id), openLogs(serverId, name) }.
// Returns the tab view; preset(serverId) starts a clone from that server (the Containers tab's button).
export function cloneView(root, ctx) {
  let detach = null; // stops the progress page from following its job
  const lead = h('p', {}, LEAD);
  const recent = h('div', { class: 'cl-recent' });
  const steps = h('ol', { class: 'clone-steps' }, ...['Choose', 'Edit variables', 'Check', 'Deploy'].map((t, i) => h('li', {}, h('span', { class: 'n' }, i + 1), t)));
  const content = h('div', { class: 'clone-content' });
  const foot = h('div', { class: 'cl-foot' });
  [...steps.children].forEach((li, i) => {
    const back = () => { if (li.classList.contains('can-go')) go(i); };
    li.addEventListener('click', back);
    li.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); back(); } });
  });
  root.append(h('div', { class: 'cl-page clone' },
    h('header', { class: 'bk-hero cl-hero' },
      h('span', { class: 'bk-hero-icon' }, icon('copy')),
      h('div', { class: 'bk-hero-text' }, h('h2', {}, TAB_TITLE), lead),
      recent),
    h('section', { class: 'cl-panel' }, steps, content),
    foot));
  // What the steps were written for: a footer for their buttons and the scroll position (the page scrolls here).
  const m = {
    foot,
    body: { get scrollTop() { return window.scrollY; }, set scrollTop(y) { window.scrollTo(0, y); } },
    close: null,
    setSub: (t) => { lead.textContent = t || LEAD; },
  };
  let step = 0;
  let shownJob = null;
  const go = (n, preset, jobId) => {
    if (preset !== undefined) ctx.preset = preset;
    detach?.();
    detach = null;
    step = n;
    shownJob = n === 3 ? jobId : null;
    [...steps.children].forEach((li, i) => {
      li.classList.toggle('active', i === n);
      li.classList.toggle('done', i < n);
      // A step already passed can be gone back to (the later ones need the checks of the steps before them).
      const can = i < n && (i === 0 || !!draft);
      li.classList.toggle('can-go', can);
      li.tabIndex = can ? 0 : -1;
      li.setAttribute('role', can ? 'button' : 'listitem');
      li.title = can ? `Back to “${li.textContent.replace(/^d+/, '')}”` : '';
    });
    foot.replaceChildren();
    content.replaceChildren();
    window.scrollTo(0, 0);
    renderRecent();
    if (n === 3) { showProgress(jobId); return; }
    m.setSub('');
    [chooseStep, editStep, reviewStep][n]({ ...ctx, m, content, go });
  };

  // Step 4: the live progress of a job (the one just started, or one picked from the recent list).
  async function showProgress(id) {
    m.setSub('The copy runs on the target server: you can switch to other tabs, this tab shows the progress.');
    if (!jobs.has(id)) {
      try { for (const v of await call('CloneJobs')) jobs.set(v.id, v); } catch { /* shown below */ }
    }
    if (!jobs.has(id)) { content.append(h('div', { class: 'empty' }, 'This clone is no longer known.')); return; }
    detach = progressView(id, {
      content, foot,
      actions: {
        close: null,
        show: (next) => go(3, undefined, next),
        editAgain: draft ? () => go(1) : null,
        newClone: () => { draft = null; go(0); },
        openServer: ctx.openServer,
        openLogs: ctx.openLogs,
      },
    });
  }

  // The latest clones, top right: a click shows one's progress.
  function renderRecent() {
    const list = [...jobs.values()].sort((a, b) => b.started - a.started).slice(0, 3);
    recent.replaceChildren(...list.map((v) => h('button', {
      type: 'button', class: `cl-recent-item s-${v.state}${shownJob === v.id ? ' active' : ''}`,
      title: `${v.source} → ${v.target} · ${v.containers.length} containers`,
      onclick: () => go(3, undefined, v.id),
    },
    h('span', { class: 'cl-recent-dot' }),
    h('span', { class: 'cl-recent-route' }, v.source, icon('arrowRight'), v.target),
    h('span', { class: 'cl-recent-state' }, v.state === 'running' ? `${Math.floor(v.percent)}%` : { done: 'Done', warning: 'Issues', failed: 'Failed', cancelled: 'Cancelled' }[v.state] || v.state))));
  }
  let lastStates = '';
  const offJobs = onJobChange(() => {
    const states = [...jobs.values()].map((v) => `${v.id}${v.state}${Math.floor(v.percent / 5)}`).join();
    if (states !== lastStates) { lastStates = states; renderRecent(); }
  });

  const running = [...jobs.values()].find((v) => v.state === 'running');
  if (running) go(3, undefined, running.id);
  else go(draft ? 1 : 0);

  return {
    update() {}, // server snapshots are read when a step needs them
    destroy() { detach?.(); offJobs(); },
    // Start a clone from this server (asks before dropping edits made for another one).
    async preset(serverId) {
      if (draft && draft.sourceId !== serverId && draft.changes.length
        && !await confirmDialog(`Discard the clone in progress (${draft.changes.length} changes)?\n\nSave them as a profile first to use them again.`, 'Discard clone')) return;
      if (!draft || draft.sourceId !== serverId) draft = null;
      if (draft && step !== 3) return;
      go(0, serverId);
    },
  };
}

const serverName = (ctx, id) => ctx.servers.get(id)?.name || 'the server';

function chooseStep(ctx) {
  const { m, content, servers, snaps, go } = ctx;
  const options = [...servers.values()].sort((a, b) => a.name.localeCompare(b.name))
    .map((s) => ({ value: s.id, label: s.name, sub: `${s.username}@${s.host}` }));
  const src = combo({ ariaLabel: 'Source server', placeholder: 'Choose the server to copy from' });
  const dst = combo({ ariaLabel: 'Target server', placeholder: 'Choose the server to create them on' });
  src.setOptions(options);
  dst.setOptions(options);
  // Back from a later step: the clone in progress fills the form (its edits are kept when settings are read again).
  src.value = draft?.sourceId || ctx.preset || '';
  if (draft?.targetId) dst.value = draft.targetId;
  const filter = h('input', { type: 'search', placeholder: 'Filter containers…', 'aria-label': 'Filter containers' });
  const list = h('div', { class: 'clone-pick' });
  const count = h('span', { class: 'muted' });
  const msg = h('div', { class: 'card-error' });
  const next = h('button', { type: 'button', class: 'btn primary' }, 'Read settings', icon('arrowRight'));
  let picked = new Set();

  const containers = () => (snaps.get(src.value)?.docker?.containers || []).slice().sort((a, b) => a.name.localeCompare(b.name));
  function renderList() {
    const all = containers();
    const q = filter.value.trim().toLowerCase();
    const shown = all.filter((c) => !q || [c.name, c.image, c.project].some((v) => v?.toLowerCase().includes(q)));
    list.replaceChildren(...(shown.length ? shown.map((c) => {
      const cb = h('input', { type: 'checkbox', checked: picked.has(c.name) });
      cb.addEventListener('change', () => { cb.checked ? picked.add(c.name) : picked.delete(c.name); sync(); });
      return h('label', { class: 'check clone-pick-row' }, cb,
        h('span', { class: 'c-name' }, c.name), h('span', { class: 'c-image' }, c.image),
        h('span', { class: `clone-state ${c.state === 'running' ? 'good' : ''}` }, c.state),
        c.project ? h('span', { class: 'muted', title: 'Started by Docker Compose: it is copied as a plain container' }, `compose: ${c.project}`) : '');
    }) : [h('div', { class: 'empty' }, !src.value ? 'Choose the source server first.' : all.length ? 'No container matches.' : 'No containers on this server (or it is not connected).')]));
    sync();
  }
  function sync() {
    count.textContent = `${picked.size} selected`;
    const same = src.value && src.value === dst.value;
    msg.textContent = same ? 'Choose two different servers.' : '';
    next.disabled = !src.value || !dst.value || same || !picked.size;
  }
  const pickRunning = () => { picked = new Set(containers().filter((c) => c.state === 'running').map((c) => c.name)); renderList(); };
  src.el.addEventListener('change', pickRunning);
  dst.el.addEventListener('change', sync);
  filter.addEventListener('input', renderList);
  const quick = (label, fn) => h('button', { type: 'button', class: 'btn sm ghost', onclick: () => { fn(); renderList(); } }, label);

  content.append(
    h('div', { class: 'clone-servers' },
      h('label', {}, 'From', src.el), h('span', { class: 'clone-arrow', 'aria-hidden': 'true' }, icon('arrowRight')), h('label', {}, 'To', dst.el)),
    h('div', { class: 'toolbar clone-pick-tools' }, filter,
      quick('Running', () => { picked = new Set(containers().filter((c) => c.state === 'running').map((c) => c.name)); }),
      quick('All', () => { picked = new Set(containers().map((c) => c.name)); }),
      quick('None', () => { picked = new Set(); }),
      h('span', { class: 'spacer' }), count),
    list,
    h('p', { class: 'hint' }, 'Their image, variables, ports, volumes, networks, restart policy and limits are copied. Data inside volumes is not: copy databases with Postgres → Copy.'),
    msg);
  if (draft && draft.sourceId === src.value) {
    picked = new Set(draft.source.specs.map((x) => x.name));
    renderList();
  } else pickRunning();

  next.addEventListener('click', async () => {
    next.disabled = true;
    next.replaceChildren(h('span', { class: 'spinner' }), 'Reading…');
    try {
      const source = await call('CloneRead', src.value, [...picked]);
      // Read again for the same clone: the edits (a list of changes) apply to the new reading; files added by hand
      // stay when the source server is the same.
      const prev = draft;
      if (prev && prev.sourceId === src.value) {
        for (const f of prev.source.files || []) {
          if (f.usage === 'manual' && !source.files.some((x) => x.path === f.path)) source.files.push(f);
        }
      }
      draft = prev
        ? { ...prev, sourceId: src.value, targetId: dst.value, source }
        : { sourceId: src.value, targetId: dst.value, source, changes: [], options: { images: 'auto', replace: false }, ui: { view: 'service', open: new Set(), showSecrets: getPref('cloneShowSecrets', '1') !== '0' } };
      go(1);
    } catch (err) {
      msg.textContent = err.message;
      next.disabled = false;
      next.replaceChildren('Read settings', icon('arrowRight'));
    }
  });
  m.foot.append(h('span', { class: 'spacer' }), next);
}

// ---------------------------------------------------------------- step 2: edit

function editStep(ctx) {
  const { m, content, go } = ctx;
  const ui = draft.ui;
  let view = derived();

  const resume = h('div', { class: 'clone-resume' },
    icon('info'), h('span', {}, `${serverName(ctx, draft.sourceId)} → ${serverName(ctx, draft.targetId)} · ${draft.source.specs.length} containers`),
    h('span', { class: 'spacer' }),
    h('button', {
      type: 'button', class: 'btn sm ghost',
      onclick: async () => {
        if (draft.changes.length && !await confirmDialog(`Start a new clone?\n\nThe ${draft.changes.length} changes made here are dropped (save them as a profile to keep them).`, 'Discard clone')) return;
        const preset = draft.sourceId;
        draft = null;
        go(0, preset);
      },
    }, 'New clone'));

  // Find and replace in every value.
  const find = h('input', { type: 'text', placeholder: 'Find (e.g. staging.example.com)', spellcheck: 'false', 'aria-label': 'Find' });
  const repl = h('input', { type: 'text', placeholder: 'Replace with', spellcheck: 'false', 'aria-label': 'Replace with' });
  const inImages = h('input', { type: 'checkbox' });
  const matches = h('span', { class: 'muted clone-matches' });
  const replBtn = h('button', { type: 'button', class: 'btn sm primary' }, 'Replace all');
  const countMatches = () => {
    const f = find.value;
    if (!f) { matches.textContent = ''; replBtn.disabled = true; return 0; }
    let n = 0;
    for (const s of view.specs) {
      for (const e of s.env) if (e.value.includes(f)) n++;
      if (inImages.checked && s.image.includes(f)) n++;
    }
    for (const file of view.files) for (const e of fileEntries(file)) if (e.type === 'string' && e.value.includes(f)) n++;
    matches.textContent = `${n} match${n === 1 ? '' : 'es'}`;
    replBtn.disabled = !n;
    return n;
  };
  find.addEventListener('input', countMatches);
  inImages.addEventListener('change', countMatches);
  const doReplace = () => {
    const n = countMatches();
    if (!n) return;
    addChange({ type: 'replace', find: find.value, to: repl.value, images: inImages.checked });
    toast(`Replaced in ${n} place${n === 1 ? '' : 's'}`);
    find.value = '';
    repl.value = '';
    refresh();
    find.focus();
  };
  replBtn.addEventListener('click', doReplace);
  for (const el of [find, repl]) el.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); doReplace(); } });

  // Profiles: saved lists of changes.
  const profileSel = h('select', { 'aria-label': 'Saved profiles', class: 'clone-profile-sel' });
  const applyBtn = h('button', { type: 'button', class: 'btn sm', title: 'Add the profile\'s changes to this clone' }, 'Apply');
  const delBtn = h('button', { type: 'button', class: 'btn sm ghost icon', title: 'Delete this profile', 'aria-label': 'Delete this profile' }, icon('trash'));
  const saveBtn = h('button', { type: 'button', class: 'btn sm', title: 'Save the changes of this clone to apply them again next time' }, icon('save'), 'Save as profile');
  let profiles = [];
  async function loadProfiles() {
    try { profiles = await call('CloneProfiles'); } catch (err) { toast(err.message, true); profiles = []; }
    profileSel.replaceChildren(h('option', { value: '' }, profiles.length ? 'Saved profiles…' : 'No saved profiles'),
      ...profiles.map((p) => h('option', { value: p.id }, `${p.name} (${p.changes?.length || 0})`)));
    syncProfile();
  }
  const syncProfile = () => { applyBtn.disabled = delBtn.disabled = !profileSel.value; profileSel.disabled = !profiles.length; };
  profileSel.addEventListener('change', syncProfile);
  applyBtn.addEventListener('click', () => {
    const p = profiles.find((x) => x.id === profileSel.value);
    if (!p) return;
    const start = draft.changes.length;
    for (const c of p.changes || []) addChange({ ...c });
    ui.profile = p.name;
    const hits = derived().hits.slice(start);
    const none = hits.filter((n) => !n).length;
    toast(`Applied “${p.name}”: ${hits.length} change${hits.length === 1 ? '' : 's'}${none ? `, ${none} with nothing to change here` : ''}`);
    refresh();
  });
  delBtn.addEventListener('click', async () => {
    const p = profiles.find((x) => x.id === profileSel.value);
    if (!p || !await confirmDialog(`Delete the profile “${p.name}”?`, 'Delete profile')) return;
    try { await call('DeleteCloneProfile', p.id); await loadProfiles(); } catch (err) { toast(err.message, true); }
  });
  saveBtn.addEventListener('click', async () => {
    if (!draft.changes.length) { toast('Make some changes first', true); return; }
    const name = await promptText('Save as profile', {
      label: 'Name', value: ui.profile || `${serverName(ctx, draft.sourceId)} → ${serverName(ctx, draft.targetId)}`, okLabel: 'Save',
      hint: 'The changes are saved encrypted on this computer. A profile with the same name is replaced.',
    });
    if (!name) return;
    try {
      await call('SaveCloneProfile', { id: '', name, changes: draft.changes });
      ui.profile = name;
      toast(`Saved profile “${name}”`);
      await loadProfiles();
    } catch (err) { toast(err.message, true); }
  });

  // Views.
  draft.source.files = draft.source.files || [];
  const tabs = ['service', 'files', 'value', 'changes'].map((key) => h('button', { type: 'button', class: 'tab', 'data-view': key, onclick: () => { ui.view = key; refresh(); } }));
  const showSecrets = h('input', { type: 'checkbox', checked: ui.showSecrets });
  // Values are shown by default; hiding them (e.g. to share the screen) is remembered.
  showSecrets.addEventListener('change', () => { ui.showSecrets = showSecrets.checked; setPref('cloneShowSecrets', showSecrets.checked ? '1' : '0'); refresh(); });
  const viewFilter = h('input', { type: 'search', placeholder: 'Filter by container, variable or value…', 'aria-label': 'Filter', value: ui.filter || '' });
  viewFilter.addEventListener('input', () => { ui.filter = viewFilter.value; renderView(); });
  const area = h('div', { class: 'clone-area' });
  const summary = h('span', { class: 'muted' });

  content.append(resume,
    h('div', { class: 'clone-bar' },
      h('div', { class: 'clone-find' }, find, icon('arrowRight'), repl, h('label', { class: 'check', title: 'Also replace inside image names (e.g. a tag)' }, inImages, 'Images too'), replBtn, matches),
      h('div', { class: 'clone-profiles' }, profileSel, applyBtn, delBtn, saveBtn)),
    h('div', { class: 'toolbar clone-viewbar' }, h('div', { class: 'tabs', role: 'tablist' }, tabs), viewFilter, h('span', { class: 'spacer' }),
      h('label', { class: 'check', title: 'Untick to hide passwords, tokens and connection strings (e.g. while sharing the screen)' }, showSecrets, 'Show secrets')),
    area);

  function refresh() {
    view = derived();
    const changed = view.specs.reduce((n, s, i) => n + changedKeys(draft.source.specs[i], s).size, 0)
      + view.files.reduce((n, f, i) => n + changedFileKeys(draft.source.files[i], f).size, 0);
    const labels = { service: `By container (${view.specs.length})`, files: `Config files (${view.files.length})`, value: 'By value', changes: `Changes (${draft.changes.length})` };
    tabs.forEach((t) => { t.textContent = labels[t.dataset.view]; t.classList.toggle('active', ui.view === t.dataset.view); });
    viewFilter.hidden = ui.view === 'changes';
    summary.textContent = `${view.specs.length} containers · ${changed} variable${changed === 1 ? '' : 's'} changed`;
    countMatches();
    renderView();
  }

  // Keeps the focused field (by its data-fk) across a redraw, so Tab between fields still works.
  function renderView() {
    const fk = document.activeElement?.dataset?.fk;
    const top = m.body.scrollTop;
    area.replaceChildren({ value: valueView, changes: changesView, files: filesView }[ui.view]?.() || serviceView());
    m.body.scrollTop = top;
    if (fk) area.querySelector(`[data-fk="${CSS.escape(fk)}"]`)?.focus();
  }

  const q = () => (ui.filter || '').trim().toLowerCase();

  function valueInput(value, { secret, fk, onCommit, multiline = false }) {
    const masked = secret && !ui.showSecrets;
    const el = multiline && !masked
      ? h('textarea', { rows: Math.min(6, value.split('\n').length), spellcheck: 'false', class: 'mono', 'data-fk': fk })
      : h('input', { type: masked ? 'password' : 'text', spellcheck: 'false', class: 'mono', 'data-fk': fk, autocomplete: 'off' });
    el.value = value;
    if (multiline && masked) el.disabled = true;
    el.addEventListener('change', () => onCommit(el.value));
    return el;
  }

  function serviceView() {
    const filter = q();
    const cards = view.specs.map((s, i) => {
      const orig = draft.source.specs[i];
      const keyMatch = !filter || s.name.toLowerCase().includes(filter) || s.key.toLowerCase().includes(filter) || s.image.toLowerCase().includes(filter)
        || s.env.some((e) => e.key.toLowerCase().includes(filter) || (e.value.toLowerCase().includes(filter) && (ui.showSecrets || !isSecret(e.key))));
      if (!keyMatch) return null;
      const changed = changedKeys(orig, s);
      const removed = orig.env.filter((e) => !s.env.some((x) => x.key === e.key)).length
        + (JSON.stringify(s.ports) !== JSON.stringify(orig.ports || []) ? 1 : 0);
      const card = h('details', { class: 'clone-card', open: ui.open.has(s.key) || !!filter });
      card.addEventListener('toggle', () => { card.open ? ui.open.add(s.key) : ui.open.delete(s.key); });
      const renamed = s.name !== s.key;
      const head = h('summary', {},
        h('span', { class: `c-name ${renamed ? 'changed-text' : ''}` }, s.name),
        renamed ? h('span', { class: 'clone-was', title: 'Its name on the source server' }, `was ${s.key}`) : '',
        h('span', { class: `c-image ${s.image !== orig.image ? 'changed-text' : ''}` }, s.image),
        !s.running ? h('span', { class: 'clone-badge', title: 'Stopped on the source server: it is started on the target too' }, 'stopped on source') : '',
        h('span', { class: 'spacer' }),
        s.warnings?.length ? h('span', { class: 'clone-badge warn', title: s.warnings.join('\n') }, icon('alert'), s.warnings.length) : '',
        changed.size || removed ? h('span', { class: 'clone-badge changed' }, `${changed.size + removed} changed`) : '',
        s.ports.length ? h('span', { class: 'muted', title: s.ports.map(portText).join('\n') }, `ports ${s.ports.map((p) => p.hostPort || '*').join(', ')}`) : '',
        h('span', { class: 'muted' }, `${s.env.length} variable${s.env.length === 1 ? '' : 's'}`));

      const image = h('input', { type: 'text', spellcheck: 'false', class: `mono ${s.image !== orig.image ? 'changed' : ''}`, value: s.image, 'data-fk': `${s.key}|@image`, 'aria-label': 'Image' });
      image.addEventListener('change', () => { addChange({ type: 'image', service: s.key, value: image.value.trim() }); refresh(); });
      const nameIn = h('input', { type: 'text', spellcheck: 'false', class: `mono ${renamed ? 'changed' : ''}`, value: s.name, 'data-fk': `${s.key}|@name`, 'aria-label': 'Name on the target' });
      if (renamed) nameIn.title = `Was: ${s.key}`;
      nameIn.addEventListener('change', () => {
        const to = nameIn.value.trim();
        const err = nameError(to, s.key, view.specs);
        if (err) { toast(err, true); nameIn.value = s.name; return; }
        if (to === s.key) draft.changes = draft.changes.filter((c) => !(c.type === 'rename' && c.service === s.key));
        else addChange({ type: 'rename', service: s.key, to });
        refresh();
      });

      let textMode = false;
      const body = h('div', { class: 'clone-card-body' });
      const fromFile = keysFromFiles(view.files, s.key);
      const toFile = (path) => { ui.view = 'files'; ui.open.add(`file:${path}`); refresh(); };
      const rows = () => {
        const tbody = h('tbody', {}, ...s.env.map((e) => {
          const was = orig.env.find((x) => x.key === e.key);
          const state = !was ? 'added' : was.value !== e.value ? 'changed' : '';
          const secret = isSecret(e.key);
          const file = fromFile.get(e.key);
          const input = valueInput(e.value, {
            secret, fk: `${s.key}|${e.key}`, multiline: e.value.includes('\n'),
            onCommit: (v) => { addChange({ type: 'set', service: s.key, key: e.key, value: v }); refresh(); },
          });
          if (state) input.classList.add('changed');
          if (state === 'changed') input.title = secret && !ui.showSecrets ? 'Changed' : `Was: ${was.value}`;
          // A variable that comes from a .env file is edited there (the file is written to the target too).
          if (file) {
            input.readOnly = true;
            input.classList.add('from-file');
            input.title = `From ${file}: edit it in the .env file`;
          }
          return h('tr', { class: state },
            h('td', { class: 'mono clone-key' }, e.key,
              secret ? h('span', { class: 'clone-lock', title: 'Looks like a secret: hidden unless “Show secrets” is on' }, icon('lock')) : '',
              file ? h('span', { class: 'clone-from', title: `From ${file}` }, '.env') : ''),
            h('td', {}, input),
            h('td', { class: 'clone-row-act' }, file
              ? h('button', { type: 'button', class: 'btn sm ghost icon', title: `Edit in ${file}`, 'aria-label': `Edit in ${file}`, onclick: () => toFile(file) }, icon('file'))
              : h('button', {
                type: 'button', class: 'btn sm ghost icon', title: `Remove ${e.key}`, 'aria-label': `Remove ${e.key}`,
                onclick: () => { addChange({ type: 'unset', service: s.key, key: e.key }); refresh(); },
              }, icon('close'))));
        }));
        const newKey = h('input', { type: 'text', placeholder: 'NEW_VARIABLE', spellcheck: 'false', class: 'mono', 'aria-label': 'New variable name', 'data-fk': `${s.key}|@newkey` });
        const newVal = h('input', { type: 'text', placeholder: 'value', spellcheck: 'false', class: 'mono', 'aria-label': 'New variable value' });
        const add = () => {
          const k = newKey.value.trim();
          if (!k) { newKey.focus(); return; }
          if (/[\s=]/.test(k)) { toast('A variable name has no spaces or “=”', true); return; }
          addChange({ type: 'set', service: s.key, key: k, value: newVal.value });
          refresh();
        };
        newVal.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
        tbody.append(h('tr', { class: 'clone-add' }, h('td', {}, newKey), h('td', {}, newVal),
          h('td', { class: 'clone-row-act' }, h('button', { type: 'button', class: 'btn sm ghost icon', title: 'Add the variable', 'aria-label': 'Add the variable', onclick: add }, icon('plus')))));
        return h('table', { class: 'clone-env' }, h('colgroup', {}, h('col', { class: 'k' }), h('col'), h('col', { class: 'a' })), tbody);
      };
      const textBtn = h('button', { type: 'button', class: 'btn sm ghost' }, 'Edit as text');
      const draw = () => {
        if (!textMode) { body.replaceChildren(rows()); textBtn.textContent = 'Edit as text'; return; }
        textBtn.textContent = 'Back to the table';
        const ta = h('textarea', { rows: Math.min(24, s.env.length + 2), spellcheck: 'false', class: 'mono clone-text' });
        ta.value = s.env.map((e) => `${e.key}=${e.value}`).join('\n');
        const apply = h('button', { type: 'button', class: 'btn sm primary' }, 'Apply');
        apply.addEventListener('click', () => {
          const next = new Map();
          for (const line of ta.value.split('\n')) {
            if (!line.trim() || line.trimStart().startsWith('#')) continue;
            const i = line.indexOf('=');
            if (i <= 0) { toast(`Not KEY=VALUE: ${line.slice(0, 60)}`, true); return; }
            next.set(line.slice(0, i).trim(), line.slice(i + 1));
          }
          // Variables from a .env file stay as the file says (they are edited in the file).
          for (const e of s.env) if (!next.has(e.key) && !fromFile.has(e.key)) addChange({ type: 'unset', service: s.key, key: e.key });
          for (const [k, v] of next) if (!fromFile.has(k) && s.env.find((e) => e.key === k)?.value !== v) addChange({ type: 'set', service: s.key, key: k, value: v });
          textMode = false;
          refresh();
        });
        body.replaceChildren(ta, h('div', { class: 'clone-text-foot' }, h('span', { class: 'hint' }, 'One KEY=VALUE per line; lines starting with # are ignored.'), h('span', { class: 'spacer' }), apply));
      };
      const multiline = s.env.some((e) => e.value.includes('\n'));
      textBtn.disabled = multiline;
      if (multiline) textBtn.title = 'A value spans several lines: edit it in the table';
      textBtn.addEventListener('click', () => { textMode = !textMode; draw(); });
      draw();

      const opts = optionsText(s);
      const mounted = view.files.filter((f) => f.containers.includes(s.key));
      card.append(head, h('div', { class: 'clone-card-inner' },
        h('div', { class: 'clone-image-row' }, h('label', { class: 'clone-name-field' }, 'Name on the target', nameIn), h('label', {}, 'Image', image), h('span', { class: 'spacer' }), textBtn),
        body,
        ...mounted.map((f) => h('div', { class: 'clone-filelink' }, icon('file'),
          h('span', {}, SYNCED.has(f.usage) ? 'Its variables come from ' : 'The app reads ', h('b', { class: 'mono' }, f.path), ` (${USAGE_TEXT[f.usage] || 'mounted'})`),
          h('button', { type: 'button', class: 'btn sm ghost', onclick: () => { ui.view = 'files'; ui.open.add(`file:${f.path}`); refresh(); } }, 'Edit the .env file', icon('arrowRight')))),
        portsEditor(s, orig),
        s.warnings?.length ? h('ul', { class: 'clone-warnings' }, ...s.warnings.map((w) => h('li', {}, icon('alert'), w))) : '',
        opts ? h('details', { class: 'clone-opts' }, h('summary', {}, 'Other settings copied as they are'), h('pre', { class: 'mono' }, opts)) : ''));
      return card;
    }).filter(Boolean);
    return h('div', { class: 'clone-cards-wrap' }, renameTool(),
      cards.length ? h('div', { class: 'clone-cards' }, cards) : h('div', { class: 'empty' }, 'Nothing matches the filter.'));
  }

  // Rename every container at once: a prefix, a suffix, or text replaced in the names (e.g. -prod → -staging).
  function renameTool() {
    const field = (ph, label) => h('input', { type: 'text', spellcheck: 'false', class: 'mono', placeholder: ph, 'aria-label': label });
    const prefix = field('prefix', 'Prefix');
    const find = field('find', 'Find in names');
    const to = field('replace with', 'Replace with');
    const suffix = field('suffix', 'Suffix');
    const keep = h('input', { type: 'checkbox', checked: draft.options.keepAliases !== false });
    keep.addEventListener('change', () => { draft.options.keepAliases = keep.checked; refresh(); });
    const preview = h('span', { class: 'muted clone-rename-preview' });
    const next = (name) => `${prefix.value.trim()}${find.value ? name.split(find.value).join(to.value.trim()) : name}${suffix.value.trim()}`;
    const sync = () => {
      const first = view.specs[0];
      preview.textContent = first && (prefix.value || suffix.value || find.value) ? `${first.name} → ${next(first.name)}` : '';
    };
    for (const el of [prefix, find, to, suffix]) el.addEventListener('input', sync);
    const apply = h('button', { type: 'button', class: 'btn sm' }, 'Rename all');
    apply.addEventListener('click', () => {
      if (!prefix.value && !suffix.value && !find.value) { prefix.focus(); return; }
      const names = view.specs.map((x) => ({ key: x.key, to: next(x.name) }));
      const all = names.map((x) => x.to);
      const bad = names.map((x) => ({ ...x, err: nameError(x.to, x.key, view.specs, all) })).find((x) => x.err);
      if (bad) { toast(`${bad.key}: ${bad.err}`, true); return; }
      let n = 0;
      for (const x of names) {
        const cur = view.specs.find((y) => y.key === x.key).name;
        if (x.to === cur) continue;
        if (x.to === x.key) draft.changes = draft.changes.filter((c) => !(c.type === 'rename' && c.service === x.key));
        else addChange({ type: 'rename', service: x.key, to: x.to });
        n++;
      }
      toast(n ? `Renamed ${n} container${n === 1 ? '' : 's'}` : 'No name changes');
      refresh();
    });
    return h('div', { class: 'clone-rename' },
      h('span', { class: 'clone-rename-title' }, 'Names on the target'),
      prefix, find, icon('arrowRight'), to, suffix, apply, preview,
      h('label', { class: 'check', title: 'Other containers that call a renamed one by its old name (http://old-name:8080) keep working on user-defined networks' }, keep, 'Keep old names as network aliases'));
  }

  // Published ports: host IP (empty: every address), host port (empty: Docker picks one) and container port.
  function portsEditor(s, orig) {
    const cur = () => s.ports.map((p) => ({ ...p }));
    const commit = (ports) => { addChange({ type: 'ports', service: s.key, ports }); refresh(); };
    const was = orig.ports || [];
    const field = (i, key, placeholder, label) => {
      const p = s.ports[i];
      const before = was[i];
      const el = h('input', {
        type: 'text', spellcheck: 'false', placeholder, 'aria-label': label, 'data-fk': `${s.key}|@port${i}${key}`,
        class: `mono ${!before || (before[key] || '') !== (p[key] || '') ? 'changed' : ''}`,
      });
      el.value = p[key] || '';
      if (before && (before[key] || '') !== (p[key] || '')) el.title = `Was: ${before[key] || '(empty)'}`;
      el.addEventListener('change', () => { const next = cur(); next[i][key] = el.value.trim(); commit(next); });
      return el;
    };
    const rows = s.ports.map((p, i) => h('tr', {},
      h('td', {}, field(i, 'hostIp', 'all addresses', 'Host IP')),
      h('td', {}, field(i, 'hostPort', 'random', 'Host port')),
      h('td', { class: 'clone-port-arrow' }, '→'),
      h('td', {}, field(i, 'port', '8080/tcp', 'Container port')),
      h('td', { class: 'clone-row-act' }, h('button', {
        type: 'button', class: 'btn sm ghost icon', title: 'Stop publishing this port', 'aria-label': 'Remove port',
        onclick: () => commit(cur().filter((_, j) => j !== i)),
      }, icon('close')))));
    const newHost = h('input', { type: 'text', placeholder: 'host port', spellcheck: 'false', class: 'mono', 'aria-label': 'New host port' });
    const newPort = h('input', { type: 'text', placeholder: 'container port, e.g. 8080/tcp', spellcheck: 'false', class: 'mono', 'aria-label': 'New container port', 'data-fk': `${s.key}|@newport` });
    const add = () => {
      let port = newPort.value.trim();
      if (!port) { newPort.focus(); return; }
      if (!port.includes('/')) port += '/tcp';
      commit([...cur(), { hostIp: '', hostPort: newHost.value.trim(), port }]);
    };
    newPort.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
    rows.push(h('tr', { class: 'clone-add' }, h('td', {}), h('td', {}, newHost), h('td', { class: 'clone-port-arrow' }, '→'), h('td', {}, newPort),
      h('td', { class: 'clone-row-act' }, h('button', { type: 'button', class: 'btn sm ghost icon', title: 'Publish this port', 'aria-label': 'Add port', onclick: add }, icon('plus')))));
    return h('div', { class: 'clone-ports' },
      h('div', { class: 'clone-sub' }, 'Published ports', h('span', { class: 'hint' }, 'host IP : host port → container port')),
      h('table', { class: 'clone-port-table' }, h('colgroup', {}, h('col', { class: 'ip' }), h('col', { class: 'hp' }), h('col', { class: 'ar' }), h('col', { class: 'cp' }), h('col', { class: 'a' })), h('tbody', {}, rows)));
  }

  // The config files (.env, .json): the apps read them, so they are written to the same path on the target.
  function filesView() {
    const filter = q();
    const cards = view.files.map((f, i) => {
      const orig = draft.source.files[i];
      const json = f.format === 'json';
      const parseError = json ? jsonLeaves(f.text).error : '';
      const vars = fileEntries(f);
      if (filter && !f.path.toLowerCase().includes(filter) && !f.containers.some((c) => c.toLowerCase().includes(filter))
        && !vars.some((l) => l.key.toLowerCase().includes(filter) || (l.value.toLowerCase().includes(filter) && (ui.showSecrets || !isSecret(l.key))))) return null;
      const before = new Map(fileEntries(orig).map((l) => [l.key, l.value]));
      const changed = changedFileKeys(orig, f);
      const key = `file:${f.path}`;
      const card = h('details', { class: 'clone-card', open: ui.open.has(key) || !!filter || view.files.length === 1 });
      card.addEventListener('toggle', () => { card.open ? ui.open.add(key) : ui.open.delete(key); });
      const write = h('input', { type: 'checkbox', checked: writesFile(f) });
      write.addEventListener('change', () => { (ui.writes || (ui.writes = new Map())).set(f.path, write.checked); });
      const head = h('summary', {},
        h('span', { class: `clone-fmt ${json ? 'json' : 'env'}` }, json ? 'JSON' : '.env'),
        h('span', { class: 'c-name mono' }, f.path),
        h('span', { class: 'muted' }, `mode ${f.mode}`),
        parseError ? h('span', { class: 'clone-badge warn', title: parseError }, icon('alert'), 'not valid JSON') : '',
        h('span', { class: 'spacer' }),
        changed.size ? h('span', { class: 'clone-badge changed' }, `${changed.size} changed`) : '',
        h('span', { class: 'muted' }, `${vars.length} ${json ? 'value' : 'variable'}${vars.length === 1 ? '' : 's'}`));

      // A JSON file that does not parse is edited as text (its values cannot be told apart).
      let textMode = !!ui.fileText?.has(f.path) || !!parseError;
      const body = h('div', { class: 'clone-card-body' });
      const textBtn = h('button', { type: 'button', class: 'btn sm ghost' });
      const draw = () => {
        textBtn.textContent = textMode ? 'Back to the table' : 'Edit as text';
        if (textMode) {
          const ta = h('textarea', { rows: Math.min(28, f.content.split('\n').length + 2), spellcheck: 'false', class: 'mono clone-text' });
          ta.value = f.content;
          const apply = h('button', { type: 'button', class: 'btn sm primary' }, 'Apply');
          apply.addEventListener('click', () => {
            if (ta.value !== f.content) addChange({ type: 'filetext', path: f.path, text: ta.value });
            textMode = false;
            refresh();
          });
          body.replaceChildren(
            parseError ? h('div', { class: 'card-error' }, `${parseError}: fix it here, or Find and replace still changes it as text.`) : '',
            ta, h('div', { class: 'clone-text-foot' }, h('span', { class: 'hint' }, 'The whole file, comments included, as it will be written on the target.'), h('span', { class: 'spacer' }), apply));
          return;
        }
        if (json) { body.replaceChildren(jsonTable()); return; }
        const tbody = h('tbody', {}, ...vars.map((l) => {
          const was = before.get(l.key);
          const state = was === undefined ? 'added' : was !== l.value ? 'changed' : '';
          const secret = isSecret(l.key);
          const input = valueInput(l.value, {
            secret, fk: `${key}|${l.key}`,
            onCommit: (v) => { addChange({ type: 'fileset', path: f.path, key: l.key, value: v }); refresh(); },
          });
          if (state) input.classList.add('changed');
          if (state === 'changed') input.title = secret && !ui.showSecrets ? 'Changed' : `Was: ${was}`;
          return h('tr', { class: state },
            h('td', { class: 'mono clone-key' }, l.key, secret ? h('span', { class: 'clone-lock', title: 'Looks like a secret: hidden unless “Show secrets” is on' }, icon('lock')) : ''),
            h('td', {}, input),
            h('td', { class: 'clone-row-act' }, h('button', {
              type: 'button', class: 'btn sm ghost icon', title: `Remove ${l.key}`, 'aria-label': `Remove ${l.key}`,
              onclick: () => { addChange({ type: 'fileunset', path: f.path, key: l.key }); refresh(); },
            }, icon('close'))));
        }));
        const newKey = h('input', { type: 'text', placeholder: 'NEW_VARIABLE', spellcheck: 'false', class: 'mono', 'aria-label': 'New variable name', 'data-fk': `${key}|@newkey` });
        const newVal = h('input', { type: 'text', placeholder: 'value', spellcheck: 'false', class: 'mono', 'aria-label': 'New variable value' });
        const add = () => {
          const k = newKey.value.trim();
          if (!k) { newKey.focus(); return; }
          if (!/^[A-Za-z_][A-Za-z0-9_.-]*$/.test(k)) { toast('A variable name has letters, digits and _ . - only', true); return; }
          addChange({ type: 'fileset', path: f.path, key: k, value: newVal.value });
          refresh();
        };
        newVal.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
        tbody.append(h('tr', { class: 'clone-add' }, h('td', {}, newKey), h('td', {}, newVal),
          h('td', { class: 'clone-row-act' }, h('button', { type: 'button', class: 'btn sm ghost icon', title: 'Add the variable', 'aria-label': 'Add the variable', onclick: add }, icon('plus')))));
        body.replaceChildren(h('table', { class: 'clone-env' }, h('colgroup', {}, h('col', { class: 'k' }), h('col'), h('col', { class: 'a' })), tbody));
      };
      // JSON: one row per value (key path → value); keys are added or removed as text.
      const jsonTable = () => {
        const tbody = h('tbody', {}, ...vars.map((l) => {
          const was = before.get(l.key);
          const state = was === undefined ? 'added' : was !== l.value ? 'changed' : '';
          const secret = isSecret(l.key);
          const input = valueInput(l.value, {
            secret, fk: `${key}|${l.key}`,
            onCommit: (v) => { addChange({ type: 'jsonset', path: f.path, key: l.key, value: v }); refresh(); },
          });
          if (l.type !== 'string') input.classList.add('clone-literal');
          if (state) input.classList.add('changed');
          if (state === 'changed') input.title = secret && !ui.showSecrets ? 'Changed' : `Was: ${was}`;
          return h('tr', { class: state },
            h('td', { class: 'mono clone-key', title: l.key }, l.key, secret ? h('span', { class: 'clone-lock', title: 'Looks like a secret: hidden unless “Show secrets” is on' }, icon('lock')) : ''),
            h('td', {}, input),
            h('td', { class: 'clone-row-act muted' }, l.type === 'string' ? '' : l.type));
        }));
        return h('div', {},
          vars.length ? h('table', { class: 'clone-env clone-json' }, h('colgroup', {}, h('col', { class: 'k' }), h('col'), h('col', { class: 'a' })), tbody) : h('div', { class: 'empty' }, 'No values in this file.'),
          h('p', { class: 'hint' }, 'Keys are shown like .NET configuration (Section:Key, Array:0). To add or remove keys, edit as text. Comments and layout are kept.'));
      };
      textBtn.addEventListener('click', () => {
        textMode = !textMode;
        ui.fileText = ui.fileText || new Set();
        textMode ? ui.fileText.add(f.path) : ui.fileText.delete(f.path);
        draw();
      });
      draw();
      card.append(head, h('div', { class: 'clone-card-inner' },
        h('div', { class: 'clone-image-row' },
          h('span', { class: 'muted' }, `${USAGE_TEXT[f.usage] || 'mounted'} · ${f.containers.length ? `used by ${f.containers.join(', ')}` : 'no copied container has these variables'}`),
          h('span', { class: 'spacer' }),
          h('label', { class: 'check', title: f.usage === 'env-file'
            ? 'Off by default: Docker already sets these variables on the container, and a deploy pipeline writes this file again'
            : 'Write this file on the target before the containers are created' }, write, f.usage === 'env-file' ? 'Write to the target (optional)' : 'Write to the target'),
          textBtn,
          f.usage === 'manual' ? h('button', {
            type: 'button', class: 'btn sm ghost icon', title: 'Remove this file from the clone', 'aria-label': 'Remove this file',
            onclick: () => {
              draft.source.files = draft.source.files.filter((x) => x.path !== f.path);
              draft.changes = draft.changes.filter((c) => c.path !== f.path);
              refresh();
            },
          }, icon('close')) : ''),
        body));
      return card;
    }).filter(Boolean);

    // Add a file by its path on the source server (one the search did not find).
    const path = h('input', { type: 'text', spellcheck: 'false', class: 'mono', placeholder: '/home/deployer/apps/orders-api/appsettings.json  or  …/.env', 'aria-label': 'Path of a config file on the source server', 'data-fk': '@addfile' });
    const addBtn = h('button', { type: 'button', class: 'btn sm primary' }, icon('plus'), 'Add file');
    const addFile = async () => {
      const p = path.value.trim();
      if (!p) { path.focus(); return; }
      if (draft.source.files.some((x) => x.path === p)) { toast('This file is already in the list', true); return; }
      addBtn.disabled = true;
      try {
        const f = await call('CloneReadFile', draft.sourceId, p, draft.source.specs);
        draft.source.files.push(f);
        ui.open.add(`file:${f.path}`);
        toast(f.containers.length ? `Added ${f.path}: the variables of ${f.containers.join(', ')} now follow it` : `Added ${f.path}: no copied container has exactly these variables, it is only written to the target`);
        refresh();
      } catch (err) {
        toast(err.message, true);
        addBtn.disabled = false;
      }
    };
    addBtn.addEventListener('click', addFile);
    path.addEventListener('keydown', (e) => { if (e.key === 'Enter') addFile(); });
    const adder = h('div', { class: 'clone-addfile' },
      h('label', {}, 'Add a config file (.env or .json) from the source server', path), addBtn);

    const intro = view.files.length
      ? h('p', { class: 'hint' }, 'The config files of these containers: .env and .json files mounted into them (appsettings.json…), .env files in their mounted folders, and .env files given to docker run --env-file (found by matching their variables; editing one also changes the variables of its containers). They are written to the same path on the target (an existing file there is kept as a .bak copy). Find and replace and By value change them too.')
      : h('div', { class: 'clone-nofiles' }, icon('info'), h('div', {},
        h('b', {}, 'No config file found for these containers.'),
        h('p', { class: 'hint' }, 'The app looks for .env files mounted into the containers, inside their mounted folders, and in /home, /root, /opt, /srv, /data, /app, /apps, /deploy, /docker and /var/www (a file whose variables all match a container’s is the one given to docker run --env-file). Their variables are still in the By container tab. If the file is somewhere else, add it by its path below.')));
    return h('div', { class: 'clone-files' }, intro, adder,
      cards.length ? h('div', { class: 'clone-cards' }, cards) : view.files.length ? h('div', { class: 'empty' }, 'Nothing matches the filter.') : '');
  }

  function valueView() {
    const filter = q();
    const map = new Map(); // value -> [{ service, key, changed }]
    view.specs.forEach((s, i) => {
      for (const e of s.env) {
        if (e.value === '') continue;
        const was = draft.source.specs[i].env.find((x) => x.key === e.key);
        if (!map.has(e.value)) map.set(e.value, []);
        map.get(e.value).push({ service: s.name, key: e.key, changed: !was || was.value !== e.value });
      }
    });
    view.files.forEach((f, i) => {
      const before = new Map(fileEntries(draft.source.files[i]).map((l) => [l.key, l.value]));
      for (const l of fileEntries(f).filter((e) => e.type === 'string')) {
        if (l.value === '') continue;
        if (!map.has(l.value)) map.set(l.value, []);
        map.get(l.value).push({ service: baseName(f.path), file: f.path, key: l.key, changed: before.get(l.key) !== l.value });
      }
    });
    const rows = [...map.entries()]
      .filter(([v, uses]) => !filter || uses.some((u) => u.key.toLowerCase().includes(filter) || u.service.toLowerCase().includes(filter))
        || (v.toLowerCase().includes(filter) && (ui.showSecrets || !uses.every((u) => isSecret(u.key)))))
      .sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
    if (!rows.length) return h('div', { class: 'empty' }, 'Nothing matches the filter.');
    return h('div', {},
      h('p', { class: 'hint' }, 'Each distinct value once, most used first: change it here and every container using it gets the new value.'),
      h('table', { class: 'clone-values' },
        h('colgroup', {}, h('col', { class: 'v' }), h('col'), h('col', { class: 'n' })),
        h('thead', {}, h('tr', {}, h('th', {}, 'Value'), h('th', {}, 'Used by'), h('th', { class: 'num' }, 'Uses'))),
        h('tbody', {}, ...rows.map(([v, uses], idx) => {
          const input = valueInput(v, {
            secret: uses.every((u) => isSecret(u.key)), fk: `value|${idx}`, multiline: v.includes('\n'),
            onCommit: (next) => { if (next !== v) { addChange({ type: 'value', find: v, to: next }); refresh(); } },
          });
          if (uses.some((u) => u.changed)) input.classList.add('changed');
          const shown = uses.slice(0, 4);
          return h('tr', {},
            h('td', {}, input),
            h('td', { class: 'clone-uses', title: uses.map((u) => `${u.file || u.service} · ${u.key}`).join('\n') },
              ...shown.map((u) => h('span', { class: `clone-use${u.file ? ' file' : ''}` }, h('b', {}, u.key), ` ${u.service}`)),
              uses.length > shown.length ? h('span', { class: 'clone-use more' }, `+${uses.length - shown.length} more`) : ''),
            h('td', { class: 'num' }, uses.length));
        }))));
  }

  function changesView() {
    if (!draft.changes.length) return h('div', { class: 'empty' }, 'No changes yet. Use Find and replace, the By value list, or edit a container’s variables.');
    const hits = view.hits;
    return h('div', {},
      h('div', { class: 'clone-changes-head' }, h('span', { class: 'hint' }, 'Applied in this order to the settings read from the source. Remove one to undo it.'),
        h('span', { class: 'spacer' }),
        h('button', {
          type: 'button', class: 'btn sm ghost',
          onclick: async () => { if (await confirmDialog('Remove every change?', 'Remove changes')) { draft.changes = []; refresh(); } },
        }, icon('trash'), 'Remove all')),
      h('ol', { class: 'clone-changes' }, ...draft.changes.map((c, i) => h('li', { class: hits[i] ? '' : 'nohit' },
        h('span', { class: 'clone-change-text' }, describeChange(c, ui.showSecrets)),
        h('span', { class: 'muted' }, hits[i] ? `${hits[i]} place${hits[i] === 1 ? '' : 's'}` : 'nothing matches'),
        h('button', {
          type: 'button', class: 'btn sm ghost icon', title: 'Undo this change', 'aria-label': 'Undo this change',
          onclick: () => { draft.changes.splice(i, 1); refresh(); },
        }, icon('close'))))));
  }

  const next = h('button', { type: 'button', class: 'btn primary' }, 'Check target', icon('arrowRight'));
  next.addEventListener('click', () => {
    const d = derived();
    const dup = d.specs.find((s) => nameError(s.name, s.key, d.specs));
    if (dup) { ui.view = 'service'; refresh(); toast(`${dup.key}: ${nameError(dup.name, dup.key, d.specs)}`, true); return; }
    const bad = d.specs.find((s) => !s.image.trim() || s.image.startsWith('sha256:'));
    if (bad) { toast(`${bad.name}: enter the image name`, true); return; }
    for (const s of d.specs) {
      const p = s.ports.find((x) => !PORT_RE.test(x.port) || (x.hostPort && !HOST_PORT_RE.test(x.hostPort)));
      if (p) {
        ui.view = 'service';
        ui.open.add(s.key);
        refresh();
        toast(`${s.name}: invalid port ${portText(p)} (host port like 8080, container port like 8080/tcp)`, true);
        return;
      }
    }
    go(2);
  });
  const back = h('button', { type: 'button', class: 'btn back', title: 'Choose other servers or containers (your edits are kept)', onclick: () => go(0) }, icon('arrowLeft'), 'Back');
  m.foot.append(back, summary, h('span', { class: 'spacer' }), next);
  loadProfiles();
  refresh();
}

// The registry of an image name ("" for Docker Hub): gitlab.example.com:5050/group/app:1 → gitlab.example.com:5050.
function registryOf(name) {
  const first = name.split('/')[0];
  return name.includes('/') && (first.includes('.') || first.includes(':') || first === 'localhost') ? first : '';
}

// Whether a config file is written on the target: mounted ones are (the apps read them); ones given to
// docker run --env-file are not (Docker already set their variables on the container, and a deploy pipeline
// writes them again), unless the user ticks it.
function writesFile(f) {
  return draft.ui.writes?.get(f.path) ?? f.usage !== 'env-file';
}

// Why a name cannot be used on the target ("" when it can). others: the names of the other copies.
function nameError(name, key, specs, others = null) {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(name)) return `“${name}” is not a valid container name (letters, digits, _ . -)`;
  const taken = others ? others.filter((x, i) => specs[i].key !== key) : specs.filter((x) => x.key !== key).map((x) => x.name);
  if (taken.includes(name)) return `another copied container is already named “${name}”`;
  return '';
}

const PORT_RE = /^\d{1,5}(-\d{1,5})?(\/(tcp|udp|sctp))?$/;
const HOST_PORT_RE = /^\d{1,5}(-\d{1,5})?$/;

// Variables of a .env file whose value differs from the source (or that were added).
function changedFileKeys(orig, f) {
  const before = new Map(fileEntries(orig).map((l) => [l.key, l.value]));
  return new Set(fileEntries(f).filter((l) => before.get(l.key) !== l.value).map((l) => l.key));
}

// Variables whose value differs from the source (or that were added).
function changedKeys(orig, s) {
  const out = new Set();
  for (const e of s.env) {
    const was = orig.env.find((x) => x.key === e.key);
    if (!was || was.value !== e.value) out.add(e.key);
  }
  return out;
}

const shellWord = (a) => (a === '' ? "''" : /[^\w@%+=:,./-]/.test(a) ? `'${a.replace(/'/g, `'\\''`)}'` : a);

// The copied options as docker create would get them, for reading.
function optionsText(s) {
  const lines = [];
  for (let i = 0; i < s.flags.length; i++) {
    const f = s.flags[i];
    const arg = s.flags[i + 1];
    if (arg !== undefined && !arg.startsWith('--')) { lines.push(`${f} ${shellWord(arg)}`); i++; } else lines.push(f);
  }
  if (s.networks?.length) {
    lines.push(...s.networks.map((n, i) => `network ${n.name}${n.aliases?.length ? ` (aliases ${n.aliases.join(', ')})` : ''}${i === 0 && !s.bridge ? ' — created on it' : ''}`));
  }
  if (s.args?.length) lines.push(`command: ${s.args.map(shellWord).join(' ')}`);
  return lines.join('\n');
}

// ---------------------------------------------------------------- step 3: check and start

function reviewStep(ctx) {
  const { m, content, go, runTask } = ctx;
  const { specs, files: allFiles } = derived();
  const files = allFiles.filter(writesFile)
    .map((f) => ({ path: f.path, content: f.content, mode: f.mode, uid: f.uid, gid: f.gid, containers: f.containers }));
  const target = serverName(ctx, draft.targetId);
  const source = serverName(ctx, draft.sourceId);
  const opts = draft.options;
  const checks = h('div', { class: 'clone-checks' }, h('div', { class: 'clone-loading' }, h('span', { class: 'spinner' }), `Checking ${target}…`));
  const start = h('button', { type: 'button', class: 'btn primary', disabled: true }, icon('play'), `Create and start ${specs.length}`);
  const back = h('button', { type: 'button', class: 'btn back', title: 'Back to the variables', onclick: () => go(1) }, icon('arrowLeft'), 'Back');
  opts.images = opts.images || 'auto';
  // How the target gets the images.
  const modes = [
    ['auto', 'Pull, else copy', 'Pull from the registry; when that fails (no docker login), copy from the source'],
    ['copy', 'Copy from source', 'Copy the exact build the source runs (no registry or login needed)'],
    ['pull', 'Pull only', 'Pull from the registry (the target must be logged in to it)'],
    ['local', 'Use the target’s', 'Use the images the target already has'],
  ];
  const modeBtns = modes.map(([key, label, hint]) => {
    const b = h('button', { type: 'button', class: 'clone-mode', role: 'radio', title: hint, onclick: () => { opts.images = key; syncModes(); if (pf) render(); } },
      h('span', { class: 'clone-mode-label' }, label), h('span', { class: 'clone-mode-hint' }, hint));
    b.dataset.mode = key;
    return b;
  });
  const syncModes = () => modeBtns.forEach((b) => { const on = b.dataset.mode === opts.images; b.classList.toggle('active', on); b.setAttribute('aria-checked', String(on)); });
  syncModes();
  const preview = h('details', { class: 'clone-preview' }, h('summary', {}, 'Show the script'), h('pre', { class: 'mono' }, 'Loading…'));
  let pf = null;

  const plan = () => ({ source, sourceId: draft.sourceId, specs, files, networks: draft.source.networks, images: opts.images, replace: opts.replace });
  preview.addEventListener('toggle', async () => {
    if (!preview.open) return;
    try { preview.lastChild.textContent = await call('ClonePreview', draft.targetId, plan()); } catch (err) { preview.lastChild.textContent = err.message; }
  });

  content.append(
    h('p', { class: 'clone-lead' }, `Create and start ${specs.length} container${specs.length === 1 ? '' : 's'} from `, h('b', {}, source), ' on ', h('b', {}, target), '.'),
    checks,
    h('div', { class: 'clone-options' }, h('div', { class: 'clone-sub' }, 'Images', h('span', { class: 'hint' }, 'how the target gets them')),
      h('div', { class: 'clone-modes', role: 'radiogroup', 'aria-label': 'How the target gets the images' }, modeBtns)),
    preview);

  const item = (tone, title, ...body) => h('div', { class: `clone-check ${tone}` },
    h('span', { class: 'clone-check-icon' }, icon(tone === 'good' ? 'check' : tone === 'info' ? 'info' : 'alert')),
    h('div', {}, h('div', { class: 'clone-check-title' }, title), ...body));
  const list = (items) => h('ul', { class: 'clone-check-list' }, ...items.map((x) => h('li', {}, x)));

  // What happens to the images with the chosen mode.
  function imagesItem() {
    const imgs = pf.images || [];
    const same = imgs.filter((x) => x.same);
    const other = imgs.filter((x) => x.onTarget && !x.same);
    const missing = imgs.filter((x) => !x.onTarget);
    const n = imgs.length;
    const names = (l) => list(l.map((x) => x.name));
    const what = `${n} image${n === 1 ? '' : 's'}`;
    switch (opts.images) {
      case 'local':
        return missing.length
          ? item('bad', `${missing.length} of ${what} missing on ${target}`, names(missing), h('p', { class: 'hint' }, 'Their containers will fail: choose “Copy from source” or “Pull, else copy”.'))
          : item('good', `All ${what} are on ${target}`, other.length ? h('p', { class: 'hint' }, `${other.length} of them are a different build than the source runs (same name).`) : '');
      case 'pull': {
        const regs = [...new Set(imgs.map((x) => registryOf(x.name)))];
        return item('info', `${what} pulled from ${regs.map((r) => r || 'Docker Hub').join(', ')}`,
          h('p', { class: 'hint' }, `${target} must be logged in to private registries. If it is not, choose “Pull, else copy”, or log in there (a terminal opens on its tab) and click Check again.`),
          h('div', { class: 'clone-login' }, ...regs.map((r) => h('button', {
            type: 'button', class: 'btn sm',
            onclick: () => runTask(draft.targetId, { kind: 'dockerlogin', arg: r, title: `docker login ${r || 'Docker Hub'}`, once: true }),
          }, icon('lock'), `docker login ${r || 'Docker Hub'}`))));
      }
      case 'copy':
        if (same.length === n) return item('good', `${n === 1 ? 'The image is' : `All ${what} are`} already on ${target}: nothing to copy`, h('p', { class: 'hint' }, `The exact build${n === 1 ? '' : 's'} ${source} runs.`));
        return item('info', `${n - same.length} of ${what} copied from ${source}`,
          h('p', { class: 'hint' }, `The exact builds ${source} runs, through this computer (compressed); the ones ${target} already has are skipped.${same.length && same.length < n ? ` Already there: ${same.length}.` : ''}`));
      default:
        if (same.length === n) return item('good', `${n === 1 ? 'The image is' : `All ${what} are`} already on ${target}: nothing to pull or copy`, h('p', { class: 'hint' }, `The exact build${n === 1 ? '' : 's'} ${source} runs.`));
        return item('info', `${n - same.length} of ${what} pulled from the registry, or copied from ${source} when the pull fails${same.length ? ` · ${same.length} already on ${target}` : ''}`,
          h('p', { class: 'hint' }, 'No registry login is needed: an image the target cannot pull is copied from the source.'));
    }
  }

  function render() {
    const rows = [];
    rows.push(item('good', `Docker ${pf.docker} is running on ${target}`));
    if (draft.source.arch && pf.arch && draft.source.arch !== pf.arch) {
      rows.push(item('warn', `Different CPU: ${source} is ${draft.source.arch}, ${target} is ${pf.arch}`,
        h('p', { class: 'hint' }, 'An image built only for the source CPU will not start on the target.')));
    }
    rows.push(imagesItem());
    if (pf.existing.length) {
      const radio = (value, label) => {
        const r = h('input', { type: 'radio', name: 'clone-existing', checked: opts.replace === value });
        r.addEventListener('change', () => { opts.replace = value; render(); });
        return h('label', { class: 'check' }, r, label);
      };
      rows.push(item(opts.replace ? 'warn' : 'info', `${pf.existing.length} already exist${pf.existing.length === 1 ? 's' : ''} on ${target}`,
        list(pf.existing.map((e) => `${e.name} (${e.state})`)),
        h('div', { class: 'clone-radios' }, radio(false, 'Skip them (leave them as they are)'), radio(true, 'Replace them (stop and remove, then create the copy)'))));
    }
    // A port held by a container that is replaced is freed first.
    const ports = pf.ports.filter((p) => !(opts.replace && p.byName && pf.existing.some((e) => e.name === p.byName) && specs.some((s) => s.name === p.byName)));
    if (ports.length) {
      rows.push(item('bad', `${ports.length} port${ports.length === 1 ? ' is' : 's are'} already in use on ${target}`,
        list(ports.map((p) => `${p.container}: ${p.port} — used by ${p.by}`)),
        h('p', { class: 'hint' }, 'Those containers will fail to start: free the port first, or stop what uses it.')));
    }
    if (files.length) {
      const replaced = files.filter((f) => pf.files?.includes(f.path)).length;
      rows.push(item(replaced ? 'warn' : 'info', `${files.length} config file${files.length === 1 ? '' : 's'} written before the containers are created${replaced ? ` · ${replaced} replace${replaced === 1 ? 's' : ''} a file already on ${target}` : ''}`,
        list(files.map((f) => `${f.path}  ${pf.files?.includes(f.path) ? '— replaces the existing file (kept as .bak)' : '— new'}`))));
    }
    const notWritten = allFiles.filter((f) => !writesFile(f));
    if (notWritten.length) {
      rows.push(item('info', `${notWritten.length} config file${notWritten.length === 1 ? '' : 's'} not written to ${target}`,
        list(notWritten.map((f) => `${f.path}${f.usage === 'env-file' ? '  — used with --env-file: the variables are already set on the container' : ''}`)),
        h('p', { class: 'hint' }, 'Tick “Write to the target” on a file in the Config files tab to write it anyway.')));
    }
    // The .env files the script writes are not missing.
    const paths = pf.paths.filter((p) => !files.some((f) => f.path === p.source));
    if (paths.length) {
      const missingFiles = paths.filter((p) => p.kind === 'file');
      rows.push(item(missingFiles.length ? 'bad' : 'warn', `${paths.length} mounted path${paths.length === 1 ? ' does' : 's do'} not exist on ${target}`,
        list(paths.map((p) => `${p.source} → ${p.container}${p.kind === 'file' ? ' (a file on the source)' : p.kind === 'dir' ? ' (a folder on the source)' : ''}`)),
        h('p', { class: 'hint' }, missingFiles.length
          ? 'Docker creates an empty folder for a missing path, so a config file mounted this way breaks the container: copy the files first (Files tab).'
          : 'Docker creates them as empty folders.')));
    }
    if (pf.networks.length) rows.push(item('info', `Networks to create: ${pf.networks.join(', ')}`));
    if (pf.volumes.length) {
      rows.push(item('info', `New volumes, created empty: ${pf.volumes.join(', ')}`,
        h('p', { class: 'hint' }, 'Their data stays on the source server.')));
    }
    const warned = specs.filter((s) => s.warnings?.length);
    if (warned.length) {
      rows.push(item('warn', `Settings not copied for ${warned.length} container${warned.length === 1 ? '' : 's'}`,
        list(warned.flatMap((s) => s.warnings.map((w) => `${s.name}: ${w}`)))));
    }
    checks.replaceChildren(...rows);
    const existing = new Set(pf.existing.map((e) => e.name));
    const n = opts.replace ? specs.length : specs.filter((s) => !existing.has(s.name)).length;
    start.replaceChildren(icon('play'), n === specs.length ? `Create and start ${n}` : `Create and start ${n} (skip ${specs.length - n})`);
    start.disabled = n === 0;
  }

  async function check() {
    try {
      pf = await call('ClonePreflight', draft.targetId, specs, files.map((f) => f.path));
      render();
    } catch (err) {
      checks.replaceChildren(item('bad', `Cannot check ${target}`, h('p', { class: 'card-error' }, err.message),
        h('button', { type: 'button', class: 'btn sm', onclick: () => { checks.replaceChildren(h('div', { class: 'clone-loading' }, h('span', { class: 'spinner' }), 'Checking…')); check(); } }, icon('refresh'), 'Try again')));
    }
  }
  check();

  start.addEventListener('click', async () => {
    if (opts.replace && pf.existing.length && !await confirmDialog(`Replace ${pf.existing.length} container${pf.existing.length === 1 ? '' : 's'} on ${target}?\n\n${pf.existing.map((e) => e.name).join('\n')}\n\n⚠ They are stopped and removed before the copies are created. Data in their named volumes stays.`, 'Replace containers')) return;
    start.disabled = true;
    start.replaceChildren(h('span', { class: 'spinner' }), 'Preparing…');
    try {
      go(3, undefined, await call('CloneStart', draft.targetId, plan()));
    } catch (err) {
      toast(err.message, true);
      render();
    }
  });
  const recheck = h('button', { type: 'button', class: 'btn ghost', title: 'Check the target server again (after a docker login, freeing a port…)' }, icon('refresh'), 'Check again');
  recheck.addEventListener('click', () => {
    checks.replaceChildren(h('div', { class: 'clone-loading' }, h('span', { class: 'spinner' }), `Checking ${target}…`));
    start.disabled = true;
    check();
  });
  m.foot.append(back, h('span', { class: 'spacer' }), recheck, start);
}
