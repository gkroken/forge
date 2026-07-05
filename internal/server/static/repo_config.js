(function () {
  var root = document.getElementById('rc-root');
  var REPO = root ? root.dataset.repo : '';
  var KIND = root ? root.dataset.kind : '';
  var FORMAT = root ? root.dataset.format : '';
  // Formats with a credible OSV source — the only ones the download gate acts on.
  var GATEABLE = FORMAT === 'npm' || FORMAT === 'maven';

  // ── HTML escape helpers ────────────────────────────────────────────────────
  function esc(s) {
    return String(s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }
  function escAttr(s) { return String(s).replace(/'/g, '&#39;').replace(/"/g, '&quot;'); }
  // Compact severity pill, mirroring browse.js sevBadge() / the Go helper.
  // '' for a falsy severity so callers append unconditionally.
  function sevBadge(sev) {
    if (!sev) return '';
    var s = String(sev).toLowerCase();
    return '<span class="badge badge-sev sev-' + esc(s) + '" title="worst severity: ' + esc(s) + '">' + esc(s) + '</span>';
  }

  // ── Toast ──────────────────────────────────────────────────────────────────
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

  // ── Confirm modal ──────────────────────────────────────────────────────────
  var modalEl;
  function confirmModal(title, body, onConfirm) {
    if (!modalEl) {
      modalEl = document.createElement('div');
      modalEl.className = 'modal-overlay hidden';
      modalEl.innerHTML =
        '<div class="modal-box">' +
          '<div class="modal-title" id="rc-modal-title"></div>' +
          '<div class="modal-body"  id="rc-modal-body"></div>' +
          '<div class="modal-footer">' +
            '<button class="btn" id="rc-modal-cancel">Cancel</button>' +
            '<button class="btn btn-danger" id="rc-modal-confirm">Confirm</button>' +
          '</div>' +
        '</div>';
      document.body.appendChild(modalEl);
      document.getElementById('rc-modal-cancel').addEventListener('click', closeModal);
      modalEl.addEventListener('click', function (e) { if (e.target === modalEl) closeModal(); });
    }
    document.getElementById('rc-modal-title').textContent = title;
    document.getElementById('rc-modal-body').textContent  = body;
    modalEl.classList.remove('hidden');
    document.getElementById('rc-modal-confirm').onclick = function () { closeModal(); onConfirm(); };
  }
  function closeModal() { if (modalEl) modalEl.classList.add('hidden'); }

  // ── Cache chart (proxy repos only) ────────────────────────────────────────
  function initCacheChart() {
    if (KIND !== 'proxy') return;
    var card = document.getElementById('cache-chart-card');
    if (!card) return;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/cache-stats')
      .then(function (r) { return r.json(); })
      .then(renderCacheChart)
      .catch(function () {
        var el = document.getElementById('cache-chart-headline');
        if (el) el.textContent = '—';
      });
  }

  function renderCacheChart(d) {
    var headline = document.getElementById('cache-chart-headline');
    var barsEl   = document.getElementById('cache-chart-bars');
    var statsEl  = document.getElementById('cache-chart-stats');
    if (!headline || !barsEl) return;

    var pct = typeof d.hit_rate_24h === 'number' ? (d.hit_rate_24h * 100).toFixed(1) + '%' : '—';
    headline.textContent = pct;

    var hourly = d.hourly || [];
    var maxTotal = 1;
    hourly.forEach(function (b) {
      var t = (b.hits || 0) + (b.misses || 0);
      if (t > maxTotal) maxTotal = t;
    });
    barsEl.innerHTML = '';
    hourly.forEach(function (b) {
      var hits   = b.hits   || 0;
      var misses = b.misses || 0;
      var total  = hits + misses;
      var hPct   = total > 0 ? Math.round(hits / total * 100) : 0;
      var barH   = Math.max(2, Math.round(total / maxTotal * 100));
      var col    = document.createElement('div');
      col.style.cssText = 'flex:1;display:flex;flex-direction:column;justify-content:flex-end;height:100%;';
      var bar = document.createElement('div');
      bar.style.cssText = 'height:' + barH + '%;border-radius:2px 2px 0 0;' +
        'background:linear-gradient(180deg,color-mix(in srgb,var(--accent) 70%,#fff),var(--accent));opacity:.85;';
      bar.title = 'Hour ' + b.hour + ': ' + hPct + '% hit (' + hits + '/' + total + ')';
      col.appendChild(bar);
      barsEl.appendChild(col);
    });

    if (statsEl) {
      var revs = d.revalidations || 0;
      var negs = d.negatives     || 0;
      statsEl.innerHTML =
        '<div class="rail-stat-row"><span class="rail-stat-label">Revalidations</span>' +
          '<span class="rail-stat-val">' + esc(revs) + '</span></div>' +
        '<div class="rail-stat-row"><span class="rail-stat-label">Negative cache</span>' +
          '<span class="rail-stat-val">' + esc(negs) + '</span></div>';
    }
  }

  // ── Circuit breaker chip ───────────────────────────────────────────────────
  function initCircuitBreaker() {
    var chip = document.getElementById('cb-chip');
    if (!chip || KIND !== 'proxy') return;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/health')
      .then(function (r) { return r.json(); })
      .then(function (d) {
        var state = d.state || 'Unknown';
        chip.textContent = state;
        chip.className = 'chip ' + (state === 'Closed' ? 'chip-ok' : state === 'Open' ? 'chip-err' : 'chip-neutral');
      })
      .catch(function () { chip.textContent = 'Unknown'; chip.className = 'chip chip-neutral'; });
  }

  // ── Action buttons ─────────────────────────────────────────────────────────
  function initActionButtons() {
    var invalidateBtn = document.getElementById('btn-invalidate');
    if (invalidateBtn) {
      invalidateBtn.addEventListener('click', function () {
        confirmModal(
          'Invalidate proxy cache',
          'Delete all cached artifacts for "' + REPO + '"? Clients will re-fetch from upstream on next request.',
          function () {
            fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/invalidate', { method: 'POST' })
              .then(function (r) { return r.json(); })
              .then(function (d) {
                toast('Deleted ' + d.deleted + ' cache entr' + (d.deleted === 1 ? 'y' : 'ies'), 'ok');
              })
              .catch(function () { toast('Cache invalidation failed', 'err'); });
          }
        );
      });
    }

    var runCleanupBtn = document.getElementById('btn-run-cleanup');
    if (runCleanupBtn) {
      runCleanupBtn.addEventListener('click', function () {
        confirmModal(
          'Run cleanup now',
          'Apply the retention policy to "' + REPO + '" and permanently delete matching artifacts? This cannot be undone. Use Dry-run first to preview.',
          function () {
            runCleanupBtn.disabled = true;
            var el = document.getElementById('cleanup-result');
            if (el) el.textContent = 'Running…';
            fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/cleanup', { method: 'POST' })
              .then(function (r) { return r.json(); })
              .then(function (d) {
                if (el) el.textContent = 'Done: ' + d.deleted + ' artifact(s) deleted, ' + ((d.freed_bytes || 0) / 1048576).toFixed(2) + ' MB freed';
                toast('Cleanup complete — ' + d.deleted + ' deleted', 'ok');
              })
              .catch(function (e) { if (el) el.textContent = 'Error: ' + e; toast('Cleanup failed', 'err'); })
              .finally(function () { runCleanupBtn.disabled = false; });
          }
        );
      });
    }

    var reindexBtn = document.getElementById('btn-reindex');
    if (reindexBtn) {
      reindexBtn.addEventListener('click', function () {
        confirmModal(
          'Rebuild index',
          'Queue an index rebuild for "' + REPO + '"?',
          function () {
            fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/reindex', { method: 'POST' })
              .then(function (r) { return r.json(); })
              .then(function () { toast('Index rebuild queued', 'ok'); })
              .catch(function () { toast('Reindex failed', 'err'); });
          }
        );
      });
    }
  }

  // ── Content tab ────────────────────────────────────────────────────────────
  function initContentTab() {
    var listEl = document.getElementById('content-pkg-list');
    if (!listEl) return;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/components?limit=0')
      .then(function (r) { return r.json(); })
      .then(function (d) { renderContentList(listEl, d.components || []); })
      .catch(function () {
        listEl.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text-muted)">Failed to load content.</div>';
      });
    // CSP-safe event delegation for the per-version Promote button (the version
    // rows are rendered lazily as packages expand).
    listEl.addEventListener('click', function (e) {
      var pb = e.target.closest('[data-promote-pkg]');
      if (pb) openPromoteModal(pb.getAttribute('data-promote-pkg'), pb.getAttribute('data-promote-ver'));
    });
    initTrash();
  }

  // ── Promote (copy to another hosted repo of the same format) ────────────────
  function openPromoteModal(pkg, ver) {
    fetch('/api/v1/repos')
      .then(function (r) { return r.json(); })
      .then(function (repos) {
        var targets = (repos || []).filter(function (rp) {
          return rp.kind === 'hosted' && rp.format === FORMAT && rp.name !== REPO;
        });
        if (!targets.length) {
          toast('No eligible target: need another hosted ' + FORMAT + ' repository', 'err');
          return;
        }
        promoteModal(pkg, ver, targets);
      })
      .catch(function () { toast('Could not load target repositories', 'err'); });
  }

  var promoteEl;
  function promoteModal(pkg, ver, targets) {
    if (!promoteEl) {
      promoteEl = document.createElement('div');
      promoteEl.className = 'modal-overlay hidden';
      promoteEl.innerHTML =
        '<div class="modal-box">' +
          '<div class="modal-title">Promote artifact</div>' +
          '<div class="modal-body">' +
            '<p style="margin:0 0 12px;font-size:13px;color:var(--text-muted)">' +
              'Copy <strong id="pr-label"></strong> into another hosted ' + esc(FORMAT) +
              ' repository. The bytes are copied through the target’s format handler and provenance is recorded; the source is unchanged.</p>' +
            '<label style="display:block;font-size:12px;margin-bottom:6px" for="pr-target">Target repository</label>' +
            '<select id="pr-target" style="width:100%"></select>' +
          '</div>' +
          '<div class="modal-footer">' +
            '<button class="btn" id="pr-cancel">Cancel</button>' +
            '<button class="btn btn-primary" id="pr-confirm">Promote</button>' +
          '</div>' +
        '</div>';
      document.body.appendChild(promoteEl);
      document.getElementById('pr-cancel').addEventListener('click', function () { promoteEl.classList.add('hidden'); });
      promoteEl.addEventListener('click', function (e) { if (e.target === promoteEl) promoteEl.classList.add('hidden'); });
    }
    document.getElementById('pr-label').textContent = pkg + ' @ ' + ver;
    var sel = document.getElementById('pr-target');
    sel.innerHTML = targets.map(function (t) {
      var imm = t.immutable ? ' — immutable' : '';
      return '<option value="' + escAttr(t.name) + '">' + esc(t.name) + esc(imm) + '</option>';
    }).join('');
    promoteEl.classList.remove('hidden');
    document.getElementById('pr-confirm').onclick = function () {
      doPromote(sel.value, pkg, ver, this);
    };
  }

  function doPromote(target, pkg, ver, btn) {
    btn.disabled = true;
    fetch('/api/v1/repos/' + encodeURIComponent(target) + '/promote', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sourceRepo: REPO, component: pkg, version: ver }),
    })
      .then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
      .then(function (res) {
        btn.disabled = false;
        if (!res.ok) {
          toast('Promote failed: ' + (res.d.error || 'error'), 'err');
          return;
        }
        promoteEl.classList.add('hidden');
        toast('Promoted ' + pkg + '@' + ver + ' → ' + target, 'ok');
      })
      .catch(function () { btn.disabled = false; toast('Promote request failed', 'err'); });
  }

  // ── Trash (soft-delete) ──────────────────────────────────────────────────────
  function humanKB(n) {
    n = n || 0;
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1073741824) return (n / 1048576).toFixed(1) + ' MB';
    return (n / 1073741824).toFixed(2) + ' GB';
  }

  function initTrash() {
    var card = document.getElementById('trash-card');
    var listEl = document.getElementById('trash-list');
    if (!card || !listEl) return;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/trash')
      .then(function (r) { return r.json(); })
      .then(function (d) { renderTrash(card, listEl, d.trash || []); })
      .catch(function () { card.style.display = 'none'; });
  }

  function renderTrash(card, listEl, items) {
    if (!items.length) { card.style.display = 'none'; return; }
    card.style.display = '';
    listEl.innerHTML = '';
    items.forEach(function (t) {
      var when = t.deletedAt ? new Date(t.deletedAt).toLocaleString() : '';
      var row = document.createElement('div');
      row.className = 'content-ver-row';
      row.innerHTML =
        '<span class="content-ver-tag">' + esc(t.component) + ' ' + esc(t.version) + '</span>' +
        '<span class="content-pkg-meta" style="flex:1">' + humanKB(t.bytes) +
          (t.deletedBy ? ' · by ' + esc(t.deletedBy) : '') +
          (when ? ' · ' + esc(when) : '') + '</span>' +
        '<span class="content-ver-actions">' +
          '<button class="btn btn-sm" data-trash-restore="' + escAttr(t.id) + '">Restore</button>' +
          '<button class="btn btn-sm btn-danger" data-trash-purge="' + escAttr(t.id) + '">Purge</button>' +
        '</span>';
      listEl.appendChild(row);
    });
  }

  function initTrashActions() {
    var listEl = document.getElementById('trash-list');
    if (listEl && !listEl.dataset.wired) {
      listEl.dataset.wired = '1';
      listEl.addEventListener('click', function (e) {
        var rb = e.target.closest('[data-trash-restore]');
        var pb = e.target.closest('[data-trash-purge]');
        if (rb) { trashRestore(rb.getAttribute('data-trash-restore'), rb); }
        else if (pb) { trashPurge(pb.getAttribute('data-trash-purge'), pb); }
      });
    }
    var purgeAll = document.getElementById('trash-purge-all');
    if (purgeAll && !purgeAll.dataset.wired) {
      purgeAll.dataset.wired = '1';
      purgeAll.addEventListener('click', function () {
        confirmModal('Purge all trash',
          'Permanently delete every trashed artifact in "' + REPO + '"? This cannot be undone.',
          function () {
            fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/trash/purge?id=all', { method: 'POST' })
              .then(function (r) { return r.ok ? r.json() : Promise.reject(); })
              .then(function (d) { toast('Purged ' + (d.purged || 0) + ' item(s)', 'ok'); initTrash(); })
              .catch(function () { toast('Purge failed', 'err'); });
          });
      });
    }
  }

  function trashRestore(id, btn) {
    btn.disabled = true;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/trash/restore?id=' + encodeURIComponent(id), { method: 'POST' })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(); })
      .then(function (d) {
        toast('Restored ' + d.component + ' ' + d.version, 'ok');
        initTrash();
        initContentTab();
      })
      .catch(function () { toast('Restore failed', 'err'); btn.disabled = false; });
  }

  function trashPurge(id, btn) {
    confirmModal('Purge from trash',
      'Permanently delete this artifact? This cannot be undone.',
      function () {
        btn.disabled = true;
        fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/trash/purge?id=' + encodeURIComponent(id), { method: 'POST' })
          .then(function (r) { return r.ok ? r.json() : Promise.reject(); })
          .then(function () { toast('Purged', 'ok'); initTrash(); })
          .catch(function () { toast('Purge failed', 'err'); btn.disabled = false; });
      });
  }

  function renderContentList(listEl, components) {
    var searchInput = document.getElementById('content-search');

    if (!components.length) {
      listEl.innerHTML = '<div style="padding:30px;text-align:center;color:var(--text-muted);font-size:13px">No artifacts stored in this repository.</div>';
      return;
    }

    function render(filter) {
      listEl.innerHTML = '';
      var filtered = filter
        ? components.filter(function (c) { return c.name.toLowerCase().indexOf(filter.toLowerCase()) !== -1; })
        : components;

      if (!filtered.length) {
        listEl.innerHTML = '<div style="padding:20px;text-align:center;color:var(--text-muted);font-size:13px">No packages match "' + esc(filter) + '".</div>';
        return;
      }

      filtered.forEach(function (c) {
        var verCount = (c.versions || []).length;
        var row = document.createElement('div');
        row.className = 'content-pkg-row';
        row.innerHTML =
          '<span class="ms" style="font-size:16px;color:var(--text-muted)">chevron_right</span>' +
          '<span class="content-pkg-name">' + esc(c.name) + '</span>' +
          sevBadge(c.severity) +
          '<span class="content-pkg-meta">' + verCount + ' version' + (verCount !== 1 ? 's' : '') + '</span>';

        var verList = document.createElement('div');
        verList.className = 'content-ver-list';
        verList.style.display = 'none';

        row.addEventListener('click', function () {
          var open = verList.style.display !== 'none';
          verList.style.display = open ? 'none' : '';
          row.querySelector('.ms').textContent = open ? 'chevron_right' : 'expand_more';
          if (!open && !verList.dataset.loaded) {
            verList.dataset.loaded = '1';
            loadVersions(verList, c.name);
          }
        });

        listEl.appendChild(row);
        listEl.appendChild(verList);
      });
    }

    render('');
    if (searchInput) {
      searchInput.addEventListener('input', function () { render(this.value); });
    }
  }

  function loadVersions(container, pkg) {
    container.innerHTML = '<div style="padding:8px 14px;font-size:12px;color:var(--text-muted)">Loading…</div>';
    fetch('/ui/browse/' + encodeURIComponent(REPO) + '/versions?pkg=' + encodeURIComponent(pkg))
      .then(function (r) { return r.json(); })
      .then(function (d) { renderVersionRows(container, pkg, d.versions || []); })
      .catch(function () {
        container.innerHTML = '<div style="padding:8px 14px;color:var(--text-muted);font-size:12px">Failed to load versions.</div>';
      });
  }

  function renderVersionRows(container, pkg, versions) {
    container.innerHTML = '';
    if (!versions.length) {
      container.innerHTML = '<div style="padding:8px 14px;font-size:12px;color:var(--text-muted)">No versions found.</div>';
      return;
    }
    versions.forEach(function (v) {
      var ver = (typeof v === 'object' && v.version) ? v.version : String(v);
      var dl = (typeof v === 'object' && v.download_url) ? v.download_url : '';
      var copyURL = dl || (window.location.origin + '/repository/' + encodeURIComponent(REPO) + '/' + encodeURIComponent(pkg));
      var row = document.createElement('div');
      row.className = 'content-ver-row';
      var vsev = (typeof v === 'object' && v.severity) ? v.severity : '';
      row.innerHTML =
        '<span class="content-ver-tag">' + esc(ver) + '</span>' +
        sevBadge(vsev) +
        '<span class="content-ver-actions">' +
          '<button class="btn btn-sm" onclick="rcCopyURL(\'' + escAttr(copyURL) + '\')">Copy URL</button>' +
          (KIND === 'hosted'
            ? '<button class="btn btn-sm" data-promote-pkg="' + escAttr(pkg) + '" data-promote-ver="' + escAttr(ver) + '">Promote…</button>'
            : '') +
          (KIND === 'proxy'
            ? '<button class="btn btn-sm" onclick="rcExpireCache(\'' + escAttr(pkg) + '\',\'' + escAttr(ver) + '\',this)">Expire</button>'
            : '') +
          '<button class="btn btn-sm btn-danger" onclick="rcDeleteVersion(\'' + escAttr(pkg) + '\',\'' + escAttr(ver) + '\',this)">Delete</button>' +
        '</span>';
      container.appendChild(row);
    });
  }

  // ── Access tab ─────────────────────────────────────────────────────────────
  function initAccessTab() {
    var el = document.getElementById('access-content');
    if (!el) return;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/access')
      .then(function (r) { return r.json(); })
      .then(function (grants) { renderAccessTab(el, grants); })
      .catch(function () {
        el.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text-muted)">Failed to load access grants.</div>';
      });
  }

  function renderAccessTab(el, grants) {
    if (!grants.length) {
      el.innerHTML =
        '<div style="padding:30px;text-align:center;color:var(--text-muted);font-size:13px">' +
          'No tokens grant access to this repository. ' +
          '<a href="/ui/admin/tokens" style="color:var(--accent)">Manage tokens</a>' +
        '</div>';
      return;
    }
    var rows = grants.map(function (g) {
      // g.role is the comma-joined action list ("read,write"); render one
      // scope badge per action, plus the selector list when present.
      var badges = String(g.role || '').split(',').map(function (a) {
        a = a.trim();
        return a ? '<span class="scope-badge scope-' + esc(a) + '">' + esc(a) + '</span>' : '';
      }).join(' ');
      var sel = (g.selectors && g.selectors.length)
        ? '<div class="grant-sel">' + esc(g.selectors.join(', ')) + '</div>' : '';
      return '<tr>' +
        '<td><div class="grant-line">' + badges + sel + '</div></td>' +
        '<td>' + esc(g.description) + '</td>' +
        '<td><span class="col-mono">' + esc(g.type) + '</span></td>' +
        '</tr>';
    }).join('');
    el.innerHTML =
      '<table class="admin-table">' +
        '<thead><tr><th>Actions</th><th>Token</th><th>Type</th></tr></thead>' +
        '<tbody>' + rows + '</tbody>' +
      '</table>' +
      '<div style="padding:12px 18px;border-top:1px solid var(--border);font-size:12px;color:var(--text-muted)">' +
        '<a href="/ui/admin/tokens" style="color:var(--accent)">Manage tokens →</a>' +
      '</div>';
  }

  // ── Activity tab ───────────────────────────────────────────────────────────
  function initActivityTab() {
    var tbody = document.getElementById('activity-tbody');
    if (!tbody) return;
    fetch('/api/v1/audit?repo=' + encodeURIComponent(REPO) + '&limit=50')
      .then(function (r) { return r.json(); })
      .then(function (d) { renderActivityTable(tbody, d); })
      .catch(function () {
        tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:20px;color:var(--text-muted)">Failed to load activity.</td></tr>';
      });
  }

  function renderActivityTable(tbody, entries) {
    if (!entries.length) {
      tbody.innerHTML = '<tr><td colspan="5" style="text-align:center;padding:24px;color:var(--text-muted)">No activity recorded for this repository.</td></tr>';
      return;
    }
    tbody.innerHTML = entries.map(function (e) {
      return '<tr>' +
        '<td class="act-time">' + esc(e.time) + '</td>' +
        '<td><span class="act-actor-init" title="' + escAttr(e.actor) + '">' + esc(e.initials) + '</span></td>' +
        '<td><code style="font-size:11px">' + esc(e.method) + '</code></td>' +
        '<td style="max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11px;font-family:\'IBM Plex Mono\',monospace">' + esc(e.path) + '</td>' +
        '<td><span class="badge ' + (e.ok ? 'badge-ok' : 'badge-err') + '" style="padding:1px 5px;font-size:10px">' + esc(e.status) + '</span></td>' +
        '</tr>';
    }).join('');
  }

  // ── Integrity tab ──────────────────────────────────────────────────────────
  // Finding kinds → severity token classes (mirrors ui_integrity.go):
  // missing/mismatch mean data loss or corruption, orphan is hygiene, drift is
  // regenerable via reindex.
  var INTEG_KIND_SEV = { missing: 'critical', mismatch: 'high', orphan: 'moderate', drift: 'low' };
  function integKindChip(kind, count) {
    var sev = INTEG_KIND_SEV[kind] || 'unknown';
    var label = count != null ? count + ' ' + kind : kind;
    return '<span class="badge badge-sev sev-' + esc(sev) + '">' + esc(label) + '</span>';
  }
  function integFmtBytes(b) {
    if (!b) return '0 B';
    var units = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
    return (i === 0 ? b : b.toFixed(1)) + ' ' + units[i];
  }
  function integAgo(iso) {
    var t = new Date(iso).getTime();
    if (!t) return '';
    var s = Math.floor((Date.now() - t) / 1000);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + 'm ago';
    if (s < 172800) return Math.floor(s / 3600) + 'h ago';
    return Math.floor(s / 86400) + 'd ago';
  }

  function initIntegrityTab() {
    var el = document.getElementById('integrity-content');
    if (!el) return;
    loadIntegrity(el, 0);
  }

  function loadIntegrity(el, polls) {
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/verify')
      .then(function (r) { return r.json(); })
      .then(function (rep) { renderIntegrity(el, rep, polls); })
      .catch(function () {
        el.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text-muted);font-size:13px">Failed to load the integrity report.</div>';
      });
  }

  function renderIntegrity(el, rep, polls) {
    var status = rep.status || 'never';
    var inFlight = status === 'queued' || status === 'running';
    var html = '';

    // Header: verdict chip + verified-at + action buttons.
    var chip;
    if (inFlight) chip = '<span class="chip chip-neutral">' + esc(status) + '…</span>';
    else if (status === 'failed') chip = '<span class="chip chip-err">verify failed</span>';
    else if (status === 'never') chip = '<span class="chip chip-neutral">never verified</span>';
    else if (rep.totalFindings > 0) chip = '<span class="chip chip-err">' + rep.totalFindings + ' finding' + (rep.totalFindings === 1 ? '' : 's') + '</span>';
    else chip = '<span class="chip chip-ok">intact</span>';

    var when = '';
    if (status === 'complete' && rep.finishedAt) {
      when = '<span style="font-size:12px;color:var(--text-muted)">verified ' + esc(integAgo(rep.finishedAt)) +
        ' · ' + esc(rep.mode || 'full') + ' mode</span>';
    }
    html += '<div style="display:flex;align-items:center;gap:12px;margin-bottom:14px;flex-wrap:wrap">' +
      '<span class="rail-card-title" style="margin-bottom:0">Storage integrity</span>' + chip + when +
      '<span style="margin-left:auto;display:flex;gap:8px">' +
      '<button class="btn btn-sm" data-integ="quick"' + (inFlight ? ' disabled' : '') + '>Quick check</button>' +
      '<button class="btn btn-sm btn-primary" data-integ="full"' + (inFlight ? ' disabled' : '') + '>Verify now</button>' +
      '</span></div>';

    if (status === 'never') {
      html += '<div style="font-size:13px;color:var(--text-muted);line-height:1.55">' +
        'No integrity report yet. <strong>Verify now</strong> re-reads every artifact and re-checks its stored ' +
        'checksums; <strong>Quick check</strong> cross-references records and blobs without hashing. ' +
        'Both are read-only — findings are reported, never repaired automatically.</div>';
    } else if (status === 'failed') {
      html += '<div style="font-size:13px;color:var(--danger)">' + esc(rep.error || 'unknown error') + '</div>';
    } else if (status === 'complete') {
      // Instrument readouts.
      html += '<div class="instrument-panel" style="margin-bottom:16px"><div class="inst-readouts">' +
        integReadout('Blobs checked', rep.blobsChecked) +
        integReadout('Records checked', rep.metaChecked) +
        integReadout('Data read', integFmtBytes(rep.bytesRead)) +
        integReadout('Duration', (rep.durationMs ? (rep.durationMs < 1000 ? rep.durationMs + ' ms' : (rep.durationMs / 1000).toFixed(1) + ' s') : '< 1 ms')) +
        '</div></div>';
      if (rep.note) {
        html += '<div style="font-size:12.5px;color:var(--text-muted);margin-bottom:14px">' + esc(rep.note) + '</div>';
      }
      if (rep.totalFindings > 0) {
        var counts = rep.counts || {};
        html += '<div style="display:flex;gap:6px;margin-bottom:10px;flex-wrap:wrap">' +
          Object.keys(counts).map(function (k) { return integKindChip(k, counts[k]); }).join('') + '</div>';
        html += '<div class="admin-table-wrap"><table class="admin-table"><thead><tr>' +
          '<th style="width:90px">Kind</th><th style="min-width:220px">Object</th><th style="width:170px">Component</th><th>What happened</th>' +
          '</tr></thead><tbody>' +
          (rep.findings || []).map(function (f) {
            // The repo prefix on blob keys is redundant inside the repo's own page.
            var obj = String(f.object || '').replace(new RegExp('^' + REPO.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '/'), '');
            return '<tr><td>' + integKindChip(f.kind) + '</td>' +
              '<td class="col-mono" style="font-size:11.5px;word-break:break-all">' + esc(obj) + '</td>' +
              '<td class="col-mono" style="font-size:12px">' + esc(f.component || '—') + (f.version ? '@' + esc(f.version) : '') + '</td>' +
              '<td style="font-size:12.5px;color:var(--text-muted)">' + esc(f.detail) + '</td></tr>';
          }).join('') + '</tbody></table></div>';
        if (rep.truncated) {
          html += '<div style="font-size:12px;color:var(--text-muted);margin-top:8px">Showing the first ' +
            (rep.findings || []).length + ' of ' + rep.totalFindings + ' findings.</div>';
        }
      } else {
        html += '<div style="font-size:13px;color:var(--text-muted)">No orphans, missing blobs, or checksum mismatches.</div>';
      }
    } else if (inFlight) {
      html += '<div style="font-size:13px;color:var(--text-muted)">Verification in progress — this page updates automatically.</div>';
    }

    el.innerHTML = html;

    el.querySelectorAll('[data-integ]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var mode = btn.dataset.integ;
        btn.disabled = true;
        fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/verify?mode=' + mode, { method: 'POST' })
          .then(function (r) {
            if (r.status !== 202) throw new Error('HTTP ' + r.status);
            toast('Verify enqueued (' + mode + ')');
            setTimeout(function () { loadIntegrity(el, 0); }, 800);
          })
          .catch(function (err) { toast('Verify failed: ' + err.message, 'err'); btn.disabled = false; });
      });
    });

    // Poll while a run is in flight (bounded at ~2 minutes).
    if (inFlight && polls < 48) {
      setTimeout(function () { loadIntegrity(el, polls + 1); }, 2500);
    }
  }

  function integReadout(label, value) {
    return '<div class="inst-readout"><div class="inst-label">' + esc(label) + '</div>' +
      '<div class="inst-value">' + esc(String(value != null ? value : '—')) + '</div></div>';
  }

  // ── Security tab ───────────────────────────────────────────────────────────
  function modePill(mode) {
    var m = (mode || 'off').toLowerCase();
    var cls = m === 'block' ? 'chip-err' : m === 'warn' ? 'chip-warn' : 'chip-neutral';
    var label = m === 'block' ? 'Block' : m === 'warn' ? 'Warn' : 'Off';
    return '<span class="chip ' + cls + '">' + label + '</span>';
  }

  function initSecurityTab() {
    var el = document.getElementById('security-content');
    if (!el) return;
    Promise.all([
      fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/security-policy').then(function (r) { return r.json(); }),
      fetch('/api/v1/security-policies').then(function (r) { return r.json(); })
    ]).then(function (res) {
      renderSecurityTab(el, res[0], res[1] || []);
    }).catch(function () {
      el.innerHTML = '<div style="padding:24px;text-align:center;color:var(--text-muted);font-size:13px">' +
        'Vulnerability scanning isn’t configured, so policies have nothing to enforce.</div>';
    });
  }

  function renderSecurityTab(el, resolved, named) {
    var pol = (resolved && resolved.policy) || { mode: 'off' };
    var source = (resolved && resolved.source) || 'off';
    var assigned = source.indexOf('named:') === 0 ? source.slice(6) : '';

    var sourceLabel = source === 'off' ? 'Scanning not configured'
      : assigned ? 'Named policy “' + esc(assigned) + '”'
      : 'Global default';

    var html = '';

    if (!GATEABLE) {
      html += '<div class="alert" style="background:var(--bg-alt);border:1px solid var(--border);color:var(--text-muted);' +
        'padding:10px 14px;border-radius:6px;margin-bottom:16px;font-size:12.5px">' +
        esc(FORMAT) + ' artifacts have no vulnerability source, so this policy never gates downloads here. ' +
        'Supported formats: npm, Maven.</div>';
    }

    // Effective policy summary.
    html += '<div style="display:flex;align-items:center;gap:10px;margin-bottom:4px">' +
      '<span class="rail-card-title" style="margin-bottom:0">Effective policy</span>' + modePill(pol.mode) + '</div>';
    html += '<table class="rc-kv" style="margin:8px 0 18px;font-size:13px;border-collapse:collapse">' +
      kvRow('Source', sourceLabel) +
      (pol.mode && pol.mode !== 'off' ? (
        kvRow('Acts at severity', '<span style="text-transform:capitalize">' + esc(pol.threshold || 'high') + '</span> &amp; up') +
        kvRow('Unscanned artifacts', pol.failOpen ? 'Served (fail open)' : '<span style="color:var(--danger)">Blocked (fail closed)</span>')
      ) : '') +
      '</table>';

    // Suppressions, if any.
    var supp = pol.suppressions || [];
    if (supp.length) {
      html += '<div class="rail-card-title" style="margin-bottom:6px">Suppressed advisories</div>' +
        '<ul style="margin:0 0 18px;padding-left:18px;font-size:12.5px;color:var(--text-muted)">' +
        supp.map(function (s) {
          return '<li><code>' + esc(s.id) + '</code>' + (s.reason ? ' — ' + esc(s.reason) : '') +
            (s.by ? ' <span style="color:var(--text-light)">(' + esc(s.by) + ')</span>' : '') + '</li>';
        }).join('') + '</ul>';
    }

    // Assignment form.
    html += '<div class="rail-card-title" style="margin-bottom:6px">Policy for this repository</div>' +
      '<div class="form-group" style="max-width:420px">' +
      '<select id="sec-assign">' +
      '<option value="">— inherit global default —</option>' +
      named.map(function (p) {
        return '<option value="' + escAttr(p.name) + '"' + (p.name === assigned ? ' selected' : '') + '>' + esc(p.name) + '</option>';
      }).join('') +
      '</select></div>' +
      '<div style="display:flex;align-items:center;gap:12px;margin-top:6px">' +
      '<button class="btn btn-primary btn-sm" id="sec-save">Save</button>' +
      (GATEABLE ? '<button class="btn btn-sm" id="sec-preview">Preview impact</button>' : '') +
      '<a href="/ui/admin/security-policies" class="btn btn-sm">Manage policies</a>' +
      '</div>' +
      '<div id="sec-preview-out" style="margin-top:12px;font-size:12.5px;color:var(--text-muted)"></div>';

    el.innerHTML = html;

    var previewBtn = document.getElementById('sec-preview');
    if (previewBtn) {
      previewBtn.addEventListener('click', function () {
        var name = document.getElementById('sec-assign').value;
        var out = document.getElementById('sec-preview-out');
        out.textContent = 'Checking…';
        fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/security-policy/dry-run', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ policyName: name })
        }).then(function (r) { return r.json(); }).then(function (d) {
          out.innerHTML = renderBlastRadius(d);
        }).catch(function () { out.textContent = 'Preview failed.'; });
      });
    }

    document.getElementById('sec-save').addEventListener('click', function () {
      var name = document.getElementById('sec-assign').value;
      fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/security-policy', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ policyName: name })
      }).then(function (r) {
        if (!r.ok) throw new Error(r.status);
        return r.json();
      }).then(function (rs) {
        toast('Security policy updated', 'ok');
        renderSecurityTab(el, rs, named);
      }).catch(function () { toast('Could not update the policy', 'err'); });
    });
  }

  function renderBlastRadius(d) {
    if (d.mode === 'off' || d.mode === '') {
      return 'This policy is <strong>Off</strong> — no downloads would be gated. ' +
        esc(d.totalScanned) + ' scanned version(s) in this repository.';
    }
    if (d.mode === 'block') {
      if (!d.blockedVersions) {
        return 'Nothing would be blocked. None of the ' + esc(d.totalScanned) +
          ' scanned version(s) meet the threshold.';
      }
      return 'Switching to this policy would <strong style="color:var(--danger)">block ' +
        esc(d.blockedVersions) + ' version(s)</strong> across ' + esc(d.blockedComponents) +
        ' component(s), out of ' + esc(d.totalScanned) + ' scanned.';
    }
    // warn
    if (!d.warnedVersions) {
      return 'Nothing would be flagged. None of the ' + esc(d.totalScanned) +
        ' scanned version(s) meet the threshold.';
    }
    return 'This policy would <strong>flag ' + esc(d.warnedVersions) + ' version(s)</strong> across ' +
      esc(d.warnedComponents) + ' component(s) (served with a warning header), out of ' +
      esc(d.totalScanned) + ' scanned.';
  }

  function kvRow(k, v) {
    return '<tr><td style="padding:3px 16px 3px 0;color:var(--text-muted);white-space:nowrap">' + esc(k) +
      '</td><td style="padding:3px 0;color:var(--text)">' + v + '</td></tr>';
  }

  // ── Global helpers exposed to inline onclick handlers ──────────────────────
  window.rcCopyURL = function (url) {
    if (navigator.clipboard) {
      navigator.clipboard.writeText(url).then(function () { toast('URL copied', 'ok'); });
    } else {
      toast('Copy not supported in this browser', 'err');
    }
  };

  window.rcExpireCache = function (pkg, ver, btn) {
    btn.disabled = true;
    fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/cache/' + encodeURIComponent(pkg) + '/' + encodeURIComponent(ver), { method: 'DELETE' })
      .then(function () { toast('Cache entry expired — next request re-fetches from upstream', 'ok'); })
      .catch(function () { toast('Failed to expire cache entry', 'err'); })
      .finally(function () { btn.disabled = false; });
  };

  window.rcDeleteVersion = function (pkg, ver, btn) {
    confirmModal(
      'Delete version',
      'Permanently delete ' + pkg + ' ' + ver + ' from "' + REPO + '"? This cannot be undone.',
      function () {
        btn.disabled = true;
        fetch('/api/v1/repos/' + encodeURIComponent(REPO) + '/component?name=' + encodeURIComponent(pkg) + '&version=' + encodeURIComponent(ver), { method: 'DELETE' })
          .then(function (r) {
            if (r.ok) {
              toast('Version deleted', 'ok');
              btn.closest('.content-ver-row').remove();
            } else {
              toast('Delete failed (' + r.status + ')', 'err');
              btn.disabled = false;
            }
          })
          .catch(function () { toast('Delete failed', 'err'); btn.disabled = false; });
      }
    );
  };

  // ── init ───────────────────────────────────────────────────────────────────
  document.addEventListener('DOMContentLoaded', function () {
    initCacheChart();
    initCircuitBreaker();
    initActionButtons();
    initContentTab();
    initTrashActions();
    initAccessTab();
    initSecurityTab();
    initIntegrityTab();
    initActivityTab();
  });
})();
