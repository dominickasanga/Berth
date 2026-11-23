// Light by default; the choice is remembered in this browser. Loaded in <head> so the page never flashes the wrong theme.
(function () {
  var root = document.documentElement, KEY = 'berth-theme';
  function saved() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
  function apply(t) { root.setAttribute('data-theme', t); }
  apply(saved() === 'dark' ? 'dark' : 'light');
  document.addEventListener('DOMContentLoaded', function () {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    function label() { btn.title = root.getAttribute('data-theme') === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'; }
    label();
    btn.addEventListener('click', function () {
      var next = root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      apply(next);
      try { localStorage.setItem(KEY, next); } catch (e) {}
      label();
    });
  });
})();
