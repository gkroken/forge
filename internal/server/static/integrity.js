// integrity.js — verify buttons on the /ui/admin/integrity rollup page.
// CSP-safe: external file, data-attribute delegation, no inline handlers.
(function () {
  var root = document.getElementById('integrity-root');
  if (!root) return;

  var toastContainer;
  function toast(msg, type) {
    if (!toastContainer) {
      toastContainer = document.createElement('div');
      toastContainer.className = 'toast-container';
      document.body.appendChild(toastContainer);
    }
    var t = document.createElement('div');
    t.className = 'toast toast-' + (type || 'ok');
    t.textContent = msg;
    toastContainer.appendChild(t);
    setTimeout(function () { t.remove(); }, 4000);
  }

  // Poll one repo's report until it leaves queued/running, then reload the
  // page so the server re-renders the row. Bounded: after ~2 minutes we stop
  // and leave the "running…" chip for a manual refresh.
  function pollUntilDone(repo, attempts) {
    if (attempts <= 0) { toast(repo + ': still running — refresh later', 'ok'); return; }
    setTimeout(function () {
      fetch('/api/v1/repos/' + encodeURIComponent(repo) + '/verify')
        .then(function (r) { return r.json(); })
        .then(function (rep) {
          if (rep.status === 'queued' || rep.status === 'running') {
            pollUntilDone(repo, attempts - 1);
          } else {
            window.location.reload();
          }
        })
        .catch(function () { pollUntilDone(repo, attempts - 1); });
    }, 2500);
  }

  root.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action="verify"]');
    if (!btn) return;
    var repo = btn.dataset.repo, mode = btn.dataset.mode || 'full';
    btn.disabled = true;
    btn.textContent = 'Verifying…';
    fetch('/api/v1/repos/' + encodeURIComponent(repo) + '/verify?mode=' + mode, { method: 'POST' })
      .then(function (r) {
        if (r.status !== 202) throw new Error('HTTP ' + r.status);
        toast('Verify enqueued for ' + repo);
        pollUntilDone(repo, 48);
      })
      .catch(function (err) {
        toast('Verify failed: ' + err.message, 'err');
        btn.disabled = false;
        btn.textContent = mode === 'quick' ? 'Quick check' : 'Verify';
      });
  });
})();
