// Security tab: who is signed in, SSH logins (failed attempts by address), authorized SSH keys, the ufw
// firewall, and the ports anyone on the network can reach — including Docker's published ports, which skip ufw.
import { h, toast } from './util.js';
import { call, confirmDialog, copyText } from './bridge.js';
import { panel, modal, fmtDateTime } from './ui.js';
import { icon } from './icons.js';

const RANGES = [[1, '24h'], [7, '7 days'], [30, '30 days']];
const RISK_LABEL = { critical: 'At risk', warning: 'Check', info: 'OK' };
const RISK_TONE = { critical: 'critical', warning: 'warning', info: 'good' };

function ago(ms) {
  if (!ms) return '–';
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

function duration(ms) {
  if (!ms) return '–';
  const m = Math.max(0, Math.floor((Date.now() - ms) / 60000));
  if (m < 60) return `${m} min`;
  if (m < 1440) return `${Math.floor(m / 60)} h ${m % 60} min`;
  return `${Math.floor(m / 1440)} d ${Math.floor((m % 1440) / 60)} h`;
}

const shortFp = (fp) => (fp ? `${fp.slice(0, 18)}…` : '');
const chip = (tone, text, title) => h('span', { class: `chip ${tone || ''}`, title: title || '' }, text);
const muted = (text) => h('span', { class: 'muted' }, text);
const n = (x) => Number(x || 0).toLocaleString('en-US');

export function securityTab(serverId, { serverName, openTask }) {
  let overview = null;
  let keys = null;
  let logins = null;
  let days = 7;
  let loaded = false;
  let showAllSources = false;

  // ---- summary
  const refreshBtn = h('button', { type: 'button', class: 'btn sm act act-restart' }, icon('refresh'), 'Refresh');
  const sumStatus = h('span', { class: 'muted' });
  const sumP = panel('span-12', 'Security checks', sumStatus, refreshBtn);
  const checksEl = h('div', { class: 'sec-checks' });
  const accessNote = h('p', { class: 'hint', hidden: true });
  sumP.body.append(checksEl, accessNote);

  // ---- exposed ports
  const expStatus = h('span', { class: 'muted' });
  const expP = panel('span-12', 'Open to the network', expStatus);
  const expBody = h('tbody');
  expP.body.append(
    h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
      h('thead', {}, h('tr', {}, ['', 'Port', 'What', 'Listening on', 'Firewall', 'How to fix'].map((x, i) => h('th', { class: i === 1 ? 'num' : '' }, x)))),
      expBody)),
    h('p', { class: 'hint' }, 'Ports reachable by anyone who can reach the server. A firewall at your hosting provider may still block some of them; this check only sees the server itself.'));

  // ---- firewall
  const fwChip = h('span');
  const fwToggle = h('button', { type: 'button', class: 'btn sm', hidden: true });
  const fwAdd = h('button', { type: 'button', class: 'btn sm act act-start', hidden: true }, icon('plus'), 'Add rule');
  const fwP = panel('span-12', 'Firewall (ufw)', fwChip, fwAdd, fwToggle);
  const fwInfo = h('div', { class: 'sec-fw-info' });
  const fwBody = h('tbody');
  const fwTable = h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
    h('thead', {}, h('tr', {}, ['#', 'Action', 'Port', 'Protocol', 'From', 'Comment', ''].map((x) => h('th', {}, x)))), fwBody));
  fwP.body.append(fwInfo, fwTable,
    h('p', { class: 'hint' }, 'ufw applies the first rule that matches, then the default. Rules are kept while the firewall is off. Turning it on adds a rule for SSH first when needed, so the app keeps its connection.'));

  // ---- sessions
  const sesStatus = h('span', { class: 'muted' });
  const sesP = panel('span-12', 'Signed in now', sesStatus);
  const sesBody = h('tbody');
  sesP.body.append(h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
    h('thead', {}, h('tr', {}, ['User', 'From', 'Terminal', 'Signed in', 'State', ''].map((x) => h('th', {}, x)))), sesBody)));

  // ---- failed logins
  const rangeBtns = RANGES.map(([d, label]) => h('button', { type: 'button', class: 'tab', 'data-days': d, onclick: () => { days = d; loadLogins(); } }, label));
  const failStatus = h('span', { class: 'muted' });
  const failP = panel('span-12', 'Failed SSH logins', failStatus, h('div', { class: 'tabs', role: 'group', 'aria-label': 'Period' }, rangeBtns));
  const failSummary = h('div', { class: 'sec-fail-sum' });
  const failBody = h('tbody');
  const failMore = h('button', { type: 'button', class: 'btn sm ghost', hidden: true, onclick: () => { showAllSources = true; renderLogins(); } });
  const failHint = h('p', { class: 'hint' });
  failP.body.append(failSummary,
    h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
      h('thead', {}, h('tr', {}, ['Address', 'Attempts', 'User names tried', 'First', 'Last', ''].map((x, i) => h('th', { class: i === 1 ? 'num' : '' }, x)))), failBody)),
    failMore, failHint);

  // ---- recent logins
  const okStatus = h('span', { class: 'muted' });
  const okP = panel('span-12', 'Recent SSH logins', okStatus);
  const okBody = h('tbody');
  okP.body.append(h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
    h('thead', {}, h('tr', {}, ['When', 'User', 'From', 'Method', 'Key', ''].map((x) => h('th', {}, x)))), okBody)));

  // ---- keys
  const keyAdd = h('button', { type: 'button', class: 'btn sm act act-start' }, icon('plus'), 'Add key');
  const keyStatus = h('span', { class: 'muted' });
  const keyP = panel('span-12', 'Authorized SSH keys', keyStatus, keyAdd);
  const keyBody = h('tbody');
  const keyNote = h('p', { class: 'hint', hidden: true });
  keyP.body.append(h('div', { class: 'table-wrap' }, h('table', { class: 'sec-table' },
    h('thead', {}, h('tr', {}, ['Key', 'Comment', 'Fingerprint', 'Restrictions', 'Last used', ''].map((x) => h('th', {}, x)))), keyBody)),
  keyNote,
  h('p', { class: 'hint' }, 'Anyone holding the private half of a key listed here can sign in as that user. Remove the keys of people who left and of computers you no longer use.'));

  const panels = [sumP.el, expP.el, fwP.el, sesP.el, failP.el, okP.el, keyP.el];

  // ---------------------------------------------------------------- summary
  function check(tone, title, value, sub, target) {
    return h('button', { type: 'button', class: `sec-check ${tone}`, onclick: () => target?.scrollIntoView({ behavior: 'smooth', block: 'start' }) },
      h('span', { class: 'sec-check-title' }, h('i', { class: `dot ${tone}` }), title),
      h('span', { class: 'sec-check-value' }, value),
      h('span', { class: 'sec-check-sub' }, sub || ''));
  }

  function renderSummary() {
    if (!overview) return;
    const o = overview;
    const fw = o.firewall;
    const items = [];
    if (!fw.readable) items.push(check('', 'Firewall', 'Unknown', 'Reading it needs root', fwP.el));
    else if (fw.kind === 'ufw') items.push(check('good', 'Firewall', 'ufw on', `${fw.rules.length} rule${fw.rules.length === 1 ? '' : 's'} · incoming ${fw.defaultIn}`, fwP.el));
    else if (fw.kind === 'none') items.push(check('critical', 'Firewall', fw.ufwInstalled ? 'ufw off' : 'None', 'Every listening port is open', fwP.el));
    else items.push(check('good', 'Firewall', fw.kind, 'Rules managed outside this app', fwP.el));

    const crit = o.exposed.filter((e) => e.risk === 'critical').length;
    const warn = o.exposed.filter((e) => e.risk === 'warning').length;
    items.push(crit ? check('critical', 'Open to the network', `${crit} at risk`, warn ? `${warn} more to check` : 'Databases or admin ports', expP.el)
      : warn ? check('warning', 'Open to the network', `${warn} to check`, 'Ports open to everyone', expP.el)
        : check('good', 'Open to the network', 'Nothing risky', `${o.exposed.length} expected port${o.exposed.length === 1 ? '' : 's'}`, expP.el));

    const ssh = o.ssh;
    if (!ssh.known) items.push(check('', 'Password login', 'Unknown', 'Reading sshd settings needs root', failP.el));
    else if (ssh.passwordAuth || ssh.kbdInteractive) items.push(check('warning', 'Password login', 'Allowed', 'Bots can guess passwords: prefer keys only', failP.el));
    else items.push(check('good', 'Password login', 'Off', 'Keys only', failP.el));
    if (ssh.known) {
      const r = ssh.permitRootLogin;
      if (r === 'yes') items.push(check('warning', 'Root login', 'Allowed', 'With a password too', keyP.el));
      else if (r === 'no') items.push(check('good', 'Root login', 'Off', 'Sign in as a user, then sudo', keyP.el));
      else items.push(check('good', 'Root login', 'Keys only', r, keyP.el));
    }
    if (logins && !logins.noJournal) {
      const tone = logins.failed24h > 0 && ssh.known && (ssh.passwordAuth || ssh.kbdInteractive) ? 'warning' : '';
      items.push(check(tone, `Failed logins · ${RANGES.find(([d]) => d === logins.days)?.[1] || `${logins.days} days`}`, n(logins.failed),
        `${n(logins.sourceCount)} address${logins.sourceCount === 1 ? '' : 'es'} · ${n(logins.failed24h)} in 24h`, failP.el));
    }
    items.push(check('', 'Signed in now', String(o.sessions.length), o.sessions.length ? [...new Set(o.sessions.map((s) => s.user))].slice(0, 4).join(', ') : 'Nobody else', sesP.el));
    checksEl.replaceChildren(...items);
    accessNote.hidden = o.me.root;
    accessNote.textContent = `Connected as ${o.me.user} without root: the firewall, the sshd settings, other users' keys and the SSH log cannot be read in full. Connect as root, or allow this user to use sudo without a password.`;
  }

  // ---------------------------------------------------------------- exposed ports
  function renderExposed() {
    if (!overview) return;
    const list = overview.exposed;
    expStatus.textContent = list.length ? `${list.length} port${list.length === 1 ? '' : 's'}` : '';
    expBody.replaceChildren(...(list.length ? list.map((e) => h('tr', { class: e.risk === 'critical' ? 'row-issue' : '' },
      h('td', {}, chip(RISK_TONE[e.risk], RISK_LABEL[e.risk])),
      h('td', { class: 'num mono' }, e.portEnd ? `${e.port}-${e.portEnd}` : e.port, muted(`/${e.proto}`)),
      h('td', {}, e.container ? h('span', {}, h('b', {}, e.container), muted(` container → ${e.target}`)) : 'Program on the server',
        e.service ? h('div', { class: 'muted small' }, e.service) : ''),
      h('td', { class: 'mono muted' }, e.addrs.map((a) => (a === '0.0.0.0' || a === '::' ? `${a} (all)` : a)).join(', ')),
      h('td', { class: 'sec-wrap' }, e.firewall),
      h('td', { class: 'sec-wrap muted' }, e.fix || '–')))
      : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, 'No port is open to the network.'))]));
  }

  // ---------------------------------------------------------------- firewall
  function ruleWhat(r) {
    if (r.app) return r.app;
    if (!r.port) return 'every port';
    return r.port;
  }

  function renderFirewall() {
    if (!overview) return;
    const fw = overview.firewall;
    const info = [];
    fwAdd.hidden = fwToggle.hidden = true;
    fwTable.hidden = true;
    if (!fw.readable) {
      fwChip.replaceChildren(chip('', 'unknown'));
      info.push(h('p', {}, 'Reading the firewall needs root: connect as root, or allow this user to use sudo without a password.'));
    } else if (!fw.ufwInstalled) {
      fwChip.replaceChildren(chip(fw.kind === 'none' ? 'critical' : '', 'not installed'));
      info.push(h('div', { class: 'sec-fw-line' }, h('span', {}, 'ufw is not installed on this server.'),
        h('button', { type: 'button', class: 'btn sm act act-start', onclick: () => openTask({ kind: 'installufw', title: 'install ufw', once: true }) }, icon('download'), 'Install ufw')));
    } else {
      fwChip.replaceChildren(chip(fw.ufwActive ? 'good' : 'critical', fw.ufwActive ? 'on' : 'off'));
      fwToggle.hidden = fwAdd.hidden = false;
      fwToggle.className = `btn sm act ${fw.ufwActive ? 'act-stop' : 'act-start'}`;
      fwToggle.replaceChildren(icon(fw.ufwActive ? 'stop' : 'shield'), fw.ufwActive ? 'Turn off' : 'Turn on');
      fwTable.hidden = false;
      info.push(h('p', {}, 'Incoming connections not matched by a rule: ', h('b', {}, fw.defaultIn === 'allow' ? 'allowed' : fw.defaultIn === 'reject' ? 'rejected' : 'blocked'), '.'));
    }
    if (fw.other?.length) {
      info.push(h('p', { class: 'muted' }, 'Other filtering on this server: ', fw.other.join('; '), '.'));
    }
    if (fw.dockerUser) info.push(h('p', { class: 'muted' }, 'The DOCKER-USER chain has rules: they filter traffic to containers.'));
    fwInfo.replaceChildren(...info);
    fwBody.replaceChildren(...(fw.rules.length ? fw.rules.map((r, i) => h('tr', {},
      h('td', { class: 'num muted' }, i + 1),
      h('td', {}, chip(r.action === 'allow' || r.action === 'limit' ? 'good' : 'critical', r.action + (r.dir === 'out' ? ' out' : '') + (r.route ? ' (routed)' : ''))),
      h('td', { class: 'mono' }, ruleWhat(r), r.iface ? muted(` on ${r.iface}`) : ''),
      h('td', {}, r.proto || 'any'),
      h('td', { class: 'mono' }, r.from === 'any' ? 'Anywhere' : r.from),
      h('td', { class: 'muted sec-wrap' }, r.comment || ''),
      h('td', {}, h('div', { class: 'c-actions' }, h('button', { type: 'button', class: 'btn sm act act-remove', onclick: () => deleteRule(r) }, icon('trash'), 'Delete')))))
      : [h('tr', {}, h('td', { colspan: 7, class: 'empty' }, 'No rules yet.'))]));
  }

  async function toggleFirewall() {
    const fw = overview?.firewall;
    if (!fw) return;
    if (fw.ufwActive) {
      if (!(await confirmDialog(`Turn off the firewall on ${serverName()}?\n\n⚠ Every port a program listens on becomes reachable from the network. The rules are kept and come back when you turn it on.`, 'Disable firewall', { confirmLabel: 'Turn off' }))) return;
      await act(() => call('FirewallDisable', serverId), 'Firewall turned off');
      return;
    }
    const port = overview.me.sshPort || 22;
    if (!(await confirmDialog(`Turn on the firewall on ${serverName()}?\n\nIncoming connections not allowed by a rule will be ${fw.defaultIn === 'allow' ? 'allowed (default allow)' : 'blocked'}. SSH (port ${port}) stays reachable: a rule is added for it first when needed.\n\n⚠ Docker's published ports are not filtered by ufw: see "Open to the network".`, 'Turn on firewall', { tone: 'primary', confirmLabel: 'Turn on' }))) return;
    await act(async () => {
      const msg = await call('FirewallEnable', serverId);
      if (msg) toast(msg);
    }, 'Firewall turned on');
  }

  async function deleteRule(r) {
    if (!(await confirmDialog(`Delete this firewall rule on ${serverName()}?\n\nufw ${r.cmd}`, 'Delete rule', { code: true }))) return;
    await act(() => call('FirewallDeleteRule', serverId, r.cmd), 'Rule deleted');
  }

  function addRuleDialog(preset = {}) {
    const m = modal({ title: 'Add a firewall rule', sub: serverName() });
    const action = h('select', { 'aria-label': 'Action' }, [['allow', 'Allow'], ['deny', 'Deny (drop silently)'], ['reject', 'Reject (refuse)'], ['limit', 'Limit (allow, block after 6 tries in 30 s)']]
      .map(([v, l]) => h('option', { value: v }, l)));
    const port = h('input', { placeholder: 'e.g. 443, 80,443 or 6000:6007 — empty for every port', spellcheck: 'false', value: preset.port || '' });
    const proto = h('select', { 'aria-label': 'Protocol' }, [['tcp', 'TCP'], ['udp', 'UDP'], ['any', 'TCP and UDP']].map(([v, l]) => h('option', { value: v }, l)));
    const from = h('input', { placeholder: 'Anywhere, or 203.0.113.5, or 10.0.0.0/8', spellcheck: 'false', value: preset.from || '' });
    const comment = h('input', { placeholder: 'Why this rule exists (optional)', maxlength: 100, value: preset.comment || '' });
    const msg = h('p', { class: 'form-msg error' });
    action.value = preset.action || 'allow';
    const form = h('form', { class: 'form plain' },
      h('label', {}, 'Action', action),
      h('div', { class: 'grid-2' }, h('label', {}, 'Port', port), h('label', {}, 'Protocol', proto)),
      h('label', {}, 'From', from),
      h('label', {}, 'Comment', comment),
      h('p', { class: 'hint' }, 'Deny and reject rules are placed first so they win over allow rules. A rule that would block SSH from this computer is refused.'),
      msg);
    const ok = h('button', { type: 'button', class: 'btn primary' }, icon('plus'), 'Add rule');
    m.body.append(form);
    m.foot.append(h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), ok);
    const submit = async () => {
      ok.disabled = true;
      msg.textContent = '';
      try {
        await call('FirewallAddRule', serverId, { action: action.value, port: port.value.trim(), proto: proto.value, from: from.value.trim() || 'any', comment: comment.value.trim() });
        m.close();
        toast('Rule added');
        loadOverview();
      } catch (err) {
        msg.textContent = err.message;
        ok.disabled = false;
      }
    };
    ok.addEventListener('click', submit);
    form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
    port.focus();
  }

  // ---------------------------------------------------------------- sessions
  function renderSessions() {
    if (!overview) return;
    const list = overview.sessions;
    sesStatus.textContent = overview.sessionsBy ? `${list.length} session${list.length === 1 ? '' : 's'}` : 'Not available on this server';
    const myIP = overview.me.ip;
    sesBody.replaceChildren(...(list.length ? list.map((s) => h('tr', {},
      h('td', {}, h('b', {}, s.user)),
      h('td', { class: 'mono' }, s.from || muted('this server'), s.from && s.from === myIP && !s.mine ? h('span', { class: 'muted' }, ' (your address)') : ''),
      h('td', {}, s.tty || muted(s.service === 'sshd' ? 'no terminal (commands, file transfer)' : '–')),
      h('td', { title: fmtDateTime(s.since) }, s.since ? `${duration(s.since)} ago` : '–'),
      h('td', {}, s.mine ? chip('good', 'this app') : chip(s.state === 'closing' ? 'warning' : '', s.state || 'active')),
      h('td', {}, s.mine ? '' : h('div', { class: 'c-actions' }, h('button', { type: 'button', class: 'btn sm act act-remove', onclick: () => endSession(s) }, icon('close'), 'Sign out')))))
      : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, overview.sessionsBy ? 'Nobody is signed in.' : 'This server has neither logind nor who.'))]));
  }

  async function endSession(s) {
    const what = `${s.user}${s.from ? ` from ${s.from}` : ''}${s.tty ? ` on ${s.tty}` : ''}`;
    if (!(await confirmDialog(`Sign out ${what}?\n\nTheir programs in this session are stopped and the connection is closed. They can sign in again unless you remove their key or block their address.`, 'Stop session', { tone: 'danger', confirmLabel: 'Sign out' }))) return;
    await act(() => call('SecurityEndSession', serverId, s.id || '', s.id ? '' : s.tty, what), 'Session ended');
  }

  // ---------------------------------------------------------------- logins
  const blockedIPs = () => new Set((overview?.firewall?.rules || []).filter((r) => (r.action === 'deny' || r.action === 'reject') && !r.port && !r.app).map((r) => r.from));

  function renderLogins() {
    rangeBtns.forEach((b) => b.classList.toggle('active', Number(b.dataset.days) === days));
    if (!logins) return;
    const lg = logins;
    if (lg.noJournal) {
      failSummary.replaceChildren(muted('This server has no systemd journal: SSH logins cannot be read.'));
      failBody.replaceChildren();
      okBody.replaceChildren();
      return;
    }
    const pw = overview?.ssh?.known ? overview.ssh.passwordAuth || overview.ssh.kbdInteractive : null;
    failSummary.replaceChildren(
      h('span', { class: 'sec-big' }, n(lg.failed)), ` failed attempt${lg.failed === 1 ? '' : 's'} from `,
      h('b', {}, n(lg.sourceCount)), ` address${lg.sourceCount === 1 ? '' : 'es'} · `, h('b', {}, n(lg.failed24h)), ' in the last 24h · ',
      h('b', {}, n(lg.accepted)), ` successful login${lg.accepted === 1 ? '' : 's'}`);
    failHint.textContent = [
      lg.limited ? 'Only part of the log is readable: connect as root, or add this user to the adm group.' : '',
      pw === false ? 'Password login is off: these attempts cannot succeed without one of the authorized keys.' : '',
      pw === true ? 'Password login is on: block addresses that keep trying, and prefer keys only (PasswordAuthentication no).' : '',
      overview?.firewall?.ufwActive ? '' : 'Blocking adds a ufw rule; it takes effect once the firewall is on.',
    ].filter(Boolean).join(' ');
    const blocked = blockedIPs();
    const rows = showAllSources ? lg.sources : lg.sources.slice(0, 25);
    failBody.replaceChildren(...(rows.length ? rows.map((s) => h('tr', {},
      h('td', { class: 'mono' }, s.ip, s.ip === overview?.me?.ip ? h('span', { class: 'muted' }, ' (this computer)') : ''),
      h('td', { class: 'num' }, n(s.attempts)),
      h('td', { class: 'sec-wrap' }, s.users.filter(Boolean).join(', ') || muted('(empty)'), s.userCount > s.users.length ? muted(` +${s.userCount - s.users.length} more`) : ''),
      h('td', { class: 'muted', title: fmtDateTime(s.first) }, ago(s.first)),
      h('td', { class: 'muted', title: fmtDateTime(s.last) }, ago(s.last)),
      h('td', {}, h('div', { class: 'c-actions' },
        blocked.has(s.ip) ? chip('good', 'blocked')
          : overview?.firewall?.ufwInstalled && s.ip !== overview?.me?.ip ? h('button', { type: 'button', class: 'btn sm act act-stop', onclick: () => block(s) }, icon('lock'), 'Block') : '',
        h('button', { type: 'button', class: 'btn sm act act-logs', title: 'Copy address', 'aria-label': `Copy ${s.ip}`, onclick: () => copyText(s.ip).then(() => toast('Address copied')) }, icon('copy'))))))
      : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, `No failed SSH login in the last ${RANGES.find(([d]) => d === lg.days)?.[1] || `${lg.days} days`}.`))]));
    failMore.hidden = showAllSources || lg.sources.length <= 25;
    failMore.textContent = `Show all ${lg.sources.length}${lg.sourceCount > lg.sources.length ? ` (of ${n(lg.sourceCount)})` : ''}`;

    const keyName = new Map();
    for (const f of keys?.files || []) for (const k of f.keys) if (k.fingerprint) keyName.set(k.fingerprint, k.comment || `${f.user}'s key`);
    okStatus.textContent = lg.recent.length ? `latest ${lg.recent.length}` : '';
    okBody.replaceChildren(...(lg.recent.length ? lg.recent.map((r) => h('tr', {},
      h('td', { title: fmtDateTime(r.time) }, ago(r.time)),
      h('td', {}, h('b', {}, r.user)),
      h('td', { class: 'mono' }, r.ip, r.ip === overview?.me?.ip && !r.mine ? h('span', { class: 'muted' }, ' (your address)') : ''),
      h('td', {}, r.method === 'publickey' ? 'key' : r.method === 'password' ? chip('warning', 'password') : r.method),
      h('td', {}, r.fingerprint ? h('span', { title: r.fingerprint }, keyName.get(r.fingerprint) || h('span', { class: 'mono muted' }, shortFp(r.fingerprint))) : '–'),
      h('td', {}, r.mine ? chip('good', 'this app') : '')))
      : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, 'No successful SSH login in this period.'))]));
  }

  async function block(s) {
    if (!(await confirmDialog(`Block ${s.ip} on ${serverName()}?\n\nA ufw rule denying every connection from this address is placed first. It made ${n(s.attempts)} failed SSH login attempt${s.attempts === 1 ? '' : 's'}.`, 'Block address', { tone: 'warning', confirmLabel: 'Block' }))) return;
    await act(() => call('FirewallBlock', serverId, s.ip, s.attempts), `${s.ip} blocked`);
  }

  // ---------------------------------------------------------------- keys
  function lastUse() {
    const m = new Map();
    for (const r of logins?.recent || []) if (r.fingerprint && !m.has(r.fingerprint)) m.set(r.fingerprint, r);
    return m;
  }

  function renderKeys() {
    if (!keys) return;
    const used = lastUse();
    const rows = [];
    let count = 0;
    for (const f of keys.files) {
      rows.push(h('tr', { class: 'sec-group' }, h('td', { colspan: 6 },
        h('b', {}, f.user), h('span', { class: 'mono muted' }, `  ${f.path}`),
        f.modTime ? muted(` · changed ${fmtDateTime(f.modTime)}`) : '')));
      if (!f.readable) { rows.push(h('tr', {}, h('td', { colspan: 6, class: 'muted' }, 'Cannot read this file (needs root).'))); continue; }
      if (f.tooBig) { rows.push(h('tr', {}, h('td', { colspan: 6, class: 'muted' }, 'This file is too large to show.'))); continue; }
      if (!f.keys.length) rows.push(h('tr', {}, h('td', { colspan: 6, class: 'muted' }, 'No keys in this file.')));
      for (const k of f.keys) {
        if (k.invalid) {
          rows.push(h('tr', {}, h('td', {}, chip('warning', 'not a key')), h('td', { colspan: 4, class: 'mono muted' }, `line ${k.line}: ${k.invalid}`), h('td', {})));
          continue;
        }
        count++;
        const isApp = keys.appKey && k.fingerprint === keys.appKey && f.user === keys.user;
        const u = used.get(k.fingerprint);
        rows.push(h('tr', { class: k.weak ? 'row-issue' : '' },
          h('td', {}, k.type.replace(/^ssh-/, '').replace(/@openssh\.com$/, ''), k.bits ? muted(` ${k.bits}`) : '',
            k.weak ? h('div', {}, chip('warning', 'weak', k.weak)) : ''),
          h('td', { class: 'sec-wrap' }, k.comment || muted('(no comment)'), isApp ? h('span', {}, ' ', chip('good', 'this app')) : ''),
          h('td', { class: 'mono muted', title: k.fingerprint }, shortFp(k.fingerprint)),
          h('td', { class: 'mono sec-wrap' }, k.options.length ? k.options.join(', ') : muted('none')),
          h('td', { title: u ? `${fmtDateTime(u.time)} from ${u.ip}` : 'Not in the SSH log of this period' }, u ? `${ago(u.time)} · ${u.ip}` : muted(logins ? `not in ${RANGES.find(([d]) => d === logins.days)?.[1] || 'period'}` : '…')),
          h('td', {}, isApp ? '' : h('div', { class: 'c-actions' }, h('button', { type: 'button', class: 'btn sm act act-remove', onclick: () => removeKey(f, k) }, icon('trash'), 'Remove')))));
      }
    }
    keyStatus.textContent = `${count} key${count === 1 ? '' : 's'}`;
    keyBody.replaceChildren(...(rows.length ? rows : [h('tr', {}, h('td', { colspan: 6, class: 'empty' }, 'No authorized_keys file found.'))]));
    keyNote.hidden = keys.root;
    keyNote.textContent = `Without root only the files ${keys.user} can read are listed.`;
  }

  async function removeKey(f, k) {
    if (!(await confirmDialog(`Remove this key from ${f.user} on ${serverName()}?\n\n${k.type} ${k.comment || ''}\n${k.fingerprint}\n\nWhoever holds it can no longer sign in as ${f.user} (sessions already open stay open).`, 'Remove key', { code: true }))) return;
    await act(() => call('SecurityRemoveKey', serverId, f.user, f.path, k.fingerprint), 'Key removed', true);
  }

  function addKeyDialog() {
    if (!keys) return;
    const m = modal({ title: 'Add an SSH key', sub: serverName() });
    const NEW = '\u0000new';
    const users = keys.users.filter((u) => u.canLogin);
    const user = h('select', { 'aria-label': 'User' },
      users.map((u) => h('option', { value: u.name }, `${u.name}${u.uid === 0 ? ' (root)' : ''}`)),
      keys.root ? h('option', { value: NEW }, 'New user…') : '');
    user.value = users.some((u) => u.name === keys.user) ? keys.user : users[0]?.name || NEW;
    // A new user signs in with keys only (no password); docker and sudo are opt-in.
    const newName = h('input', { spellcheck: 'false', autocomplete: 'off', placeholder: 'e.g. anna or deploy', maxlength: 32 });
    const docker = h('input', { type: 'checkbox' });
    const sudo = h('input', { type: 'checkbox' });
    const newBox = h('div', { class: 'sec-newuser' },
      h('label', {}, 'New user name', newName),
      keys.docker ? h('label', { class: 'check' }, docker, h('span', {}, 'Can use Docker ', muted('(docker group: as powerful as root)'))) : '',
      h('label', { class: 'check' }, sudo, h('span', {}, 'Administrator ', muted('(may run any command as root with sudo, without a password)'))),
      h('p', { class: 'hint' }, 'The user gets a home folder and bash, and no password: it signs in with this key only.'));
    const line = h('textarea', { rows: 4, spellcheck: 'false', placeholder: 'ssh-ed25519 AAAAC3Nza… name@laptop', class: 'mono' });
    const msg = h('p', { class: 'form-msg error' });
    const form = h('form', { class: 'form plain' },
      h('label', {}, 'User', user),
      newBox,
      h('label', {}, 'Public key', line),
      h('p', { class: 'hint' }, 'Paste the content of the .pub file (for example ~/.ssh/id_ed25519.pub). Never paste a private key.'),
      keys.root ? '' : h('p', { class: 'hint' }, 'Creating users needs root: connect as root, or allow this user to use sudo without a password.'),
      msg);
    const isNew = () => user.value === NEW;
    const sync = () => { newBox.hidden = !isNew(); };
    user.addEventListener('change', () => { sync(); (isNew() ? newName : line).focus(); });
    sync();
    const ok = h('button', { type: 'button', class: 'btn primary' }, icon('plus'), 'Add key');
    m.body.append(form);
    m.foot.append(h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn ghost', onclick: () => m.close() }, 'Cancel'), ok);
    ok.addEventListener('click', async () => {
      const create = isNew();
      const name = create ? newName.value.trim() : user.value;
      if (create && !/^[a-z_][a-z0-9_-]{0,31}$/.test(name)) {
        msg.textContent = 'The user name must start with a letter and use only lowercase letters, digits, - and _.';
        newName.focus();
        return;
      }
      if (create && keys.users.some((u) => u.name === name)) {
        msg.textContent = `The user ${name} already exists: choose it in the list instead.`;
        newName.focus();
        return;
      }
      if (create && sudo.checked && !(await confirmDialog(`Make ${name} an administrator on ${serverName()}?\n\n⚠ Whoever holds this key will be able to run any command as root, without a password.`, 'Grant administrator rights', { tone: 'warning', confirmLabel: 'Continue' }))) return;
      ok.disabled = true;
      msg.textContent = '';
      try {
        const k = await call('SecurityAddKey', serverId, name, line.value.trim(), { create, docker: create && docker.checked, sudo: create && sudo.checked });
        m.close();
        toast(create ? `User ${name} created with this key` : `Key added for ${name}${k.comment ? ` (${k.comment})` : ''}`);
        loadKeys();
      } catch (err) {
        msg.textContent = err.message;
        ok.disabled = false;
      }
    });
    (isNew() ? newName : line).focus();
  }

  // ---------------------------------------------------------------- loading
  async function act(fn, done, reloadKeys = false) {
    try {
      await fn();
      toast(done);
    } catch (err) {
      toast(err.message, true);
    }
    await loadOverview();
    if (reloadKeys) loadKeys();
    else renderLogins();
  }

  function renderOverview() {
    renderSummary();
    renderExposed();
    renderFirewall();
    renderSessions();
  }

  async function loadOverview() {
    sumStatus.textContent = 'Loading…';
    try {
      overview = await call('SecurityOverview', serverId);
      sumStatus.textContent = '';
    } catch (err) {
      sumStatus.textContent = err.message;
      return;
    }
    renderOverview();
  }

  async function loadKeys() {
    keyStatus.textContent = 'Loading…';
    try {
      keys = await call('SecurityKeys', serverId);
    } catch (err) {
      keyStatus.textContent = err.message;
      return;
    }
    renderKeys();
    renderLogins();
  }

  async function loadLogins() {
    showAllSources = false;
    rangeBtns.forEach((b) => b.classList.toggle('active', Number(b.dataset.days) === days));
    failStatus.textContent = 'Reading the SSH log…';
    try {
      logins = await call('SecurityLogins', serverId, days);
      failStatus.textContent = '';
    } catch (err) {
      failStatus.textContent = err.message;
      return;
    }
    renderLogins();
    renderKeys();
    renderSummary();
  }

  function loadAll() {
    loaded = true;
    // The SSH log may take a while on a busy server: the rest shows first. Keys come after the log so the
    // app's key (found in the log when it signs in through an agent) is known.
    loadOverview();
    loadLogins().then(loadKeys);
  }

  refreshBtn.addEventListener('click', loadAll);
  fwToggle.addEventListener('click', toggleFirewall);
  fwAdd.addEventListener('click', () => addRuleDialog());
  keyAdd.addEventListener('click', addKeyDialog);
  renderLogins();

  return {
    panels,
    setActive(v) {
      if (v && !loaded) loadAll();
    },
    destroy() {},
  };
}
