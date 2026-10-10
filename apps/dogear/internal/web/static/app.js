// Small progressive enhancements. Every page works without this file.
(function () {
  "use strict";
  // Copy buttons: <button data-copy="#id">copy</button>
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var el = document.querySelector(btn.getAttribute("data-copy"));
    if (!el || !navigator.clipboard) return;
    navigator.clipboard.writeText(el.textContent.trim()).then(function () {
      var old = btn.textContent;
      btn.textContent = "copied";
      setTimeout(function () { btn.textContent = old; }, 1200);
    });
  });
  // Register the service worker that caches static assets and the offline page.
  if ("serviceWorker" in navigator) {
    window.addEventListener("load", function () {
      navigator.serviceWorker.register("/sw.js").catch(function () {});
    });
  }
})();
