// Confirm dialogs: <form data-confirm="..."> (inline handlers are blocked by our CSP).
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute && e.target.getAttribute('data-confirm');
  if (msg && !confirm(msg)) e.preventDefault();
});

// Live logs over server-sent events, with inline / side panel / full screen views.
(function () {
  var box = document.getElementById('logbox');
  if (!box) return;
  var $ = function (id) { return document.getElementById(id); };
  var start = $('logs-start'), stop = $('logs-stop'), pre = $('logs'), latest = $('logs-latest'),
      side = $('logs-side'), full = $('logs-full'), dock = $('logs-dock');
  var es = null, mode = 'inline', MAX = 2000;

  function atBottom() { return pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40; }
  function toBottom() { pre.scrollTop = pre.scrollHeight; }

  // Follow new lines only while the reader is at the bottom. This is measured from the real
  // scroll position each time a line arrives, so scrolling up reliably pauses following.
  pre.addEventListener('scroll', function () {
    if (atBottom()) latest.hidden = true;
  });

  function setMode(m) {
    mode = m;
    box.classList.remove('side', 'full');
    document.body.classList.remove('logs-side');
    if (m !== 'inline') box.classList.add(m);
    if (m === 'side') document.body.classList.add('logs-side');
    var wasAtBottom = atBottom();
    var shown = !pre.hidden;
    side.hidden = !shown || m === 'side';
    full.hidden = !shown || m === 'full';
    dock.hidden = !shown || m === 'inline';
    if (wasAtBottom) requestAnimationFrame(toBottom);
  }

  function add(line) {
    var stick = atBottom();
    pre.appendChild(document.createTextNode(line + '\n')); // append only: never rebuild the text (that resets scroll)
    while (pre.childNodes.length > MAX) pre.removeChild(pre.firstChild);
    if (stick) toBottom(); else latest.hidden = false;
  }

  function end() {
    if (es) { es.close(); es = null; add('[stream ended]'); }
    start.hidden = false; stop.hidden = true;
  }

  start.addEventListener('click', function () {
    pre.hidden = false; pre.textContent = ''; latest.hidden = true;
    start.hidden = true; stop.hidden = false;
    setMode(mode);
    es = new EventSource(start.getAttribute('data-url'));
    es.onmessage = function (m) { add(m.data); };
    es.addEventListener('end', end);
    es.onerror = end;
  });
  stop.addEventListener('click', end);
  side.addEventListener('click', function () { setMode('side'); });
  full.addEventListener('click', function () { setMode('full'); });
  dock.addEventListener('click', function () { setMode('inline'); });
  latest.addEventListener('click', function () { latest.hidden = true; toBottom(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape' && mode !== 'inline') setMode('inline'); });
  if (start.hasAttribute('data-autostart')) start.click(); // the Berth Logs page starts streaming on load
  // The "Logs" button in the Status card (re-rendered every 10s, so listen on the document).
  document.addEventListener('click', function (e) {
    if (!e.target.closest || !e.target.closest('[data-open-logs]')) return;
    if (!start.hidden) start.click(); // not streaming yet: start
    setMode('side');
  });
})();

// Deploy logs are re-fetched every 2s while a deploy runs and swapped in by htmx, which would
// reset the scroll to the top. Keep the reader's position, or stay at the end if they were there.
document.addEventListener('htmx:beforeSwap', function (e) {
  var t = e.detail.target, p = t && t.querySelector && t.querySelector('pre');
  if (p) t._keep = { top: p.scrollTop, bottom: p.scrollHeight - p.scrollTop - p.clientHeight < 40 };
});
document.addEventListener('htmx:afterSwap', function (e) {
  var t = e.detail.target, p = t && t.querySelector && t.querySelector('pre');
  if (!p) return;
  p.scrollTop = (!t._keep || t._keep.bottom) ? p.scrollHeight : t._keep.top;
});

// Highlight the current section in the top navigation.
(function () {
  var base = document.documentElement.getAttribute('data-base') || '';
  var path = location.pathname.slice(base.length) || '/';
  document.querySelectorAll('header nav a').forEach(function (a) {
    var href = a.getAttribute('href').slice(base.length) || '/';
    var here = href === '/' ? (path === '/' || path.indexOf('/apps') === 0) : path.indexOf(href) === 0;
    if (here) a.setAttribute('aria-current', 'page');
  });
})();

// Service form: show only the chosen kind's settings (and fill in its usual port when adding).
(function () {
  var sel = document.getElementById('kindsel');
  var kindSource = sel || document.querySelector('#svcform input[name=kind]');
  if (!kindSource) return;
  var port = document.getElementById('portin');
  function show() {
    document.querySelectorAll('.kindbox').forEach(function (b) {
      var on = b.getAttribute('data-kind') === kindSource.value;
      b.hidden = !on;
      // only the visible kind's fields are submitted, so another kind's empty fields cannot override them
      b.querySelectorAll('input').forEach(function (i) { i.disabled = !on; });
    });
  }
  if (sel) sel.addEventListener('change', function () {
    var p = sel.options[sel.selectedIndex].getAttribute('data-port');
    if (port && p && p !== '0') port.value = p;
    show();
  });
  show();
})();
