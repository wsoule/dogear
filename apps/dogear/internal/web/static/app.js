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

  // Register the service worker that caches static assets and the offline page.
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function () {
        // Offline support is optional.
      });
    });
  }
})();
