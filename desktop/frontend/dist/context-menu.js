import { h } from './util.js';
import { icon } from './icons.js';

// A single right-click menu shared by the whole app.
const menu = h('div', { class: 'ctx-menu', role: 'menu', hidden: true });
document.body.append(menu);
let returnFocus = null;

export function closeMenu() {
  if (menu.hidden) return;
  menu.hidden = true;
  menu.replaceChildren();
  returnFocus?.focus?.({ preventScroll: true });
  returnFocus = null;
}

// items: [{ label, action, danger, icon, disabled, title }] or { sep: true } for a divider. Opens at the pointer,
// or under `anchor` when triggered from the keyboard (context-menu key / Shift+F10). below: right-aligned under
// the anchor, for a "more actions" button.
export function openMenu(e, anchor, items, { below = false } = {}) {
  e.preventDefault();
  closeMenu();
  returnFocus = document.activeElement;
  menu.replaceChildren(...items.map((it) => (it.sep ? h('div', { class: 'ctx-sep', role: 'separator' }) : h('button', {
    type: 'button',
    role: 'menuitem',
    class: it.danger ? 'danger' : '',
    disabled: !!it.disabled,
    title: it.title,
    onclick: () => { closeMenu(); it.action(); },
  }, it.icon ? icon(it.icon) : '', it.label))));
  menu.hidden = false;

  let x = e.clientX;
  let y = e.clientY;
  const { width, height } = menu.getBoundingClientRect();
  if (below && anchor) {
    const r = anchor.getBoundingClientRect();
    x = r.right - width;
    y = r.bottom + 6;
  } else if (!x && !y && anchor) {
    const r = anchor.getBoundingClientRect();
    x = r.left + 16;
    y = r.top + 16;
  }
  // Keep the menu inside the window.
  menu.style.left = `${Math.max(4, Math.min(x, innerWidth - width - 4))}px`;
  menu.style.top = `${Math.max(4, Math.min(y, innerHeight - height - 4))}px`;
  menu.querySelector('button:not(:disabled)')?.focus({ preventScroll: true });
}

menu.addEventListener('keydown', (e) => {
  const buttons = [...menu.querySelectorAll('button:not(:disabled)')];
  const i = buttons.indexOf(document.activeElement);
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    const next = (i + (e.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length;
    buttons[next].focus();
  } else if (e.key === 'Escape' || e.key === 'Tab') {
    e.preventDefault();
    closeMenu();
  }
});
document.addEventListener('pointerdown', (e) => { if (!menu.contains(e.target)) closeMenu(); }, true);
window.addEventListener('blur', closeMenu);
window.addEventListener('resize', closeMenu);
document.addEventListener('scroll', closeMenu, true);
