// browse.js — 3-panel repo browse. Loaded as external script; no inline JS allowed (CSP).
// Reads data-repo from .browse-shell so the template can inject the repo name safely.

const REPO = document.querySelector('.browse-shell').dataset.repo;
let allPkgs = [];

// ── Package list ──────────────────────────────────────────────────────────────
async function loadPkgs() {
  try {
    const res = await fetch('/api/v1/repos/' + REPO + '/components?limit=200');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    allPkgs = d.components || [];
    renderPkgs(allPkgs);
  } catch (e) {
    document.getElementById('pkg-list').innerHTML =
      '<div class="browse-msg browse-err">Failed to load: ' + esc(String(e)) + '</div>';
  }
}

function renderPkgs(pkgs) {
  const el = document.getElementById('pkg-list');
  if (!pkgs.length) {
    el.innerHTML = '<div class="browse-msg">No packages in this repository.</div>';
    return;
  }
  el.innerHTML = pkgs.map(p =>
    '<div class="browse-pkg" data-name="' + esc(p.name) + '">' +
    '<span class="ms browse-pkg-icon">package_2</span>' +
    '<span class="browse-pkg-name">' + esc(p.name) + '</span>' +
    '<span class="browse-pkg-ver">' + esc((p.versions && p.versions[0]) || '') + '</span>' +
    '</div>'
  ).join('');
}

function filterPkgs(q) {
  const ql = q.toLowerCase();
  renderPkgs(ql ? allPkgs.filter(p => p.name.toLowerCase().includes(ql)) : allPkgs);
}

// ── Versions ──────────────────────────────────────────────────────────────────
async function selectPkg(pkg, el) {
  document.querySelectorAll('.browse-pkg').forEach(i => i.classList.remove('active'));
  if (el) el.classList.add('active');
  const cp = document.getElementById('center-pane');
  cp.innerHTML = '<div class="browse-msg">Loading…</div>';
  document.getElementById('detail-pane').innerHTML =
    '<div class="browse-placeholder"><span class="ms" style="font-size:40px;color:var(--text-muted)">info</span><p>Select a version.</p></div>';
  try {
    const res = await fetch('/ui/browse/' + REPO + '/versions?pkg=' + encodeURIComponent(pkg));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    renderVersions(d);
  } catch (e) {
    cp.innerHTML = '<div class="browse-msg browse-err">Failed to load versions: ' + esc(String(e)) + '</div>';
  }
}

function renderVersions(d) {
  const cp = document.getElementById('center-pane');
  if (!d.versions || !d.versions.length) {
    cp.innerHTML = '<div class="browse-msg">No versions found.</div>';
    return;
  }
  let h = '<div class="browse-ver-title">' + esc(d.name) + '</div>';
  h += '<table class="browse-ver-tbl"><thead><tr><th>Version</th><th>Published</th></tr></thead><tbody>';
  for (const v of d.versions) {
    h += '<tr class="browse-ver-row" data-pkg="' + esc(d.pkg) + '" data-ver="' + esc(v.version) + '">';
    h += '<td class="col-mono">' + esc(v.version) + '</td>';
    h += '<td class="col-date">' + (v.published_at ? v.published_at.substring(0, 10) : '—') + '</td>';
    h += '</tr>';
  }
  h += '</tbody></table>';
  cp.innerHTML = h;
}

// ── Asset detail ──────────────────────────────────────────────────────────────
async function selectVer(pkg, ver, el) {
  document.querySelectorAll('.browse-ver-row').forEach(r => r.classList.remove('active'));
  if (el) el.classList.add('active');
  const dp = document.getElementById('detail-pane');
  dp.innerHTML = '<div class="browse-msg">Loading…</div>';
  try {
    const res = await fetch('/ui/browse/' + REPO + '/detail?pkg=' + encodeURIComponent(pkg) + '&ver=' + encodeURIComponent(ver));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    renderDetail(d);
  } catch (e) {
    dp.innerHTML = '<div class="browse-msg browse-err">Failed to load detail: ' + esc(String(e)) + '</div>';
  }
}

function renderDetail(d) {
  let h = '<div class="browse-detail">';
  h += '<div class="browse-detail-ver">' + esc(d.version) + '</div>';
  h += '<div class="browse-detail-pkg">' + esc(d.name) + '</div>';
  if (d.download_url) {
    h += '<div class="browse-detail-actions">';
    h += '<a href="' + esc(d.download_url) + '" class="btn btn-sm">↓ Download</a>';
    h += '<button class="btn btn-sm" id="copy-url-btn">⧉ Copy URL</button>';
    h += '</div>';
  }
  const rows = [
    ['Format',     d.format],
    ['Repository', d.repo],
    ['Published',  d.published_at ? d.published_at.substring(0, 10) : null],
  ];
  h += '<dl class="browse-meta">';
  for (const [k, v] of rows) {
    if (v) h += '<dt>' + esc(k) + '</dt><dd>' + esc(String(v)) + '</dd>';
  }
  h += '</dl></div>';
  document.getElementById('detail-pane').innerHTML = h;

  const copyBtn = document.getElementById('copy-url-btn');
  if (copyBtn) {
    copyBtn.addEventListener('click', function() {
      navigator.clipboard.writeText(d.download_url);
    });
  }
}

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

// ── Event delegation (no inline handlers) ────────────────────────────────────
document.getElementById('pkg-search').addEventListener('input', function() {
  filterPkgs(this.value);
});

document.getElementById('pkg-list').addEventListener('click', function(e) {
  const pkg = e.target.closest('.browse-pkg');
  if (pkg) selectPkg(pkg.dataset.name, pkg);
});

document.getElementById('center-pane').addEventListener('click', function(e) {
  const row = e.target.closest('.browse-ver-row');
  if (row) selectVer(row.dataset.pkg, row.dataset.ver, row);
});

loadPkgs();
