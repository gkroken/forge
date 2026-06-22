(function () {
  var root = document.getElementById('rc-root');
  var REPO = root ? root.dataset.repo : '';
  var KIND = root ? root.dataset.kind : '';

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
      return '<tr>' +
        '<td><span class="badge badge-' + esc(g.role === 'admin' ? 'err' : g.role === 'write' ? 'ok' : 'ok') + '" style="padding:2px 6px;font-size:11px">' + esc(g.role) + '</span></td>' +
        '<td>' + esc(g.description) + '</td>' +
        '<td><span class="col-mono">' + esc(g.type) + '</span></td>' +
        '</tr>';
    }).join('');
    el.innerHTML =
      '<table class="admin-table">' +
        '<thead><tr><th>Role</th><th>Token</th><th>Type</th></tr></thead>' +
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
    initAccessTab();
    initActivityTab();
  });
})();
