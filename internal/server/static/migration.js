// migration.js — the /ui/admin/migration console.
// CSP-safe: external file, data-attribute delegation, no inline handlers.
// The page has three states, all hydrated from GET /api/v1/migration:
//   1. no plan   → connect form only
//   2. plan      → plan tables + Apply button
//   3. run       → same tables showing live per-repo progress (polled)
(function () {
  var root = document.getElementById('migration-root');
  if (!root) return;
  var canApply = root.dataset.canApply === '1';

  var el = function (id) { return document.getElementById(id); };
  var pollTimer = null;

  function esc(s) {
    var d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }

  function chip(label, cls) {
    return '<span class="chip ' + cls + '">' + esc(label) + '</span>';
  }

  function actionChip(action) {
    switch (action) {
      case 'create': return chip('create', 'chip-ok');
      case 'exists': return chip('reuse', 'chip-neutral');
      case 'skip': return chip('skip', 'chip-warn');
      default: return chip(action || '—', 'chip-neutral');
    }
  }

  function grantText(g) {
    var s = g.repo + ': ' + (g.actions || []).join(',');
    if (g.selectors && g.selectors.length) s += ' [' + g.selectors.join(', ') + ']';
    return s;
  }

  function renderRepos(st) {
    var plan = st.plan;
    var states = {};
    (st.repos || []).forEach(function (r) { states[r.repo] = r; });
    var rows = (plan.repos || []).map(function (rp) {
      var s = states[rp.target] || null;
      var progress = '—', verify = '—', notes = rp.reason || '';
      if (rp.action === 'skip') {
        progress = '';
        verify = '';
      } else if (s) {
        var done = (s.migrated || 0) + (s.skipped || 0);
        if (s.status === 'running') {
          progress = chip('running', 'chip-neutral') + ' <span class="col-mono">' + done + (s.sourceAssets ? '/' + s.sourceAssets : '') + '</span>';
        } else if (s.status === 'failed') {
          progress = chip(s.failed + ' failed', 'chip-err') + ' <span class="col-mono">' + done + '/' + (s.sourceAssets || '?') + '</span>';
          if (s.failures && s.failures.length) {
            notes = s.failures.slice(0, 3).map(function (f) { return f.path + ': ' + f.error; }).join(' · ');
          }
        } else if (s.status === 'complete') {
          var countsOK = s.sourceAssets === 0 || done >= s.sourceAssets;
          progress = chip(countsOK ? 'counts match' : done + '/' + s.sourceAssets, countsOK ? 'chip-ok' : 'chip-warn') +
            ' <span class="col-mono">' + s.migrated + ' new · ' + s.skipped + ' kept</span>';
          notes = s.note || notes;
        }
        if (s.verifyEnqueued) {
          verify = '<a href="/ui/admin/repos/' + encodeURIComponent(rp.target) + '/edit?tab=integrity" data-verify-repo="' + esc(rp.target) + '">' + chip('…', 'chip-neutral') + '</a>';
        }
      }
      return '<tr>' +
        '<td>' + esc(rp.source) + (rp.target && rp.target !== rp.source ? ' → ' + esc(rp.target) : '') + '</td>' +
        '<td><span class="badge badge-' + esc(rp.targetFormat || rp.sourceFormat) + '">' + esc(rp.targetFormat || rp.sourceFormat) + '</span></td>' +
        '<td><span class="badge badge-' + esc(rp.targetKind || rp.sourceType) + '">' + esc(rp.targetKind || rp.sourceType) + '</span></td>' +
        '<td>' + actionChip(rp.action) + '</td>' +
        '<td class="col-num col-mono">' + (rp.assets ? rp.assets : '—') + '</td>' +
        '<td>' + progress + '</td>' +
        '<td data-verify-cell="' + esc(rp.target) + '">' + verify + '</td>' +
        '<td style="font-size:12px;color:var(--text-muted);">' + esc(notes) + '</td>' +
        '</tr>';
    }).join('');
    el('mig-repo-rows').innerHTML = rows || '<tr><td colspan="8" style="text-align:center;padding:1.5rem;color:var(--text-muted);">Nothing in the plan.</td></tr>';
    el('mig-repos').style.display = '';
    fillVerifyChips(st);
  }

  // Verify chips resolve asynchronously from each repo's integrity report.
  function fillVerifyChips(st) {
    (st.repos || []).forEach(function (s) {
      if (!s.verifyEnqueued) return;
      var cell = document.querySelector('[data-verify-cell="' + CSS.escape(s.repo) + '"]');
      if (!cell) return;
      fetch('/api/v1/repos/' + encodeURIComponent(s.repo) + '/verify')
        .then(function (r) { return r.json(); })
        .then(function (rep) {
          var c;
          if (rep.status === 'complete') {
            c = rep.totalFindings === 0 ? chip('intact', 'chip-ok') : chip(rep.totalFindings + ' findings', 'chip-err');
          } else if (rep.status === 'failed') {
            c = chip('verify failed', 'chip-err');
          } else {
            c = chip(rep.status || 'queued', 'chip-neutral');
          }
          cell.innerHTML = '<a href="/ui/admin/repos/' + encodeURIComponent(s.repo) + '/edit?tab=integrity">' + c + '</a>';
        })
        .catch(function () {});
    });
  }

  function renderSecurity(st) {
    var sec = st.plan && st.plan.security;
    if (!sec) { el('mig-security').style.display = 'none'; return; }
    el('mig-role-rows').innerHTML = (sec.roles || []).map(function (r) {
      var grants = (r.grants || []).map(function (g) { return '<code>' + esc(grantText(g)) + '</code>'; }).join('<br>');
      var missed = (r.notes || []).map(function (n) { return esc(n.privilege) + ' — ' + esc(n.reason); }).join('<br>');
      return '<tr><td>' + esc(r.id) + '</td><td>' + actionChip(r.action) + '</td>' +
        '<td style="font-size:12px;">' + (grants || '<span style="color:var(--text-muted);">' + esc(r.reason || '—') + '</span>') + '</td>' +
        '<td style="font-size:12px;color:var(--text-muted);">' + (missed || '—') + '</td></tr>';
    }).join('') || '<tr><td colspan="4" style="text-align:center;color:var(--text-muted);padding:1rem;">No roles found.</td></tr>';

    el('mig-user-rows').innerHTML = (sec.users || []).map(function (u) {
      return '<tr><td>' + esc(u.username) + (u.displayName ? ' <span style="color:var(--text-muted);">(' + esc(u.displayName) + ')</span>' : '') + '</td>' +
        '<td>' + actionChip(u.action) + '</td><td>' + esc(u.role || '—') + '</td>' +
        '<td style="font-size:12px;color:var(--text-muted);">' + esc(u.reason || '') + '</td></tr>';
    }).join('') || '<tr><td colspan="4" style="text-align:center;color:var(--text-muted);padding:1rem;">No users found.</td></tr>';

    var notes = (sec.notes || []).slice();
    if (sec.anonymousReadRepos && sec.anonymousReadRepos.length) {
      notes.push('Anonymous read will be enabled on: ' + sec.anonymousReadRepos.join(', '));
    }
    el('mig-security-notes').textContent = notes.join(' — ');
    el('mig-security').style.display = '';
  }

  function renderRun(st) {
    var panel = el('mig-run-panel');
    var applyBtn = el('mig-apply-btn');
    var resetBtn = el('mig-reset-btn');
    if (!st.plan) { panel.style.display = 'none'; return; }
    panel.style.display = '';
    var run = st.run;
    var chipEl = el('mig-run-chip');
    var detail = el('mig-run-detail');
    applyBtn.style.display = 'none';
    resetBtn.style.display = 'none';

    if (!run || run.status === 'never') {
      chipEl.className = 'chip chip-neutral';
      chipEl.textContent = 'plan ready';
      detail.textContent = 'Planned against ' + st.plan.sourceUrl + '. Review below, then apply.';
      if (canApply) applyBtn.style.display = '';
      resetBtn.style.display = '';
    } else if (run.status === 'queued' || run.status === 'running') {
      chipEl.className = 'chip chip-neutral';
      chipEl.textContent = run.status;
      detail.textContent = run.currentRepo ? 'Migrating ' + run.currentRepo + '…' : 'Waiting for the worker…';
      schedulePoll();
    } else if (run.status === 'complete') {
      var failedRepos = (st.repos || []).filter(function (r) { return r.status === 'failed'; }).length;
      chipEl.className = failedRepos ? 'chip chip-err' : 'chip chip-ok';
      chipEl.textContent = failedRepos ? 'complete — ' + failedRepos + ' repo(s) had failures' : 'complete';
      var d = '';
      if (run.securityApplied) {
        d = 'Roles: ' + run.securityApplied.rolesCreated + ' created · users: ' + run.securityApplied.usersCreated + ' created (disabled, set passwords to activate).';
      }
      detail.textContent = d;
      if (canApply) { applyBtn.style.display = ''; applyBtn.textContent = 'Re-run (resume)'; }
      resetBtn.style.display = '';
    } else {
      chipEl.className = 'chip chip-err';
      chipEl.textContent = run.status;
      detail.textContent = run.error || '';
      if (canApply) applyBtn.style.display = '';
      resetBtn.style.display = '';
    }
  }

  function schedulePoll() {
    if (pollTimer) return;
    pollTimer = setTimeout(function () {
      pollTimer = null;
      refresh();
    }, 2500);
  }

  function render(st) {
    if (st.spec && st.spec.url) el('mig-url').value = st.spec.url;
    if (st.spec && st.spec.username) el('mig-user').value = st.spec.username;
    if (st.plan) {
      renderRepos(st);
      renderSecurity(st);
      var notesEl = el('mig-plan-notes');
      notesEl.textContent = (st.plan.notes || []).join(' — ');
      notesEl.style.display = st.plan.notes && st.plan.notes.length ? '' : 'none';
    } else {
      el('mig-repos').style.display = 'none';
      el('mig-security').style.display = 'none';
      el('mig-plan-notes').style.display = 'none';
    }
    renderRun(st);
  }

  function refresh() {
    fetch('/api/v1/migration')
      .then(function (r) { return r.json(); })
      .then(render)
      .catch(function () {});
  }

  el('mig-plan-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var btn = el('mig-plan-btn');
    var errEl = el('mig-plan-error');
    errEl.style.display = 'none';
    btn.disabled = true;
    btn.textContent = 'Planning…';
    fetch('/api/v1/migration/plan', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        url: el('mig-url').value,
        username: el('mig-user').value,
        password: el('mig-pass').value,
        includeSecurity: el('mig-security').checked
      })
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (b) { throw new Error(b.error || 'HTTP ' + r.status); });
        return r.json();
      })
      .then(function () { refresh(); })
      .catch(function (err) {
        errEl.textContent = err.message;
        errEl.style.display = '';
      })
      .then(function () {
        btn.disabled = false;
        btn.textContent = 'Plan migration';
      });
  });

  el('mig-apply-btn').addEventListener('click', function () {
    var btn = el('mig-apply-btn');
    btn.disabled = true;
    fetch('/api/v1/migration/apply', { method: 'POST' })
      .then(function (r) {
        if (r.status !== 202) return r.json().then(function (b) { throw new Error(b.error || 'HTTP ' + r.status); });
      })
      .then(function () { refresh(); })
      .catch(function (err) { alert('Apply failed: ' + err.message); })
      .then(function () { btn.disabled = false; });
  });

  el('mig-reset-btn').addEventListener('click', function () {
    if (!confirm('Reset migration state? Migrated content and repositories stay; only the plan and progress bookkeeping are cleared.')) return;
    fetch('/api/v1/migration/reset', { method: 'POST' })
      .then(function () { window.location.reload(); })
      .catch(function () {});
  });

  refresh();
})();
