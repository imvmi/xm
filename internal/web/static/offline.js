'use strict';
(() => {
  const button = document.getElementById('offline-retry');
  const status = document.getElementById('offline-status');
  async function reconnect() {
    if (button.disabled) return;
    button.disabled = true;
    status.textContent = '正在连接…';
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch('/healthz', {cache:'no-store', signal:controller.signal});
      if (!response.ok) throw new Error('unavailable');
      if (location.pathname === '/offline.html') location.replace('/' + location.hash);
      else location.reload();
    } catch (_) {
      status.textContent = '仍无法连接，请稍后重试。';
    } finally {
      clearTimeout(timer);
      button.disabled = false;
    }
  }
  button.addEventListener('click', reconnect);
  window.addEventListener('online', reconnect);
})();
