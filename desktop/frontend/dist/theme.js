// Apply the saved theme before first paint to avoid a color flash ("system" = follow Windows).
try {
  const t = localStorage.getItem('theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch {}
