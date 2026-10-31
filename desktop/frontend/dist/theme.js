// Apply the saved theme (light or dark) before first paint to avoid a color flash; none saved yet = what Windows uses.
let t = null;
try { t = localStorage.getItem('theme'); } catch {}
document.documentElement.dataset.theme = t === 'light' || t === 'dark' ? t : (window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
