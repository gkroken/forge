// Cleanup admin pages (policy list, policy editor, per-repo run). Runs the
// policy/repo cleanup actions that used to live in inline <script> blocks —
// dead under the page CSP (script-src 'self'). Wiring is delegated on
// data-action so one file serves all three pages; handlers no-op when their
// buttons aren't present.
(function () {
  'use strict';

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  // ── cleanup_policies.html: dry-run / run a named policy across its repos ──
  function runPolicy(name, dry, btn) {
    if (!dry && !confirm("Run cleanup policy '" + name + "' now? Matching artifacts will be permanently deleted from every repository it is applied to.")) return;
    var orig = btn.textContent;
    btn.disabled = true;
    btn.textContent = dry ? 'Previewing…' : 'Running…';
    fetch('/api/v1/cleanup-policies/' + encodeURIComponent(name) + '/run?dry=' + dry, { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var mb = (d.total_freed / 1048576).toFixed(2);
        var verb = dry ? 'would delete' : 'deleted';
        alert("Policy '" + name + "' across " + d.repos.length + " repo(s): " + verb + " " +
          d.total_deleted + " artifact(s), " + mb + " MB" + (dry ? " (dry-run, nothing removed)" : "") + ".");
        if (!dry) location.reload();
      })
      .catch(function (e) { alert('Run failed: ' + e); })
      .finally(function () { btn.disabled = false; btn.textContent = orig; });
  }

  // ── cleanup_policy_form.html: delete a policy ────────────────────────────
  function deletePolicy(name) {
    if (!confirm("Delete policy '" + name + "'?")) return;
    fetch('/ui/admin/cleanup-policies/' + encodeURIComponent(name), { method: 'DELETE' })
      .then(function (r) { if (r.ok) location.href = '/ui/admin/cleanup-policies'; });
  }

  // ── cleanup_run.html: per-repo dry-run / live run ────────────────────────
  function runDry(name, btn) {
    btn.disabled = true;
    var status = document.getElementById('dry-run-status');
    var result = document.getElementById('dry-run-result');
    status.textContent = 'Running…';
    result.innerHTML = '';
    fetch('/api/v1/repos/' + encodeURIComponent(name) + '/cleanup?dry=true', { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var cands = d.candidates || [];
        status.textContent = cands.length + ' candidate(s) found';
        if (cands.length === 0) {
          result.innerHTML = '<p style="font-size:.85em;color:var(--text-muted)">Nothing to clean up.</p>';
          return;
        }
        var html = '<div class="admin-table-wrap"><table class="admin-table"><thead><tr>' +
          '<th>Component</th><th>Version</th><th>Age (days)</th><th>Size</th><th>Reason</th>' +
          '</tr></thead><tbody>';
        cands.forEach(function (c) {
          html += '<tr><td>' + esc(c.component) + '</td><td class="col-mono">' + esc(c.version) +
            '</td><td>' + c.age_days + '</td><td>' +
            (c.size_bytes / 1048576).toFixed(2) + ' MB</td><td style="font-size:.85em;color:var(--text-muted)">' +
            esc(c.reason) + '</td></tr>';
        });
        html += '</tbody></table></div>';
        result.innerHTML = html;
      })
      .catch(function (e) { status.textContent = 'Error: ' + e; })
      .finally(function () { btn.disabled = false; });
  }

  function runLive(name, btn) {
    if (!confirm('Permanently delete all artifacts matching this policy in "' + name + '"? This cannot be undone.')) return;
    var dryBtn = document.getElementById('dry-run-btn');
    btn.disabled = true; if (dryBtn) dryBtn.disabled = true;
    var status = document.getElementById('dry-run-status');
    var result = document.getElementById('dry-run-result');
    status.textContent = 'Running…';
    result.innerHTML = '';
    fetch('/api/v1/repos/' + encodeURIComponent(name) + '/cleanup', { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        status.textContent = 'Deleted ' + (d.deleted || 0) + ' · freed ' +
          ((d.freed_bytes || 0) / 1048576).toFixed(2) + ' MB — refreshing…';
        setTimeout(function () { location.reload(); }, 900);
      })
      .catch(function (e) {
        status.textContent = 'Error: ' + e;
        btn.disabled = false; if (dryBtn) dryBtn.disabled = false;
      });
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action]');
    if (!btn) return;
    var action = btn.getAttribute('data-action');
    if (action === 'policy-run') runPolicy(btn.getAttribute('data-policy'), btn.getAttribute('data-dry') === 'true', btn);
    else if (action === 'policy-delete') deletePolicy(btn.getAttribute('data-policy'));
    else if (action === 'cleanup-dry') runDry(btn.getAttribute('data-repo'), btn);
    else if (action === 'cleanup-live') runLive(btn.getAttribute('data-repo'), btn);
  });
})();
