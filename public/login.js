const form = document.getElementById('login-form');
const msg = document.getElementById('login-msg');
form.addEventListener('submit', async (e) => {
  e.preventDefault();
  msg.textContent = '';
  const body = Object.fromEntries(new FormData(form));
  const res = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (res.ok) location.href = '/';
  else msg.textContent = (await res.json().catch(() => ({}))).error || 'Đăng nhập thất bại';
});
