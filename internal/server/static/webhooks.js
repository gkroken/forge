// Webhooks admin page: register endpoints, send test pings, and inspect the
// per-endpoint delivery trace (recent attempts + dead-letter). Loaded as an
// external script because the page CSP forbids inline handlers.
(function () {
  'use strict';

  function whError(msg) {
    var el = document.getElementById('wh-error');
    if (!el) return;
    el.textContent = msg;
    el.style.display = msg ? 'block' : 'none';
  }

  function addEndpoint(e) {
    e.preventDefault();
    whError('');
    var f = e.target;
    var checked = Array.prototype.slice
      .call(f.querySelectorAll('input[name=event]:checked'))
      .map(function (c) { return c.value; });
    var all = f.querySelectorAll('input[name=event]').length;
    var body = {
      name: f.name.value.trim(),
      url: f.url.value.trim(),
      secret: f.secret.value,
      repo: f.repo.value,
      // All checked = subscribe to everything (empty filter); otherwise the subset.
      events: checked.length === all ? [] : checked,
      enabled: true
    };
    var btn = f.querySelector('button[type=submit]');
    btn.disabled = true; btn.textContent = 'Adding…';
    fetch('/api/v1/webhooks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
      location.reload();
    }).catch(function (err) {
      whError("Couldn't add endpoint: " + err.message);
      btn.disabled = false; btn.textContent = 'Add endpoint';
    });
  }

  function testEndpoint(id, btn) {
    var orig = btn.textContent;
    btn.disabled = true; btn.textContent = 'Sending…';
    fetch('/api/v1/webhooks/' + encodeURIComponent(id) + '/test', { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        btn.textContent = d.ok ? 'Delivered ✓' : 'Failed';
        if (!d.ok) alert('Test delivery failed: ' + (d.error || 'unknown error'));
        setTimeout(function () { btn.disabled = false; btn.textContent = orig; }, 2000);
      })
      .catch(function (err) {
        alert('Test failed: ' + err);
        btn.disabled = false; btn.textContent = orig;
      });
  }

  function deleteEndpoint(id, name, btn) {
    if (!confirm("Delete endpoint '" + name + "'? It will stop receiving events.")) return;
    btn.disabled = true; btn.textContent = 'Deleting…';
    fetch('/api/v1/webhooks/' + encodeURIComponent(id), { method: 'DELETE' })
      .then(function (r) {
        if (!r.ok && r.status !== 204) throw new Error('HTTP ' + r.status);
        location.reload();
      })
      .catch(function (err) { alert('Delete failed: ' + err); btn.disabled = false; btn.textContent = 'Delete'; });
  }

  // --- delivery trace ------------------------------------------------------
  var traceState = { id: null, records: [] };

  function pillClass(s) { return s === 'success' ? 'pill-ok' : s === 'failed' ? 'pill-warn' : 'pill-err'; }

  function relTime(iso) {
    var t = new Date(iso).getTime();
    if (!t) return '';
    var s = Math.max(0, Math.round((Date.now() - t) / 1000));
    if (s < 60) return s + 's ago';
    if (s < 3600) return Math.round(s / 60) + 'm ago';
    if (s < 86400) return Math.round(s / 3600) + 'h ago';
    return Math.round(s / 86400) + 'd ago';
  }

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : s; return d.innerHTML; }

  function showDeliveries(id, name) {
    traceState.id = id;
    document.getElementById('trace-ep').textContent = '· ' + name;
    document.getElementById('trace-deadletter').checked = false;
    var panel = document.getElementById('wh-trace');
    panel.style.display = 'block';
    panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    refreshTrace();
  }

  function refreshTrace() {
    if (!traceState.id) return;
    fetch('/api/v1/webhooks/' + encodeURIComponent(traceState.id) + '/deliveries')
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
      .then(function (recs) { traceState.records = recs || []; renderTrace(); })
      .catch(function (err) {
        document.getElementById('trace-rows').innerHTML =
          '<tr><td colspan="6" class="empty" style="padding:1.5rem;">Couldn\'t load deliveries: ' + esc(err.message) + '</td></tr>';
      });
  }

  function renderTrace() {
    var recs = traceState.records;
    var c = { success: 0, failed: 0, dropped: 0 };
    recs.forEach(function (r) { if (c[r.status] != null) c[r.status]++; });
    document.getElementById('tc-success').textContent = c.success;
    document.getElementById('tc-failed').textContent = c.failed;
    document.getElementById('tc-dropped').textContent = c.dropped;

    var deadOnly = document.getElementById('trace-deadletter').checked;
    var rows = deadOnly ? recs.filter(function (r) { return r.status === 'dropped'; }) : recs;
    var tbody = document.getElementById('trace-rows');
    if (!rows.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty" style="padding:1.5rem;">' +
        (deadOnly ? 'No dropped deliveries. Every event reached this endpoint.'
                  : 'No deliveries yet. Attempts will appear here as events fire.') + '</td></tr>';
      return;
    }
    tbody.innerHTML = rows.map(function (r) {
      return '<tr>' +
        '<td><span class="status-pill ' + pillClass(r.status) + '">' + esc(r.status) + '</span></td>' +
        '<td class="col-mono">' + (r.httpCode ? r.httpCode : '—') + '</td>' +
        '<td class="col-mono">#' + esc(r.attempt) + '</td>' +
        '<td class="col-mono" style="font-size:11.5px;color:var(--text-muted);">' + esc(r.event) + '</td>' +
        '<td style="font-size:12px;color:var(--text-muted);" title="' + esc(r.timestamp) + '">' + esc(relTime(r.timestamp)) + '</td>' +
        '<td class="trace-err" title="' + esc(r.error) + '">' + esc(r.error || '') + '</td>' +
        '</tr>';
    }).join('');
  }

  // --- wiring (event delegation; no inline handlers per CSP) ----------------
  document.addEventListener('DOMContentLoaded', function () {
    var form = document.getElementById('wh-form');
    if (form) form.addEventListener('submit', addEndpoint);

    document.addEventListener('click', function (e) {
      var btn = e.target.closest('[data-action]');
      if (!btn) return;
      var action = btn.getAttribute('data-action');
      var id = btn.getAttribute('data-id');
      if (action === 'deliveries') showDeliveries(id, btn.getAttribute('data-name'));
      else if (action === 'test') testEndpoint(id, btn);
      else if (action === 'delete') deleteEndpoint(id, btn.getAttribute('data-name'), btn);
      else if (action === 'trace-refresh') refreshTrace();
      else if (action === 'trace-close') document.getElementById('wh-trace').style.display = 'none';
    });

    var dead = document.getElementById('trace-deadletter');
    if (dead) dead.addEventListener('change', renderTrace);
  });
})();
