// Confirm dialogs: <form data-confirm="..."> (inline handlers are blocked by our CSP).
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute && e.target.getAttribute('data-confirm');
  if (msg && !confirm(msg)) e.preventDefault();
});

// Log colouring. Every log line becomes one <span> whose class says what it is (error, warning,
// ok, debug, section); the colours come from the theme variables in app.css, so light and dark both
// work. Text only goes in through textContent, so a log line can never inject markup.
var berthLog = (function () {
  var TS = /^(\d{4}[-\/]\d\d[-\/]\d\d[T ]\d\d:\d\d:\d\d(?:[.,]\d+)?Z?)(?=\s)/;
  var ANSI = /\x1b\[[0-9;]*[A-Za-z]/g;
  function kind(line) {
    if (/^==\s.+\s==$/.test(line)) return 'll-sec';                                   // Berth's "== build =="
    if (/\b(DEPLOY FAILED|ERROR|FATAL|PANIC|CRITICAL)\b/.test(line) || /\blevel=(error|fatal|panic)\b/i.test(line)) return 'll-err';
    if (/\b(WARN|WARNING)\b/.test(line) || /\blevel=warn/i.test(line)) return 'll-warn';
    if (/\bDEPLOY OK\b/.test(line)) return 'll-ok';
    if (/\b(failed|failure|unhealthy|refused|denied)\b(?!=0\b)/i.test(line)) return 'll-err'; // "failed=0" is not a failure
    if (/\[GIN-debug\]|\bDEBUG\b/.test(line) || /\blevel=debug/i.test(line)) return 'll-dim';
    if (/^#\d+ /.test(line)) return 'll-dim';                                         // docker build step noise
    if (/\b(Healthy|Successfully)\b/.test(line)) return 'll-ok';
    return '';
  }
  function line(text) {
    text = String(text).replace(ANSI, '');
    var span = document.createElement('span'), cls = kind(text), m = TS.exec(text);
    if (cls) span.className = cls;
    if (m) {
      var ts = document.createElement('span');
      ts.className = 'll-ts';
      ts.textContent = m[1];
      span.appendChild(ts);
      span.appendChild(document.createTextNode(text.slice(m[1].length) + '\n'));
    } else {
      span.textContent = text + '\n';
    }
    return span;
  }
  // Re-draw a <pre> that holds plain text (a deploy log) as coloured lines.
  function paint(pre) {
    var text = pre.textContent;
    if (!text) return;
    var lines = text.split('\n'), frag = document.createDocumentFragment();
    if (lines[lines.length - 1] === '') lines.pop();
    lines.forEach(function (l) { frag.appendChild(line(l)); });
    pre.textContent = '';
    pre.appendChild(frag);
  }
  return { kind: kind, line: line, paint: paint };
})();

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
    pre.appendChild(berthLog.line(line)); // append only: never rebuild the text (that resets scroll)
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
  if (t.closest('[data-term]')) berthLog.paint(p); // colour the deploy output
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

// Deploy terminal: a button that takes one deploy's output over the whole window (Escape or the
// same button to leave). The button sits outside the part htmx re-fetches, so it survives the refresh.
(function () {
  function setFull(term, on) {
    term.classList.toggle('full', on);
    document.body.classList.toggle('term-full', on);
    var label = term.querySelector('[data-term-full] .term-label');
    if (label) label.textContent = on ? 'Exit full screen' : 'Full screen';
  }
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-term-full]');
    if (!b) return;
    var term = b.closest('[data-term]');
    setFull(term, !term.classList.contains('full'));
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    var open = document.querySelector('[data-term].full');
    if (open) setFull(open, false);
  });
})();
