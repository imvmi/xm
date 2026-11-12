'use strict';
(() => {
  const dialog = document.getElementById('pwa-dialog');
  const help = document.getElementById('pwa-install-help');
  const install = document.getElementById('pwa-install');
  const update = document.getElementById('pwa-update');
  const updateButton = document.getElementById('pwa-update-button');
  const standalone = window.matchMedia('(display-mode: standalone)');
  let prompt = null;
  let registration = null;
  let reloadOnUpdate = false;
  let installing = false;
  let installed = false;

  function renderInstall() {
    const running = installed || standalone.matches || navigator.standalone === true;
    document.querySelectorAll('[data-pwa-open]').forEach(button => {
      button.textContent = running ? '应用已安装' : '安装应用';
    });
    install.hidden = running || !prompt;
    install.disabled = installing;
    if (running) help.textContent = 'XM 已作为应用打开或安装，可从主屏幕或应用列表再次访问。';
    else if (!window.isSecureContext) help.textContent = '请通过 HTTPS 地址访问后安装。在本机使用时，也可以通过 localhost 或 127.0.0.1 访问。';
    else if (prompt) help.textContent = '安装后可从桌面或主屏幕直接打开 XM，使用独立窗口浏览档案。';
    else if (/iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)) help.textContent = '在 Safari 中打开此页面，轻点「分享」，选择「添加到主屏幕」，然后轻点「添加」。';
    else help.textContent = '在浏览器菜单中选择「安装应用」或「添加到主屏幕」。如果没有此选项，请使用支持安装的浏览器打开。';
  }
  window.addEventListener('beforeinstallprompt', event => {
    event.preventDefault();
    prompt = event;
    renderInstall();
  });
  window.addEventListener('appinstalled', () => {
    installed = true;
    prompt = null;
    renderInstall();
  });
  standalone.addEventListener('change', renderInstall);
  document.querySelectorAll('[data-pwa-open]').forEach(button => {
    button.addEventListener('click', () => { renderInstall(); dialog.showModal(); });
  });
  document.getElementById('pwa-close').addEventListener('click', () => dialog.close());
  install.addEventListener('click', async () => {
    if (!prompt || installing) return;
    const pending = prompt;
    installing = true;
    renderInstall();
    try {
      await pending.prompt();
      const choice = await pending.userChoice;
      if (choice.outcome === 'accepted') dialog.close();
    } catch (_) {
      // The native prompt may be dismissed or no longer available.
    } finally {
      prompt = null;
      installing = false;
      renderInstall();
    }
  });
  function connectivity() {
    document.getElementById('pwa-offline').hidden = navigator.onLine;
  }
  window.addEventListener('online', connectivity);
  window.addEventListener('offline', connectivity);
  connectivity();
  renderInstall();

  if (!window.isSecureContext || !('serviceWorker' in navigator)) return;
  document.getElementById('pwa-update-later').addEventListener('click', () => { update.hidden = true; });
  function showUpdate() {
    update.hidden = !(registration?.waiting && navigator.serviceWorker.controller);
  }
  updateButton.addEventListener('click', () => {
    if (reloadOnUpdate) { location.reload(); return; }
    if (!registration?.waiting) return;
    reloadOnUpdate = true;
    updateButton.disabled = true;
    registration.waiting.postMessage({type:'SKIP_WAITING'});
  });
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (reloadOnUpdate) {
      // Keep retry available if the existing unsaved-settings guard cancels reload.
      updateButton.disabled = false;
      updateButton.textContent = '刷新应用';
      location.reload();
    }
    else showUpdate();
  });
  navigator.serviceWorker.register('/sw.js', {scope:'/', updateViaCache:'none'}).then(value => {
    registration = value;
    showUpdate();
    registration.addEventListener('updatefound', () => {
      registration.installing?.addEventListener('statechange', showUpdate);
    });
    // Returning to an installed app may not trigger a navigation.
    let lastCheck = Date.now();
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && navigator.onLine && Date.now() - lastCheck > 60 * 60 * 1000) {
        lastCheck = Date.now();
        registration.update().catch(() => {});
      }
    });
  }).catch(() => {
    document.getElementById('pwa-cache-note').textContent = '离线页面暂未准备好，请联网后重新打开 XM。浏览器访问不受影响。';
  });
})();
