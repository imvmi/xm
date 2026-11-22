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

  // Installed mobile apps manage their own bottom bar; browser chrome cannot
  // collapse this navigation for us. Keep desktop and normal browser behavior.
  const mobile = window.matchMedia('(max-width: 760px)');
  const nav = document.querySelector('.sidebar nav');
  const app = document.getElementById('app');
  const root = document.documentElement;
  let lastScroll = 0, travel = 0, scrollFrame = 0;
  function scrollPosition() {
    const max = Math.max(0, document.documentElement.scrollHeight - innerHeight);
    return Math.max(0, Math.min(scrollY, max)); // Ignore iOS rubber-band overscroll.
  }
  function autoHideNavigation() {
    return mobile.matches && (standalone.matches || navigator.standalone === true) && !app.hidden;
  }
  function showNavigation() {
    root.classList.remove('pwa-nav-hidden');
    nav.inert = false;
    lastScroll = scrollPosition();
    travel = 0;
  }
  function resetNavigation() {
    cancelAnimationFrame(scrollFrame);
    scrollFrame = 0;
    root.classList.toggle('pwa-nav-auto-hide', autoHideNavigation());
    showNavigation();
  }
  function updateNavigation() {
    scrollFrame = 0;
    const y = scrollPosition(), delta = y - lastScroll;
    lastScroll = y;
    if (!autoHideNavigation() || y < 64 || document.querySelector('dialog[open],.video-expanded') ||
        (nav.contains(document.activeElement) && document.activeElement.matches(':focus-visible'))) {
      showNavigation();
      return;
    }
    if (!delta) return;
    travel = Math.sign(delta) === Math.sign(travel) ? travel + delta : delta;
    if (travel <= -16) showNavigation();
    else if (travel >= 32 && y > 96) {
      root.classList.add('pwa-nav-hidden');
      nav.inert = true;
      travel = 0;
    }
  }
  window.addEventListener('scroll', () => {
    if (!scrollFrame) scrollFrame = requestAnimationFrame(updateNavigation);
  }, {passive:true});
  window.addEventListener('hashchange', resetNavigation);
  window.addEventListener('resize', resetNavigation);
  window.addEventListener('pageshow', resetNavigation);
  mobile.addEventListener('change', resetNavigation);
  standalone.addEventListener('change', resetNavigation);
  document.addEventListener('keydown', event => { if (event.key === 'Tab') showNavigation(); });
  nav.addEventListener('focusin', showNavigation);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) resetNavigation(); });
  new MutationObserver(resetNavigation).observe(app, {attributes:true, attributeFilter:['hidden']});
  resetNavigation();

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
