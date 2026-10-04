import { h } from './util.js';

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

// items: [{ label, action, danger }]. Opens at the pointer, or under `anchor` when
// triggered from the keyboard (context-menu key / Shift+F10).
export function openMenu(e, anchor, items) {
  e.preventDefault();
  closeMenu();
  returnFocus = document.activeElement;
  menu.replaceChildren(...items.map((it) => h('button', {
    type: 'button',
    role: 'menuitem',
    class: it.danger ? 'danger' : '',
    onclick: () => { closeMenu(); it.action(); },
  }, it.label)));
  menu.hidden = false;

  let x = e.clientX;
  let y = e.clientY;
  if (!x && !y && anchor) {
    const r = anchor.getBoundingClientRect();
    x = r.left + 16;
    y = r.top + 16;
  }
  // Keep the menu inside the window.
  const { width, height } = menu.getBoundingClientRect();
  menu.style.left = `${Math.max(4, Math.min(x, innerWidth - width - 4))}px`;
  menu.style.top = `${Math.max(4, Math.min(y, innerHeight - height - 4))}px`;
  menu.querySelector('button')?.focus({ preventScroll: true });
}

menu.addEventListener('keydown', (e) => {
  const buttons = [...menu.querySelectorAll('button')];
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
