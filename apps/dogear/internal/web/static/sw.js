// Minimal service worker: caches static assets and an offline page only.
// Feeds and API responses are never cached.
// Static URLs carry a ?v=<content hash>, so each version is cached under its
// own URL; bumping CACHE clears entries from older service workers.
const CACHE = 'dogear-v2';
const ASSETS = ['/offline'];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches
      .open(CACHE)
      .then((c) => c.addAll(ASSETS))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))
        )
      )
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== location.origin) return;
  if (url.pathname.startsWith('/static/')) {
    e.respondWith(
      caches.match(req).then((hit) => {
        if (hit !== undefined) return hit;
        return fetch(req).then((resp) => {
          const copy = resp.clone();
          void caches.open(CACHE).then((c) => c.put(req, copy));
          return resp;
        });
      })
    );
    return;
  }
  if (req.mode === 'navigate') {
    e.respondWith(
      fetch(req).catch(() =>
        caches.match('/offline').then((hit) => hit ?? Response.error())
      )
    );
  }
});
