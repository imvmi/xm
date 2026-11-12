'use strict';
const CACHE_PREFIX = 'xm-pwa-';
const CACHE_NAME = CACHE_PREFIX + '__XM_REVISION__';
// Only public recovery assets are persisted. Never cache login HTML, API
// responses, archives, avatars or media, including after logout.
const OFFLINE_ASSETS = [
  '/offline.html', '/assets/offline.css', '/assets/offline.js',
  '/assets/favicon.svg', '/assets/icon-192.png', '/assets/icon-512.png',
  '/assets/icon-maskable-512.png', '/assets/apple-touch-icon.png'
];
self.addEventListener('install', event => {
  event.waitUntil(caches.open(CACHE_NAME).then(cache => cache.addAll(
    OFFLINE_ASSETS.map(url => new Request(url, {cache: 'reload'}))
  )));
});
self.addEventListener('activate', event => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter(key => key.startsWith(CACHE_PREFIX) && key !== CACHE_NAME).map(key => caches.delete(key)));
    await self.clients.claim();
  })());
});
self.addEventListener('message', event => {
  if (event.data?.type === 'SKIP_WAITING') self.skipWaiting();
});
self.addEventListener('fetch', event => {
  const request = event.request;
  const url = new URL(request.url);
  if (request.method !== 'GET' || url.origin !== self.location.origin) return;
  if (request.mode === 'navigate' && url.pathname === '/') {
    event.respondWith((async () => {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 8000);
      try {
        const response = await fetch(request, {signal:controller.signal});
        if (response.status < 500) return response;
      } catch (_) { /* An unavailable server uses the same recovery screen. */ }
      finally { clearTimeout(timer); }
      const cache = await caches.open(CACHE_NAME);
      return await cache.match('/offline.html') || Response.error();
    })());
  } else if (OFFLINE_ASSETS.includes(url.pathname) && !url.search) {
    event.respondWith(caches.open(CACHE_NAME).then(async cache => await cache.match(request) || fetch(request)));
  }
});
