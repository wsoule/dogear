// Small progressive enhancements. Every page works without this file.
(function () {
  'use strict';

  // ⌘K / Ctrl+K opens the search dialog from any page.
  const dialog = /** @type {HTMLDialogElement | null} */ (
    document.getElementById('cmdk')
  );
  function openSearch() {
    if (dialog === null || typeof dialog.showModal !== 'function') {
      window.location.href = '/search';
      return;
    }
    if (!dialog.open) dialog.showModal();
    const input = dialog.querySelector('input');
    if (input !== null) input.select();
  }
  document.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      openSearch();
    }
  });
  if (dialog !== null) {
    // Clicking the backdrop closes the dialog.
    dialog.addEventListener('click', function (e) {
      if (e.target === dialog) dialog.close();
    });
  }

  document.addEventListener('click', function (e) {
    const target = e.target instanceof Element ? e.target : null;
    if (target === null) return;
    // The nav "search" link opens the dialog instead of navigating.
    if (target.closest('[data-cmdk]') !== null && !e.metaKey && !e.ctrlKey) {
      e.preventDefault();
      openSearch();
      return;
    }
    // Copy buttons: <button data-copy="#id">copy</button>
    const btn = target.closest('[data-copy]');
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

  // WebMCP: when a browser agent submits the search tool, answer with
  // structured results from the JSON API instead of navigating.
  document.addEventListener('submit', function (e) {
    const form = e.target;
    if (!(form instanceof HTMLFormElement)) return;
    if (form.getAttribute('toolname') !== 'search_dogear') return;
    /** @type {SubmitEvent & {agentInvoked?: boolean, respondWith?: (p: Promise<unknown>) => void}} */
    const ev = /** @type {any} */ (e);
    if (ev.agentInvoked !== true || typeof ev.respondWith !== 'function')
      return;
    e.preventDefault();
    const q = new FormData(form).get('q');
    const url =
      '/api/v1/items?limit=10&q=' + encodeURIComponent(String(q ?? ''));
    ev.respondWith(
      fetch(url, { headers: { Accept: 'application/json' } }).then(
        function (r) {
          return r.json();
        }
      )
    );
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
