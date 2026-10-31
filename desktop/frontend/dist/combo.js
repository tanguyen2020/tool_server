// A dropdown with a search box: type to filter, arrows + Enter to pick, Esc to close.
// Behaves like a <select> for its users: .value, .disabled, a "change" event on .el.
// With multiple: true, Enter / click tick options and the list stays open; .values holds the ticked ones.
import { h } from './util.js';

let openCombo = null; // only one list is open at a time
// The path is taken at dispatch: ticking an option redraws the list, so e.target may be gone from the page by now.
document.addEventListener('mousedown', (e) => { if (openCombo && !e.composedPath().includes(openCombo.el)) openCombo.close(); });
window.addEventListener('blur', () => openCombo?.close());

// options: [{ value, label, sub, disabled }]
export function combo({ ariaLabel = '', placeholder = 'Choose…', searchPlaceholder = 'Search…', empty = 'Nothing matches', multiple = false, noun = 'items' } = {}) {
  const label = h('span', { class: 'combo-label' });
  const sub = h('span', { class: 'combo-sub' });
  const btn = h('button', { type: 'button', class: 'combo-btn', 'aria-haspopup': 'listbox', 'aria-expanded': 'false', 'aria-label': ariaLabel },
    label, sub, h('span', { class: 'combo-caret', 'aria-hidden': 'true' }, '▾'));
  const search = h('input', { type: 'search', class: 'combo-search', placeholder: searchPlaceholder, spellcheck: 'false', 'aria-label': `Search ${ariaLabel}` });
  const list = h('div', { class: 'combo-list', role: 'listbox', 'aria-multiselectable': multiple ? 'true' : null });
  const count = h('span', { class: 'combo-count' });
  const allBtn = h('button', { type: 'button', class: 'btn ghost sm' }, 'All');
  const noneBtn = h('button', { type: 'button', class: 'btn ghost sm' }, 'None');
  const tools = multiple ? h('div', { class: 'combo-tools' }, count, h('span', { class: 'spacer' }), allBtn, noneBtn) : '';
  const pop = h('div', { class: 'combo-pop', hidden: true }, search, tools, list);
  const el = h('div', { class: `combo${multiple ? ' multi' : ''}` }, btn, pop);
  let options = [];
  let picked = new Set(); // ticked values (one at most without multiple)
  let shown = [];
  let active = -1;
  let ph = placeholder;

  const ordered = () => options.filter((o) => picked.has(o.value)); // in the options' order

  function paint() {
    const sel = ordered();
    btn.classList.toggle('placeholder', !sel.length);
    if (!sel.length) { label.textContent = ph; sub.textContent = ''; }
    else if (sel.length === 1) { label.textContent = sel[0].label; sub.textContent = sel[0].sub || ''; }
    else { label.textContent = `${sel.length} ${noun}`; sub.textContent = sel.map((o) => o.label).join(', '); }
    if (multiple) count.textContent = `${sel.length} of ${options.length} selected`;
  }

  function render() {
    const words = search.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    shown = options.filter((o) => {
      const text = `${o.label} ${o.sub || ''}`.toLowerCase();
      return words.every((w) => text.includes(w));
    });
    if (active >= shown.length) active = shown.length - 1;
    list.replaceChildren(...(shown.length ? shown.map((o, i) => h('div', {
      role: 'option',
      class: `combo-opt${i === active ? ' active' : ''}${picked.has(o.value) ? ' selected' : ''}${o.disabled ? ' disabled' : ''}`,
      'aria-selected': String(picked.has(o.value)),
      onmousedown: (e) => { e.preventDefault(); if (!o.disabled) pick(o.value); },
      onmousemove: () => { if (active !== i) { active = i; mark(); } },
    }, multiple ? h('span', { class: 'combo-tick', 'aria-hidden': 'true' }, picked.has(o.value) ? '✓' : '') : '',
    h('span', { class: 'combo-opt-label' }, o.label), o.sub ? h('span', { class: 'combo-opt-sub' }, o.sub) : '')) : [h('div', { class: 'combo-empty' }, empty)]));
  }

  function mark() {
    [...list.children].forEach((c, i) => c.classList.toggle('active', i === active));
    list.children[active]?.scrollIntoView({ block: 'nearest' });
  }

  function open() {
    if (btn.disabled || !pop.hidden) return;
    openCombo?.close();
    openCombo = api;
    pop.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    search.value = '';
    active = Math.max(0, options.findIndex((o) => picked.has(o.value)));
    render();
    mark();
    // Open upwards when there is no room below.
    const r = btn.getBoundingClientRect();
    pop.classList.toggle('up', window.innerHeight - r.bottom < 300 && r.top > window.innerHeight - r.bottom);
    search.focus();
  }

  function close() {
    if (pop.hidden) return;
    pop.hidden = true;
    btn.setAttribute('aria-expanded', 'false');
    if (openCombo === api) openCombo = null;
  }

  const changed = () => { paint(); el.dispatchEvent(new Event('change')); };

  function pick(v) {
    if (multiple) {
      picked.has(v) ? picked.delete(v) : picked.add(v);
      render();
      changed();
      return;
    }
    close();
    btn.focus();
    if (picked.has(v)) return;
    picked = new Set([v]);
    changed();
  }

  // All / None act on the options the search shows.
  allBtn.addEventListener('click', () => { shown.filter((o) => !o.disabled).forEach((o) => picked.add(o.value)); render(); changed(); search.focus(); });
  noneBtn.addEventListener('click', () => { shown.forEach((o) => picked.delete(o.value)); render(); changed(); search.focus(); });

  btn.addEventListener('click', () => (pop.hidden ? open() : close()));
  btn.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ' || (e.key.length === 1 && !e.ctrlKey && !e.metaKey)) {
      e.preventDefault();
      open();
      if (e.key.length === 1 && e.key !== ' ') { search.value = e.key; render(); }
    }
  });
  search.addEventListener('input', () => { active = 0; render(); });
  search.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (!shown.length) return;
      active = (active + (e.key === 'ArrowDown' ? 1 : shown.length - 1)) % shown.length;
      mark();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const o = shown[active];
      if (o && !o.disabled) pick(o.value);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation(); // do not close a dialog around it
      close();
      btn.focus();
    } else if (e.key === 'Tab') {
      close();
    }
  });

  const api = {
    el,
    close,
    get value() { return ordered()[0]?.value ?? ''; },
    set value(v) { picked = new Set(v ? [v] : []); paint(); },
    get values() { return ordered().map((o) => o.value); },
    set values(list) { picked = new Set(list); paint(); },
    get disabled() { return btn.disabled; },
    set disabled(d) { btn.disabled = !!d; if (d) close(); },
    // Replaces the options; keeps the ticked values that are still there. text: what shows when nothing is chosen.
    setOptions(list, text = placeholder) {
      options = list;
      ph = text;
      picked = new Set([...picked].filter((v) => options.some((o) => o.value === v)));
      paint();
      if (!pop.hidden) render();
    },
    has: (v) => options.some((o) => o.value === v),
  };
  paint();
  return api;
}
