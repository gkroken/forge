/**
 * browse.js — 3-panel repository browse.
 *
 * The left pane is FORMAT-AWARE. Maven's blob layout is a real directory
 * hierarchy (groupId path segments → artifactId), so it gets a folder-tree
 * browser. Every other format (npm, helm, cran, oci) stores flat named
 * packages with no meaningful folder structure, so they get a searchable
 * flat package list instead.
 *
 *   FORMAT === "maven"  → initTreeBrowse()
 *                         uses GET /ui/browse/{repo}/tree?prefix=
 *                         folders expand/collapse inline; leaf click → versions
 *
 *   FORMAT !== "maven"  → initFlatBrowse()
 *                         uses GET /api/v1/repos/{repo}/components?limit=200
 *                         client-side text filter via the search input
 *
 * Center pane: version list (GET /ui/browse/{repo}/versions?pkg=)
 * Right pane:  asset detail  (GET /ui/browse/{repo}/detail?pkg=&ver=)
 *
 * No inline onclick/oninput attributes — event delegation throughout so the
 * page's CSP (script-src 'self') is satisfied.
 */

const shell  = document.querySelector('.browse-shell');
const REPO   = shell.dataset.repo;
const FORMAT = shell.dataset.format;

// ── Format-aware init ─────────────────────────────────────────────────────────
if (FORMAT === 'maven') {
  document.querySelector('.browse-search-wrap').style.display = 'none';
  initTreeBrowse();
} else {
  initFlatBrowse();
}

// ════════════════════════════════════════════════════════════════════════════════
// FLAT BROWSE (npm / helm / cran / oci)
// ════════════════════════════════════════════════════════════════════════════════

let allPkgs = [];

function initFlatBrowse() {
  document.getElementById('pkg-search').addEventListener('input', function() {
    filterPkgs(this.value);
  });
  document.getElementById('pkg-list').addEventListener('click', function(e) {
    const item = e.target.closest('.browse-pkg');
    if (item) selectPkg(item.dataset.name, item);
  });
  loadPkgs();
}

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

// ════════════════════════════════════════════════════════════════════════════════
// TREE BROWSE (maven)
// ════════════════════════════════════════════════════════════════════════════════

function initTreeBrowse() {
  // Event delegation: folder toggle or leaf selection anywhere in the list.
  document.getElementById('pkg-list').addEventListener('click', function(e) {
    const node = e.target.closest('.browse-tree-node');
    if (!node) return;
    if (node.dataset.isDir === 'true') {
      toggleTreeFolder(node);
    } else {
      selectTreeLeaf(node);
    }
  });
  loadTreeLevel('', 0, document.getElementById('pkg-list'));
}

async function loadTreeLevel(prefix, depth, container) {
  container.innerHTML = '<div class="browse-msg">Loading…</div>';
  try {
    const res = await fetch('/ui/browse/' + REPO + '/tree?prefix=' + encodeURIComponent(prefix));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const nodes = await res.json();
    renderTreeNodes(nodes, depth, container, /* replace */ true);
  } catch (e) {
    container.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
  }
}

function renderTreeNodes(nodes, depth, container, replace) {
  if (!nodes.length) {
    if (replace) container.innerHTML = '<div class="browse-msg">Empty.</div>';
    return;
  }
  const indent = 12 + depth * 16;
  const html = nodes.map(n => {
    const icon = n.is_dir ? 'folder' : 'package_2';
    return '<div class="browse-tree-node" ' +
      'data-path="' + esc(n.path) + '" ' +
      'data-is-dir="' + n.is_dir + '" ' +
      'data-depth="' + depth + '" ' +
      'style="padding-left:' + indent + 'px">' +
      (n.is_dir
        ? '<span class="ms browse-tree-toggle">chevron_right</span>'
        : '<span class="browse-tree-toggle-spacer"></span>') +
      '<span class="ms browse-tree-icon">' + icon + '</span>' +
      '<span class="browse-tree-name">' + esc(n.name) + '</span>' +
      '</div>' +
      // placeholder for children (collapsed by default)
      (n.is_dir ? '<div class="browse-tree-children" data-for="' + esc(n.path) + '" style="display:none"></div>' : '');
  }).join('');

  if (replace) {
    container.innerHTML = html;
  } else {
    container.insertAdjacentHTML('beforeend', html);
  }
}

async function toggleTreeFolder(node) {
  const path = node.dataset.path;
  const depth = parseInt(node.dataset.depth, 10);
  const children = document.querySelector('.browse-tree-children[data-for="' + CSS.escape(path) + '"]');
  if (!children) return;

  const toggle = node.querySelector('.browse-tree-toggle');
  const icon   = node.querySelector('.browse-tree-icon');
  const isOpen = children.style.display !== 'none';

  if (isOpen) {
    // Collapse
    children.style.display = 'none';
    toggle.textContent = 'chevron_right';
    icon.textContent   = 'folder';
  } else {
    // Expand — fetch children if not yet loaded
    toggle.textContent = 'expand_more';
    icon.textContent   = 'folder_open';
    children.style.display = 'block';
    if (!children.dataset.loaded) {
      children.innerHTML = '<div class="browse-msg" style="padding-left:' + (12 + (depth + 1) * 16) + 'px">Loading…</div>';
      try {
        const res = await fetch('/ui/browse/' + REPO + '/tree?prefix=' + encodeURIComponent(path));
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const nodes = await res.json();
        children.innerHTML = '';
        renderTreeNodes(nodes, depth + 1, children, false);
        children.dataset.loaded = '1';
      } catch (e) {
        children.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
      }
    }
  }
}

function selectTreeLeaf(node) {
  document.querySelectorAll('.browse-tree-node').forEach(n => n.classList.remove('active'));
  node.classList.add('active');
  // Use the leaf path as the package identifier for the versions endpoint.
  selectPkg(node.dataset.path, null);
}

// ════════════════════════════════════════════════════════════════════════════════
// SHARED: versions + detail panes
// ════════════════════════════════════════════════════════════════════════════════

async function selectPkg(pkg, el) {
  if (el) {
    document.querySelectorAll('.browse-pkg').forEach(i => i.classList.remove('active'));
    el.classList.add('active');
  }
  const cp = document.getElementById('center-pane');
  cp.innerHTML = '<div class="browse-msg">Loading…</div>';
  document.getElementById('detail-pane').innerHTML =
    '<div class="browse-placeholder">' +
    '<span class="ms" style="font-size:40px;color:var(--text-muted)">info</span>' +
    '<p>Select a version.</p></div>';
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

document.getElementById('center-pane').addEventListener('click', function(e) {
  const row = e.target.closest('.browse-ver-row');
  if (row) selectVer(row.dataset.pkg, row.dataset.ver, row);
});

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

// ── Shared util ───────────────────────────────────────────────────────────────
function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
