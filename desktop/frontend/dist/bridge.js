// Bridge to the Go backend (Wails): call Go methods and receive realtime events.
async function ready() {
  for (let i = 0; i < 100 && !window.go?.main?.App; i++) await new Promise((r) => setTimeout(r, 20));
}

export async function call(method, ...args) {
  await ready();
  try {
    return await window.go.main.App[method](...args);
  } catch (err) {
    throw new Error(typeof err === 'string' ? err : err?.message || String(err));
  }
}

export async function on(event, cb) {
  await ready();
  window.runtime.EventsOn(event, cb);
}

// In-app confirmation dialog (see ui.js): resolves true when the user confirms.
export const confirmDialog = (message, title = 'Confirm', opts = {}) => import('./ui.js').then((m) => m.confirmModal(message, title, opts));

export async function copyText(text) {
  await ready();
  return window.runtime.ClipboardSetText(text);
}
