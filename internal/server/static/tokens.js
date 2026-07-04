// tokens.js — grant builder on the token create form (CSP-safe, no inline JS).
// Rows submit as g{N}_repo / g{N}_actions / g{N}_selectors; indices may end up
// sparse after removals (the server scans keys, so no re-indexing needed).
(function () {
  'use strict';

  var rows = document.getElementById('grant-rows');
  var tpl = document.getElementById('grant-row-tpl');
  if (!rows || !tpl) return;

  var SEL_PLACEHOLDER = 'Optional content selectors, e.g. com/acme/**, @acme/**';

  function nextIndex() {
    var max = -1;
    rows.querySelectorAll('[data-grant-row] select').forEach(function (sel) {
      var m = /^g(\d+)_repo$/.exec(sel.name);
      if (m) max = Math.max(max, parseInt(m[1], 10));
    });
    return max + 1;
  }

  // Admin is repo-wide: while it is checked, the selector input is disabled
  // (and therefore not submitted), matching the server-side validation.
  function syncAdmin(row) {
    var admin = row.querySelector('input[type="checkbox"][value="admin"]');
    var sel = row.querySelector('.grant-selectors');
    if (!admin || !sel) return;
    sel.disabled = admin.checked;
    sel.placeholder = admin.checked
      ? 'admin covers the whole repository — selectors do not apply'
      : SEL_PLACEHOLDER;
  }

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action]');
    if (!btn) return;
    if (btn.dataset.action === 'add-grant') {
      var holder = document.createElement('div');
      holder.innerHTML = tpl.innerHTML.replace(/__I__/g, String(nextIndex()));
      var row = holder.querySelector('[data-grant-row]');
      rows.appendChild(row);
      syncAdmin(row);
    } else if (btn.dataset.action === 'remove-grant') {
      var target = btn.closest('[data-grant-row]');
      if (target && rows.querySelectorAll('[data-grant-row]').length > 1) {
        target.remove();
      }
    }
  });

  document.addEventListener('change', function (e) {
    if (e.target.matches('input[type="checkbox"][value="admin"]')) {
      var row = e.target.closest('[data-grant-row]');
      if (row) syncAdmin(row);
    }
  });

  rows.querySelectorAll('[data-grant-row]').forEach(syncAdmin);
})();
