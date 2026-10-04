// Áp theme đã chọn trước khi vẽ trang để không bị nháy màu.
try {
  const t = localStorage.getItem('theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch {}
