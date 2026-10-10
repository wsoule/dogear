// Small progressive enhancements. Every page works without this file.
(function () {
  'use strict';
  // Copy buttons: <button data-copy="#id">copy</button>
  document.addEventListener('click', function (e) {
    const target = /** @type {Element | null} */ (e.target);
    const btn =
      target instanceof Element ? target.closest('[data-copy]') : null;
    if (btn === null) return;
    const el = document.querySelector(btn.getAttribute('data-copy') ?? '');
    if (el === null || el.textContent === null) return;
    const text = el.textContent.trim();
    void navigator.clipboard.writeText(text).then(function () {
      const old = btn.textContent;
      btn.textContent = 'copied';
      setTimeout(function () {
        btn.textContent = old;
      }, 1200);
    });
  });
  // Register the service worker that caches static assets and the offline page.
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function () {
        // Offline support is optional.
      });
    });
  }
})();
