// Confirm dialogs: <form data-confirm="..."> (inline handlers are blocked by our CSP).
document.addEventListener('submit', function (e) {
  var msg = e.target.getAttribute && e.target.getAttribute('data-confirm');
  if (msg && !confirm(msg)) e.preventDefault();
});

// Live logs over server-sent events.
(function () {
  var start = document.getElementById('logs-start');
  if (!start) return;
  var stop = document.getElementById('logs-stop'), pre = document.getElementById('logs'), es = null;
  function end() {
    if (es) { es.close(); es = null; }
    start.hidden = false; stop.hidden = true;
  }
  start.addEventListener('click', function () {
    pre.hidden = false; pre.textContent = ''; start.hidden = true; stop.hidden = false;
    es = new EventSource(start.getAttribute('data-url'));
    es.onmessage = function (m) {
      pre.textContent += m.data + '\n';
      var lines = pre.textContent.split('\n');
      if (lines.length > 2000) pre.textContent = lines.slice(-2000).join('\n');
      pre.scrollTop = pre.scrollHeight;
    };
    es.addEventListener('end', end);
    es.onerror = end;
  });
  stop.addEventListener('click', end);
})();
