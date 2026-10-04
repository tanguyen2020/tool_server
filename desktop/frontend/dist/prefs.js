// View preferences (chosen view, sort, last tab, last folder…). Saved in settings.json through the Go side,
// because the WebView's own storage loses recent writes when the app is killed; localStorage is kept as a
// fast local copy and as the source for values read before the settings arrive.
import { call } from './bridge.js';

let saved = null; // from settings.json, set at start-up

export function initPrefs(fromSettings) {
  saved = { ...(fromSettings || {}) };
}

export function getPref(key, fallback = null) {
  if (saved && Object.hasOwn(saved, key)) return saved[key];
  try {
    const v = localStorage.getItem(key);
    if (v != null) return v;
  } catch {}
  return fallback;
}

export function getJSONPref(key, fallback = {}) {
  try {
    const v = getPref(key);
    return v ? JSON.parse(v) : fallback;
  } catch {
    return fallback;
  }
}

const timers = new Map();
export function setPref(key, value) {
  const v = value == null ? '' : String(value);
  if (saved) saved[key] = v;
  try { localStorage.setItem(key, v); } catch {}
  // Coalesce quick changes (typing, dragging) into one write.
  clearTimeout(timers.get(key));
  timers.set(key, setTimeout(() => { timers.delete(key); call('SetUIPref', key, v).catch(() => {}); }, 300));
}

export const setJSONPref = (key, value) => setPref(key, JSON.stringify(value));
